//go:build linux

package bridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	inAttrib     = 0x00000004
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

const watchMask = inAttrib | inCloseWrite | inMovedFrom | inMovedTo | inCreate | inDelete | inDeleteSelf | inMoveSelf | inQOverflow | inIgnored | inOnlyDir | inDontFollow | inExclUnlink

type inotifyWatcher struct {
	root     string
	fd       int
	wdToPath map[int]string
	pathToWd map[string]int
}

func NewWatcher(root string) (Watcher, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	w := &inotifyWatcher{
		root:     root,
		fd:       fd,
		wdToPath: make(map[int]string),
		pathToWd: make(map[string]int),
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
		_, _ = syscall.InotifyRmWatch(w.fd, uint32(wd))
		delete(w.pathToWd, rel)
	}
	for wd := range w.wdToPath {
		delete(w.wdToPath, wd)
	}
	return filepath.WalkDir(w.root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(w.root, abs)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		} else {
			rel = filepath.ToSlash(rel)
		}
		if !d.IsDir() {
			return nil
		}
		if rel != "" && !IsWatchableDir(rel) {
			return filepath.SkipDir
		}
		wd, err := syscall.InotifyAddWatch(w.fd, abs, watchMask)
		if err != nil {
			return fmt.Errorf("add watch %s: %w", abs, err)
		}
		w.wdToPath[wd] = rel
		w.pathToWd[rel] = wd
		return nil
	})
}

func (w *inotifyWatcher) Run(ctx context.Context, trigger chan<- struct{}) error {
	buf := make([]byte, 1024*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := syscall.Read(w.fd, buf)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EINTR {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return err
		}
		if n == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		rebuild := false
		signal := false
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
			base := w.wdToPath[wd]
			rel := base
			if name != "" {
				if rel == "" {
					rel = name
				} else {
					rel = rel + "/" + name
				}
			}
			if mask&inIgnored != 0 {
				delete(w.pathToWd, base)
				delete(w.wdToPath, wd)
				continue
			}
			if mask&inQOverflow != 0 {
				rebuild = true
				signal = true
				continue
			}
			if mask&inIsDir != 0 {
				if rel != "" && !IsWatchableDir(rel) {
					continue
				}
				rebuild = true
				signal = true
				continue
			}
			if mask&(inCreate|inDelete|inMovedFrom|inMovedTo|inCloseWrite|inAttrib|inDeleteSelf|inMoveSelf) != 0 && IsTrackedFile(rel) {
				signal = true
			}
		}
		if rebuild {
			if err := w.Rebuild(); err != nil {
				return err
			}
		}
		if signal {
			select {
			case trigger <- struct{}{}:
			default:
			}
		}
	}
}
