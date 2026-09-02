package promql_test

import (
	"context"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// The duplicate-row contract of cerberus issue #2914 is only in force for a
// window the PromQL head actually stamped. These cases pin that stamping at
// the head's PUBLIC boundary — the entry points every handler calls — rather
// than at the internal sweep, so a lowering path that grows its own
// RangeWindow constructor, or a new entry point that forgets the sweep, is
// caught here instead of surfacing as a query that counts duplicated rows
// while its siblings do not.

// distinctSampleRowsAnchor is the deterministic eval grid these cases lower
// against; the specific instant does not matter, only that Start/End/Step are
// pinned so a matrix shape is reachable.
var (
	distinctSampleRowsStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	distinctSampleRowsEnd   = distinctSampleRowsStart.Add(5 * time.Minute)
)

const distinctSampleRowsStep = 30 * time.Second

// parseDistinctSampleRowsExpr parses query with experimental functions
// enabled, so the family table can name mad_over_time.
func parseDistinctSampleRowsExpr(t *testing.T, query string) promparser.Expr {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	return expr
}

// rangeWindowFuncFlags walks plan and returns, per range function name, the
// DistinctSampleRows value every RangeWindow carrying that name was stamped
// with — and fails when two windows of the SAME function disagree, since that
// is itself the divergence the field exists to prevent.
//
// chplan.WalkDeep, matching the sweep under test: a window reachable only
// through a ScalarSubquery's Expr slot must be found here too, or the case
// below could pass while the sweep missed it.
func rangeWindowFuncFlags(t *testing.T, plan chplan.Node) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	seen := map[string]bool{}
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		rw, ok := n.(*chplan.RangeWindow)
		if !ok {
			return true
		}
		if seen[rw.Func] && out[rw.Func] != rw.DistinctSampleRows {
			t.Fatalf("two %s windows in one plan disagree on DistinctSampleRows", rw.Func)
		}
		seen[rw.Func] = true
		out[rw.Func] = rw.DistinctSampleRows
		return true
	})
	if len(out) == 0 {
		t.Fatal("plan holds no RangeWindow; this case would assert nothing")
	}
	return out
}

// TestDistinctSampleRows_StampedByEveryLoweringEntryPoint pins that each
// public lowering entry point returns a plan whose `*_over_time` windows
// carry the duplicate-row contract, and whose rate-family windows do not.
//
// The rate family's exclusion is asserted rather than merely intended: it
// already collapses to one sample per distinct TIMESTAMP (cerberus issue
// #1092), a strictly stronger rule, and setting this field for it would be
// inert on the arms carrying that collapse while introducing a divergence on
// the lagInFrame adjacency arm that carries none.
func TestDistinctSampleRows_StampedByEveryLoweringEntryPoint(t *testing.T) {
	const query = "sum_over_time(m[5m]) + rate(m[5m])"
	s := schema.DefaultOTelMetrics()

	entryPoints := map[string]func(promparser.Expr) (chplan.Node, error){
		"Lower": func(e promparser.Expr) (chplan.Node, error) {
			return promql.Lower(context.Background(), e, s)
		},
		"LowerAt": func(e promparser.Expr) (chplan.Node, error) {
			return promql.LowerAt(context.Background(), e, s,
				distinctSampleRowsStart, distinctSampleRowsEnd)
		},
		"LowerAtRange": func(e promparser.Expr) (chplan.Node, error) {
			return promql.LowerAtRange(context.Background(), e, s,
				distinctSampleRowsStart, distinctSampleRowsEnd, distinctSampleRowsStep)
		},
		"LowerAtRangeOpts": func(e promparser.Expr) (chplan.Node, error) {
			return promql.LowerAtRangeOpts(context.Background(), e, s,
				distinctSampleRowsStart, distinctSampleRowsEnd, distinctSampleRowsStep,
				promql.LowerOpts{})
		},
	}

	for name, lower := range entryPoints {
		t.Run(name, func(t *testing.T) {
			plan, err := lower(parseDistinctSampleRowsExpr(t, query))
			if err != nil {
				t.Fatalf("%s(%q): %v", name, query, err)
			}
			flags := rangeWindowFuncFlags(t, plan)
			if got, ok := flags["sum_over_time"]; !ok || !got {
				t.Errorf("%s: sum_over_time window has DistinctSampleRows=%v (present=%v), want true — "+
					"this entry point returns a plan that would count a duplicated sample row twice",
					name, got, ok)
			}
			if got, ok := flags["rate"]; !ok || got {
				t.Errorf("%s: rate window has DistinctSampleRows=%v (present=%v), want false — "+
					"the rate family carries the stronger per-timestamp collapse of cerberus issue #1092",
					name, got, ok)
			}
		})
	}
}

