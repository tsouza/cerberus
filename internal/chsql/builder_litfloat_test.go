package chsql_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestBuilder_LitFloat_WrapsInToFloat64 pins the central
// `internal/chsql/Builder.Expr` LitFloat → `toFloat64(?)` wrap
// installed by #190.
//
// Before this central wrap, three separate callsites carried the
// responsibility per-emission site:
//
//   - internal/promql/absent.go::lowerAbsent (original, #322 era)
//   - internal/promql/synthetic.go::synthFloatValue (#642 / #189)
//   - internal/logql/literal.go::synthFloatValue (#634)
//
// Each was added in response to the same wire failure mode: the
// clickhouse-go/v2 driver renders Go `float64(N.0)` as the bare
// SQL literal `N` (no decimal — its `bind.go::format()` has no
// `case float64` and falls through to `fmt.Sprint(v)`, which uses
// Go's `%v` for float64 and prints `1` for whole numbers). CH
// narrows the bare literal to `UInt8`, and a downstream binop
// promotes to `UInt16`. Once the column lands in
// `chclient.Sample.Value` (declared `float64`), clickhouse-go's
// Scan refuses the `UInt8 / UInt16 -> *float64` conversion with
// `converting UInt8 to *float64 is unsupported. try using *uint8`
// — surfaced as the 502 Grafana sees on
// `vector(1)+vector(1)`, `absent(<empty>)`, `group(...)`, the
// LogQL `1+1` reduce path, etc.
//
// The central wrap means every new LitFloat callsite is wire-safe
// by construction. The per-callsite helpers were removed; the
// test asserts that any standalone LitFloat emitted through
// Builder.Expr carries the wrap (and the argument is still bound
// positionally).
func TestBuilder_LitFloat_WrapsInToFloat64(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		v    float64
	}{
		{"one", 1.0},
		{"zero", 0},
		{"negative", -3.5},
		{"large", 1e9},
		{"small", 1e-9},
		{"fractional", 0.5},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Wrap the literal in a Project over a OneRow source so the
			// emitter has a complete plan to traverse. The Value
			// projection carries the LitFloat under test.
			plan := &chplan.Project{
				Input: &chplan.OneRow{},
				Projections: []chplan.Projection{
					{Expr: &chplan.LitFloat{V: tc.v}, Alias: "v"},
				},
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, "toFloat64(?) AS `v`") {
				t.Fatalf("expected SQL to wrap LitFloat in toFloat64(?); "+
					"got %s", sql)
			}
			if len(args) != 1 {
				t.Fatalf("args: got %d, want 1: %v", len(args), args)
			}
			if got, ok := args[0].(float64); !ok || got != tc.v {
				t.Errorf("args[0]: got %v (%T), want %v (float64)",
					args[0], args[0], tc.v)
			}
		})
	}
}

// TestBuilder_LitFloat_NonFiniteInline asserts the inline-division
// path for ±Inf / NaN is NOT wrapped — those values render as
// `(1.0/0)` / `(-1.0/0)` / `(0.0/0)` directly inside the SQL (the
// driver can't bind them as `?` because real CH 24.x rejects the
// mixed-case `Inf` / `NaN` identifier strings clickhouse-go
// emits). The inline form is already a Float64 division, so no
// outer toFloat64 wrap is needed.
func TestBuilder_LitFloat_NonFiniteInline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		v    float64
		want string
	}{
		{"pos_inf", math.Inf(+1), "(1.0/0)"},
		{"neg_inf", math.Inf(-1), "(-1.0/0)"},
		{"nan", math.NaN(), "(0.0/0)"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan := &chplan.Project{
				Input: &chplan.OneRow{},
				Projections: []chplan.Projection{
					{Expr: &chplan.LitFloat{V: tc.v}, Alias: "v"},
				},
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, tc.want) {
				t.Fatalf("expected SQL to contain inline %s; got %s",
					tc.want, sql)
			}
			if strings.Contains(sql, "toFloat64(") {
				t.Errorf("non-finite LitFloat must NOT be wrapped in "+
					"toFloat64 (the inline-division form is already "+
					"Float64); got %s", sql)
			}
			if len(args) != 0 {
				t.Errorf("non-finite path should not bind any args; got %v",
					args)
			}
		})
	}
}
