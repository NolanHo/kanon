//go:build !cgo

package index

func newJiebaTokenizerForEval() chineseTokenizer {
	return newGSETokenizer()
}
