package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NolanHo/kanon/internal/core"
	"github.com/NolanHo/kanon/internal/protocol"
	"github.com/NolanHo/kanon/internal/version"
)

func main() {
	addr := flag.String("addr", ":39090", "listen address")
	root := flag.String("root", "/root/docs", "authoritative content root on linux")
	stateDir := flag.String("state-dir", os.ExpandEnv("$HOME/.local/state/kanon/server"), "server state directory")
	filterConfig := flag.String("filter-config", "", "optional filter config file")
	reconcileInterval := flag.Duration("reconcile-interval", time.Hour, "full reconcile interval")
	watchDebounce := flag.Duration("watch-debounce", 200*time.Millisecond, "delay before reconciling after watcher activity")
	flag.Parse()

	cfg, err := core.LoadFilterConfig(*filterConfig)
	if err != nil {
		log.Fatal(err)
	}
	core.SetFilterConfig(cfg)

	store, err := core.OpenStore(*root, *stateDir)
	if err != nil {
		log.Fatal(err)
	}
	metrics := newServerMetrics()
	initialStart := time.Now()
	result, err := store.Reconcile()
	if err != nil {
		log.Fatal(err)
	}
	metrics.recordReconcile(true, result, time.Since(initialStart))
	log.Printf("initial reconcile upserts=%d deletes=%d current_seq=%d", result.Upserts, result.Deletes, store.CurrentSeq())

	watcher, err := core.NewWatcher(*root)
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	trigger := make(chan core.WatchChange, 4096)
	watchState := newWatcherState()
	go runWatcher(ctx, watcher, trigger, watchState)
	go func() {
		<-ctx.Done()
		_ = watcher.Close()
	}()
	go func() {
		var tickerC <-chan time.Time
		if *reconcileInterval > 0 {
			ticker := time.NewTicker(*reconcileInterval)
			defer ticker.Stop()
			tickerC = ticker.C
		}
		drain := func(change core.WatchChange) []core.WatchChange {
			changes := []core.WatchChange{change}
			if *watchDebounce > 0 {
				time.Sleep(*watchDebounce)
			}
			for {
				select {
				case next := <-trigger:
					changes = append(changes, next)
				default:
					return compactWatchChanges(changes)
				}
			}
		}
		for {
			if tickerC == nil {
				select {
				case <-ctx.Done():
					return
				case change := <-trigger:
					metrics.recordWatchEvent()
					reconcileWatchChangesAndLog(store, drain(change), metrics)
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case change := <-trigger:
				metrics.recordWatchEvent()
				reconcileWatchChangesAndLog(store, drain(change), metrics)
			case <-tickerC:
				reconcileAndLog(store, metrics)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		watchHealth := watchState.health()
		m := metrics.health()
		writeJSON(w, protocol.HealthResponse{
			Status:               "ok",
			ProtocolVersion:      protocol.CurrentVersion,
			CurrentSeq:           store.CurrentSeq(),
			WatcherRunning:       watchHealth.Running,
			WatcherRestarts:      watchHealth.Restarts,
			WatcherLastError:     watchHealth.LastError,
			WatcherLastErrorTS:   watchHealth.LastErrorTS,
			LastReconcileTS:      m.LastReconcileTS,
			LastReconcileFull:    m.LastReconcileFull,
			LastReconcileMS:      m.LastReconcileMS,
			LastReconcileUpserts: m.LastReconcileUpserts,
			LastReconcileDeletes: m.LastReconcileDeletes,
			LastWatchEventTS:     m.LastWatchEventTS,
			TriggerQueueLen:      len(trigger),
			TriggerQueueCap:      cap(trigger),
		})
	})
	mux.HandleFunc("/v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		current, files := store.Snapshot()
		writeJSON(w, protocol.SnapshotResponse{Current: current, Files: files})
	})
	mux.HandleFunc("/v1/changes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		since, err := parseInt64(r, "since", 0)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limit, err := parseInt(r, "limit", 10000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		events, hasMore := store.EventRows(since, limit)
		writeJSON(w, protocol.ChangesResponse{
			Since:   since,
			Current: store.CurrentSeq(),
			HasMore: hasMore,
			Events:  events,
		})
	})
	mux.HandleFunc("/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		since, err := parseInt64(r, "since", 0)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limit, err := parseInt(r, "limit", 10000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		poll, err := parseDurationSeconds(r, "poll_interval", 0)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		ctx := r.Context()
		for {
			if err := store.WaitForSeq(ctx, since); err != nil {
				return
			}
			events, hasMore := store.EventRows(since, limit)
			if len(events) > 0 {
				for _, event := range events {
					if err := enc.Encode(event); err != nil {
						return
					}
					since = event.Seq
				}
				flusher.Flush()
				if hasMore {
					continue
				}
			}
			if poll > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(poll):
				}
			}
		}
	})
	mux.HandleFunc("/v1/archive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req protocol.ArchiveRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		paths, err := cleanArchivePaths(req.Paths)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		if err := writeArchive(w, *root, paths); err != nil {
			log.Printf("archive error: %v", err)
		}
	})
	mux.HandleFunc("/v1/file", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rel, err := core.CleanRel(r.URL.Query().Get("path"))
		if err != nil || !core.IsTrackedFile(rel) {
			http.NotFound(w, r)
			return
		}
		abs := filepath.Join(*root, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, abs)
	})

	server := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("kanon-server version=%s commit=%s addr=%s root=%s state_dir=%s filter_config=%s", version.Version, version.Commit, *addr, *root, *stateDir, *filterConfig)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func parseInt64(r *http.Request, key string, fallback int64) (int64, error) {
	text := r.URL.Query().Get(key)
	if text == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func parseInt(r *http.Request, key string, fallback int) (int, error) {
	text := r.URL.Query().Get(key)
	if text == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func parseDurationSeconds(r *http.Request, key string, fallback time.Duration) (time.Duration, error) {
	text := r.URL.Query().Get(key)
	if text == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(value * float64(time.Second)), nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func cleanArchivePaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		rel, err := core.CleanRel(raw)
		if err != nil {
			return nil, err
		}
		if !core.IsTrackedFile(rel) {
			return nil, os.ErrNotExist
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		out = append(out, rel)
	}
	return out, nil
}

func writeArchive(w io.Writer, root string, paths []string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	for _, rel := range paths {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		h := &tar.Header{
			Name:    rel,
			Mode:    int64(info.Mode().Perm()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		f, err := os.Open(abs)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

type serverHealth struct {
	LastReconcileTS      string
	LastReconcileFull    bool
	LastReconcileMS      int64
	LastReconcileUpserts int
	LastReconcileDeletes int
	LastWatchEventTS     string
}

type serverMetrics struct {
	mu sync.RWMutex
	h  serverHealth
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{}
}

func (m *serverMetrics) health() serverHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.h
}

func (m *serverMetrics) recordReconcile(full bool, result core.ReconcileResult, d time.Duration) {
	m.mu.Lock()
	m.h.LastReconcileTS = time.Now().UTC().Format(time.RFC3339)
	m.h.LastReconcileFull = full
	m.h.LastReconcileMS = d.Milliseconds()
	m.h.LastReconcileUpserts = result.Upserts
	m.h.LastReconcileDeletes = result.Deletes
	m.mu.Unlock()
}

func (m *serverMetrics) recordWatchEvent() {
	m.mu.Lock()
	m.h.LastWatchEventTS = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
}

type watcherHealth struct {
	Running     bool
	Restarts    int64
	LastError   string
	LastErrorTS string
}

type watcherState struct {
	mu sync.RWMutex
	h  watcherHealth
}

func newWatcherState() *watcherState {
	return &watcherState{h: watcherHealth{Running: true}}
}

func (s *watcherState) health() watcherHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.h
}

func (s *watcherState) recordError(err error) {
	s.mu.Lock()
	s.h.Running = false
	s.h.LastError = err.Error()
	s.h.LastErrorTS = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()
}

func (s *watcherState) recordRestart() {
	s.mu.Lock()
	s.h.Running = true
	s.h.Restarts++
	s.mu.Unlock()
}

func (s *watcherState) recordStop() {
	s.mu.Lock()
	s.h.Running = false
	s.mu.Unlock()
}

func runWatcher(ctx context.Context, watcher core.Watcher, trigger chan<- core.WatchChange, state *watcherState) {
	for {
		if err := watcher.Run(ctx, trigger); err != nil && err != context.Canceled {
			log.Printf("watcher error: %v", err)
			state.recordError(err)
			if rebuildErr := watcher.Rebuild(); rebuildErr != nil {
				log.Printf("watcher rebuild error: %v", rebuildErr)
			} else if sendWatchChange(ctx, trigger, core.WatchChange{Full: true}) != nil {
				state.recordStop()
				return
			} else {
				state.recordRestart()
			}
			continue
		}
		state.recordStop()
		return
	}
}

func sendWatchChange(ctx context.Context, ch chan<- core.WatchChange, change core.WatchChange) error {
	select {
	case ch <- change:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func reconcileAndLog(store *core.Store, metrics *serverMetrics) {
	start := time.Now()
	result, err := store.Reconcile()
	d := time.Since(start)
	if err != nil {
		log.Printf("reconcile error: %v", err)
		return
	}
	metrics.recordReconcile(true, result, d)
	if result.Upserts > 0 || result.Deletes > 0 {
		log.Printf("reconcile upserts=%d deletes=%d duration_ms=%d current_seq=%d", result.Upserts, result.Deletes, d.Milliseconds(), store.CurrentSeq())
	}
}

func reconcileWatchChangesAndLog(store *core.Store, changes []core.WatchChange, metrics *serverMetrics) {
	start := time.Now()
	result := core.ReconcileResult{}
	full := false
	for _, change := range changes {
		full = full || change.Full || change.Path == ""
		part, err := store.ReconcileWatchChange(change)
		if err != nil {
			log.Printf("watch reconcile error path=%s full=%t: %v", change.Path, change.Full, err)
			return
		}
		result.Upserts += part.Upserts
		result.Deletes += part.Deletes
	}
	d := time.Since(start)
	metrics.recordReconcile(full, result, d)
	if result.Upserts > 0 || result.Deletes > 0 {
		log.Printf("watch reconcile changes=%d upserts=%d deletes=%d duration_ms=%d current_seq=%d", len(changes), result.Upserts, result.Deletes, d.Milliseconds(), store.CurrentSeq())
	}
}

func compactWatchChanges(changes []core.WatchChange) []core.WatchChange {
	for _, change := range changes {
		if change.Full || change.Path == "" {
			return []core.WatchChange{{Full: true}}
		}
	}
	dirs := make(map[string]core.WatchChange)
	files := make(map[string]core.WatchChange)
	for _, change := range changes {
		if change.IsDir {
			dirs[change.Path] = change
			continue
		}
		files[change.Path] = change
	}
	for dir := range dirs {
		prefix := dir + "/"
		for file := range files {
			if file == dir || strings.HasPrefix(file, prefix) {
				delete(files, file)
			}
		}
	}
	out := make([]core.WatchChange, 0, len(dirs)+len(files))
	for _, change := range dirs {
		out = append(out, change)
	}
	for _, change := range files {
		out = append(out, change)
	}
	return out
}
