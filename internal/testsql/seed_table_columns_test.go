package testsql

import (
	"slices"
	"strings"
	"testing"
)

func TestSeedTableColumns_LeadingCommentParentheses(t *testing.T) {
	t.Parallel()
	for _, comment := range []string{
		"-- coalesce(nullIf(TraceId, ''), ResourceAttributes['TraceId'])\n",
		"-- catalog note (unfinished\n",
	} {
		t.Run(strings.TrimSpace(comment), func(t *testing.T) {
			t.Parallel()
			seed := comment + `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String DEFAULT '-- literal (with, parentheses)',
    TraceId String MATERIALIZED 'trace',
    ResourceAttributes Map(String, String) ALIAS map()
) ENGINE = Memory;`
			got := SeedTableColumns(seed)["otel_logs"]
			want := []string{"Timestamp", "Body"}
			if !slices.Equal(got, want) {
				t.Fatalf("SeedTableColumns() = %q, want %q", got, want)
			}
		})
	}
}

func TestExpandStarProjection_SeedLeadingCommentParentheses(t *testing.T) {
	t.Parallel()
	const seed = `-- derived key (comment_token, other_token)
CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    TraceId String MATERIALIZED 'trace'
) ENGINE = Memory;`
	const query = "SELECT s.* FROM (SELECT * FROM `otel_logs`) AS s"
	got := ExpandStarProjection(query, SeedTableColumns(seed))
	head, _ := splitOuterSelect(got)
	want := "s.`Timestamp`, s.`Body`"
	if strings.TrimSpace(head) != want {
		t.Fatalf("expanded projection = %q, want %q", head, want)
	}
}

// TestSeedTableColumns_QuotedNamesUnquoted pins that a backtick-quoted
// CREATE TABLE yields the same table key and column names as an unquoted
// one: callers look columns up by their bare names.
func TestSeedTableColumns_QuotedNamesUnquoted(t *testing.T) {
	t.Parallel()
	const table = "otel_metrics_exponential_histogram"
	want := []string{"MetricName", "Count", "Attributes"}
	for name, seed := range map[string]string{
		"quoted": "CREATE OR REPLACE TABLE `Otel_Metrics_Exponential_Histogram` " +
			"(`MetricName` String, `Count` UInt64, `Attributes` Map(String, String)) ENGINE = Memory;",
		"bare": "CREATE OR REPLACE TABLE Otel_Metrics_Exponential_Histogram " +
			"(MetricName String, Count UInt64, Attributes Map(String, String)) ENGINE = Memory;",
	} {
		got := SeedTableColumns(seed)
		if len(got) != 1 || !slices.Equal(got[table], want) {
			t.Errorf("%s: SeedTableColumns() = %q, want map[%s:%q]", name, got, table, want)
		}
	}
}
