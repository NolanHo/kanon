package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NolanHo/kanon/internal/protocol"
)

type ReconcileResult struct {
	Upserts int `json:"upserts"`
	Deletes int `json:"deletes"`
}

type storeMeta struct {
	Root       string `json:"root"`
	CurrentSeq int64  `json:"current_seq"`
}

type Store struct {
	mu           sync.RWMutex
	notifyMu     sync.Mutex
	notifyCh     chan struct{}
	root         string
	stateDir     string
	snapshot     map[string]protocol.FileMeta
	events       []protocol.Event
	currentSeq   int64
	metaPath     string
	snapshotPath string
	eventsPath   string
}

func OpenStore(root, stateDir string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		root:         root,
		stateDir:     stateDir,
		snapshot:     make(map[string]protocol.FileMeta),
		notifyCh:     make(chan struct{}),
		metaPath:     filepath.Join(stateDir, "meta.json"),
		snapshotPath: filepath.Join(stateDir, "snapshot.json"),
		eventsPath:   filepath.Join(stateDir, "events.jsonl"),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	if err := s.loadMeta(); err != nil {
		return err
	}
	if err := s.loadSnapshot(); err != nil {
		return err
	}
	if err := s.loadEvents(); err != nil {
		return err
	}
	return s.persistLocked()
}

func (s *Store) loadMeta() error {
	data, err := os.ReadFile(s.metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var meta storeMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("read meta: %w", err)
	}
	if meta.Root != "" && meta.Root != s.root {
		return fmt.Errorf("state root mismatch: %s != %s", meta.Root, s.root)
	}
	s.currentSeq = meta.CurrentSeq
	return nil
}

func (s *Store) loadSnapshot() error {
	data, err := os.ReadFile(s.snapshotPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snapshot map[string]protocol.FileMeta
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	s.snapshot = snapshot
	return nil
}

func (s *Store) loadEvents() error {
	file, err := os.Open(s.eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event protocol.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("read events: %w", err)
		}
		s.events = append(s.events, event)
		if event.Seq > s.currentSeq {
			s.currentSeq = event.Seq
		}
	}
	return scanner.Err()
}

func (s *Store) CurrentSeq() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentSeq
}

func (s *Store) Snapshot() (int64, map[string]protocol.FileMeta) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]protocol.FileMeta, len(s.snapshot))
	for rel, meta := range s.snapshot {
		out[rel] = meta
	}
	return s.currentSeq, out
}

func (s *Store) EventRows(since int64, limit int) ([]protocol.Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 10000
	}
	idx := sort.Search(len(s.events), func(i int) bool {
		return s.events[i].Seq > since
	})
	if idx >= len(s.events) {
		return nil, false
	}
	end := idx + limit
	if end > len(s.events) {
		end = len(s.events)
	}
	rows := append([]protocol.Event(nil), s.events[idx:end]...)
	return rows, end < len(s.events)
}

func (s *Store) WaitForSeq(ctx context.Context, since int64) error {
	for {
		if s.CurrentSeq() > since {
			return nil
		}
		ch := s.waitCh()
		if s.CurrentSeq() > since {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Store) Reconcile() (ReconcileResult, error) {
	current, err := ScanRoot(s.root)
	if err != nil {
		return ReconcileResult{}, err
	}
	return s.applySnapshotDiff("", current)
}

func (s *Store) ReconcileWatchChange(change WatchChange) (ReconcileResult, error) {
	if change.Full || change.Path == "" {
		return s.Reconcile()
	}
	rel, err := CleanRel(change.Path)
	if err != nil {
		return ReconcileResult{}, err
	}
	if change.IsDir {
		return s.reconcileSubtree(rel)
	}
	return s.reconcilePath(rel)
}

func (s *Store) reconcilePath(rel string) (ReconcileResult, error) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return ReconcileResult{}, err
	}
	if exists && !info.Mode().IsRegular() {
		exists = false
	}
	if exists && !IsTrackedFile(rel) {
		exists = false
	}

	var events []protocol.Event
	result := ReconcileResult{}

	s.mu.Lock()
	defer s.mu.Unlock()

	old, ok := s.snapshot[rel]
	if exists {
		meta := protocol.FileMeta{MtimeNS: info.ModTime().UnixNano(), Size: info.Size()}
		if ok && old == meta {
			return result, nil
		}
		s.snapshot[rel] = meta
		events = append(events, s.makeEventLocked("upsert", rel, &meta))
		result.Upserts = 1
	} else if ok {
		delete(s.snapshot, rel)
		events = append(events, s.makeEventLocked("delete", rel, nil))
		result.Deletes = 1
	}

	if result.Upserts == 0 && result.Deletes == 0 {
		return result, nil
	}
	if err := s.appendEventsLocked(events); err != nil {
		return ReconcileResult{}, err
	}
	if err := s.persistLocked(); err != nil {
		return ReconcileResult{}, err
	}
	go s.signalChanged()
	return result, nil
}

