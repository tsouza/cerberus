package logql

import (
	"testing"
	"time"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The LogQL stream identity is a ClickHouse Map, and a CH Map compares
// and groups POSITIONALLY over its (keys, values) arrays. The OTel-CH
// exporter writes OTLP attributes in wire order and never sorts, so one
// logical stream redelivered under a different physical key order splits
// into two — silently, as a shifted value rather than a visibly doubled
// series. [canonicalIdentityExpr] is what stops that, and the TXTAR
// round-trip cannot defend it on its own: the spec harness EXECUTES the
// recorded `-- sql --` section, so neutering the lowering leaves every
// chDB fixture green until the goldens are regenerated. The pins below
// assert the shape at the plan-IR level, where regeneration cannot reach.

// requireCanonicalIdentity asserts that a lowered stream-identity
// expression carries the key-order canonicalisation and returns the
// expression it wraps, so a caller can keep inspecting the underlying
// shape. Every helper that reaches into an identity projection routes
// through this, which makes the wrap load-bearing for the whole
// package's structural tests rather than for one dedicated case.
func requireCanonicalIdentity(t *testing.T, identity chplan.Expr) chplan.Expr {
	t.Helper()
	fc, ok := identity.(*chplan.FuncCall)
	if !ok || fc.Name != chplan.CanonicalMapFunc {
		t.Fatalf("stream identity = %T (%q), want *chplan.FuncCall(%q): an un-canonicalised "+
			"identity Map splits one logical stream across two physical key orders",
			identity, funcName(identity), chplan.CanonicalMapFunc)
	}
	if len(fc.Args) != 1 {
		t.Fatalf("%s has %d args, want exactly 1", chplan.CanonicalMapFunc, len(fc.Args))
	}
	return fc.Args[0]
}

// TestCanonicalIdentityExpr_WrapsInMapSort pins the primitive: the
// emitters and the `mapSort` name are what make the invariant real, so a
// rename or an accidental pass-through must fail here first.
func TestCanonicalIdentityExpr_WrapsInMapSort(t *testing.T) {
	t.Parallel()
	inner := &chplan.ColumnRef{Name: "ResourceAttributes"}
	got, ok := canonicalIdentityExpr(inner).(*chplan.FuncCall)
	if !ok {
		t.Fatalf("got %T, want *chplan.FuncCall", canonicalIdentityExpr(inner))
	}
	if got.Name != "mapSort" {
		t.Fatalf("func name: got %q, want %q", got.Name, "mapSort")
	}
	if len(got.Args) != 1 || got.Args[0] != chplan.Expr(inner) {
		t.Fatalf("args: got %#v, want exactly the input expr", got.Args)
	}
}

// TestCanonicalIdentityExpr_Idempotent pins the property that lets a
// binding site wrap defensively: re-wrapping an already-canonical map
// must not add a second sort. Without it, a caller that cannot cheaply
// prove its input is un-canonicalised has to choose between a redundant
// per-row sort and skipping the wrap.
func TestCanonicalIdentityExpr_Idempotent(t *testing.T) {
	t.Parallel()
	once := canonicalIdentityExpr(&chplan.ColumnRef{Name: "ResourceAttributes"})
	twice := canonicalIdentityExpr(once)
	if twice != once {
		t.Fatalf("re-wrap produced a new expr %#v, want the same expr back", twice)
	}
}

// TestRangeAggregationIdentityIsCanonical sweeps the three mutually
// exclusive identity shapes [lowerRangeAggregation] can project — the
// default per-stream identity, the `by (...)` / `without (...)`
// group-key override, and the unwrap conversion-error bypass — plus the
// unwrap-with-parser variant that materialises `_logql_merged_labels`.
// Each must reach the RangeWindow GROUP BY canonicalised; the GROUP BY
// itself must stay a bare alias reference so ClickHouse's "SELECT alias
// shadows the FROM column" resolution order never bites.
func TestRangeAggregationIdentityIsCanonical(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	cases := []struct {
		name  string
		query string
	}{
		{"default_identity", `rate({app="api"}[5m])`},
		{"by_grouping", `avg_over_time({app="api"} | logfmt | unwrap latency [5m]) by (status)`},
		{"without_grouping", `max_over_time({app="api"} | logfmt | unwrap latency [5m]) without (pod)`},
		{"unwrap_no_parser", `avg_over_time({app="api"} | unwrap latency [5m])`},
		{"unwrap_with_parser", `avg_over_time({app="api"} | logfmt | unwrap latency [5m])`},
		{"unwrap_error_bypass", `sum_over_time({app="api"} | logfmt | unwrap latency | status > 100 [5m])`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			ra, ok := expr.(*syntax.RangeAggregationExpr)
			if !ok {
				t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", tc.query, expr)
			}
			plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: time.Minute})
			if err != nil {
				t.Fatalf("lowerRangeAggregation(%q): %v", tc.query, err)
			}
			rw, ok := plan.(*chplan.RangeWindow)
			if !ok {
				t.Fatalf("lowered top node = %T, want *chplan.RangeWindow", plan)
			}
			requireCanonicalIdentity(t, rangeWindowIdentityExpr(t, plan))

			// The partition key must name the projection's ALIAS, not
			// re-render its expression: `SELECT mapSort(X) AS X ... GROUP BY
			// mapSort(X)` resolves the inner X to the alias and groups by
			// mapSort(mapSort(X)), which CH rejects with NOT_AN_AGGREGATE.
			if len(rw.GroupBy) != 1 {
				t.Fatalf("RangeWindow.GroupBy has %d keys, want 1", len(rw.GroupBy))
			}
			col, ok := rw.GroupBy[0].(*chplan.ColumnRef)
			if !ok || col.Name != s.ResourceAttributesColumn {
				t.Fatalf("RangeWindow.GroupBy[0] = %#v, want bare ColumnRef(%q)", rw.GroupBy[0], s.ResourceAttributesColumn)
			}
		})
	}
}
