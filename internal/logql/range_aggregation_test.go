package logql

import (
	"context"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerRangeAggregationAppliesUnwrapPostFilters pins the
// `e.Left.Unwrap != nil && len(e.Left.Unwrap.PostFilters) > 0` guard at
// the top of [lowerRangeAggregation]. A CONDITIONALS_NEGATION mutant
// flips `> 0` to `<= 0`, so a non-empty PostFilters slice would skip
// the branch entirely and the resulting plan would lack the
// post-filter predicate.
//
// A query carrying a real post-filter (`| status > 100`) MUST surface
// that predicate in the emitted SQL. The CONDITIONALS_NEGATION mutant
// drops the post-filter; the SQL no longer carries the `status` key.
func TestLowerRangeAggregationAppliesUnwrapPostFilters(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// `status > 100` rides as a post-filter on top of `unwrap
	// latency`. The post-filter MapAccess key is "status" — a
	// distinct identifier from the unwrap's "latency" so the
	// presence-check below isolates the post-filter contribution
	// from the unwrap value extraction.
	query := `sum_over_time({app="api"} | logfmt | unwrap latency | status > 100 [5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Left.Unwrap == nil {
		t.Fatalf("ParseExpr(%q): Unwrap is nil — fixture invalid", query)
	}
	if len(ra.Left.Unwrap.PostFilters) == 0 {
		t.Fatalf("ParseExpr(%q): PostFilters is empty — fixture invalid", query)
	}

	plan, err := lowerRangeAggregation(ra, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerRangeAggregation: %v", err)
	}

	// Emit the SQL and confirm the post-filter `status` key
	// surfaces as a literal argument. The CONDITIONALS_NEGATION
	// mutant would skip the AND-fold entirely; without the
	// post-filter, no `status` substring appears anywhere.
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
	if !argsContain(args, "status") {
		t.Fatalf("emitted SQL args do not carry the post-filter key %q\nargs=%v\nsql=%s", "status", args, sqlStr)
	}
}

// argsContain reports whether any arg passed to chsql.Emit contains the
// given substring. Loki's `| status > 100` post-filter becomes a
// MapAccess(RA, 'status') in the emitted SQL, surfacing 'status' as
// a bound parameter string.
func argsContain(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, want) {
			return true
		}
	}
	return false
}
