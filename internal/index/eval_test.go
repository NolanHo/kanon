package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NolanHo/kanon/internal/protocol"
)

type tokenizerEvalCase struct {
	Name     string
	Query    string
	Expected string
}

func TestTokenizerBackendRetrieval(t *testing.T) {
	cases := []tokenizerEvalCase{
		{Name: "Chinese title", Query: "知识库", Expected: "kanon/naming.md"},
		{Name: "Chinese runtime alias", Query: "运行时", Expected: "pi-agent/pi-runtime/runtime-layout.md"},
		{Name: "Chinese observability alias", Query: "观测", Expected: "grafana/obsharness.md"},
		{Name: "Mixed Chinese English", Query: "知识库 pi", Expected: "pi-agent/pi-knowledge-base.md"},
		{Name: "Path English", Query: "runtime layout", Expected: "pi-agent/pi-runtime/runtime-layout.md"},
	}
	backends := map[string]func() chineseTokenizer{"jieba": newJiebaTokenizerForEval, "gse": newGSETokenizer}
	for name, newTokenizer := range backends {
		t.Run(name, func(t *testing.T) {
			result := runTokenizerEval(t, newTokenizer(), cases)
			t.Logf("backend=%s recall@5=%.2f mrr=%.2f misses=%v", name, result.RecallAt5, result.MRR, result.Misses)
			if result.RecallAt5 < 1.0 {
				t.Fatalf("%s recall@5=%.2f MRR=%.2f misses=%v", name, result.RecallAt5, result.MRR, result.Misses)
			}
		})
	}
}

type tokenizerEvalResult struct {
	RecallAt5 float64
	MRR       float64
	Misses    []string
}

func runTokenizerEval(t *testing.T, tok chineseTokenizer, cases []tokenizerEvalCase) tokenizerEvalResult {
	t.Helper()
	defer tok.Close()
	root := t.TempDir()
	writeEvalDoc(t, root, "kanon/naming.md", "# Kanon 命名\n\nKanon 是 /root/docs 的知识库索引层。")
	writeEvalDoc(t, root, "pi-agent/pi-runtime/runtime-layout.md", "# Runtime Layout\n\nPi runtime layout documents launcher and binary paths.")
	writeEvalDoc(t, root, "grafana/obsharness.md", "# Observability Harness\n\nobsh and grafana telemetry notes.")
	writeEvalDoc(t, root, "pi-agent/pi-knowledge-base.md", "# Pi Knowledge Base\n\nknowledge-base extension indexes docs.")
	idx, err := Open(root, filepath.Join(root, "state", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.tok.Close()
	idx.tok = tok
	files := map[string]protocol.FileMeta{}
	for _, path := range []string{"kanon/naming.md", "pi-agent/pi-runtime/runtime-layout.md", "grafana/obsharness.md", "pi-agent/pi-knowledge-base.md"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = protocol.FileMeta{MtimeNS: info.ModTime().UnixNano(), Size: info.Size()}
	}
	if err := idx.Bootstrap(context.Background(), int64(len(files)), files); err != nil {
		t.Fatal(err)
	}
	hits := 0
	rr := 0.0
	misses := []string{}
	for _, tc := range cases {
		resp, err := idx.Query(context.Background(), QueryRequest{Query: tc.Query, Limit: 5}, int64(len(files)))
		if err != nil {
			t.Fatal(err)
		}
		rank := 0
		for i, match := range resp.Matches {
			if match.Path == tc.Expected {
				rank = i + 1
				break
			}
		}
		if rank == 0 {
			misses = append(misses, tc.Name+":"+tc.Query)
			continue
		}
		hits++
		rr += 1.0 / float64(rank)
	}
	return tokenizerEvalResult{RecallAt5: float64(hits) / float64(len(cases)), MRR: rr / float64(len(cases)), Misses: misses}
}

func writeEvalDoc(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
