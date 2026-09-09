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

// TestParseExprPermissive_RelaxesOnlyTheMatcherRule pins the scope of
// the permissive contract: an empty-compatible selector buys the query
// relief from the empty-compatible-matcher rejection and from NOTHING
// else.
//
// The retry used to go through syntax.ParseExprWithoutValidation, which
// drops the entire post-parse walk. One intentionally-relaxed rule
// therefore relaxed every other parse-time rejection with it, and the
// justifying comment's claim — that the lowering re-raises them
// downstream — was false: the `err` fields are unexported and the
// Selector()-borne rejections are reachable only through Selector(),
// which lower.go never calls. So each query below was ACCEPTED and
// answered while its `{job="x"}` twin was rejected, i.e. adding a
// match-all matcher disabled validation for the whole expression
// (cerberus issue #3183).
//
// Each case is stated as a PAIR so the assertion cannot pass by the
// permissive path simply rejecting everything: the twin with a
// constraining matcher must produce the SAME rejection through the
// strict parser, which is the rule upstream Loki applies.
func TestParseExprPermissive_RelaxesOnlyTheMatcherRule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// emptyCompatible routes through the permissive retry (its
		// selector is `{app=~".*"}`); constraining is the same shape
		// with a matcher that constrains, so the strict parser sees it.
		emptyCompatible string
		constraining    string
	}{
		{
			name:            "grouping_not_allowed_on_range_aggregation",
			emptyCompatible: `count_over_time({app=~".*"}[5m]) + count_over_time({job="x"}[5m]) by (level)`,
			constraining:    `count_over_time({job="y"}[5m]) + count_over_time({job="x"}[5m]) by (level)`,
		},
		{
			name:            "sort_does_not_allow_grouping",
			emptyCompatible: `sort(count_over_time({app=~".*"}[5m])) by (level)`,
			constraining:    `sort(count_over_time({job="x"}[5m])) by (level)`,
		},
		{
			name:            "invalid_label_replace_regex",
			emptyCompatible: `label_replace(count_over_time({app=~".*"}[5m]), "d", "$1", "src", "[")`,
			constraining:    `label_replace(count_over_time({job="x"}[5m]), "d", "$1", "src", "[")`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, strictErr := syntax.ParseExpr(tc.constraining)
			if strictErr == nil {
				t.Fatalf("ParseExpr(%q) accepted; the pair is only meaningful if the "+
					"constraining twin is rejected by the rule under test", tc.constraining)
			}

			_, permErr := logql.ParseExprPermissive(tc.emptyCompatible)
			if permErr == nil {
				t.Fatalf("ParseExprPermissive(%q) accepted a shape upstream rejects with %q — "+
					"the empty-compatible matcher relaxed a rule it has nothing to do with",
					tc.emptyCompatible, strictErr)
			}
			if permErr.Error() != strictErr.Error() {
				t.Fatalf("ParseExprPermissive(%q) = %q, want the same rejection its "+
					"constraining twin gets: %q", tc.emptyCompatible, permErr, strictErr)
			}

			// The relaxation itself must still work: the same selector
			// with no other defect is accepted.
			if _, err := logql.ParseExprPermissive(`count_over_time({app=~".*"}[5m])`); err != nil {
				t.Fatalf("ParseExprPermissive rejected a plain empty-compatible selector: %v", err)
			}
		})
	}
}
