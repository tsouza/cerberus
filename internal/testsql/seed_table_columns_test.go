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
