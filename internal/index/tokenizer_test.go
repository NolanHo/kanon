package index

import "testing"

func TestChineseTokenizerDomainTokens(t *testing.T) {
	tok := newChineseTokenizer()
	defer tok.Close()
	tokens := tok.SearchTokens("知识库检索和运行时布局")
	if !contains(tokens, "知识库") {
		t.Fatalf("tokens=%v missing 知识库", tokens)
	}
	if !contains(tokens, "运行") && !contains(tokens, "运行时") {
		t.Fatalf("tokens=%v missing runtime-related Chinese token", tokens)
	}
}

func TestAliasExpansionForChineseQuery(t *testing.T) {
	idx := &Index{tok: newChineseTokenizer()}
	defer idx.tok.Close()
	tokens := idx.queryTokens("知识库")
	for _, want := range []string{"知识库", "knowledge", "base", "kanon"} {
		if !contains(tokens, want) {
			t.Fatalf("tokens=%v missing %s", tokens, want)
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
