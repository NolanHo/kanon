package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/NolanHo/vault-bridge/internal/bridge"
	"github.com/NolanHo/vault-bridge/internal/protocol"
	"github.com/NolanHo/vault-bridge/internal/version"
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	root := flag.String("root", "/srv/vault-bridge/source", "authoritative content root on linux")
	stateDir := flag.String("state-dir", os.ExpandEnv("$HOME/.local/state/vault-bridge/server"), "server state directory")
	filterConfig := flag.String("filter-config", "", "optional filter config file")
	reconcileInterval := flag.Duration("reconcile-interval", 10*time.Minute, "full reconcile interval")
	watchDebounce := flag.Duration("watch-debounce", time.Second, "delay before reconciling after watcher activity")
	flag.Parse()

	cfg, err := bridge.LoadFilterConfig(*filterConfig)
	if err != nil {
		log.Fatal(err)
	}
	bridge.SetFilterConfig(cfg)

	store, err := bridge.OpenStore(*root, *stateDir)
	if err != nil {
		log.Fatal(err)
	}
	result, err := store.Reconcile()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("initial reconcile upserts=%d deletes=%d current_seq=%d", result.Upserts, result.Deletes, store.CurrentSeq())

	watcher, err := bridge.NewWatcher(*root)
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	trigger := make(chan struct{}, 1)
	go func() {
		if err := watcher.Run(ctx, trigger); err != nil && err != context.Canceled {
			log.Printf("watcher error: %v", err)
		}
	}()
	go func() {
		var tickerC <-chan time.Time
		var ticker *time.Ticker
		if *reconcileInterval > 0 {
			ticker = time.NewTicker(*reconcileInterval)
			defer ticker.Stop()
			tickerC = ticker.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
				if *watchDebounce > 0 {
					time.Sleep(*watchDebounce)
				}
				for {
					select {
					case <-trigger:
					default:
						goto reconcile
					}
				}
			reconcile:
				reconcileAndLog(store)
			case <-tickerC:
				reconcileAndLog(store)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, protocol.HealthResponse{
			Status:          "ok",
			ProtocolVersion: protocol.CurrentVersion,
			CurrentSeq:      store.CurrentSeq(),
		})
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
		poll, err := parseDurationSeconds(r, "poll_interval", time.Second)
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
		for {
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
			select {
			case <-r.Context().Done():
				return
			case <-time.After(poll):
			}
		}
	})
	mux.HandleFunc("/v1/file", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rel, err := bridge.CleanRel(r.URL.Query().Get("path"))
		if err != nil || !bridge.IsTrackedFile(rel) {
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

	log.Printf("vault-bridge-server version=%s commit=%s addr=%s root=%s state_dir=%s filter_config=%s", version.Version, version.Commit, *addr, *root, *stateDir, *filterConfig)
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

func reconcileAndLog(store *bridge.Store) {
	result, err := store.Reconcile()
	if err != nil {
		log.Printf("reconcile error: %v", err)
		return
	}
	if result.Upserts > 0 || result.Deletes > 0 {
		log.Printf("reconcile upserts=%d deletes=%d current_seq=%d", result.Upserts, result.Deletes, store.CurrentSeq())
	}
}
