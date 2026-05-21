package logql_test

import (
	"strings"
	"testing"

	lokisyntax "github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/logql"
)

// TestParseExprPermissive_MatchAllAccepted pins the load-bearing
// behaviour for cerberus task #219: a stream selector whose sole
// matcher is a regex match-all (`{label=~".*"}`) must round-trip
// through the LogQL parser without bouncing on Loki's
// "empty-compatible matcher" validation.
//
// Upstream Loki's syntax.ParseExpr runs validateMatchers, which calls
// util.SplitFiltersAndMatchers — that helper unconditionally `continue`s
// past MatchRegexp matchers whose Value is exactly `.*` (dropping them
// rather than promoting to a filter), leaving an empty matcher slice
// that fails validation with "queries require at least one regexp or
// equality matcher that does not have an empty-compatible value".
// Cerberus is a query gateway, not an index-scoped store, so the
// rejection is gateway-side noise the user perceives as
// "{service_name=~\".*\"} returns 0 streams". ParseExprPermissive
// catches the rejection by error-string substring and retries via
// ParseExprWithoutValidation so the lowering can emit the equivalent
// CH predicate `match(ResourceAttributes['service_name'], '.*')`.
func TestParseExprPermissive_MatchAllAccepted(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{service_name=~".*"}`,
		`{job=~".*"}`,
		// metric-form query with a match-all selector — also rejected by
		// strict ParseExpr because the validator descends into the
		// sample expr's selector.
		`count_over_time({service_name=~".*"}[5m])`,
		`rate({job=~".*"}[1m])`,
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			// Confirm strict ParseExpr rejects the shape (otherwise the
			// permissive helper is fixing nothing).
			if _, err := lokisyntax.ParseExpr(q); err == nil {
				t.Fatalf("strict ParseExpr(%q) unexpectedly accepted; the permissive helper isn't load-bearing for this query", q)
			} else if !strings.Contains(err.Error(), "empty-compatible") {
				t.Fatalf("strict ParseExpr(%q) failed with unexpected error %q; want 'empty-compatible' rejection", q, err)
			}
			expr, err := logql.ParseExprPermissive(q)
			if err != nil {
				t.Fatalf("ParseExprPermissive(%q): %v; want acceptance via the without-validation fallback", q, err)
			}
			if expr == nil {
				t.Fatalf("ParseExprPermissive(%q) returned nil expr", q)
			}
		})
	}
}

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
	sel, ok := expr.(lokisyntax.LogSelectorExpr)
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
