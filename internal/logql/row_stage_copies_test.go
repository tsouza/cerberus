package logql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// Signatures of the per-row values a range aggregation's row stage
// computes, as they appear in the emitted statement. Each occurrence is
// one copy of the value's definition in the statement text.
const (
	// The logfmt parser stage.
	logfmtParserSignature = "extractKeyValuePairs"
	// The detected_level source cascade's SeverityText arm.
	detectedLevelSignature = "multiIf((`SeverityText` != ?), `SeverityText`"
	// The duration conversion's component split.
	durationParseSignature = string(chplan.FnRegexExtractAllGroupsHorizontal)
	// The lookup of a parsed label that falls back to structured
	// metadata: one per pipeline stage that reads the label.
	parsedLabelLookupSignature = "[?], `LogAttributes`[?])"
)

// TestRowStageValuesAreDefinedOnce pins that a LogQL range aggregation
// computes each per-row value once in the emitted statement and reads
// it by column name everywhere else.
//
// ClickHouse evaluates identical subexpressions once per block, but it
// parses and analyses every copy in the statement text, and a value
// nested inside another value is copied once per read of the outer one.
// An `| unwrap duration(...)` over a logfmt-parsed label used to copy the
// parser stage 48 times and the detected_level cascade 32 times, an
// 82 KB statement whose analysis took longer than its execution.
func TestRowStageValuesAreDefinedOnce(t *testing.T) {
	t.Parallel()

	const selector = `{service_name="mimir"}`
	cases := []struct {
		name  string
		query string
		// Copies of each signature the statement may carry.
		parser, level, duration, lookup int
	}{
		{
			name:  "count",
			query: `count_over_time(` + selector + ` [5m])`,
			level: 1,
		},
		{
			name:   "unwrap",
			query:  `sum_over_time(` + selector + ` | logfmt | unwrap duration [5m])`,
			parser: 1, level: 1, lookup: 1,
		},
		{
			name:   "unwrap_duration",
			query:  `sum_over_time(` + selector + ` | logfmt | unwrap duration(duration) [5m])`,
			parser: 1, level: 1, duration: 1, lookup: 1,
		},
		{
			// The label filter and the unwrap are two stages reading
			// the label, one lookup each.
			name:   "label_filter_unwrap_duration",
			query:  `sum_over_time(` + selector + ` | logfmt | duration != "" | unwrap duration(duration) [5m])`,
			parser: 1, level: 1, duration: 1, lookup: 2,
		},
		{
			// Each `detected_level` filter compares the level column;
			// the `level` filter is the one stage reading a parsed label.
			name: "detected_level_filters",
			query: `sum by (detected_level) (count_over_time(` + selector +
				` | detected_level="debug" or detected_level="info" or detected_level="warn" | logfmt | level=~"(?i)info" [5m]))`,
			parser: 1, level: 1, lookup: 1,
		},
		{
			// Each duration filter parses its label once.
			name:   "duration_filters",
			query:  `count_over_time(` + selector + ` | logfmt | latency > 250ms or latency < 1ms [5m])`,
			parser: 1, level: 1, duration: 2, lookup: 2,
		},
		{
			name: "by_detected_level",
			query: `sum by (detected_level) (sum_over_time(` + selector +
				` | logfmt | duration != "" | unwrap duration(duration) [5m]))`,
			parser: 1, level: 1, duration: 1, lookup: 2,
		},
	}
	start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := logql.ParseExprPermissive(tc.query)
			if err != nil {
				t.Fatalf("ParseExprPermissive(%q): %v", tc.query, err)
			}
			plan, err := logql.LowerAtRange(context.Background(), expr, schema.DefaultOTelLogs(),
				start, start.Add(24*time.Hour), time.Minute)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
			}
			sqlStr, _, err := chsql.Emit(context.Background(), optimizer.Default().Run(context.Background(), plan))
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			for _, sig := range []struct {
				what      string
				signature string
				want      int
			}{
				{"logfmt parser stage", logfmtParserSignature, tc.parser},
				{"detected_level derivation", detectedLevelSignature, tc.level},
				{"duration conversion", durationParseSignature, tc.duration},
				{"parsed label lookup", parsedLabelLookupSignature, tc.lookup},
			} {
				if got := strings.Count(sqlStr, sig.signature); got != sig.want {
					t.Errorf("%s: %d copies in the statement, want %d (%d bytes)", sig.what, got, sig.want, len(sqlStr))
				}
			}
		})
	}
}
