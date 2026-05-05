package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NolanHo/kanon/internal/core"
	"github.com/NolanHo/kanon/internal/protocol"
	_ "modernc.org/sqlite"
)

const schemaVersion = "1"

const defaultEventBatchLimit = 1000

type Index struct {
	mu     sync.RWMutex
	db     *sql.DB
	root   string
	path   string
	lastMu sync.RWMutex
	last   error
}

type Health struct {
	Root          string `json:"root"`
	DBPath        string `json:"db_path"`
	SchemaVersion string `json:"schema_version"`
	CurrentSeq    int64  `json:"current_seq"`
	IndexedSeq    int64  `json:"indexed_seq"`
	Lag           int64  `json:"lag"`
	DocumentCount int64  `json:"document_count"`
	LastIndexedAt string `json:"last_indexed_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Stale         bool   `json:"stale"`
}

type QueryRequest struct {
	Query      string `json:"query"`
	Limit      int    `json:"limit"`
	PathPrefix string `json:"pathPrefix"`
	Kind       string `json:"kind"`
}

type QueryResponse struct {
	Query   string       `json:"query"`
	Limit   int          `json:"limit"`
	Index   Health       `json:"index"`
	Matches []QueryMatch `json:"matches"`
}

type QueryMatch struct {
	Path    string   `json:"path"`
	Title   string   `json:"title,omitempty"`
	Heading string   `json:"heading,omitempty"`
	Kind    string   `json:"kind"`
	Score   float64  `json:"score"`
	Matched []string `json:"matched,omitempty"`
	Snippet string   `json:"snippet,omitempty"`
}

type documentFields struct {
	Path     string
	Kind     string
	Title    string
	Headings []string
	Body     string
	Size     int64
	MtimeNS  int64
	SHA256   string
	EventSeq int64
}

func Open(root, dbPath string) (*Index, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	idx := &Index{db: db, root: rootAbs, path: dbPath}
	if err := idx.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := idx.setMeta(context.Background(), "root", rootAbs); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idx, nil
}

func (idx *Index) Close() error {
	return idx.db.Close()
}

func (idx *Index) migrate() error {
	stmts := []string{
		`pragma journal_mode=WAL`,
		`pragma synchronous=NORMAL`,
		`create table if not exists index_meta (key text primary key, value text not null)`,
		`create table if not exists documents (
			id integer primary key,
			path text not null unique,
			title text,
			headings_json text not null default '[]',
			kind text not null,
			size integer not null,
			mtime_ns integer not null,
			sha256 text not null,
			event_seq integer not null,
			indexed_at text not null
		)`,
		`create virtual table if not exists documents_fts using fts5(path, title, headings, body)`,
		`create table if not exists aliases (
			term text not null,
			expansion text not null,
			weight real not null default 1.0,
			primary key (term, expansion)
		)`,
		`create index if not exists documents_path_idx on documents(path)`,
		`create index if not exists documents_kind_idx on documents(kind)`,
		`delete from documents_fts where rowid not in (select id from documents)`,
	}
	for _, stmt := range stmts {
		if _, err := idx.db.Exec(stmt); err != nil {
			return err
		}
	}
	return idx.setMeta(context.Background(), "schema_version", schemaVersion)
}

func (idx *Index) Bootstrap(ctx context.Context, currentSeq int64, files map[string]protocol.FileMeta) error {
	indexedSeq, err := idx.IndexedSeq(ctx)
	if err != nil {
		return err
	}
	count, err := idx.DocumentCount(ctx)
	if err != nil {
		return err
	}
	if indexedSeq >= currentSeq && count > 0 {
		return nil
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	for _, path := range paths {
		if err := idx.upsertPath(ctx, path, files[path], currentSeq); err != nil {
			idx.setLastError(err)
			return err
		}
	}
	if err := idx.setIndexedSeq(ctx, currentSeq); err != nil {
		idx.setLastError(err)
		return err
	}
	idx.setLastError(nil)
	return nil
}

func (idx *Index) Run(ctx context.Context, store eventStore) {
	for {
		indexedSeq, err := idx.IndexedSeq(ctx)
		if err != nil {
			idx.setLastError(err)
			if !sleepOrDone(ctx, time.Second) {
				return
			}
			continue
		}
		events, hasMore := store.EventRows(indexedSeq, defaultEventBatchLimit)
		if len(events) == 0 {
			if err := store.WaitForSeq(ctx, indexedSeq); err != nil {
				return
			}
			continue
		}
		for _, event := range events {
			if err := idx.ApplyEvent(ctx, event); err != nil {
				idx.setLastError(err)
				break
			}
			indexedSeq = event.Seq
		}
		if !hasMore {
			idx.setLastError(nil)
		}
	}
}

type eventStore interface {
	EventRows(since int64, limit int) ([]protocol.Event, bool)
	WaitForSeq(ctx context.Context, since int64) error
}

func (idx *Index) ApplyEvent(ctx context.Context, event protocol.Event) error {
	switch event.Kind {
	case "upsert":
		meta := protocol.FileMeta{}
		if event.MtimeNS != nil {
			meta.MtimeNS = *event.MtimeNS
		}
		if event.Size != nil {
			meta.Size = *event.Size
		}
		return idx.upsertPath(ctx, event.Path, meta, event.Seq)
	case "delete":
		return idx.deletePath(ctx, event.Path, event.Seq)
	default:
		return fmt.Errorf("unknown event kind %q", event.Kind)
	}
}

func (idx *Index) upsertPath(ctx context.Context, rel string, meta protocol.FileMeta, eventSeq int64) error {
	fields, err := idx.parsePath(rel, meta, eventSeq)
	if err != nil {
		return err
	}
	return idx.writeDocument(ctx, fields)
}

func (idx *Index) deletePath(ctx context.Context, rel string, eventSeq int64) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `select id from documents where path = ?`, rel).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if _, err := tx.ExecContext(ctx, `delete from documents_fts where rowid = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `delete from documents where id = ?`, id); err != nil {
			return err
		}
	}
	if err := setMetaTx(ctx, tx, "last_indexed_seq", fmt.Sprint(eventSeq)); err != nil {
		return err
	}
	if err := setMetaTx(ctx, tx, "last_indexed_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

func (idx *Index) parsePath(rel string, meta protocol.FileMeta, eventSeq int64) (documentFields, error) {
	clean, err := core.CleanRel(rel)
	if err != nil {
		return documentFields{}, err
	}
	if !core.IsTrackedFile(clean) {
		return documentFields{}, fmt.Errorf("untracked file: %s", clean)
	}
	abs := filepath.Join(idx.root, filepath.FromSlash(clean))
	file, err := os.Open(abs)
	if err != nil {
		return documentFields{}, err
	}
	defer file.Close()
	h := sha256.New()
	data, err := io.ReadAll(io.TeeReader(file, h))
	if err != nil {
		return documentFields{}, err
	}
	if meta.Size == 0 {
		if info, err := os.Stat(abs); err == nil {
			meta.Size = info.Size()
			meta.MtimeNS = info.ModTime().UnixNano()
		}
	}
	body := string(data)
	title, headings := parseMarkdownFields(body, filepath.Base(clean))
	return documentFields{
		Path:     clean,
		Kind:     kindForPath(clean),
		Title:    title,
		Headings: headings,
		Body:     body,
		Size:     meta.Size,
		MtimeNS:  meta.MtimeNS,
		SHA256:   hex.EncodeToString(h.Sum(nil)),
		EventSeq: eventSeq,
	}, nil
}

func (idx *Index) writeDocument(ctx context.Context, fields documentFields) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	headingsJSON, err := json.Marshal(fields.Headings)
	if err != nil {
		return err
	}
	headingsText := strings.Join(fields.Headings, "\n")
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	var oldSHA string
	err = tx.QueryRowContext(ctx, `select id, sha256 from documents where path = ?`, fields.Path).Scan(&id, &oldSHA)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && oldSHA == fields.SHA256 {
		if _, err := tx.ExecContext(ctx, `update documents set size = ?, mtime_ns = ?, event_seq = ?, indexed_at = ? where id = ?`, fields.Size, fields.MtimeNS, fields.EventSeq, now, id); err != nil {
			return err
		}
	} else if err == nil {
		if _, err := tx.ExecContext(ctx, `update documents set title = ?, headings_json = ?, kind = ?, size = ?, mtime_ns = ?, sha256 = ?, event_seq = ?, indexed_at = ? where id = ?`, fields.Title, string(headingsJSON), fields.Kind, fields.Size, fields.MtimeNS, fields.SHA256, fields.EventSeq, now, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `delete from documents_fts where rowid = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into documents_fts(rowid, path, title, headings, body) values (?, ?, ?, ?, ?)`, id, fields.Path, fields.Title, headingsText, fields.Body); err != nil {
			return err
		}
	} else {
		result, err := tx.ExecContext(ctx, `insert into documents(path, title, headings_json, kind, size, mtime_ns, sha256, event_seq, indexed_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`, fields.Path, fields.Title, string(headingsJSON), fields.Kind, fields.Size, fields.MtimeNS, fields.SHA256, fields.EventSeq, now)
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into documents_fts(rowid, path, title, headings, body) values (?, ?, ?, ?, ?)`, id, fields.Path, fields.Title, headingsText, fields.Body); err != nil {
			return err
		}
	}
	if err := setMetaTx(ctx, tx, "last_indexed_seq", fmt.Sprint(fields.EventSeq)); err != nil {
		return err
	}
	if err := setMetaTx(ctx, tx, "last_indexed_at", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (idx *Index) Health(ctx context.Context, currentSeq int64) (Health, error) {
	indexedSeq, err := idx.IndexedSeq(ctx)
	if err != nil {
		return Health{}, err
	}
	count, err := idx.DocumentCount(ctx)
	if err != nil {
		return Health{}, err
	}
	lastIndexedAt, _ := idx.meta(ctx, "last_indexed_at")
	health := Health{
		Root:          idx.root,
		DBPath:        idx.path,
		SchemaVersion: schemaVersion,
		CurrentSeq:    currentSeq,
		IndexedSeq:    indexedSeq,
		Lag:           max(currentSeq-indexedSeq, 0),
		DocumentCount: count,
		LastIndexedAt: lastIndexedAt,
		Stale:         indexedSeq < currentSeq,
	}
	if err := idx.LastError(); err != nil {
		health.LastError = err.Error()
	}
	return health, nil
}

func (idx *Index) Query(ctx context.Context, req QueryRequest, currentSeq int64) (QueryResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	q := strings.TrimSpace(req.Query)
	health, err := idx.Health(ctx, currentSeq)
	if err != nil {
		return QueryResponse{}, err
	}
	if q == "" {
		return QueryResponse{Query: q, Limit: limit, Index: health}, nil
	}
	ftsQuery := buildFTSQuery(q)
	if ftsQuery == "" {
		return QueryResponse{Query: q, Limit: limit, Index: health}, nil
	}
	where := []string{"documents_fts match ?"}
	args := []any{ftsQuery}
	if req.PathPrefix != "" {
		prefix, err := cleanOptionalPrefix(req.PathPrefix)
		if err != nil {
			return QueryResponse{}, err
		}
		where = append(where, "d.path = ? or d.path like ?")
		args = append(args, prefix, prefix+"/%")
	}
	if req.Kind != "" {
		where = append(where, "d.kind = ?")
		args = append(args, req.Kind)
	}
	args = append(args, limit)
	query := fmt.Sprintf(`select d.path, coalesce(d.title, ''), d.headings_json, d.kind, substr(documents_fts.body, 1, 240), bm25(documents_fts, 6.0, 4.0, 2.0, 1.0) as rank
		from documents_fts join documents d on d.id = documents_fts.rowid
		where %s
		order by rank asc, d.path asc
		limit ?`, strings.Join(where, " and "))
	rows, err := idx.db.QueryContext(ctx, query, args...)
	if err != nil {
		return QueryResponse{}, err
	}
	defer rows.Close()
	matches := []QueryMatch{}
	for rows.Next() {
		var path, title, headingsRaw, kind, snippet string
		var rank float64
		if err := rows.Scan(&path, &title, &headingsRaw, &kind, &snippet, &rank); err != nil {
			return QueryResponse{}, err
		}
		headings := []string{}
		_ = json.Unmarshal([]byte(headingsRaw), &headings)
		matches = append(matches, QueryMatch{
			Path:    path,
			Title:   title,
			Heading: first(headings),
			Kind:    kind,
			Score:   -rank,
			Matched: matchedFields(q, path, title, headings),
			Snippet: snippet,
		})
	}
	if err := rows.Err(); err != nil {
		return QueryResponse{}, err
	}
	return QueryResponse{Query: q, Limit: limit, Index: health, Matches: matches}, nil
}

func (idx *Index) IndexedSeq(ctx context.Context) (int64, error) {
	value, err := idx.meta(ctx, "last_indexed_seq")
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, nil
	}
	var seq int64
	_, err = fmt.Sscan(value, &seq)
	return seq, err
}

func (idx *Index) DocumentCount(ctx context.Context) (int64, error) {
	var count int64
	err := idx.db.QueryRowContext(ctx, `select count(*) from documents`).Scan(&count)
	return count, err
}

func (idx *Index) meta(ctx context.Context, key string) (string, error) {
	var value string
	err := idx.db.QueryRowContext(ctx, `select value from index_meta where key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (idx *Index) setMeta(ctx context.Context, key, value string) error {
	_, err := idx.db.ExecContext(ctx, `insert into index_meta(key, value) values (?, ?) on conflict(key) do update set value = excluded.value`, key, value)
	return err
}

func (idx *Index) setIndexedSeq(ctx context.Context, seq int64) error {
	if err := idx.setMeta(ctx, "last_indexed_seq", fmt.Sprint(seq)); err != nil {
		return err
	}
	return idx.setMeta(ctx, "last_indexed_at", time.Now().UTC().Format(time.RFC3339))
}

func setMetaTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `insert into index_meta(key, value) values (?, ?) on conflict(key) do update set value = excluded.value`, key, value)
	return err
}

