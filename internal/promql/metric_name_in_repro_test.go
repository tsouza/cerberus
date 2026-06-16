package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// countMetricNameTerms counts how many `MetricName =`/`MetricName !=`
// equality terms appear in the rendered SQL. We assert on the SQL text
// rather than the IR so the test pins the exact 256KB-blowup shape
// ClickHouse rejected: a long inline disjunction of equalities.
func countMetricNameTerms(sql, col string) int {
	return strings.Count(sql, col+" =") + strings.Count(sql, col+" !=")
}

// TestMetricNamePredicate_DoesNotBlowUpAsInlineOrChain is the Step-1
// reproduction pin for the PR #790 follow-up. A single `__name__` equality
// matcher over a heavily-underscored span-metric name fans out — via the
// dotted-candidate powerset (PromLabelToOTelCandidates) — into many
// MetricName equalities. The pre-fix lowering folded those into a
// left-associative `(... OR (MetricName = 'lit'))` chain rendered inline,
// which on the metrics-explorer broad probe crossed ClickHouse's 256KB
// max_query_size at position 262124 (code 62, "Max query size exceeded").
//
// The fix renders the candidate set as a single flat, parameterized
// `MetricName IN (?, …)` instead.
func TestMetricNamePredicate_DoesNotBlowUpAsInlineOrChain(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()

	// A span-metric base name with exactly the underscore count that drives
	// the maximal 2^6 = 64-candidate powerset fan-out — the shape behind the
	// traces_service_graph_* family the metrics-explorer probes.
	name := "traces_service_graph_request_server_seconds_sum"

	m, err := labels.NewMatcher(labels.MatchEqual, "__name__", name)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	pred := metricNamePredicate(m, s)

	// The predicate must be a single InList membership test — not a Binary
	// OR-chain. This is the load-bearing structural assertion.
	in, ok := pred.(*chplan.InList)
	if !ok {
		// Count how many MetricName equality leaves the OR-chain carries so
		// the failure message shows the blowup magnitude.
		var leaves int
		var walk func(chplan.Expr)
		walk = func(e chplan.Expr) {
			switch v := e.(type) {
			case *chplan.Binary:
				if v.Op == chplan.OpOr || v.Op == chplan.OpAnd {
					walk(v.Left)
					walk(v.Right)
					return
				}
				leaves++
			default:
			}
		}
		walk(pred)
		t.Fatalf("metricNamePredicate returned %T (an inline OR/AND chain of %d MetricName comparisons), want *chplan.InList — this is the 256KB-blowup shape", pred, leaves)
	}
	if len(in.List) < 2 {
		t.Fatalf("expected the dotted-candidate fan-out to produce >1 candidate, got %d", len(in.List))
	}
}

// TestMetricNameQueryRange_RenderedSQLBounded drives the full lowering +
// SQL emission for a heavily-underscored span metric and asserts the
// rendered ClickHouse SQL uses a flat parameterized IN, not an inline
// OR-chain of MetricName literals.
func TestMetricNameQueryRange_RenderedSQLBounded(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	col := s.MetricNameColumn

	name := "traces_service_graph_request_server_seconds_sum"
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(name)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	if !strings.Contains(sql, "IN (") {
		t.Errorf("rendered SQL does not use a flat IN membership for the candidate set:\n%s", sql)
	}
	// No inline string literal of the dotted candidate should appear in the
	// SQL text — every candidate must bind through `?`.
	if strings.Contains(sql, "traces.service") {
		t.Errorf("rendered SQL inlines a dotted candidate literal (not parameterized):\n%s", sql)
	}
	// A flat IN renders the column name in at most one comparison term.
	if n := countMetricNameTerms(sql, col); n > 1 {
		t.Errorf("rendered SQL chains %d MetricName comparisons — inline OR/AND blowup, want a single IN:\n%s", n, sql)
	}

	// Size pin: a single 64-candidate span-metric arm rendered ~3035 bytes
	// of inline OR-chain pre-fix. Crossed with the metadata handlers' 128-arm
	// chunk cap that is ~388KB — over ClickHouse's 256KB max_query_size,
	// which is the exact `code: 62 … Max query size exceeded` at position
	// 262124 the compose-smoke probe hit. With the flat IN the same arm
	// renders ~1067 bytes; the rc.5 resource-attribute projection adds a
	// fixed per-arm `mapUpdate(sanitize(RA), sanitize(Attributes))` wrapper
	// (~1577 bytes/arm). 128 arms ≈ 197KB, still comfortably under the
	// ceiling. Pin a 1.75KB per-arm bound (128 arms ≈ 229KB < 256KB) so the
	// chunk cap can never re-cross 256KB.
	const perArmBound = 1792
	if len(sql) >= perArmBound {
		t.Errorf("single 64-candidate span-metric arm rendered %d bytes (want < %d) — risks re-crossing max_query_size when UNION-ALL'd across the chunk cap:\n%.400s",
			len(sql), perArmBound, sql)
	}
}
