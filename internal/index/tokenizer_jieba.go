//go:build cgo

package index

import "github.com/yanyiwu/gojieba"

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

func (t *jiebaTokenizer) Name() string { return "jieba" }

func (t *jiebaTokenizer) SearchTokens(text string) []string {
	return filterChineseTokens(t.jieba.CutForSearch(text, true))
}

func (t *jiebaTokenizer) Close() {
	if t.jieba != nil {
		t.jieba.Free()
	}
}
