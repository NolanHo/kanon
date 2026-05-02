package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
)

type FilterConfig struct {
	TrackedSuffixes      []string `json:"tracked_suffixes"`
	ExcludedDirs         []string `json:"excluded_dirs"`
	ExcludedFiles        []string `json:"excluded_files"`
	ExcludedFilePatterns []string `json:"excluded_file_patterns"`
	ExcludedPathPatterns []string `json:"excluded_path_patterns"`
}

type pathMatcher struct {
	pattern  string
	basename bool
	prefix   bool
	regex    *regexp.Regexp
}

type compiledFilterConfig struct {
	cfg           FilterConfig
	excludedDirs  map[string]struct{}
	excludedFiles map[string]struct{}
	fileMatchers  []pathMatcher
	matchers      []pathMatcher
}

var (
	filterMu       = &sync.RWMutex{}
	filterCfg      = normalizeFilterConfig(defaultFilterConfig())
	filterCompiled = compileFilterConfig(filterCfg)
)

func defaultFilterConfig() FilterConfig {
	return FilterConfig{
		TrackedSuffixes:      []string{".md", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".pdf", ".canvas"},
		ExcludedDirs:         []string{".git", ".obsidian", ".venv", "venv", "node_modules", "__pycache__", ".ruff_cache", ".mypy_cache", ".pytest_cache"},
		ExcludedFiles:        []string{".DS_Store"},
		ExcludedFilePatterns: []string{"*.log", "*.tmp", "*.swp", "*.swo"},
		ExcludedPathPatterns: []string{"logs/**", "**/logs/**", "log/**", "**/log/**"},
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
	filterCompiled = compileFilterConfig(filterCfg)
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
	if rel == "" {
		return true
	}
	clean, err := CleanRel(rel)
	if err != nil {
		return false
	}
	return !currentCompiledFilter().isExcluded(clean)
}

func IsTrackedFile(rel string) bool {
	clean, err := CleanRel(rel)
	if err != nil {
		return false
	}
	cfg := currentCompiledFilter()
	if cfg.isExcluded(clean) {
		return false
	}
	parts := strings.Split(clean, "/")
	name := parts[len(parts)-1]
	if cfg.isExcludedFile(clean, name) {
		return false
	}
	lowerName := strings.ToLower(name)
	for _, suffix := range cfg.cfg.TrackedSuffixes {
		if strings.HasSuffix(lowerName, suffix) {
			return true
		}
	}
	return false
}

func cloneFilterConfig(cfg FilterConfig) FilterConfig {
	return FilterConfig{
		TrackedSuffixes:      append([]string(nil), cfg.TrackedSuffixes...),
		ExcludedDirs:         append([]string(nil), cfg.ExcludedDirs...),
		ExcludedFiles:        append([]string(nil), cfg.ExcludedFiles...),
		ExcludedFilePatterns: append([]string(nil), cfg.ExcludedFilePatterns...),
		ExcludedPathPatterns: append([]string(nil), cfg.ExcludedPathPatterns...),
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
	if len(out.ExcludedFilePatterns) == 0 {
		out.ExcludedFilePatterns = DefaultFilterConfig().ExcludedFilePatterns
	}
	for i, item := range out.ExcludedFilePatterns {
		out.ExcludedFilePatterns[i] = normalizeFilePattern(item)
	}
	for i, item := range out.ExcludedPathPatterns {
		out.ExcludedPathPatterns[i] = normalizePathPattern(item)
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

func currentCompiledFilter() compiledFilterConfig {
	filterMu.RLock()
	defer filterMu.RUnlock()
	return filterCompiled
}

func compileFilterConfig(cfg FilterConfig) compiledFilterConfig {
	out := compiledFilterConfig{
		cfg:           cloneFilterConfig(cfg),
		excludedDirs:  sliceSet(cfg.ExcludedDirs),
		excludedFiles: sliceSet(cfg.ExcludedFiles),
	}
	for _, pattern := range cfg.ExcludedFilePatterns {
		matcher := compileFileMatcher(pattern)
		if matcher.pattern == "" {
			continue
		}
		out.fileMatchers = append(out.fileMatchers, matcher)
	}
	for _, pattern := range cfg.ExcludedPathPatterns {
		matcher := compilePathMatcher(pattern)
		if matcher.pattern == "" {
			continue
		}
		out.matchers = append(out.matchers, matcher)
	}
	return out
}

func (cfg compiledFilterConfig) isExcluded(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if _, blocked := cfg.excludedDirs[part]; blocked {
			return true
		}
	}
	for _, matcher := range cfg.matchers {
		if matcher.matches(rel) {
			return true
		}
	}
	return false
}

func (cfg compiledFilterConfig) isExcludedFile(rel, name string) bool {
	if _, blocked := cfg.excludedFiles[name]; blocked {
		return true
	}
	for _, matcher := range cfg.fileMatchers {
		if matcher.matches(rel) {
			return true
		}
	}
	return false
}

func normalizeFilePattern(pattern string) string {
	pattern = strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
	pattern = strings.TrimPrefix(pattern, "/")
	return pattern
}

func normalizePathPattern(pattern string) string {
	pattern = strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
	if pattern == "" {
		return ""
	}
	for strings.HasPrefix(pattern, "./") {
		pattern = strings.TrimPrefix(pattern, "./")
	}
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return ""
	}
	cleaned := path.Clean(pattern)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func compileFileMatcher(pattern string) pathMatcher {
	pattern = normalizeFilePattern(pattern)
	if pattern == "" {
		return pathMatcher{}
	}
	basename := !strings.Contains(pattern, "/")
	return pathMatcher{
		pattern:  pattern,
		basename: basename,
		regex:    regexp.MustCompile("^" + globToRegex(pattern) + "$"),
	}
}

func compilePathMatcher(pattern string) pathMatcher {
	pattern = normalizePathPattern(pattern)
	if pattern == "" {
		return pathMatcher{}
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pathMatcher{pattern: pattern, prefix: true}
	}
	descendants := strings.HasSuffix(pattern, "/**")
	base := pattern
	if descendants {
		base = strings.TrimSuffix(base, "/**")
	}
	regexText := "^" + globToRegex(base)
	if descendants {
		regexText += "(?:/.*)?"
	}
	regexText += "$"
	return pathMatcher{
		pattern: pattern,
		regex:   regexp.MustCompile(regexText),
	}
}

func (m pathMatcher) matches(rel string) bool {
	if m.pattern == "" {
		return false
	}
	if m.basename {
		parts := strings.Split(rel, "/")
		rel = parts[len(parts)-1]
	}
	if m.prefix {
		return rel == m.pattern || strings.HasPrefix(rel, m.pattern+"/")
	}
	return m.regex.MatchString(rel)
}

func globToRegex(pattern string) string {
	var b strings.Builder
	b.Grow(len(pattern) * 2)
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	return b.String()
}
