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

// TestCounterTemporalityColumnMembership pins WHICH range functions carry
// the schema's AggregationTemporality column down to the emitter, in both
// the plain matrix-selector shape (lowerRangeVectorCall) and the
// subquery-inner shape (lowerSubqueryOverCall).
//
// The membership is a semantic claim, not a taste: a function is on the
// list exactly when its per-window arithmetic differs between a CUMULATIVE
// and a DELTA counter. rate / increase reconstruct a window's increase from
// the samples (#1628); irate does the same over the window's last two
// samples (#1963, item 1). delta / idelta are GAUGE functions that never
// applied the counter-reset rule, and resets / changes count shape events
// in the raw series rather than reconstructing an increase — none of the
// three has a temporality-dependent branch, so carrying the column for them
// would add a dead SELECT-list entry to every query.
//
// Asserting the ABSENCE half is what makes this a real pin: without it, a
// future "just add every range function" widening would pass unnoticed
// while changing the emitted SQL of every gauge window in the corpus.
func TestCounterTemporalityColumnMembership(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if s.AggregationTemporalityColumn == "" {
		t.Fatal("default OTel metrics schema declares no AggregationTemporality column — " +
			"this test would then assert nothing in either direction")
	}
	anchor := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	cases := []struct {
		fn   string
		want bool
	}{
		{fn: "rate", want: true},
		{fn: "increase", want: true},
		{fn: "irate", want: true},
		{fn: "delta", want: false},
		{fn: "idelta", want: false},
		{fn: "resets", want: false},
		{fn: "changes", want: false},
		{fn: "max_over_time", want: false},
	}

	// Each shape spells the same range-vector call at a different lowering
	// site. The subquery form wraps it in an outer reducer, which is what
	// routes it through lowerSubqueryOverCall.
	shapes := []struct {
		name  string
		query func(fn string) string
	}{
		{name: "matrix_selector", query: func(fn string) string {
			return fn + "(http_requests_total[5m])"
		}},
		{name: "subquery_inner", query: func(fn string) string {
			return "max_over_time(" + fn + "(http_requests_total[5m])[10m:1m])"
		}},
	}

	for _, shape := range shapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.fn, func(t *testing.T) {
				t.Parallel()

				query := shape.query(tc.fn)
				p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
				expr, err := p.ParseExpr(query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", query, err)
				}
				plan, err := promql.LowerAt(context.Background(), expr, s, anchor, anchor)
				if err != nil {
					t.Fatalf("LowerAt(%q): %v", query, err)
				}

				rw := rangeWindowForFunc(plan, tc.fn)
				if rw == nil {
					t.Fatalf("LowerAt(%q): no RangeWindow with Func=%q in the plan — the "+
						"lowering shape changed and this case no longer probes what it names",
						query, tc.fn)
				}
				got := rw.TemporalityColumn
				if tc.want && got != s.AggregationTemporalityColumn {
					t.Fatalf("LowerAt(%q): RangeWindow(%s).TemporalityColumn = %q, want %q — "+
						"%s reconstructs a counter's increase, so it MUST branch at runtime "+
						"between the DELTA and CUMULATIVE readings (issues #1628, #1963)",
						query, tc.fn, got, s.AggregationTemporalityColumn, tc.fn)
				}
				if !tc.want && got != "" {
					t.Fatalf("LowerAt(%q): RangeWindow(%s).TemporalityColumn = %q, want empty — "+
						"%s has no temporality-dependent arithmetic, so carrying the column "+
						"only adds a dead column to every emitted query",
						query, tc.fn, got, tc.fn)
				}
			})
		}
	}
}

// rangeWindowForFunc returns the first chplan.RangeWindow in n whose Func
// is fn, or nil when the plan holds none.
func rangeWindowForFunc(n chplan.Node, fn string) *chplan.RangeWindow {
	var found *chplan.RangeWindow
	chplan.Walk(n, func(node chplan.Node) bool {
		if found != nil {
			return false
		}
		if rw, ok := node.(*chplan.RangeWindow); ok && rw.Func == fn {
			found = rw
			return false
		}
		return true
	})
	return found
}
