package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/NolanHo/kanon/internal/core"
	"github.com/NolanHo/kanon/internal/protocol"
	"github.com/NolanHo/kanon/internal/version"
)

type mirrorClient struct {
	serverURL        *url.URL
	server           string
	localRoot        string
	stateDir         string
	cursorPath       string
	lockPath         string
	logPath          string
	batchLimit       int
	stream           bool
	streamPoll       time.Duration
	debounce         time.Duration
	reconnect        time.Duration
	verifyOnStart    bool
	printPathLimit   int
	syncMode         string
	rsyncSource      string
	rsyncBin         string
	rsyncShell       string
	httpClient       *http.Client
	streamHTTPClient *http.Client
	tunnel           *sshTunnel
	bannerPrinted    bool
}

type batchStats struct {
	Mode           string   `json:"mode"`
	Cursor         int64    `json:"cursor"`
	Deleted        int      `json:"deleted"`
	Upserted       int      `json:"upserted"`
	DeletedPaths   []string `json:"deleted_paths"`
	UpsertedPaths  []string `json:"upserted_paths"`
	TransferMode   string   `json:"transfer_mode"`
	FallbackReason string   `json:"fallback_reason,omitempty"`
	DurationNS     int64    `json:"duration_ns"`
}

type sshTunnel struct {
	sshBin     string
	host       string
	remoteHost string
	remotePort int
	localPort  int
	cmd        *exec.Cmd
	waitCh     chan error
	stderr     *bytes.Buffer
}

func main() {
	defaultState := filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "kanon")
	defaultLog := filepath.Join(defaultState, "client.log")

	server := flag.String("server", "http://127.0.0.1:39090", "kanon server base URL")
	localRoot := flag.String("local-root", "", "destination root on macOS")
	stateDir := flag.String("state-dir", defaultState, "client state directory")
	logFile := flag.String("log-file", defaultLog, "jsonl log file")
	stream := flag.Bool("stream", false, "keep a long-lived stream open")
	batchLimit := flag.Int("batch-limit", 10000, "maximum journal events fetched per batch")
	streamPoll := flag.Duration("stream-poll-interval", 0, "server idle wait in stream mode; 0 keeps the stream blocked until new changes arrive")
	debounce := flag.Duration("debounce", time.Second, "batch streamed events before one apply")
	reconnect := flag.Duration("reconnect", 5*time.Second, "wait before reconnecting after a stream error")
	verifyOnStart := flag.Bool("verify-on-start", true, "compare local mirror against server snapshot before syncing")
	printPathLimit := flag.Int("print-path-limit", 50, "maximum changed paths printed per batch; negative prints all")
	syncMode := flag.String("sync-mode", "auto", "transfer mode: auto, archive, rsync, or http")
	rsyncSource := flag.String("rsync-source", "", "rsync source root, for example server-host:/root/docs/")
	rsyncBin := flag.String("rsync-bin", "/opt/homebrew/bin/rsync", "local rsync binary")
	rsyncShell := flag.String("rsync-shell", "ssh", "remote shell for rsync when rsync-source is remote")
	tunnelHost := flag.String("tunnel-host", "", "SSH host used to create a local tunnel for the HTTP control plane")
	tunnelRemoteHost := flag.String("tunnel-remote-host", "", "remote host reached from the SSH server; defaults to the host part of -server")
	tunnelRemotePort := flag.Int("tunnel-remote-port", 0, "remote port reached from the SSH server; defaults to the port part of -server")
	tunnelLocalPort := flag.Int("tunnel-local-port", 0, "local port for the tunnel; 0 means auto-pick a free port above 30000")
	tunnelBin := flag.String("tunnel-bin", "ssh", "SSH client used to create the tunnel")
	jsonOut := flag.Bool("json", false, "emit final one-shot stats as json; ignored in stream mode")
	flag.Parse()

	if *localRoot == "" {
		fmt.Fprintln(os.Stderr, "-local-root is required")
		os.Exit(1)
	}
	client, err := newMirrorClient(
		*server,
		*localRoot,
		*stateDir,
		*logFile,
		*batchLimit,
		*stream,
		*streamPoll,
		*debounce,
		*reconnect,
		*verifyOnStart,
		*printPathLimit,
		*syncMode,
		*rsyncSource,
		*rsyncBin,
		*rsyncShell,
		*tunnelHost,
		*tunnelRemoteHost,
		*tunnelRemotePort,
		*tunnelLocalPort,
		*tunnelBin,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	stats, code := client.run()
	if code != 0 {
		os.Exit(code)
	}
	if stats != nil && *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(stats)
	}
}

