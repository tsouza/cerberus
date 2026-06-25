package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// compareNode builds a minimal valid MetricsCompare (no root lookup —
// the join shape is covered by the lowering-level tests + TXTAR
// fixtures; this file pins the emitter's own contract).
func compareNode() *chplan.MetricsCompare {
	return &chplan.MetricsCompare{
		Selection: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "StatusCode"},
			Right: &chplan.LitString{V: "Error"},
		},
		TopN: 10,
		Pairs: &chplan.FuncCall{Name: "array", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "tuple", Args: []chplan.Expr{
				&chplan.LitString{V: "name"},
				&chplan.ColumnRef{Name: "SpanName"},
			}},
		}},
		SelAlias:   "is_selection",
		AttrAlias:  "attr",
		ValAlias:   "val",
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
}

// compareNodeWithRoot extends compareNode with the per-trace root
// lookup leg (the LEFT JOIN shape that the production traceql
// drilldown compare emits). It mirrors internal/traceql's
// compareRootLookup: an Aggregate over a Filter(ParentSpanId empty)
// GROUP BY TraceId. The join shape is what makes scan-bound pushdown
// non-trivial (a window filter above `s LEFT JOIN r` cannot prune
// either MergeTree leg), so the matrix pushdown test below exercises
// this node rather than the join-free compareNode.
func compareNodeWithRoot() *chplan.MetricsCompare {
	m := compareNode()
	m.TraceIDColumn = "TraceId"
	m.RootLookup = &chplan.Aggregate{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_traces"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "ParentSpanId"},
				Right: &chplan.LitString{V: ""},
			},
		},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		AggFuncs: []chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}}, Alias: "__root_name"},
		},
	}
	return m
}

// TestEmitRangeWindowCompare_JoinScanPushdown pins the FIX-1 scan-
// bounding pushdown for the join (RootLookup) shape — the prod
// traces-drilldown OOM. The (Start - range, End] Timestamp window must
// land INSIDE each MergeTree scan of `s LEFT JOIN r`, never on the
// SELECT wrapping the join (CH 24.12 cannot push a join-level predicate
// into either leg):
//
//   - the `s` span leg carries the bound in its own WHERE, immediately
//     above the `AS s` alias;
//   - the `r` root leg is seeded with `TraceId IN (<bounded cohort
//     trace-ids>)` so the same window prunes the root scan through the
//     GROUP BY TraceId aggregate, while preserving rootName enrichment
//     for every trace the join can match.
func TestEmitRangeWindowCompare_JoinScanPushdown(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{
		Input:           compareNodeWithRoot(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := chsql.Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	lo := "`Timestamp` > toDateTime64('2026-05-12 10:00:00.000000000', 9) - toIntervalNanosecond(60000000000)"
	hi := "`Timestamp` <= toDateTime64('2026-05-12 10:03:00.000000000', 9)"

	// The 's' span leg: bound sits in the WHERE immediately preceding the
	// `AS s` alias — i.e. inside the scan, below the join.
	sLeg := "WHERE " + lo + " AND " + hi + ") AS s"
	if !strings.Contains(sql, sLeg) {
		t.Errorf("matrix join SQL must bound the 's' scan inside the join (want %q):\n%s", sLeg, sql)
	}

	// The 'r' root leg: seeded by the bounded cohort's trace-id set so
	// the root scan prunes the same way, with enrichment preserved.
	rSeed := "WHERE `TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM (SELECT * FROM `otel_traces`) " +
		"WHERE " + lo + " AND " + hi + ") AS _cmp_seed)) AS r"
	if !strings.Contains(sql, rSeed) {
		t.Errorf("matrix join SQL must seed the 'r' root leg by bounded trace-ids (want %q):\n%s", rSeed, sql)
	}

	// Regression guard: the bound must NOT sit on the SELECT that wraps
	// the whole `s LEFT JOIN r` (the original un-prunable placement). The
	// join's ON clause is the last token before the wrapping SELECT's
	// own scope; assert no Timestamp predicate trails the join's ON.
	onIdx := strings.Index(sql, "ON s.`TraceId` = r.`TraceId`")
	if onIdx < 0 {
		t.Fatalf("expected the LEFT JOIN ON clause in:\n%s", sql)
	}
	if strings.Contains(sql[onIdx:], "`Timestamp` >") || strings.Contains(sql[onIdx:], "`Timestamp` <=") {
		t.Errorf("Timestamp bound must not sit above the join (found after ON clause):\n%s", sql[onIdx:])
	}
}

// TestEmitMetricsCompare_BareShape — bare emission groups by
// (cohort, attr, val) with a deterministic ORDER BY and the Float64
// count reducer.
func TestEmitMetricsCompare_BareShape(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), compareNode())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"arrayJoin(array(tuple(",
		"AS `is_selection`",
		"tupleElement(kv, ?) AS `attr`",
		"tupleElement(kv, ?) AS `val`",
		"toFloat64(count(?)) AS `Value`",
		"GROUP BY `is_selection`, `attr`, `val`",
		"ORDER BY `is_selection`, `attr`, `val`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("bare SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "anchor_ts") {
		t.Errorf("bare SQL must not contain the matrix anchor column:\n%s", sql)
	}
}

// TestEmitRangeWindowCompare_MatrixShape — the RangeWindow wrap adds
// the sample-side anchor fanout, the anchor GROUP BY axis, and the
// (Start - range, End] scan-bound pushdown.
func TestEmitRangeWindowCompare_MatrixShape(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{
		Input:           compareNode(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := chsql.Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"AS `anchor_ts`",
		"GROUP BY `is_selection`, `attr`, `val`, `anchor_ts`",
		"`Timestamp` > toDateTime64('2026-05-12 10:00:00.000000000', 9) - toIntervalNanosecond(60000000000)",
		"`Timestamp` <= toDateTime64('2026-05-12 10:03:00.000000000', 9)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("matrix SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "ORDER BY") {
		t.Errorf("matrix SQL must not pin an ORDER BY (the handler owns series assembly):\n%s", sql)
	}
}

// TestEmitMetricsCompare_ErrorPaths — nil Selection / Pairs / Inner and
// a non-positive matrix Step surface as synchronous emit errors.
func TestEmitMetricsCompare_ErrorPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		node chplan.Node
		want string
	}{
		{"nilSelection", func() chplan.Node { m := compareNode(); m.Selection = nil; return m }(), "Selection is nil"},
		{"nilPairs", func() chplan.Node { m := compareNode(); m.Pairs = nil; return m }(), "Pairs is nil"},
		{"nilInner", func() chplan.Node { m := compareNode(); m.Inner = nil; return m }(), "Inner is nil"},
		{"rootLookupWithoutTraceID", func() chplan.Node {
			m := compareNode()
			m.RootLookup = &chplan.Scan{Table: "otel_traces"}
			m.TraceIDColumn = ""
			return m
		}(), "TraceIDColumn empty"},
		{"matrixZeroStep", &chplan.RangeWindow{
			Input:           compareNode(),
			TimestampColumn: "Timestamp",
		}, "requires Step > 0"},
		{"matrixNoTsColumn", &chplan.RangeWindow{
			Input: compareNode(),
			Step:  time.Minute,
		}, "TimestampColumn unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := chsql.Emit(context.Background(), tc.node)
			if err == nil {
				t.Fatalf("Emit should fail for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q missing %q", err, tc.want)
			}
		})
	}
}