func (idx *Index) setLastError(err error) {
	idx.lastMu.Lock()
	defer idx.lastMu.Unlock()
	idx.last = err
}

func (idx *Index) LastError() error {
	idx.lastMu.RLock()
	defer idx.lastMu.RUnlock()
	return idx.last
}

func parseMarkdownFields(body, fallbackTitle string) (string, []string) {
	headings := []string{}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimLeft(trimmed, "#")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == "" {
			continue
		}
		headings = append(headings, trimmed)
	}
	title := fallbackTitle
	if len(headings) > 0 {
		title = headings[0]
	}
	return title, headings
}

func kindForPath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".md"):
		return "markdown"
	case strings.HasSuffix(lower, ".pdf"):
		return "pdf"
	case strings.HasSuffix(lower, ".canvas"):
		return "canvas"
	case strings.HasSuffix(lower, ".png"), strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"), strings.HasSuffix(lower, ".gif"), strings.HasSuffix(lower, ".webp"), strings.HasSuffix(lower, ".svg"):
		return "image"
	default:
		return "file"
	}
}

func buildFTSQuery(query string) string {
	tokens := queryTokens(query)
	if len(tokens) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		quoted = append(quoted, `"`+strings.ReplaceAll(token, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

func queryTokens(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == '/')
	})
	seen := map[string]struct{}{}
	out := []string{}
	for _, field := range fields {
		for _, token := range strings.FieldsFunc(field, func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == '/' }) {
			if len(token) < 2 {
				continue
			}
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			out = append(out, token)
		}
	}
	return out
}

func cleanOptionalPrefix(prefix string) (string, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return "", nil
	}
	return core.CleanRel(prefix)
}

func matchedFields(query, path, title string, headings []string) []string {
	lowerQuery := strings.ToLower(query)
	matches := []string{}
	if strings.Contains(strings.ToLower(path), lowerQuery) {
		matches = append(matches, "path")
	}
	if title != "" && strings.Contains(strings.ToLower(title), lowerQuery) {
		matches = append(matches, "title")
	}
	for _, heading := range headings {
		if strings.Contains(strings.ToLower(heading), lowerQuery) {
			matches = append(matches, "heading")
			break
		}
	}
	if len(matches) == 0 {
		matches = append(matches, "body")
	}
	return matches
}

func first(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
