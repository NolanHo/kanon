package index

import "github.com/go-ego/gse"

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

func (t *gseTokenizer) Name() string { return "gse" }

func (t *gseTokenizer) SearchTokens(text string) []string {
	return filterChineseTokens(t.seg.CutSearch(text, true))
}

func (t *gseTokenizer) Close() {}

func addDomainTokens(seg *gse.Segmenter) {
	for _, token := range domainChineseTokens() {
		_ = seg.AddToken(token, 100000, "nz")
	}
}
