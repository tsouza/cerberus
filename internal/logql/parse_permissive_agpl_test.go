//go:build agpl_oracle

// A/B oracle: TestParseExprPermissive_MatchAllAccepted confirms upstream
// Loki's strict parser actually rejects the "empty-compatible matcher"
// shape ParseExprPermissive works around — otherwise the permissive
// fallback would be fixing a bug that doesn't exist. This is the ONLY
// test in this package that imports the AGPL grafana/loki syntax
// package (the sibling parse_permissive_test.go pins the rest of
// ParseExprPermissive's behaviour without it), gated behind the
// `agpl_oracle` build tag so neither the production build nor the
// default test run links it. Run explicitly with:
//
//	go test -tags agpl_oracle ./internal/logql/
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