// distinctSampleRowsDeclaration is the whole declared set, one lowerable
// query per range function, paired with the DistinctSampleRows value its
// window must carry.
//
// It exists because the differential that MEASURES each function's answer is
// chdb-tagged (internal/promql's duplicate_row_range_family_chdb_test.go) and
// therefore runs on merge rather than on every pull request. This case is the
// default-tag lane's own statement of the same contract: it cannot tell a
// right answer from a wrong one, but it fails the moment a function silently
// leaves the set — which is exactly how cerberus issue #2927's five legs went
// unnoticed after #2914 closed the sixteenth.
//
// The `want: false` rows are as load-bearing as the true ones. rate /
// increase / delta carry the STRICTLY STRONGER per-timestamp collapse of
// cerberus issue #1092, so declaring this weaker rule for them would be inert
// where that collapse runs and would state a second, contradictory rule for
// the one shape the two disagree about.
var distinctSampleRowsDeclaration = []struct {
	fn    string
	query string
	want  bool
}{
	{fn: "sum_over_time", query: "sum_over_time(m[5m])", want: true},
	{fn: "quantile_over_time", query: "quantile_over_time(0.5, m[5m])", want: true},
	{fn: "ts_of_max_over_time", query: "ts_of_max_over_time(m[5m])", want: true},
	{fn: "irate", query: "irate(m[5m])", want: true},
	{fn: "idelta", query: "idelta(m[5m])", want: true},
	{fn: "deriv", query: "deriv(m[5m])", want: true},
	{fn: "predict_linear", query: "predict_linear(m[5m], 60)", want: true},
	// double_exponential_smoothing lowers under the canonical "holt_winters"
	// IR name (internal/promql/subquery.go), so this row pins that mapping
	// too: naming the PromQL spelling in the declared set would silently
	// match nothing.
	{fn: "holt_winters", query: "double_exponential_smoothing(m[5m], 0.5, 0.5)", want: true},
	{fn: "changes", query: "changes(m[5m])", want: true},
	{fn: "resets", query: "resets(m[5m])", want: true},
	{fn: "rate", query: "rate(m[5m])", want: false},
	{fn: "increase", query: "increase(m[5m])", want: false},
	{fn: "delta", query: "delta(m[5m])", want: false},
}

// TestDistinctSampleRows_WholeDeclaredSet lowers every row of
// distinctSampleRowsDeclaration and pins the flag its window carries.
func TestDistinctSampleRows_WholeDeclaredSet(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, c := range distinctSampleRowsDeclaration {
		t.Run(c.fn, func(t *testing.T) {
			plan, err := promql.LowerAtRange(context.Background(),
				parseDistinctSampleRowsExpr(t, c.query), s,
				distinctSampleRowsStart, distinctSampleRowsEnd, distinctSampleRowsStep)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", c.query, err)
			}
			got, ok := rangeWindowFuncFlags(t, plan)[c.fn]
			if !ok {
				t.Fatalf("%q lowered to no RangeWindow named %q", c.query, c.fn)
			}
			if got != c.want {
				t.Errorf("%s window has DistinctSampleRows=%v, want %v", c.fn, got, c.want)
			}
		})
	}
}

// TestDistinctSampleRows_StampedInsideAScalarSubquery pins the reason the
// sweep uses chplan.WalkDeep rather than chplan.Walk: a per-step scalar
// argument binds its vector as a chplan.ScalarSubquery, hanging that window
// off an Expr slot Walk does not follow. A window missed there would count
// duplicated rows while the sibling window in the same query did not.
func TestDistinctSampleRows_StampedInsideAScalarSubquery(t *testing.T) {
	// The phi argument is a scalar() over its own *_over_time window, so the
	// plan holds one window on the row-flow spine (mad_over_time) and one
	// reachable only through the ScalarExprs slot (count_over_time).
	const query = "mad_over_time(m[5m]) * scalar(count_over_time(other[5m]))"

	plan, err := promql.LowerAtRange(context.Background(),
		parseDistinctSampleRowsExpr(t, query), schema.DefaultOTelMetrics(),
		distinctSampleRowsStart, distinctSampleRowsEnd, distinctSampleRowsStep)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}

	// The spine-only traversal must NOT already see both windows, or this
	// case would pass without exercising the Expr-slot descent at all.
	spineOnly := map[string]bool{}
	chplan.Walk(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok {
			spineOnly[rw.Func] = true
		}
		return true
	})
	if spineOnly["count_over_time"] {
		t.Fatalf("chplan.Walk already reaches the scalar-subquery window for %q; "+
			"this case no longer distinguishes Walk from WalkDeep", query)
	}

	flags := rangeWindowFuncFlags(t, plan)
	for _, fn := range []string{"mad_over_time", "count_over_time"} {
		got, ok := flags[fn]
		if !ok {
			t.Fatalf("plan holds no %s window for %q", fn, query)
		}
		if !got {
			t.Errorf("%s window has DistinctSampleRows=false; the sweep did not reach it", fn)
		}
	}
}
