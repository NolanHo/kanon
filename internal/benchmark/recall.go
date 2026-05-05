package benchmark

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type Case struct {
	Name       string   `json:"name"`
	Query      string   `json:"query"`
	Expected   []string `json:"expected"`
	Limit      int      `json:"limit,omitempty"`
	PathPrefix string   `json:"pathPrefix,omitempty"`
	Kind       string   `json:"kind,omitempty"`
}

type Match struct {
	Path  string  `json:"path"`
	Score float64 `json:"score"`
}

type CaseResult struct {
	Case     Case    `json:"case"`
	Hit      bool    `json:"hit"`
	Rank     int     `json:"rank,omitempty"`
	MRR      float64 `json:"mrr"`
	TopPaths []Match `json:"topPaths"`
}

type Result struct {
	Cases    []CaseResult `json:"cases"`
	Count    int          `json:"count"`
	Hits     int          `json:"hits"`
	RecallAt int          `json:"recallAt"`
	Recall   float64      `json:"recall"`
	MRR      float64      `json:"mrr"`
}

func LoadCases(path string) ([]Case, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	var cases []Case
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, err
	}
	for i, c := range cases {
		if strings.TrimSpace(c.Query) == "" {
			return nil, fmt.Errorf("case %d has empty query", i)
		}
		if len(c.Expected) == 0 {
			return nil, fmt.Errorf("case %q has no expected paths", c.Name)
		}
	}
	return cases, nil
}

func Evaluate(cases []Case, search func(Case) ([]Match, error)) (Result, error) {
	result := Result{Cases: make([]CaseResult, 0, len(cases)), Count: len(cases)}
	limit := 0
	for _, c := range cases {
		if c.Limit > limit {
			limit = c.Limit
		}
		matches, err := search(c)
		if err != nil {
			return Result{}, err
		}
		caseResult := CaseResult{Case: c, TopPaths: matches}
		for i, match := range matches {
			if expectedPath(c.Expected, match.Path) {
				caseResult.Hit = true
				caseResult.Rank = i + 1
				caseResult.MRR = 1.0 / float64(i+1)
				break
			}
		}
		if caseResult.Hit {
			result.Hits++
			result.MRR += caseResult.MRR
		}
		result.Cases = append(result.Cases, caseResult)
	}
	if result.Count > 0 {
		result.Recall = float64(result.Hits) / float64(result.Count)
		result.MRR /= float64(result.Count)
	}
	result.RecallAt = limit
	return result, nil
}

func Misses(result Result) []CaseResult {
	misses := []CaseResult{}
	for _, c := range result.Cases {
		if !c.Hit {
			misses = append(misses, c)
		}
	}
	return misses
}

func PrintText(w io.Writer, result Result) {
	fmt.Fprintf(w, "cases=%d hits=%d recall@%d=%.3f mrr=%.3f\n", result.Count, result.Hits, result.RecallAt, result.Recall, result.MRR)
	misses := Misses(result)
	if len(misses) == 0 {
		return
	}
	sort.Slice(misses, func(i, j int) bool { return misses[i].Case.Name < misses[j].Case.Name })
	for _, miss := range misses {
		fmt.Fprintf(w, "MISS %s query=%q expected=%s\n", miss.Case.Name, miss.Case.Query, strings.Join(miss.Case.Expected, ","))
		for i, match := range miss.TopPaths {
			fmt.Fprintf(w, "  %d. %s %.3f\n", i+1, match.Path, match.Score)
		}
	}
}

func expectedPath(expected []string, path string) bool {
	for _, want := range expected {
		if path == want {
			return true
		}
	}
	return false
}
