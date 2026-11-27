package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fuzirao-png/cosco-smu-ai-e-booking-growth/internal/growth"
)

type row struct {
	ID               string   `json:"id"`
	Query            string   `json:"query"`
	Answer           string   `json:"answer"`
	Intent           string   `json:"intent"`
	Status           string   `json:"status"`
	ExpectedIntent   string   `json:"expected_intent"`
	ExpectedStatus   string   `json:"expected_status"`
	ExpectedEvidence string   `json:"expected_evidence"`
	ExpectedTotal    *int     `json:"expected_total"`
	Passed           bool     `json:"passed"`
	Sources          []string `json:"sources"`
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	mode := flag.String("embedding", "local", "embedding mode (local or bge)")
	check := flag.Bool("check", false, "fail on case failures or Recall@8 below 0.875")
	useJudge := flag.Bool("judge", false, "score the sample with the configured LLM judge")
	count := flag.Int("sample", 24, "sample size")
	exportCSV := flag.String("sample-csv", "", "write the sample to a CSV for human rating")
	humanCSV := flag.String("human-csv", "", "input CSV with completed human scores")
	flag.Parse()
	if *count < 1 {
		return fmt.Errorf("sample size must be positive")
	}
	if err := growth.LoadEnv(); err != nil {
		return err
	}
	all, recall, err := evaluate(ctx, *mode)
	if err != nil {
		return err
	}
	failures := []row{}
	for _, item := range all {
		if !item.Passed {
			failures = append(failures, item)
		}
	}
	report := map[string]any{"embedding": *mode, "cases": len(all), "failures": failures,
		"qrels": len(qrels), "recall_at_8": math.Round(recall*10000) / 10000}
	size := min(*count, len(all))
	sample := make([]row, size)
	for index := range sample {
		sample[index] = all[index*len(all)/size]
	}
	if *exportCSV != "" {
		if err = export(*exportCSV, sample); err != nil {
			return err
		}
	}
	if *useJudge {
		if err = judge(ctx, sample, *humanCSV, report); err != nil {
			return err
		}
	}
	output := json.NewEncoder(os.Stdout)
	output.SetIndent("", "  ")
	if err = output.Encode(report); err != nil {
		return err
	}
	if *check && (len(failures) > 0 || recall < 0.875) {
		return fmt.Errorf("evaluation gate failed")
	}
	return nil
}

func evaluate(ctx context.Context, mode string) ([]row, float64, error) {
	embedder, err := growth.EmbeddingsFromEnv(mode)
	if err != nil {
		return nil, 0, err
	}
	index, err := growth.NewIndex(ctx, embedder)
	if err != nil {
		return nil, 0, err
	}
	directory, err := os.MkdirTemp("", "growth-eval-*")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(directory)
	store, err := growth.OpenStore(ctx, filepath.Join(directory, "evaluation.sqlite3"))
	if err != nil {
		return nil, 0, err
	}
	defer store.Close()
	agent := growth.Agent{Store: store, Index: index}
	var results []row
	for i, item := range cases() {
		session := growth.NewSession("meridian", int64(i+1), growth.Profile{SchemaVersion: 1})
		result, err := agent.Respond(ctx, session, item.query)
		if err != nil {
			return nil, 0, fmt.Errorf("case %d: %w", i+1, err)
		}
		passed := result.Intent == item.intent && result.Status == item.status &&
			strings.Contains(result.Text, item.evidence)
		var expectedTotal *int
		if item.total > 0 {
			expectedTotal = &item.total
			passed = passed && result.Data["total"] == item.total
		}
		sources := make([]string, 0, len(result.Sources))
		for _, source := range result.Sources {
			sources = append(sources, source.Text)
		}
		results = append(results, row{fmt.Sprintf("case-%03d", i+1), item.query, result.Text, result.Intent,
			result.Status, item.intent, item.status, item.evidence, expectedTotal, passed, sources})
	}

	var recall float64
	for _, label := range qrels {
		sources, err := index.Search(ctx, label.query)
		if err != nil {
			return nil, 0, err
		}
		var hits int
		for _, expected := range label.expected {
			if slices.ContainsFunc(sources, func(source growth.Chunk) bool { return source.ID == expected }) {
				hits++
			}
		}
		recall += float64(hits) / float64(len(label.expected))
	}
	return results, recall / float64(len(qrels)), nil
}

func export(path string, rows []row) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = file.WriteString("\uFEFF"); err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	header := []string{"case_id", "query", "answer", "intent", "status"}
	for _, dimension := range criteria {
		header = append(header, dimension.key)
	}
	if err = writer.Write(header); err != nil {
		return err
	}
	for _, item := range rows {
		fields := []string{item.ID, item.Query, item.Answer, item.Intent, item.Status}
		fields = append(fields, make([]string, len(criteria))...)
		if err = writer.Write(fields); err != nil {
			return err
		}
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		return err
	}
	return file.Close()
}
