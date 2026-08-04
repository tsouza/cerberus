package logql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/logql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestParseExprPermissive_WellFormedPassesThrough pins that the helper
// is a no-op for queries the strict ParseExpr already accepts — it
// MUST NOT silently downgrade validation for shapes Loki considers
// well-formed.
func TestParseExprPermissive_WellFormedPassesThrough(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{service_name="api"}`,
		`{service_name=~"api|web"}`,
		`{service_name=~".+"}`,
		`{service_name!=""}`,
		`{job="api"} | json`,
		`rate({job="api"}[5m])`,
		`sum by (level) (count_over_time({job="api"}[5m]))`,
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := logql.ParseExprPermissive(q)
			if err != nil {
				t.Fatalf("ParseExprPermissive(%q): %v", q, err)
			}
			if expr == nil {
				t.Fatalf("ParseExprPermissive(%q) returned nil expr", q)
			}
		})
	}
}

// TestParseExprPermissive_GenuineErrorsStillSurface pins that
// non-empty-compatible parse failures (unterminated strings, missing
// values, invalid stage syntax, …) keep returning errors — the helper
// must only widen the one specific rejection class.
func TestParseExprPermissive_GenuineErrorsStillSurface(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{job="api"`,        // unterminated brace
		`{job=}`,            // missing matcher value
		`rate({job="api"})`, // missing range
		`{job="api"} |~ "(`, // unterminated regex group
		`{job="api"} | unknown_parser_stage`,
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			if _, err := logql.ParseExprPermissive(q); err == nil {
				t.Fatalf("ParseExprPermissive(%q) accepted malformed input; the permissive helper must keep rejecting non-empty-compatible failures", q)
			}
		})
	}
}

// TestParseExprPermissive_MatchAllMatcherSurvives confirms the
// permissive path's returned AST still carries the match-all matcher,
// so cerberus's lowering can emit the expected
// `match(ResourceAttributes['<label>'], '.*')` predicate (which CH
// then prunes via PREWHERE / sparse index skip).
//
// Without this, even if Parse succeeded, the lowering would walk an
// empty matcher slice and produce a Scan with no Filter — selecting
// every row in the logs table. That's catastrophically expensive on a
// production CH cluster, so the matcher MUST survive the fallback.
func TestParseExprPermissive_MatchAllMatcherSurvives(t *testing.T) {
	t.Parallel()

	expr, err := logql.ParseExprPermissive(`{service_name=~".*"}`)
	if err != nil {
		t.Fatalf("ParseExprPermissive: %v", err)
	}
	sel, ok := expr.(syntax.LogSelectorExpr)
	if !ok {
		t.Fatalf("expr is not a LogSelectorExpr: %T", expr)
	}
	matchers := sel.Matchers()
	if len(matchers) != 1 {
		t.Fatalf("matchers len = %d; want 1 (the surviving .* matcher)", len(matchers))
	}
	m := matchers[0]
	if m.Name != "service_name" {
		t.Errorf("matcher.Name = %q; want %q", m.Name, "service_name")
	}
	if m.Value != ".*" {
		t.Errorf("matcher.Value = %q; want %q", m.Value, ".*")
	}
}