func newMirrorClient(server, localRoot, stateDir, logPath string, batchLimit int, stream bool, streamPoll, debounce, reconnect time.Duration, verifyOnStart bool, printPathLimit int, syncMode, rsyncSource, rsyncBin, rsyncShell, tunnelHost, tunnelRemoteHost string, tunnelRemotePort, tunnelLocalPort int, tunnelBin string) (*mirrorClient, error) {
	server = strings.TrimRight(server, "/")
	parsed, err := url.Parse(server)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid server url: %s", server)
	}
	localRoot, err = filepath.Abs(localRoot)
	if err != nil {
		return nil, err
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	logPath, err = filepath.Abs(logPath)
	if err != nil {
		return nil, err
	}
	syncMode = strings.ToLower(strings.TrimSpace(syncMode))
	if syncMode != "auto" && syncMode != "archive" && syncMode != "rsync" && syncMode != "http" {
		return nil, fmt.Errorf("invalid -sync-mode: %s", syncMode)
	}

	var tunnel *sshTunnel
	if strings.TrimSpace(tunnelHost) != "" {
		remoteHost := strings.TrimSpace(tunnelRemoteHost)
		if remoteHost == "" {
			remoteHost = parsed.Hostname()
		}
		remotePort := tunnelRemotePort
		if remotePort == 0 {
			remotePort = serverPort(parsed)
		}
		if remotePort == 0 {
			return nil, fmt.Errorf("cannot infer remote port from -server; set -tunnel-remote-port")
		}
		tunnel = &sshTunnel{
			sshBin:     strings.TrimSpace(tunnelBin),
			host:       strings.TrimSpace(tunnelHost),
			remoteHost: remoteHost,
			remotePort: remotePort,
			localPort:  tunnelLocalPort,
		}
	}

	return &mirrorClient{
		serverURL:        parsed,
		server:           server,
		localRoot:        localRoot,
		stateDir:         stateDir,
		cursorPath:       filepath.Join(stateDir, "cursor"),
		lockPath:         filepath.Join(stateDir, "client.lock"),
		logPath:          logPath,
		batchLimit:       batchLimit,
		stream:           stream,
		streamPoll:       streamPoll,
		debounce:         debounce,
		reconnect:        reconnect,
		verifyOnStart:    verifyOnStart,
		printPathLimit:   printPathLimit,
		syncMode:         syncMode,
		rsyncSource:      strings.TrimSpace(rsyncSource),
		rsyncBin:         strings.TrimSpace(rsyncBin),
		rsyncShell:       strings.TrimSpace(rsyncShell),
		httpClient:       &http.Client{Timeout: 30 * time.Second},
		streamHTTPClient: &http.Client{},
		tunnel:           tunnel,
	}, nil
}

func (c *mirrorClient) run() (*batchStats, int) {
	if err := os.MkdirAll(c.stateDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 1
	}
	if err := os.MkdirAll(c.localRoot, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 1
	}
	if err := os.MkdirAll(filepath.Dir(c.logPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 1
	}

	lock, err := os.OpenFile(c.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 1
	}
	defer lock.Close()
	defer c.closeTunnel()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "another sync is already running")
		return nil, 1
	}

	if c.verifyOnStart {
		if err := c.verifySnapshot(); err != nil {
			c.recordError(err.Error(), map[string]any{"mode": "verify"})
			fmt.Fprintln(os.Stderr, err)
			return nil, 1
		}
	}

	if c.stream {
		return nil, c.runStream()
	}
	stats, err := c.runOnce()
	if err != nil {
		c.recordError(err.Error(), map[string]any{"mode": "once"})
		fmt.Fprintln(os.Stderr, err)
		return nil, 1
	}
	return stats, 0
}

