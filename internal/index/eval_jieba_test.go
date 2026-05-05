//go:build cgo

package index

import "github.com/yanyiwu/gojieba"

func newJiebaTokenizerForEval() chineseTokenizer {
	jieba := gojieba.NewJieba()
	if jieba == nil {
		return noopChineseTokenizer{}
	}
	return &jiebaTokenizer{jieba: jieba}
}
