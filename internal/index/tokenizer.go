package index

import (
	"strings"

	"github.com/go-ego/gse"
	"github.com/yanyiwu/gojieba"
)

type chineseTokenizer interface {
	SearchTokens(text string) []string
	Close()
}

type jiebaTokenizer struct {
	jieba *gojieba.Jieba
}

func newChineseTokenizer() chineseTokenizer {
	jieba := gojieba.NewJieba()
	if jieba != nil {
		return &jiebaTokenizer{jieba: jieba}
	}
	return newGSETokenizer()
}

func (t *jiebaTokenizer) SearchTokens(text string) []string {
	return filterChineseTokens(t.jieba.CutForSearch(text, true))
}

func (t *jiebaTokenizer) Close() {
	if t.jieba != nil {
		t.jieba.Free()
	}
}

type gseTokenizer struct {
	seg gse.Segmenter
}

func newGSETokenizer() chineseTokenizer {
	seg, err := gse.NewEmbed()
	if err != nil {
		return noopChineseTokenizer{}
	}
	addDomainTokens(&seg)
	return &gseTokenizer{seg: seg}
}

func (t *gseTokenizer) SearchTokens(text string) []string {
	return filterChineseTokens(t.seg.CutSearch(text, true))
}

func (t *gseTokenizer) Close() {}

type noopChineseTokenizer struct{}

func (noopChineseTokenizer) SearchTokens(string) []string { return nil }
func (noopChineseTokenizer) Close()                       {}

func addDomainTokens(seg *gse.Segmenter) {
	for _, token := range domainChineseTokens() {
		_ = seg.AddToken(token, 100000, "nz")
	}
}

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
