package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// fakeExplainer returns a canned Explanation per query string, so BuildReport /
// Write can be exercised offline without the engine.
type fakeExplainer struct {
	byQuery map[string]Explanation
}

func (f fakeExplainer) Explain(_ context.Context, q HarvestedQuery) Explanation {
	if ex, ok := f.byQuery[q.Expr]; ok {
		return ex
	}
	return Explanation{Err: errors.New("no canned explanation")}
}

// TestFileSourceHarvest writes a small rule file and asserts both a recording
// and an alerting rule are harvested with the right kind + source, and that an
// unmatched path is counted as a skip rather than dropped.
func TestFileSourceHarvest(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: cpu
    rules:
      - record: job:cpu:rate5m
        expr: sum(rate(cpu_seconds_total[5m])) by (job)
      - alert: HighErrorRate
        expr: rate(errors_total[5m]) > 0.5
      - record: empty:rule
        expr: "   "
`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(dir, "nope-*.yml")
	src := FileSource{RulePaths: []string{file, missing}}
	queries, skipped, err := src.Harvest(context.Background())
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}

	if len(queries) != 2 {
		t.Fatalf("expected 2 harvested queries, got %d: %+v", len(queries), queries)
	}
	if queries[0].Kind != KindRecord || !strings.Contains(queries[0].Source, "/cpu/job:cpu:rate5m") {
		t.Errorf("recording rule harvested wrong: %+v", queries[0])
	}
	if queries[1].Kind != KindAlert || !strings.Contains(queries[1].Source, "/cpu/HighErrorRate") {
		t.Errorf("alerting rule harvested wrong: %+v", queries[1])
	}

	// One skip for the empty-expr rule, one for the unmatched glob.
	if len(skipped) != 2 {
		t.Fatalf("expected 2 skips, got %d: %+v", len(skipped), skipped)
	}
	var sawEmpty, sawNoMatch bool
	for _, s := range skipped {
		if strings.Contains(s.Source, "empty:rule") && strings.Contains(s.Reason, "empty expr") {
			sawEmpty = true
		}
		if s.Source == missing && strings.Contains(s.Reason, "no files matched") {
			sawNoMatch = true
		}
	}
	if !sawEmpty {
		t.Error("empty-expr rule should be counted as a skip")
	}
	if !sawNoMatch {
		t.Error("unmatched glob should be counted as a skip")
	}
}

// TestTables pins that Tables collects Table + UnionTables across scans,
// qualifies with Database, and returns a deduplicated, sorted list.
func TestTables(t *testing.T) {
	plan := &chplan.Filter{
		Input: &chplan.Scan{
			Database:    "otel",
			UnionTables: []string{"otel_metrics_sum", "otel_metrics_gauge"},
		},
		Predicate: &chplan.ColumnRef{Name: "x"},
	}
	got := Tables(plan)
	want := []string{"otel.otel_metrics_gauge", "otel.otel_metrics_sum"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Tables = %v, want %v", got, want)
	}
}

// TestLintSubqueryFanout pins that a subquery RangeWindow is flagged with its
// anchor count, and that a plain range window is not flagged.
func TestLintSubqueryFanout(t *testing.T) {
	subquery := &chplan.RangeWindow{
		Input:      &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:       "rate",
		OuterRange: 10 * time.Minute,
		Step:       time.Minute,
	}
	risks := Lint(subquery)
	if len(risks) != 1 || !strings.Contains(risks[0], "11 anchors") {
		t.Fatalf("expected a subquery fan-out risk with 11 anchors, got %v", risks)
	}

	plain := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate"}
	if risks := Lint(plain); len(risks) != 0 {
		t.Errorf("plain range window should carry no offline risk, got %v", risks)
	}
}

// TestBuildReportAndWrite exercises the full assembly: a supported query gets
// SQL + tables, a broken query is marked UNSUPPORTED (build keeps going), and
// the rendered report carries the honesty note plus both outcomes.
func TestBuildReportAndWrite(t *testing.T) {
	src := staticSource{queries: []HarvestedQuery{
		{Expr: "up", Source: "rule:f/g/up_rec", Kind: KindRecord},
		{Expr: "!!!", Source: "rule:f/g/broken", Kind: KindAlert},
	}}
	ex := fakeExplainer{byQuery: map[string]Explanation{
		"up": {
			SQL:  "SELECT * FROM otel_metrics_gauge",
			Plan: &chplan.Scan{Table: "otel_metrics_gauge"},
		},
		"!!!": {Err: errors.New("parse: unexpected token")},
	}}

	rep, err := BuildReport(context.Background(), src, ex)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(rep.Queries) != 2 {
		t.Fatalf("expected 2 query reports, got %d", len(rep.Queries))
	}
	if rep.Queries[0].SQL != "SELECT * FROM otel_metrics_gauge" {
		t.Errorf("supported query SQL wrong: %q", rep.Queries[0].SQL)
	}
	if got := rep.Queries[0].Tables; len(got) != 1 || got[0] != "otel_metrics_gauge" {
		t.Errorf("supported query tables wrong: %v", got)
	}
	if rep.Queries[1].Unsupported == "" {
		t.Error("broken query should be marked unsupported")
	}

	var sb strings.Builder
	if err := rep.Write(&sb); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"cardinality is NOT knowable offline",
		"SELECT * FROM otel_metrics_gauge",
		"otel_metrics_gauge",
		"UNSUPPORTED: parse: unexpected token",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
}

// staticSource is a CorpusSource that returns a fixed query list.
type staticSource struct {
	queries []HarvestedQuery
	skipped []SkippedEntry
}

func (s staticSource) Harvest(context.Context) ([]HarvestedQuery, []SkippedEntry, error) {
	return s.queries, s.skipped, nil
}
