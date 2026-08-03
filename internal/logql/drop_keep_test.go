package logql_test

import (
	"strings"
	"testing"

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

// TestLowerDropKeep_OnAggregation is the metric-form counterpart of
// TestLowerDropKeep_NoSQLImpact, and asserts the OPPOSITE: in a range
// aggregation the projection is NOT post-fetch-only, because the series
// identity it narrows is also the RangeWindow's grouping key.
//
// Two streams differing only in a label the query drops are one series
// upstream. If the projection never reaches the GROUP BY, ClickHouse
// aggregates them separately and the caller gets a split matrix whose
// labels look plausible — the silently-wrong shape of issue #1491.
func TestLowerDropKeep_OnAggregation(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cases := []struct {
		name string
		// `with` carries the projection stage; `without` strips it.
		// Unlike the log form, these must lower to DIFFERENT SQL.
		with    string
		without string
	}{
		{
			name:    "drop bare name narrows the grouping key",
			with:    `count_over_time({job="api"} | drop env [5m])`,
			without: `count_over_time({job="api"} [5m])`,
		},
		{
			name:    "drop matcher form narrows the grouping key",
			with:    `count_over_time({job="api"} | drop env="prod" [5m])`,
			without: `count_over_time({job="api"} [5m])`,
		},
		{
			name:    "keep narrows the grouping key",
			with:    `rate({job="api"} | keep job [5m])`,
			without: `rate({job="api"} [5m])`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotWith := emitSQL(t, tc.with, s)
			gotWithout := emitSQL(t, tc.without, s)
			if gotWith == gotWithout {
				t.Errorf("projection stage left the SQL untouched, so it never reached the grouping key\nSQL: %s",
					gotWith)
			}
			if !strings.Contains(gotWith, "mapFilter") {
				t.Errorf("lowered SQL carries no mapFilter narrowing\nSQL: %s", gotWith)
			}
		})
	}
}
