package index

import "strings"

type chineseTokenizer interface {
	Name() string
	SearchTokens(text string) []string
	Close()
}

type noopChineseTokenizer struct{}

func (noopChineseTokenizer) Name() string                 { return "none" }
func (noopChineseTokenizer) SearchTokens(string) []string { return nil }
func (noopChineseTokenizer) Close()                       {}

func filterChineseTokens(words []string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.TrimSpace(strings.ToLower(word))
		if word == "" || !hasCJK(word) {
			continue
		}
		out = append(out, word)
	}
	return out
}

func domainChineseTokens() []string {
	return []string{
		"知识库", "运行时", "观测", "遥测", "上下文", "索引", "文档", "路由", "看板", "工作区",
		"Kanon", "ActRail", "TwinPulse", "TermDeck", "MinT", "OpenClaw",
		"pi-agent", "pi-knowledge-base", "pi-runtime", "obsh", "Context7",
	}
}
