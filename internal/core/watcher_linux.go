//go:build linux

package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	inCloseWrite = 0x00000008
	inMovedFrom  = 0x00000040
	inMovedTo    = 0x00000080
	inCreate     = 0x00000100
	inDelete     = 0x00000200
	inDeleteSelf = 0x00000400
	inMoveSelf   = 0x00000800
	inQOverflow  = 0x00004000
	inIgnored    = 0x00008000
	inOnlyDir    = 0x01000000
	inDontFollow = 0x02000000
	inExclUnlink = 0x04000000
	inIsDir      = 0x40000000
)

const watchMask = inCloseWrite | inMovedFrom | inMovedTo | inCreate | inDelete | inDeleteSelf | inMoveSelf | inQOverflow | inIgnored | inOnlyDir | inDontFollow | inExclUnlink

type inotifyWatcher struct {
	root      string
	fd        int
	wdToPath  map[int]string
	pathToWd  map[string]int
	removedWd map[int]struct{}
}

func NewWatcher(root string) (Watcher, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	w := &inotifyWatcher{
		root:      root,
		fd:        fd,
		wdToPath:  make(map[int]string),
		pathToWd:  make(map[string]int),
		removedWd: make(map[int]struct{}),
	}
	if err := w.Rebuild(); err != nil {
		_ = w.Close()
		return nil, err
	}
	return w, nil
}

func (w *inotifyWatcher) Close() error {
	if w.fd >= 0 {
		err := syscall.Close(w.fd)
		w.fd = -1
		return err
	}
	return nil
}

func (w *inotifyWatcher) Rebuild() error {
	for rel, wd := range w.pathToWd {
		w.removedWd[wd] = struct{}{}
		_, _ = syscall.InotifyRmWatch(w.fd, uint32(wd))
		delete(w.pathToWd, rel)
	}
	for wd := range w.wdToPath {
		delete(w.wdToPath, wd)
	}
	return w.addWatchSubtree(w.root, "")
}

func (w *inotifyWatcher) addWatch(abs, rel string) error {
	wd, err := syscall.InotifyAddWatch(w.fd, abs, watchMask)
	if err != nil {
		if rel != "" && isPathGone(err) {
			return filepath.SkipDir
		}
		return fmt.Errorf("add watch %s: %w", abs, err)
	}
	w.wdToPath[wd] = rel
	w.pathToWd[rel] = wd
	return nil
}

func (w *inotifyWatcher) addWatchSubtree(abs, rel string) error {
	if rel != "" && !IsWatchableDir(rel) {
		return filepath.SkipDir
	}
	return filepath.WalkDir(abs, func(curAbs string, d os.DirEntry, err error) error {
		if err != nil {
			if isPathGone(err) && (rel != "" || curAbs != abs) {
				return filepath.SkipDir
			}
			return err
		}
		relPath, err := filepath.Rel(w.root, curAbs)
		if err != nil {
			return err
		}
		if relPath == "." {
			relPath = ""
		} else {
			relPath = filepath.ToSlash(relPath)
		}
		if !d.IsDir() {
			return nil
		}
		if relPath != "" && !IsWatchableDir(relPath) {
			return filepath.SkipDir
		}
		if _, ok := w.pathToWd[relPath]; ok {
			return nil
		}
		return w.addWatch(curAbs, relPath)
	})
}

func (w *inotifyWatcher) removeWatchPrefix(rel string) {
	for path, wd := range w.pathToWd {
		if path != rel && !strings.HasPrefix(path, rel+"/") {
			continue
		}
		w.removedWd[wd] = struct{}{}
		_, _ = syscall.InotifyRmWatch(w.fd, uint32(wd))
		delete(w.pathToWd, path)
		delete(w.wdToPath, wd)
	}
}

func (w *inotifyWatcher) Run(ctx context.Context, trigger chan<- WatchChange) error {
	buf := make([]byte, 1024*1024)
	for {
		n, err := syscall.Read(w.fd, buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EBADF {
				return ctx.Err()
			}
			return err
		}
		if n == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}

		full := false
		changes := make([]WatchChange, 0, 16)
		offset := 0
		for offset+16 <= n {
			wd := int(int32(binary.LittleEndian.Uint32(buf[offset : offset+4])))
			mask := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
			nameLen := int(binary.LittleEndian.Uint32(buf[offset+12 : offset+16]))
			offset += 16
			if offset+nameLen > n {
				break
			}
			nameBytes := buf[offset : offset+nameLen]
			offset += nameLen
			name := strings.TrimRight(string(nameBytes), "\x00")

			if mask&inIgnored != 0 {
				if _, ok := w.removedWd[wd]; ok {
					delete(w.removedWd, wd)
					continue
				}
			}

			base := w.wdToPath[wd]
			rel := base
			if name != "" {
				if rel == "" {
					rel = name
				} else {
					rel = rel + "/" + name
				}
			}
			abs := filepath.Join(w.root, filepath.FromSlash(rel))
			isDir := mask&inIsDir != 0

			switch {
			case mask&inQOverflow != 0:
				full = true
			case mask&inIgnored != 0:
				w.removeWatchPrefix(rel)
				full = true
			case mask&(inDeleteSelf|inMoveSelf) != 0:
				if rel == "" {
					full = true
				} else {
					w.removeWatchPrefix(rel)
					changes = append(changes, WatchChange{Path: rel, IsDir: true})
				}
			case isDir && mask&(inCreate|inMovedTo) != 0:
				if rel == "" || IsWatchableDir(rel) {
					if err := w.addWatchSubtree(abs, rel); err != nil && err != filepath.SkipDir {
						return err
					}
					changes = append(changes, WatchChange{Path: rel, IsDir: true})
				}
			case isDir && mask&(inDelete|inMovedFrom) != 0:
				if rel == "" {
					full = true
				} else {
					w.removeWatchPrefix(rel)
					changes = append(changes, WatchChange{Path: rel, IsDir: true})
				}
			case !isDir && mask&(inCloseWrite|inMovedTo|inDelete|inMovedFrom) != 0:
				if IsTrackedFile(rel) {
					changes = append(changes, WatchChange{Path: rel})
				}
			}
		}

		if full {
			if err := w.Rebuild(); err != nil {
				return err
			}
			if err := sendChange(ctx, trigger, WatchChange{Full: true}); err != nil {
				return err
			}
			continue
		}
		for _, change := range changes {
			if err := sendChange(ctx, trigger, change); err != nil {
				return err
			}
		}
	}
}

func sendChange(ctx context.Context, ch chan<- WatchChange, change WatchChange) error {
	select {
	case ch <- change:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isPathGone(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOENT)
}
