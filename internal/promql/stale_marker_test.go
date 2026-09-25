package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestStaleMarkers_EverySelectorShapeReadsFlags pins that every selector
// shape reading a Value-bearing arm applies the stale-marker rules under a
// schema whose Flags column was established on every metric table
// (FlagsColumnProbed), and that the default schema — nothing has probed its
// tables — and a schema declaring no Flags column emit SQL that never names
// the column. The chDB fixtures
// test/spec/promql/stale_marker_*.txtar pin the answers against reference
// Prometheus; this pins the reach across the selector builders.
func TestStaleMarkers_EverySelectorShapeReadsFlags(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	const step = 30 * time.Second
	cases := []struct {
		name  string
		query string
		// wantDrop: the range-selection rule (the marker row is dropped at
		// the scan). wantEncode: the instant-selection rule (the marker's
		// Value is rewritten, and the latest-sample pick filtered on it).
		wantDrop, wantEncode bool
		rangeMode            bool
	}{
		{name: "instant bare", query: `up`, wantEncode: true},
		{name: "range-mode bare", query: `up`, wantEncode: true, rangeMode: true},
		{name: "range-mode absolute at", query: `up @ 1767225600`, wantEncode: true, rangeMode: true},
		{name: "instant rate", query: `rate(http_requests_total[5m])`, wantDrop: true},
		{name: "range-mode rate", query: `rate(http_requests_total[5m])`, wantDrop: true, rangeMode: true},
		{name: "companion count union", query: `http_duration_seconds_count`, wantEncode: true},
		{name: "companion count union rate", query: `rate(http_duration_seconds_count[5m])`, wantDrop: true},
		{name: "regex name", query: `{__name__=~"http_.*"}`, wantEncode: true},
		{name: "regex name rate", query: `rate({__name__=~"http_.*"}[5m])`, wantDrop: true},
		{name: "range-mode subquery inner", query: `max_over_time(up[5m:1m])`, wantEncode: true, rangeMode: true},
	}
	p := parser.NewParser(parser.Options{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.query, err)
			}
			emit := func(s schema.Metrics) string {
				t.Helper()
				var sql string
				if tc.rangeMode {
					plan, err := LowerAtRange(context.Background(), expr, s, start, end, step)
					if err != nil {
						t.Fatalf("lower %q: %v", tc.query, err)
					}
					sql, _, err = chsql.Emit(context.Background(), plan)
					if err != nil {
						t.Fatalf("emit %q: %v", tc.query, err)
					}
					return sql
				}
				plan, err := LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower %q: %v", tc.query, err)
				}
				sql, _, err = chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatalf("emit %q: %v", tc.query, err)
				}
				return sql
			}

			probed := schema.DefaultOTelMetrics()
			probed.FlagsColumnProbed = true
			sql := emit(probed)
			const (
				dropMark   = "not((bitAnd(`Flags`, ?) != ?))"
				encodeMark = "if((bitAnd(`Flags`, ?) != ?), reinterpretAsFloat64(?)"
				latestMark = "reinterpretAsUInt64(`Value`) != ?"
			)
			if got := strings.Contains(sql, dropMark); got != tc.wantDrop {
				t.Errorf("range-selection drop present = %v, want %v:\n%s", got, tc.wantDrop, sql)
			}
			if got := strings.Contains(sql, encodeMark) && strings.Contains(sql, latestMark); got != tc.wantEncode {
				t.Errorf("instant-selection encoding present = %v, want %v:\n%s", got, tc.wantEncode, sql)
			}

			if sql := emit(schema.DefaultOTelMetrics()); strings.Contains(sql, "Flags") || strings.Contains(sql, "reinterpretAs") {
				t.Errorf("a schema whose Flags column was never probed must not read one:\n%s", sql)
			}
			noFlags := probed
			noFlags.FlagsColumn = ""
			if sql := emit(noFlags); strings.Contains(sql, "Flags") || strings.Contains(sql, "reinterpretAs") {
				t.Errorf("a schema with no Flags column must not read one:\n%s", sql)
			}
		})
	}
}

// TestStaleMarkers_MetadataIgnoresFlags pins that metadata enumeration
// reads no Flags column: a series whose only in-window sample is a stale
// marker still exists for /series, /labels and /label/<name>/values.
func TestStaleMarkers_MetadataIgnoresFlags(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`up`)
	if err != nil {
		t.Fatal(err)
	}
	probed := schema.DefaultOTelMetrics()
	probed.FlagsColumnProbed = true
	plan, err := LowerMetadataRange(context.Background(), expr, probed, start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "Flags") {
		t.Errorf("metadata lowering must not read Flags:\n%s", sql)
	}
}