func (s *Store) reconcileSubtree(rel string) (ReconcileResult, error) {
	current, err := scanSubtree(s.root, rel)
	if err != nil {
		return ReconcileResult{}, err
	}
	return s.applySnapshotDiff(rel, current)
}

func (s *Store) applySnapshotDiff(scope string, current map[string]protocol.FileMeta) (ReconcileResult, error) {
	result := ReconcileResult{}
	upsertPaths := make([]string, 0, len(current))
	for rel := range current {
		upsertPaths = append(upsertPaths, rel)
	}
	sort.Strings(upsertPaths)

	deletePaths := make([]string, 0)
	prefix := ""
	if scope != "" {
		prefix = scope + "/"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	events := make([]protocol.Event, 0, len(upsertPaths))
	for _, rel := range upsertPaths {
		meta := current[rel]
		old, ok := s.snapshot[rel]
		if ok && old == meta {
			continue
		}
		s.snapshot[rel] = meta
		events = append(events, s.makeEventLocked("upsert", rel, &meta))
		result.Upserts++
	}

	for rel := range s.snapshot {
		if scope == "" {
			if _, ok := current[rel]; ok {
				continue
			}
		} else if rel != scope && !strings.HasPrefix(rel, prefix) {
			continue
		} else if _, ok := current[rel]; ok {
			continue
		}
		deletePaths = append(deletePaths, rel)
	}
	sort.Strings(deletePaths)
	for _, rel := range deletePaths {
		delete(s.snapshot, rel)
		events = append(events, s.makeEventLocked("delete", rel, nil))
		result.Deletes++
	}

	if result.Upserts == 0 && result.Deletes == 0 {
		return result, nil
	}
	if err := s.appendEventsLocked(events); err != nil {
		return ReconcileResult{}, err
	}
	if err := s.persistLocked(); err != nil {
		return ReconcileResult{}, err
	}
	s.signalChanged()
	return result, nil
}

func (s *Store) makeEventLocked(kind, rel string, meta *protocol.FileMeta) protocol.Event {
	s.currentSeq++
	event := protocol.Event{
		Seq:  s.currentSeq,
		TS:   time.Now().UTC().Format(time.RFC3339),
		Kind: kind,
		Path: rel,
	}
	if meta != nil {
		mtime := meta.MtimeNS
		size := meta.Size
		event.MtimeNS = &mtime
		event.Size = &size
	}
	return event
}

func (s *Store) appendEventsLocked(events []protocol.Event) error {
	if len(events) == 0 {
		return nil
	}
	file, err := os.OpenFile(s.eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			return err
		}
		s.events = append(s.events, event)
	}
	return nil
}

func (s *Store) persistLocked() error {
	meta := storeMeta{Root: s.root, CurrentSeq: s.currentSeq}
	if err := writeJSONAtomic(s.metaPath, meta); err != nil {
		return err
	}
	return writeJSONAtomic(s.snapshotPath, s.snapshot)
}

func (s *Store) waitCh() chan struct{} {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifyCh
}

func (s *Store) signalChanged() {
	s.notifyMu.Lock()
	close(s.notifyCh)
	s.notifyCh = make(chan struct{})
	s.notifyMu.Unlock()
}

func scanSubtree(root, rel string) (map[string]protocol.FileMeta, error) {
	out := make(map[string]protocol.FileMeta)
	baseAbs := root
	if rel != "" {
		baseAbs = filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(baseAbs)
		if err != nil {
			if os.IsNotExist(err) {
				return out, nil
			}
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("not a directory: %s", rel)
		}
	}
	if err := filepath.WalkDir(baseAbs, func(abs string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relPath, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		if d.IsDir() {
			if !IsWatchableDir(relPath) {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsTrackedFile(relPath) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		out[relPath] = protocol.FileMeta{MtimeNS: info.ModTime().UnixNano(), Size: info.Size()}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func ScanRoot(root string) (map[string]protocol.FileMeta, error) {
	return scanSubtree(root, "")
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
