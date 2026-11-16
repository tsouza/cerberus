package logql_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// corpusParserQuery is the Loki compatibility corpus's drilldown query
// (regression/drilldown-patterns.yaml): two parser stages, a drop and a
// label filter over the merged labels.
const corpusParserQuery = `{service_name="web-server"} | json | logfmt | drop __error__, __error_details__ | level="error" or level="warn"`

// logQuerySQL returns the statement a limited, backward log query sends
// ClickHouse, optimized as the engine optimizes it.
func logQuerySQL(t *testing.T, q string) string {
	t.Helper()
	start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	l := &logql.Lang{
		Schema: schema.DefaultOTelLogs(), Start: start, End: start.Add(24 * time.Hour),
		LogLineLimit: 100, LogLineBackward: true,
	}
	dr, err := (&engine.Engine{Optimizer: optimizer.Default()}).DryRunSQL(context.Background(), l, q)
	if err != nil {
		t.Fatalf("DryRunSQL(%s): %v", q, err)
	}
	return dr.SQL
}

// TestLogQueryDefinesEachLabelsMapTwiceAtMost pins the corpus query's
// copy bounds. Each parser stage's merged labels map is defined once below
// the ORDER BY … LIMIT, for the label filter, and once above it, for the
// returned rows; the sort carries neither. The detected_level cascade is
// defined once.
func TestLogQueryDefinesEachLabelsMapTwiceAtMost(t *testing.T) {
	t.Parallel()
	sql := logQuerySQL(t, corpusParserQuery)

	for _, alias := range []string{"_logql_labels_0", "_logql_labels_1"} {
		if got := strings.Count(sql, "AS `"+alias+"`"); got != 2 {
			t.Errorf("%s is defined %d times, want 2: once below the limit and once above it", alias, got)
		}
	}
	// The logfmt stage parses the line once per definition of its map.
	if got := strings.Count(sql, "extractKeyValuePairs("); got != 2 {
		t.Errorf("the logfmt parse appears %d times, want 2", got)
	}
	if got := strings.Count(sql, "multiIf((`SeverityText` != ?), `SeverityText`"); got != 1 {
		t.Errorf("the detected_level source cascade appears %d times, want 1", got)
	}

	// The SELECT the sort reads lists plain columns, none of them a
	// labels map.
	if got := sortedSelectList(t, sql); !regexp.MustCompile("^`[A-Za-z]+`(, `[A-Za-z]+`)*$").MatchString(got) {
		t.Errorf("the sort carries %q, want table columns only", got)
	}
}

// sortedSelectList returns the select list of the subquery the statement's
// ORDER BY sorts: the parenthesised SELECT that ends right before it.
func sortedSelectList(t *testing.T, sql string) string {
	t.Helper()
	end := strings.LastIndex(sql, ") ORDER BY")
	if end < 0 {
		t.Fatalf("no sorted subquery in %s", sql)
	}
	depth := 0
	for i := end; i >= 0; i-- {
		switch sql[i] {
		case ')':
			depth++
		case '(':
			depth--
		}
		if depth == 0 {
			sub := strings.TrimPrefix(sql[i+1:end], "SELECT ")
			before, _, ok := strings.Cut(sub, " FROM ")
			if !ok {
				t.Fatalf("sorted subquery %q has no FROM", sub)
			}
			return before
		}
	}
	t.Fatalf("unbalanced parentheses before the ORDER BY in %s", sql)
	return ""
}