// TestStaleMarkers_BucketLayoutIsTimeBounded pins the scan contract of the
// `_bucket` stale-marker layout window: on every instant-selection shape
// the window's input is filtered on the timestamp from both sides — beneath
// the window, where ClickHouse can still prune on it — and the window
// aggregate is the flat `groupUniqArrayArray`, never a per-row
// `groupArray` of every array in the partition. For a range-mode subquery
// the bound is the subquery's widened input window.
func TestStaleMarkers_BucketLayoutIsTimeBounded(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	const step = time.Minute
	cases := []struct {
		name      string
		query     string
		rangeMode bool
		// earliest is the latest the bound's lower edge may sit at: the
		// earliest sample the consumer reads.
		earliest time.Time
		// literal: the bound spans the query range, so its edges are
		// literal instants rather than anchor-relative.
		literal bool
	}{
		{name: "instant", query: `http_duration_seconds_bucket`, earliest: end.Add(-instantLookback)},
		{name: "instant le", query: `http_duration_seconds_bucket{le="1"}`, earliest: end.Add(-instantLookback)},
		{name: "instant regex", query: `{__name__=~"http_duration_seconds_bucket"}`, earliest: end.Add(-instantLookback)},
		{name: "range", query: `http_duration_seconds_bucket`, rangeMode: true, earliest: start.Add(-instantLookback), literal: true},
		{name: "range at", query: `http_duration_seconds_bucket @ 1767225600`, rangeMode: true, earliest: start.Add(-instantLookback)},
		{name: "instant subquery", query: `max_over_time(http_duration_seconds_bucket[10m:1m])`, earliest: end.Add(-10*time.Minute - subqueryStalenessLookback)},
		{name: "range subquery", query: `max_over_time(http_duration_seconds_bucket[10m:1m])`, rangeMode: true, earliest: start.Add(-10*time.Minute - subqueryStalenessLookback), literal: true},
		{name: "instant label_replace subquery", query: `max_over_time(label_replace(http_duration_seconds_bucket, "x", "y", "", "")[10m:1m])`, earliest: end.Add(-10*time.Minute - subqueryStalenessLookback)},
		{name: "range label_replace subquery", query: `max_over_time(label_replace(http_duration_seconds_bucket, "x", "y", "", "")[10m:1m])`, rangeMode: true, earliest: start.Add(-10*time.Minute - subqueryStalenessLookback), literal: true},
	}
	s := schema.DefaultOTelMetrics()
	s.FlagsColumnProbed = true
	p := parser.NewParser(parser.Options{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.query, err)
			}
			var plan chplan.Node
			if tc.rangeMode {
				plan, err = LowerAtRange(context.Background(), expr, s, start, end, step)
			} else {
				plan, err = LowerAt(context.Background(), expr, s, end, end)
			}
			if err != nil {
				t.Fatalf("lower %q: %v", tc.query, err)
			}
			layouts := 0
			chplan.Walk(plan, func(n chplan.Node) bool {
				proj, ok := n.(*chplan.Project)
				if !ok {
					return true
				}
				for _, pr := range proj.Projections {
					chplan.InspectExpr(pr.Expr, func(e chplan.Expr) bool {
						if w, ok := e.(*chplan.WindowExpr); ok && w.Fn != chplan.FnGroupUniqArrayArray {
							t.Errorf("layout window aggregate is %s, want %s", w.Fn, chplan.FnGroupUniqArrayArray)
						}
						return true
					})
				}
				bound, ok := staleMarkerLayoutBoundFilter(proj)
				if !ok {
					return true
				}
				layouts++
				lower, upper := timestampBoundSides(bound.Predicate, s.TimestampColumn)
				if !lower || !upper {
					t.Errorf("layout input bound must bound %s from both sides (lower %v, upper %v): %#v",
						s.TimestampColumn, lower, upper, bound.Predicate)
				}
				lo, ok := literalLowerBound(bound.Predicate)
				switch {
				case tc.literal && !ok:
					t.Errorf("layout input bound must span the query range with literal edges: %#v", bound.Predicate)
				case ok && lo.After(tc.earliest):
					t.Errorf("layout input bound starts at %s, after the earliest sample read at %s", lo, tc.earliest)
				}
				return true
			})
			if layouts == 0 {
				t.Fatalf("no time-bounded stale-marker layout in the plan of %q", tc.query)
			}
		})
	}
}

// timestampBoundSides reports whether pred compares col against a lower
// and an upper edge.
func timestampBoundSides(pred chplan.Expr, col string) (lower, upper bool) {
	chplan.InspectExpr(pred, func(e chplan.Expr) bool {
		b, ok := e.(*chplan.Binary)
		if !ok {
			return true
		}
		if ref, ok := b.Left.(*chplan.ColumnRef); ok && ref.Name == col {
			switch b.Op {
			case chplan.OpGt, chplan.OpGe:
				lower = true
			case chplan.OpLe, chplan.OpLt:
				upper = true
			}
		}
		return true
	})
	return lower, upper
}

// literalLowerBound reads the lower edge of a literal layout bound
// ([staleLayoutWindowBound]); ok is false for an anchor-relative bound.
func literalLowerBound(pred chplan.Expr) (time.Time, bool) {
	var lo time.Time
	found := false
	chplan.InspectExpr(pred, func(e chplan.Expr) bool {
		b, ok := e.(*chplan.Binary)
		if !ok || b.Op != chplan.OpGe {
			return true
		}
		call, ok := b.Right.(*chplan.FuncCall)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if lit, ok := call.Args[0].(*chplan.LitString); ok {
			if parsed, err := time.Parse("2006-01-02 15:04:05.000000000", lit.V); err == nil {
				lo, found = parsed, true
			}
		}
		return true
	})
	return lo, found
}
