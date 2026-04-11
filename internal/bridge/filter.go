package bridge

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NolanHo/vault-bridge/internal/protocol"
)

type FilterConfig struct {
	TrackedSuffixes []string `json:"tracked_suffixes"`
	ExcludedDirs    []string `json:"excluded_dirs"`
	ExcludedFiles   []string `json:"excluded_files"`
}

var (
	filterMu  = &sync.RWMutex{}
	filterCfg = defaultFilterConfig()
)

func defaultFilterConfig() FilterConfig {
	return FilterConfig{
		TrackedSuffixes: []string{".md", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".pdf", ".canvas"},
		ExcludedDirs:    []string{".git", ".obsidian"},
		ExcludedFiles:   []string{".DS_Store"},
	}
}

func DefaultFilterConfig() FilterConfig {
	return cloneFilterConfig(defaultFilterConfig())
}

func CurrentFilterConfig() FilterConfig {
	filterMu.RLock()
	defer filterMu.RUnlock()
	return cloneFilterConfig(filterCfg)
}

func LoadFilterConfig(path string) (FilterConfig, error) {
	if strings.TrimSpace(path) == "" {
		return DefaultFilterConfig(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FilterConfig{}, err
	}
	cfg := DefaultFilterConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FilterConfig{}, fmt.Errorf("read filter config: %w", err)
	}
	return normalizeFilterConfig(cfg), nil
}

func SetFilterConfig(cfg FilterConfig) {
	filterMu.Lock()
	defer filterMu.Unlock()
	filterCfg = normalizeFilterConfig(cfg)
}

func CleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("path must be relative")
	}
	clean := path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if clean == "." || clean == "" {
		return "", fmt.Errorf("path is empty")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes root")
	}
	return clean, nil
}

func IsWatchableDir(rel string) bool {
	cfg := CurrentFilterConfig()
	excludedDirs := sliceSet(cfg.ExcludedDirs)
	if rel == "" {
		return true
	}
	for _, part := range strings.Split(rel, "/") {
		if _, blocked := excludedDirs[part]; blocked {
			return false
		}
	}
	return true
}

func IsTrackedFile(rel string) bool {
	cfg := CurrentFilterConfig()
	excludedDirs := sliceSet(cfg.ExcludedDirs)
	excludedFiles := sliceSet(cfg.ExcludedFiles)
	clean, err := CleanRel(rel)
	if err != nil {
		return false
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts[:len(parts)-1] {
		if _, blocked := excludedDirs[part]; blocked {
			return false
		}
	}
	name := parts[len(parts)-1]
	if _, blocked := excludedFiles[name]; blocked {
		return false
	}
	lowerName := strings.ToLower(name)
	for _, suffix := range cfg.TrackedSuffixes {
		if strings.HasSuffix(lowerName, suffix) {
			return true
		}
	}
	return false
}

func ScanRoot(root string) (map[string]protocol.FileMeta, error) {
	out := make(map[string]protocol.FileMeta)
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if !IsWatchableDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsTrackedFile(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		out[rel] = protocol.FileMeta{
			MtimeNS: info.ModTime().UnixNano(),
			Size:    info.Size(),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func cloneFilterConfig(cfg FilterConfig) FilterConfig {
	return FilterConfig{
		TrackedSuffixes: append([]string(nil), cfg.TrackedSuffixes...),
		ExcludedDirs:    append([]string(nil), cfg.ExcludedDirs...),
		ExcludedFiles:   append([]string(nil), cfg.ExcludedFiles...),
	}
}

func normalizeFilterConfig(cfg FilterConfig) FilterConfig {
	out := cloneFilterConfig(cfg)
	if len(out.TrackedSuffixes) == 0 {
		out.TrackedSuffixes = DefaultFilterConfig().TrackedSuffixes
	}
	if len(out.ExcludedDirs) == 0 {
		out.ExcludedDirs = DefaultFilterConfig().ExcludedDirs
	}
	if len(out.ExcludedFiles) == 0 {
		out.ExcludedFiles = DefaultFilterConfig().ExcludedFiles
	}
	for i, suffix := range out.TrackedSuffixes {
		suffix = strings.ToLower(strings.TrimSpace(suffix))
		if suffix == "" {
			continue
		}
		if !strings.HasPrefix(suffix, ".") {
			suffix = "." + suffix
		}
		out.TrackedSuffixes[i] = suffix
	}
	for i, item := range out.ExcludedDirs {
		out.ExcludedDirs[i] = strings.TrimSpace(item)
	}
	for i, item := range out.ExcludedFiles {
		out.ExcludedFiles[i] = strings.TrimSpace(item)
	}
	return out
}

func sliceSet(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		if x == "" {
			continue
		}
		out[x] = struct{}{}
	}
	return out
}
