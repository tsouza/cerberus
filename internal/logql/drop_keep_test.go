package logql_test

import (
	"context"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerDropKeep_NoSQLImpact pins the contract that `| drop` and
// `| keep` are post-fetch stages: they project the output label map in
// Go after the rows return, so the lowered SQL contains exactly the
// same predicates as the equivalent query without the projection stage.
//
// This mirrors the existing decolorize / line_format / label_format /
// unpack / pattern stages — see internal/logql/lower.go for the dispatch.
func TestLowerDropKeep_NoSQLImpact(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []struct {
		name string
		// `with` includes the projection stage; `without` strips it.
		// Both should lower to the same SQL.
		with    string
		without string
	}{
		{
			name:    "drop single label on bare selector",
			with:    `{job="api"} | drop env`,
			without: `{job="api"}`,
		},
		{
			name:    "drop multiple labels after line filter",
			with:    `{job="api"} |= "error" | drop env, pod`,
			without: `{job="api"} |= "error"`,
		},
		{
			name:    "drop matcher form before label filter",
			with:    `{job="api"} | drop env="prod" | level="error"`,
			without: `{job="api"} | level="error"`,
		},
		{
			name:    "keep single label on bare selector",
			with:    `{job="api"} | keep job`,
			without: `{job="api"}`,
		},
		{
			name:    "keep multiple labels after line filter",
			with:    `{job="api"} |= "info" | keep job, env`,
			without: `{job="api"} |= "info"`,
		},
		{
			name:    "keep matcher form combined with line filter",
			with:    `{job="api"} |= "warn" | keep env="prod"`,
			without: `{job="api"} |= "warn"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotWith := emitSQL(t, tc.with, s)
			gotWithout := emitSQL(t, tc.without, s)
			if gotWith != gotWithout {
				t.Errorf("projection stage altered SQL\nwith stage:    %s\nwithout stage: %s",
					gotWith, gotWithout)
			}
		})
	}
}

// TestLowerDropKeep_OnAggregation pins that `| drop` and `| keep` are
// also accepted in the metric form (e.g. `count_over_time({...} | drop
// env [5m])`). The projection runs after rows return; it doesn't change
// the SQL counting shape.
func TestLowerDropKeep_OnAggregation(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []string{
		`count_over_time({job="api"} | drop env [5m])`,
		`rate({job="api"} | keep job, env [5m])`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			if _, err := logql.Lower(context.Background(), expr, s); err != nil {
				t.Errorf("Lower(%q) unexpectedly failed: %v", q, err)
			}
		})
	}
}
