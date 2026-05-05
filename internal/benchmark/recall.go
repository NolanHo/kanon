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
	Negative   []string `json:"negative,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	MaxRank    int      `json:"maxRank,omitempty"`
	PathPrefix string   `json:"pathPrefix,omitempty"`
	Kind       string   `json:"kind,omitempty"`
}

type Match struct {
	Path  string  `json:"path"`
	Score float64 `json:"score"`
}

type CaseResult struct {
	Case            Case    `json:"case"`
	Hit             bool    `json:"hit"`
	Rank            int     `json:"rank,omitempty"`
	MRR             float64 `json:"mrr"`
	RankPass        bool    `json:"rankPass"`
	NegativePass    bool    `json:"negativePass"`
	FirstNegative   string  `json:"firstNegative,omitempty"`
	FirstNegativeAt int     `json:"firstNegativeAt,omitempty"`
	TopPaths        []Match `json:"topPaths"`
}

type Result struct {
	Cases          []CaseResult `json:"cases"`
	Count          int          `json:"count"`
	Hits           int          `json:"hits"`
	RankPasses     int          `json:"rankPasses"`
	NegativePasses int          `json:"negativePasses"`
	Failures       int          `json:"failures"`
	RecallAt       int          `json:"recallAt"`
	Recall         float64      `json:"recall"`
	MRR            float64      `json:"mrr"`
	Top1           float64      `json:"top1"`
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
		caseResult := CaseResult{Case: c, TopPaths: matches, RankPass: true, NegativePass: true}
		for i, match := range matches {
			if expectedPath(c.Expected, match.Path) && caseResult.Rank == 0 {
				caseResult.Hit = true
				caseResult.Rank = i + 1
				caseResult.MRR = 1.0 / float64(i+1)
			}
			if expectedPath(c.Negative, match.Path) && caseResult.FirstNegativeAt == 0 {
				caseResult.FirstNegative = match.Path
				caseResult.FirstNegativeAt = i + 1
			}
		}
		if c.MaxRank > 0 {
			caseResult.RankPass = caseResult.Hit && caseResult.Rank <= c.MaxRank
		}
		if len(c.Negative) > 0 && caseResult.FirstNegativeAt > 0 {
			caseResult.NegativePass = caseResult.Hit && caseResult.Rank < caseResult.FirstNegativeAt
		}
		if caseResult.Hit {
			result.Hits++
			result.MRR += caseResult.MRR
			if caseResult.Rank == 1 {
				result.Top1++
			}
		}
		if caseResult.RankPass {
			result.RankPasses++
		}
		if caseResult.NegativePass {
			result.NegativePasses++
		}
		if !caseResult.Hit || !caseResult.RankPass || !caseResult.NegativePass {
			result.Failures++
		}
		result.Cases = append(result.Cases, caseResult)
	}
	if result.Count > 0 {
		result.Recall = float64(result.Hits) / float64(result.Count)
		result.MRR /= float64(result.Count)
		result.Top1 /= float64(result.Count)
	}
	result.RecallAt = limit
	return result, nil
}

func Failures(result Result) []CaseResult {
	failures := []CaseResult{}
	for _, c := range result.Cases {
		if !c.Hit || !c.RankPass || !c.NegativePass {
			failures = append(failures, c)
		}
	}
	return failures
}

func PrintText(w io.Writer, result Result) {
	fmt.Fprintf(w, "cases=%d hits=%d recall@%d=%.3f top1=%.3f mrr=%.3f rank_pass=%d/%d negative_pass=%d/%d failures=%d\n", result.Count, result.Hits, result.RecallAt, result.Recall, result.Top1, result.MRR, result.RankPasses, result.Count, result.NegativePasses, result.Count, result.Failures)
	failures := Failures(result)
	if len(failures) == 0 {
		return
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Case.Name < failures[j].Case.Name })
	for _, failure := range failures {
		fmt.Fprintf(w, "FAIL %s query=%q expected=%s rank=%d maxRank=%d negative=%s negativeRank=%d hit=%t rankPass=%t negativePass=%t\n", failure.Case.Name, failure.Case.Query, strings.Join(failure.Case.Expected, ","), failure.Rank, failure.Case.MaxRank, failure.FirstNegative, failure.FirstNegativeAt, failure.Hit, failure.RankPass, failure.NegativePass)
		for i, match := range failure.TopPaths {
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
