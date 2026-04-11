package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/NolanHo/vault-bridge/internal/protocol"
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

func (s *Store) Reconcile() (ReconcileResult, error) {
	current, err := ScanRoot(s.root)
	if err != nil {
		return ReconcileResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	result := ReconcileResult{}
	for rel, meta := range current {
		old, ok := s.snapshot[rel]
		if ok && old == meta {
			continue
		}
		s.snapshot[rel] = meta
		if err := s.appendEventLocked("upsert", rel, &meta); err != nil {
			return ReconcileResult{}, err
		}
		result.Upserts++
	}

	toDelete := make([]string, 0)
	for rel := range s.snapshot {
		if _, ok := current[rel]; ok {
			continue
		}
		toDelete = append(toDelete, rel)
	}
	sort.Strings(toDelete)
	for _, rel := range toDelete {
		delete(s.snapshot, rel)
		if err := s.appendEventLocked("delete", rel, nil); err != nil {
			return ReconcileResult{}, err
		}
		result.Deletes++
	}

	if err := s.persistLocked(); err != nil {
		return ReconcileResult{}, err
	}
	return result, nil
}

func (s *Store) appendEventLocked(kind, rel string, meta *protocol.FileMeta) error {
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
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(s.eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return err
	}
	s.events = append(s.events, event)
	return nil
}

func (s *Store) persistLocked() error {
	meta := storeMeta{Root: s.root, CurrentSeq: s.currentSeq}
	if err := writeJSONAtomic(s.metaPath, meta); err != nil {
		return err
	}
	return writeJSONAtomic(s.snapshotPath, s.snapshot)
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