func (c *mirrorClient) runOnce() (*batchStats, error) {
	cursor, err := c.readCursor()
	if err != nil {
		return nil, err
	}
	var final *batchStats
	for {
		payload, err := c.fetchChanges(cursor)
		if err != nil {
			return nil, err
		}
		if len(payload.Events) == 0 {
			if final == nil {
				final = &batchStats{Mode: "once", Cursor: cursor, DeletedPaths: []string{}, UpsertedPaths: []string{}, TransferMode: c.idleTransferMode()}
			}
			return final, nil
		}
		stats, err := c.applyEvents("once", payload.Events)
		if err != nil {
			return nil, err
		}
		cursor = stats.Cursor
		final = stats
		if !payload.HasMore && cursor >= payload.Current {
			return final, nil
		}
	}
}

func (c *mirrorClient) runStream() int {
	for {
		if !c.bannerPrinted {
			if err := c.printBanner(); err != nil {
				c.recordError(err.Error(), map[string]any{"mode": "stream", "phase": "banner"})
				fmt.Fprintln(os.Stderr, err)
				time.Sleep(c.reconnect)
				continue
			}
			c.bannerPrinted = true
		}
		cursor, err := c.readCursor()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := c.consumeStream(cursor); err != nil {
			c.recordError(err.Error(), map[string]any{"mode": "stream", "cursor": cursor})
			fmt.Fprintln(os.Stderr, err)
			time.Sleep(c.reconnect)
			continue
		}
		return 0
	}
}

