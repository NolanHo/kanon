package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/NolanHo/kanon/internal/benchmark"
)

type queryRequest struct {
	Kind       string `json:"kind,omitempty"`
	Limit      int    `json:"limit"`
	PathPrefix string `json:"pathPrefix,omitempty"`
	Query      string `json:"query"`
}

type queryResponse struct {
	Matches []benchmark.Match `json:"matches"`
}

func main() {
	server := flag.String("server", "http://127.0.0.1:39090", "Kanon server base URL")
	casesPath := flag.String("cases", "testdata/docs-recall-cases.json", "JSON recall cases")
	limit := flag.Int("limit", 10, "default query limit")
	jsonOut := flag.Bool("json", false, "print full JSON result")
	failUnder := flag.Float64("fail-under", 0, "exit nonzero when recall is below this value")
	failOnCase := flag.Bool("fail-on-case", false, "exit nonzero when any case misses maxRank or negative assertions")
	flag.Parse()

	cases, err := benchmark.LoadCases(*casesPath)
	if err != nil {
		fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	base := strings.TrimRight(*server, "/")
	result, err := benchmark.Evaluate(cases, func(c benchmark.Case) ([]benchmark.Match, error) {
		l := c.Limit
		if l <= 0 {
			l = *limit
		}
		body, err := json.Marshal(queryRequest{Query: c.Query, Limit: l, PathPrefix: c.PathPrefix, Kind: c.Kind})
		if err != nil {
			return nil, err
		}
		resp, err := client.Post(base+"/v1/query", "application/json", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("query %q failed: HTTP %s", c.Query, resp.Status)
		}
		var qr queryResponse
		if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
			return nil, err
		}
		return qr.Matches, nil
	})
	if err != nil {
		fatal(err)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fatal(err)
		}
	} else {
		benchmark.PrintText(os.Stdout, result)
	}
	if *failUnder > 0 && result.Recall < *failUnder {
		os.Exit(1)
	}
	if *failOnCase && result.Failures > 0 {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
