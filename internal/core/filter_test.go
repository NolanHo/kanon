package core

import (
	"os"
	"testing"
	"time"
)

func TestPathPatternFiltering(t *testing.T) {
	cfg := normalizeFilterConfig(FilterConfig{
		TrackedSuffixes:      []string{".md", ".png"},
		ExcludedDirs:         []string{".git", ".obsidian"},
		ExcludedFiles:        []string{".DS_Store"},
		ExcludedFilePatterns: []string{"*.log"},
		ExcludedPathPatterns: []string{
			"mint/issues/issue432/02_live_validation/**",
			"deep-research/**/.venv/**",
			"logs/*.tmp",
		},
	})
	SetFilterConfig(cfg)
	t.Cleanup(func() {
		SetFilterConfig(DefaultFilterConfig())
	})

	cases := []struct {
		path      string
		watchable bool
		tracked   bool
	}{
		{
			path:      "mint/issues/issue432/02_live_validation/20260330_1452_e2e_start",
			watchable: false,
			tracked:   false,
		},
		{
			path:      "deep-research/github-activity-2026-04/.venv/lib/site.py",
			watchable: false,
			tracked:   false,
		},
		{
			path:      "logs/run.tmp",
			watchable: false,
			tracked:   false,
		},
		{
			path:      "notes/debug.log",
			watchable: true,
			tracked:   false,
		},
		{
			path:      "paper/myblog/wezterm/post_v1.md",
			watchable: true,
			tracked:   true,
		},
		{
			path:      ".obsidian/workspace.json",
			watchable: false,
			tracked:   false,
		},
	}

	for _, tc := range cases {
		if got := IsWatchableDir(tc.path); got != tc.watchable {
			t.Fatalf("IsWatchableDir(%q)=%v want %v", tc.path, got, tc.watchable)
		}
		if got := IsTrackedFile(tc.path); got != tc.tracked {
			t.Fatalf("IsTrackedFile(%q)=%v want %v", tc.path, got, tc.tracked)
		}
	}
}

func TestExcludedFilePatternCanOverrideTrackedSuffix(t *testing.T) {
	cfg := normalizeFilterConfig(FilterConfig{
		TrackedSuffixes:      []string{".md", ".log"},
		ExcludedDirs:         []string{".git"},
		ExcludedFiles:        []string{".DS_Store"},
		ExcludedFilePatterns: []string{"*.log", "secrets/*.md"},
	})
	SetFilterConfig(cfg)
	t.Cleanup(func() {
		SetFilterConfig(DefaultFilterConfig())
	})

	if IsTrackedFile("runs/train.log") {
		t.Fatal("runs/train.log should be excluded by basename file pattern")
	}
	if IsTrackedFile("secrets/token.md") {
		t.Fatal("secrets/token.md should be excluded by path file pattern")
	}
	if !IsTrackedFile("notes/keep.md") {
		t.Fatal("notes/keep.md should remain tracked")
	}
}

func TestNormalizePathPattern(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		" ./mint/issues/** ":           "mint/issues/**",
		"/deep-research/.venv":         "deep-research/.venv",
		"paper//myblog/../myblog/*.md": "paper/myblog/*.md",
	}

	for input, want := range cases {
		if got := normalizePathPattern(input); got != want {
			t.Fatalf("normalizePathPattern(%q)=%q want %q", input, got, want)
		}
	}
}

func TestNoPersistWhenReconcileHasNoChanges(t *testing.T) {
	dir := t.TempDir()
	root := dir + "/root"
	state := dir + "/state"
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/note.md", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	SetFilterConfig(DefaultFilterConfig())
	t.Cleanup(func() {
		SetFilterConfig(DefaultFilterConfig())
	})

	store, err := OpenStore(root, state)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if first.Upserts != 1 || first.Deletes != 0 {
		t.Fatalf("first reconcile=%+v want upserts=1 deletes=0", first)
	}

	metaBefore, err := os.Stat(store.metaPath)
	if err != nil {
		t.Fatal(err)
	}
	snapBefore, err := os.Stat(store.snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	seqBefore := store.CurrentSeq()

	time.Sleep(1100 * time.Millisecond)

	second, err := store.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if second.Upserts != 0 || second.Deletes != 0 {
		t.Fatalf("second reconcile=%+v want no changes", second)
	}
	if store.CurrentSeq() != seqBefore {
		t.Fatalf("current seq=%d want %d", store.CurrentSeq(), seqBefore)
	}

	metaAfter, err := os.Stat(store.metaPath)
	if err != nil {
		t.Fatal(err)
	}
	snapAfter, err := os.Stat(store.snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if !metaAfter.ModTime().Equal(metaBefore.ModTime()) {
		t.Fatalf("meta mtime changed: before=%s after=%s", metaBefore.ModTime(), metaAfter.ModTime())
	}
	if !snapAfter.ModTime().Equal(snapBefore.ModTime()) {
		t.Fatalf("snapshot mtime changed: before=%s after=%s", snapBefore.ModTime(), snapAfter.ModTime())
	}
}
