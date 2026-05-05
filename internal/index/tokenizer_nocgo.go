//go:build !cgo

package index

func newChineseTokenizer() chineseTokenizer {
	return newGSETokenizer()
}
