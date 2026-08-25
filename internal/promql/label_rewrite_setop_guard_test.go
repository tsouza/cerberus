package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_LabelRewrite_OverVectorSetOp_KeysOnAttributesOnly pins a
// pre-release audit finding: guardKeysOnTimestamp had no case for
// *chplan.VectorSetOp / *chplan.NaryVectorSetOp, so it fell through to the
// trailing `return true` ("many rows per series at distinct timestamps,
// key on timestamp") even for an INSTANT-mode `or`, whose operands are
// already collapsed to one row per series — the same shape the
// *chplan.Aggregate case correctly excludes.
//
// That made guardLabelRewriteCollision group by (Attributes, TimeUnix)
// instead of Attributes alone. label_replace(a or b, "job", "merged",
// "job", ".*") where the two colliding series carry different own-last-
// sample timestamps (the common case) would then silently emit two output
// rows sharing the identical rewritten label set instead of raising the
// required 422 "vector cannot contain metrics with the same labelset":
// each colliding row lands in its own single-row (Attributes, TimeUnix)
// group and `count() > 1` never fires.
//
// This test pins the PLAN SHAPE the fix restores — the collision guard's
// Aggregate must group on Attributes alone for an instant-mode set-op
// input, never on the timestamp column — for both the plain-float
// VectorSetOp path (binary.go's lowerVectorSetOp) exercised here via
// label_replace/label_join, and is mirrored by the histogram-mixed
// VectorSetOp path (histogram_native_mixed_or_label.go) in
// TestLower_LabelRewrite_OverMixedExpHistogramSetOp_KeysOnAttributesOnly.
func TestLower_LabelRewrite_OverVectorSetOp_KeysOnAttributesOnly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "label_replace",
			query: `label_replace(metric_a or metric_b, "job", "merged", "job", ".*")`,
		},
		{
			name:  "label_join",
			query: `label_join(metric_a or metric_b, "job", ",", "job")`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			// Sanity: the guard's input really is a VectorSetOp (or its
			// n-ary flattened form) — otherwise this test would pass
			// vacuously for an unrelated reason.
			var sawSetOp bool
			chplan.Walk(plan, func(n chplan.Node) bool {
				switch n.(type) {
				case *chplan.VectorSetOp, *chplan.NaryVectorSetOp:
					sawSetOp = true
				}
				return true
			})
			if !sawSetOp {
				t.Fatalf("Lower(%q): no VectorSetOp/NaryVectorSetOp in plan; test no longer exercises the guarded shape", tc.query)
			}

			var agg *chplan.Aggregate
			chplan.Walk(plan, func(n chplan.Node) bool {
				if candidate, ok := n.(*chplan.Aggregate); ok && agg == nil {
					agg = candidate
				}
				return true
			})
			if agg == nil {
				t.Fatalf("Lower(%q): no collision-guard Aggregate in plan", tc.query)
			}

			for _, alias := range agg.GroupByAliases {
				if alias == s.TimestampColumn {
					t.Fatalf(
						"Lower(%q): collision guard grouped on %q for an instant-mode set-op input (GroupByAliases=%v); "+
							"each series already carries its OWN last-sample timestamp here, so keying on it "+
							"splits every colliding pair into its own single-row group and the duplicate-labelset "+
							"guard never fires",
						tc.query, s.TimestampColumn, agg.GroupByAliases,
					)
				}
			}
			// The group key is (Attributes, MetricName) — label_replace/
			// label_join preserve the name (see guardLabelRewriteCollision's
			// doc comment), so it joins the key alongside Attributes. The
			// timestamp check above is the load-bearing assertion; this is
			// a sanity check that nothing else crept into the key either.
			wantKey := map[string]bool{s.AttributesColumn: true, s.MetricNameColumn: true}
			if len(agg.GroupByAliases) != len(wantKey) {
				t.Fatalf("Lower(%q): collision guard GroupByAliases = %v, want exactly %v (order-independent)", tc.query, agg.GroupByAliases, wantKey)
			}
			for _, alias := range agg.GroupByAliases {
				if !wantKey[alias] {
					t.Fatalf("Lower(%q): collision guard GroupByAliases = %v, want exactly %v (order-independent)", tc.query, agg.GroupByAliases, wantKey)
				}
			}
		})
	}
}

// TestLower_LabelRewrite_OverMixedExpHistogramSetOp_KeysOnAttributesOnly is
// the histogram-mixed VectorSetOp sibling of
// TestLower_LabelRewrite_OverVectorSetOp_KeysOnAttributesOnly — the same
// guardKeysOnTimestamp gap, reached via
// histogram_native_mixed_or_label.go's lowerLabelCallOverMixedExpHistogramSetOp
// instead of the plain-float binary.go path.
func TestLower_LabelRewrite_OverMixedExpHistogramSetOp_KeysOnAttributesOnly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	expr, err := p.ParseExpr(`label_replace(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist), "dst", "x", "service", ".*")`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	var agg *chplan.Aggregate
	chplan.Walk(plan, func(n chplan.Node) bool {
		if candidate, ok := n.(*chplan.Aggregate); ok && agg == nil {
			agg = candidate
		}
		return true
	})
	if agg == nil {
		t.Fatalf("no collision-guard Aggregate in plan")
	}
	for _, alias := range agg.GroupByAliases {
		if alias == s.TimestampColumn {
			t.Fatalf(
				"collision guard grouped on %q for an instant-mode mixed set-op input (GroupByAliases=%v); "+
					"the duplicate-labelset guard would never fire for two colliding series with "+
					"different own-sample timestamps",
				s.TimestampColumn, agg.GroupByAliases,
			)
		}
	}
}
