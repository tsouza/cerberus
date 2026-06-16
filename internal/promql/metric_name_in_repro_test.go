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

	// Size pin: a single 64-candidate span-metric query rendered ~3035 bytes
	// of inline OR-chain pre-fix. With the flat IN (PR #795) the candidate
	// set folds to one `MetricName IN (?,…)` term; the rc.5 resource-attr
	// projection adds a fixed `mapUpdate(sanitize(RA), sanitize(Attributes))`
	// wrapper per arm; and a `_sum`/`_count` selector unions THREE arms —
	// histogram (bare), sum (suffixed) and gauge (suffixed) — so a standalone
	// `<x>_sum` gauge resolves. This 3-arm union renders ~2.5KB for the
	// 64-candidate worst case (vs ~1.6KB for the prior 2-arm union).
	//
	// This bounds a SINGLE user query, which is nowhere near 256KB. The
	// metadata handlers' 128-variant fan-out — where size genuinely matters —
	// is protected unconditionally by maxRenderedQueryBytes (metadata.go),
	// which re-measures each built chunk's BOUND byte length and splits it
	// further (down to one arm) whenever it breaches the budget. So the 256KB
	// ceiling never depends on this heuristic; the pin just catches a gross
	// per-query regression. 3KB leaves margin for the gauge arm.
	const perQueryBound = 3072
	if len(sql) >= perQueryBound {
		t.Errorf("single 64-candidate span-metric query rendered %d bytes (want < %d) — gross per-query size regression:\n%.400s",
			len(sql), perQueryBound, sql)
	}
}