func (c *mirrorClient) consumeStream(cursor int64) error {
	base, err := c.controlBase()
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v1/stream?since=%d&limit=%d&poll_interval=%g", base, cursor, c.batchLimit, c.streamPoll.Seconds())
	resp, err := c.streamHTTPClient.Get(endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("stream status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	eventsCh := make(chan protocol.Event)
	errCh := make(chan error, 1)
	go func() {
		defer close(eventsCh)
		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var event protocol.Event
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				errCh <- fmt.Errorf("invalid stream event: %w", err)
				return
			}
			eventsCh <- event
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
			return
		}
		errCh <- io.EOF
	}()

	batch := make([]protocol.Event, 0, 128)
	var timer *time.Timer
	var timerC <-chan time.Time
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		stats, err := c.applyEvents("stream", batch)
		if err != nil {
			return err
		}
		cursor = stats.Cursor
		batch = batch[:0]
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		timerC = nil
		return nil
	}

	for {
		select {
		case event, ok := <-eventsCh:
			if !ok {
				if err := flush(); err != nil {
					return err
				}
				err := <-errCh
				if errors.Is(err, io.EOF) {
					return fmt.Errorf("stream ended")
				}
				return err
			}
			batch = append(batch, event)
			if timer == nil {
				timer = time.NewTimer(c.debounce)
				timerC = timer.C
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(c.debounce)
			timerC = timer.C
		case <-timerC:
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

func (c *mirrorClient) fetchSnapshot() (*protocol.SnapshotResponse, error) {
	base, err := c.controlBase()
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/v1/snapshot", base)
	resp, err := c.httpClient.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("snapshot status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload protocol.SnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return &payload, nil
}

func (c *mirrorClient) fetchChanges(cursor int64) (*protocol.ChangesResponse, error) {
	base, err := c.controlBase()
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/v1/changes?since=%d&limit=%d", base, cursor, c.batchLimit)
	resp, err := c.httpClient.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("changes status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload protocol.ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return &payload, nil
}

func (c *mirrorClient) verifySnapshot() error {
	snapshot, err := c.fetchSnapshot()
	if err != nil {
		return err
	}
	deletes, upserts, err := c.diffSnapshot(snapshot.Files)
	if err != nil {
		return err
	}
	if len(deletes) > 0 {
		if _, err := c.applyDeletes(deletes); err != nil {
			return err
		}
	}
	transferMode, fallbackReason, err := c.transferUpserts(upserts)
	if err != nil {
		return err
	}
	if err := c.verifyLocalFiles(snapshot.Files, upserts); err != nil {
		return err
	}
	if err := c.writeCursor(snapshot.Current); err != nil {
		return err
	}
	if len(deletes) > 0 || len(upserts) > 0 {
		stats := &batchStats{
			Mode:           "verify",
			Cursor:         snapshot.Current,
			Deleted:        len(deletes),
			Upserted:       len(upserts),
			DeletedPaths:   deletes,
			UpsertedPaths:  upserts,
			TransferMode:   transferMode,
			FallbackReason: fallbackReason,
		}
		c.emitBatch(stats)
		c.recordOK(stats)
	}
	return nil
}

func (c *mirrorClient) diffSnapshot(snapshot map[string]protocol.FileMeta) ([]string, []string, error) {
	local, err := c.scanLocal()
	if err != nil {
		return nil, nil, err
	}
	deletes := make([]string, 0)
	upserts := make([]string, 0)
	for rel, meta := range local {
		remote, ok := snapshot[rel]
		if !ok {
			deletes = append(deletes, rel)
			continue
		}
		if remote.Size != meta.Size {
			upserts = append(upserts, rel)
		}
	}
	for rel := range snapshot {
		if _, ok := local[rel]; !ok {
			upserts = append(upserts, rel)
		}
	}
	sort.Strings(deletes)
	sort.Strings(upserts)
	return deletes, upserts, nil
}

func (c *mirrorClient) scanLocal() (map[string]protocol.FileMeta, error) {
	out := make(map[string]protocol.FileMeta)
	if err := filepath.WalkDir(c.localRoot, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if abs == c.localRoot {
				return nil
			}
			rel, err := filepath.Rel(c.localRoot, abs)
			if err != nil {
				return err
			}
			if !core.IsWatchableDir(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(c.localRoot, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !core.IsTrackedFile(rel) {
			return nil
		}
		out[rel] = protocol.FileMeta{MtimeNS: info.ModTime().UnixNano(), Size: info.Size()}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *mirrorClient) applyEvents(mode string, events []protocol.Event) (*batchStats, error) {
	start := time.Now()
	deletes, upserts, cursor := coalesce(events)
	deleted, err := c.applyDeletes(deletes)
	if err != nil {
		return nil, err
	}
	transferMode, fallbackReason, err := c.transferUpserts(upserts)
	if err != nil {
		return nil, err
	}
	if err := c.verifyEventUpserts(events, upserts); err != nil {
		return nil, err
	}
	if err := c.writeCursor(cursor); err != nil {
		return nil, err
	}
	stats := &batchStats{
		Mode:           mode,
		Cursor:         cursor,
		Deleted:        deleted,
		Upserted:       len(upserts),
		DeletedPaths:   deletes,
		UpsertedPaths:  upserts,
		TransferMode:   transferMode,
		FallbackReason: fallbackReason,
		DurationNS:     time.Since(start).Nanoseconds(),
	}
	if len(deletes) > 0 || len(upserts) > 0 {
		c.emitBatch(stats)
		c.recordOK(stats)
	}
	return stats, nil
}

func (c *mirrorClient) verifyEventUpserts(events []protocol.Event, upserts []string) error {
	meta := make(map[string]protocol.FileMeta, len(events))
	for _, event := range events {
		if event.Kind != "upsert" || event.MtimeNS == nil || event.Size == nil {
			continue
		}
		meta[event.Path] = protocol.FileMeta{MtimeNS: *event.MtimeNS, Size: *event.Size}
	}
	return c.verifyLocalFiles(meta, upserts)
}

func (c *mirrorClient) verifyLocalFiles(meta map[string]protocol.FileMeta, paths []string) error {
	for _, rel := range paths {
		expected, ok := meta[rel]
		if !ok {
			continue
		}
		localPath := filepath.Join(c.localRoot, filepath.FromSlash(rel))
		info, err := os.Stat(localPath)
		if err != nil {
			return fmt.Errorf("verify %s: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("verify %s: not a regular file", rel)
		}
		if info.Size() != expected.Size {
			return fmt.Errorf("verify %s: got size=%d want size=%d", rel, info.Size(), expected.Size)
		}
	}
	return nil
}

func (c *mirrorClient) transferUpserts(paths []string) (string, string, error) {
	if len(paths) == 0 {
		return c.idleTransferMode(), "", nil
	}
	switch c.syncMode {
	case "archive":
		return "archive", "", c.transferArchive(paths)
	case "http":
		return "http", "", c.transferHTTP(paths)
	case "rsync":
		if err := c.transferRsync(paths); err != nil {
			return "rsync", "", err
		}
		return "rsync", "", nil
	case "auto":
		if err := c.transferArchive(paths); err == nil {
			return "archive", "", nil
		}
		fallbackReason := "archive failed"
		if c.canUseRsync() {
			if err := c.transferRsync(paths); err == nil {
				return "rsync", fallbackReason, nil
			}
			fallbackReason = "archive and rsync failed"
		}
		if err := c.transferHTTP(paths); err != nil {
			return "http", fallbackReason, err
		}
		return "http", fallbackReason, nil
	default:
		return "", "", fmt.Errorf("invalid sync mode: %s", c.syncMode)
	}
}

func (c *mirrorClient) canUseRsync() bool {
	if c.rsyncSource == "" || c.rsyncBin == "" {
		return false
	}
	if _, err := exec.LookPath(c.rsyncBin); err != nil {
		return false
	}
	return true
}

func (c *mirrorClient) idleTransferMode() string {
	if c.syncMode == "auto" {
		return "archive"
	}
	return c.syncMode
}

func (c *mirrorClient) transferRsync(paths []string) error {
	if c.rsyncSource == "" {
		return fmt.Errorf("-rsync-source is required for rsync mode")
	}
	rs := c.rsyncBin
	if _, err := exec.LookPath(rs); err != nil {
		return fmt.Errorf("rsync binary not found: %s", rs)
	}
	file, err := os.CreateTemp("", "kanon-files-*.txt")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	for _, rel := range paths {
		if _, err := file.WriteString(rel); err != nil {
			file.Close()
			return err
		}
		if _, err := file.Write([]byte{0}); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}

	source := c.rsyncSource
	if !strings.HasSuffix(source, "/") {
		source += "/"
	}
	args := []string{
		"-rtW",
		"--from0",
		"--files-from=" + tmpPath,
		"--omit-dir-times",
		"--times",
	}
	if isRemoteRsyncSource(source) && c.rsyncShell != "" {
		args = append(args, "-e", c.rsyncShell)
	}
	args = append(args, source, c.localRoot)
	cmd := exec.Command(rs, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func isRemoteRsyncSource(source string) bool {
	if strings.Contains(source, "::") {
		return true
	}
	idx := strings.Index(source, ":")
	if idx <= 0 {
		return false
	}
	return !strings.Contains(source[:idx], "/")
}

func (c *mirrorClient) transferArchive(paths []string) error {
	base, err := c.controlBase()
	if err != nil {
		return err
	}
	body, err := json.Marshal(protocol.ArchiveRequest{Paths: paths})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v1/archive", base)
	resp, err := c.httpClient.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("archive status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return c.unpackArchive(resp.Body)
}

func (c *mirrorClient) unpackArchive(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		rel, err := core.CleanRel(h.Name)
		if err != nil || !core.IsTrackedFile(rel) {
			return fmt.Errorf("archive contains invalid path: %s", h.Name)
		}
		localPath := filepath.Join(c.localRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return err
		}
		tmp := localPath + ".tmp"
		file, err := os.Create(tmp)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(file, tr, h.Size)
		closeErr := file.Close()
		if copyErr != nil {
			_ = os.Remove(tmp)
			return copyErr
		}
		if closeErr != nil {
			_ = os.Remove(tmp)
			return closeErr
		}
		if err := os.Rename(tmp, localPath); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
}

func (c *mirrorClient) transferHTTP(paths []string) error {
	for _, rel := range paths {
		if err := c.downloadFile(rel); err != nil {
			return err
		}
	}
	return nil
}

func (c *mirrorClient) downloadFile(rel string) error {
	base, err := c.controlBase()
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v1/file?path=%s", base, url.QueryEscape(rel))
	resp, err := c.httpClient.Get(endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("file %s status=%d body=%s", rel, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	localPath := filepath.Join(c.localRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	tmp := localPath + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, resp.Body); err != nil {
		file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, localPath)
}

func (c *mirrorClient) applyDeletes(paths []string) (int, error) {
	deleted := 0
	for _, rel := range paths {
		localPath := filepath.Join(c.localRoot, filepath.FromSlash(rel))
		info, err := os.Lstat(localPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return deleted, err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if err := os.Remove(localPath); err != nil {
			return deleted, err
		}
		deleted++
		c.pruneEmpty(filepath.Dir(localPath))
	}
	return deleted, nil
}

func (c *mirrorClient) pruneEmpty(dir string) {
	for dir != c.localRoot && dir != "." && dir != "/" {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func (c *mirrorClient) readCursor() (int64, error) {
	data, err := os.ReadFile(c.cursorPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0, nil
	}
	var cursor int64
	_, err = fmt.Sscanf(text, "%d", &cursor)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor file: %s", c.cursorPath)
	}
	return cursor, nil
}

func (c *mirrorClient) writeCursor(cursor int64) error {
	tmp := c.cursorPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", cursor)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.cursorPath)
}

func (c *mirrorClient) emitBatch(stats *batchStats) {
	stamp := localNow()
	fallback := ""
	if stats.FallbackReason != "" {
		fallback = " fallback"
	}
	fmt.Printf("%s sync %s put=%d del=%d%s\n", stamp, stats.TransferMode, len(stats.UpsertedPaths), len(stats.DeletedPaths), fallback)
	c.printPaths("PUT", stats.UpsertedPaths)
	c.printPaths("DEL", stats.DeletedPaths)
}

func (c *mirrorClient) printPaths(op string, paths []string) {
	limit := len(paths)
	if c.printPathLimit >= 0 && limit > c.printPathLimit {
		limit = c.printPathLimit
	}
	for _, rel := range paths[:limit] {
		fmt.Printf("  %s %s\n", op, rel)
	}
	if limit < len(paths) {
		fmt.Printf("  ... %d more %s\n", len(paths)-limit, op)
	}
}

func (c *mirrorClient) printBanner() error {
	control, err := c.controlBase()
	if err != nil {
		return err
	}
	fmt.Printf("kanon  %s\n", version.Version)
	fmt.Printf("  control: %s\n", control)
	if c.tunnel != nil {
		fmt.Printf("  tunnel:  ssh %s -> %s:%d\n", c.tunnel.host, c.tunnel.remoteHost, c.tunnel.remotePort)
	}
	fmt.Printf("  local:   %s\n", c.localRoot)
	fmt.Printf("  state:   %s\n", c.stateDir)
	fmt.Printf("  data:    %s\n", c.describeDataPlane())
	fmt.Println()
	return nil
}

func (c *mirrorClient) describeDataPlane() string {
	desc := c.syncMode
	if c.rsyncSource != "" {
		desc += " rsync=" + c.rsyncSource
	}
	if c.syncMode == "auto" {
		desc += " fallback=rsync,http"
	}
	return desc
}

func (c *mirrorClient) recordOK(stats *batchStats) {
	payload := map[string]any{
		"ts":              utcNow(),
		"status":          "ok",
		"mode":            stats.Mode,
		"cursor":          stats.Cursor,
		"deleted":         stats.Deleted,
		"upserted":        stats.Upserted,
		"deleted_paths":   stats.DeletedPaths,
		"upserted_paths":  stats.UpsertedPaths,
		"transfer_mode":   stats.TransferMode,
		"fallback_reason": stats.FallbackReason,
		"duration_ns":     stats.DurationNS,
	}
	if c.tunnel != nil {
		payload["control_server"] = c.server
		payload["tunnel_host"] = c.tunnel.host
	}
	c.appendLog(payload)
}

func (c *mirrorClient) recordError(msg string, extra map[string]any) {
	payload := map[string]any{
		"ts":     utcNow(),
		"status": "error",
		"error":  msg,
	}
	for k, v := range extra {
		payload[k] = v
	}
	if c.tunnel != nil {
		payload["tunnel_host"] = c.tunnel.host
	}
	c.appendLog(payload)
}

func (c *mirrorClient) appendLog(payload map[string]any) {
	file, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	line, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = file.Write(append(line, '\n'))
}

func (c *mirrorClient) controlBase() (string, error) {
	if c.tunnel == nil {
		return c.server, nil
	}
	if err := c.tunnel.ensure(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", c.serverURL.Scheme, c.tunnel.localPort), nil
}

func (c *mirrorClient) closeTunnel() {
	if c.tunnel != nil {
		c.tunnel.close()
	}
}

func (t *sshTunnel) ensure() error {
	if t.host == "" {
		return nil
	}
	if t.localPort == 0 {
		port, err := pickHighPort()
		if err != nil {
			return err
		}
		t.localPort = port
	}
	if t.alive() {
		return nil
	}

	if _, err := exec.LookPath(t.sshBin); err != nil {
		return fmt.Errorf("ssh binary not found: %s", t.sshBin)
	}
	stderr := &bytes.Buffer{}
	args := []string{
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-N",
		"-L", fmt.Sprintf("127.0.0.1:%d:%s:%d", t.localPort, t.remoteHost, t.remotePort),
		t.host,
	}
	cmd := exec.Command(t.sshBin, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ssh tunnel: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	addr := fmt.Sprintf("127.0.0.1:%d", t.localPort)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := dialOK(addr); err == nil {
			t.cmd = cmd
			t.waitCh = waitCh
			t.stderr = stderr
			return nil
		}
		select {
		case err := <-waitCh:
			return fmt.Errorf("ssh tunnel failed: %v: %s", err, strings.TrimSpace(stderr.String()))
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	<-waitCh
	return fmt.Errorf("ssh tunnel did not become ready on 127.0.0.1:%d", t.localPort)
}

func (t *sshTunnel) alive() bool {
	if t.cmd == nil || t.cmd.Process == nil {
		return false
	}
	return t.cmd.Process.Signal(syscall.Signal(0)) == nil
}

func (t *sshTunnel) close() {
	if t.cmd == nil || t.cmd.Process == nil {
		return
	}
	_ = t.cmd.Process.Kill()
	if t.waitCh != nil {
		select {
		case <-t.waitCh:
		case <-time.After(2 * time.Second):
		}
	}
	t.cmd = nil
	t.waitCh = nil
}

func dialOK(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func pickHighPort() (int, error) {
	for i := 0; i < 64; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if port > 30000 {
			return port, nil
		}
	}
	return 0, fmt.Errorf("cannot allocate a free local port above 30000")
}

func serverPort(u *url.URL) int {
	if port := u.Port(); port != "" {
		var p int
		_, _ = fmt.Sscanf(port, "%d", &p)
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return 443
	case "http":
		return 80
	default:
		return 0
	}
}

func coalesce(events []protocol.Event) ([]string, []string, int64) {
	last := make(map[string]protocol.Event)
	order := make([]string, 0, len(events))
	var cursor int64
	for _, event := range events {
		if _, ok := last[event.Path]; !ok {
			order = append(order, event.Path)
		}
		last[event.Path] = event
		if event.Seq > cursor {
			cursor = event.Seq
		}
	}
	deletes := make([]string, 0)
	upserts := make([]string, 0)
	for _, rel := range order {
		event := last[rel]
		switch event.Kind {
		case "delete":
			deletes = append(deletes, rel)
		case "upsert":
			upserts = append(upserts, rel)
		}
	}
	sort.Strings(deletes)
	sort.Strings(upserts)
	return deletes, upserts, cursor
}

func localNow() string {
	return time.Now().Format("15:04:05")
}

func utcNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}
