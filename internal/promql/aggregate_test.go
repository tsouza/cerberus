package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Group_WrapsLitInOToFloat64 pins the fix for the
// `group(...)` 502: CH narrows the bound `int64(1)` literal to UInt8,
// `any(UInt8)` returns UInt8, and clickhouse-go/v2's typed Scan into
// `chclient.Sample.Value` (`*float64`) errors with
// `converting UInt8 to *float64 is unsupported`. The lowering wraps
// the literal in `toFloat64(...)` so the column projects as Float64
// on the wire regardless of CH's narrowing inference. Mirrors the
// analogous `count(...)` wrap pinned by
// test/spec/promql/count_agg_returns_float.txtar.
//
// `intReturningAggregates` (chsql/emit_node.go) can't carry this fix
// because `any(...)` is also used over Float64 (`any(Value)`) and
// Array(Float64) (`any(ExplicitBounds)` in histogram_quantile) — an
// unconditional outer toFloat64 wrap would break the latter. The fix
// lives at the literal, not the aggregate-name dispatch.
//
// The chDB round-trip layer (test/spec/promql/group_basic.txtar /
// group_by_job.txtar) exercises the end-to-end Scan path; this unit
// pin keeps the SQL byte-shape stable so an accidental regression in
// `lower.go` surfaces here rather than only at the chDB layer.
func TestLower_Group_WrapsLitInOToFloat64(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	for _, q := range []string{`group(up)`, `group by (job) (up)`} {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", q, err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			// `any(toFloat64(?))` is the canonical wrapped shape;
			// the bare `any(?)` (no inner toFloat64) is what
			// produces the UInt8 → *float64 502.
			if !strings.Contains(sql, "any(toFloat64(?))") {
				t.Errorf("group lowering missing any(toFloat64(?)) wrap.\nSQL: %s", sql)
			}
			if strings.Contains(sql, "any(?) AS `Value`") {
				t.Errorf("group lowering still emits unwrapped any(?) — UInt8 narrowing path.\nSQL: %s", sql)
			}
		})
	}
}

// TestLower_Aggregate_Errors covers the aggregate paths whose error
// messages are observable contract (param / no-param mismatch, computed
// quantile phi, count_values argument-shape rejections). topk/bottomk
// and count_values now both accept `without(...)`: topk lowers into a
// MapWithoutKeys partition expression on chplan.TopK.By, count_values
// into a MapWithoutKeys group key + mapConcat overlay (see
// lowerCountValues).
func TestLower_Aggregate_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			// `topk(scalar(<vector>), v)` and `bottomk(scalar(<vector>), v)`
			// are now lowered into the computed-K shape (KExpr on
			// chplan.TopK). Other computed-K shapes — `topk(2 + scalar(x), v)`,
			// `topk(time(), v)`, etc. — still error since the lowering
			// only recognises `scalar(<vector>)` as a K source.
			name:    "topk K must be scalar literal or scalar(...)",
			query:   `topk(time(), latency_seconds)`,
			wantErr: "must be a scalar literal or scalar(<vector>)",
		},
		{
			// Mixed arithmetic around a scalar() subquery is still
			// rejected: `2 + scalar(x)` lowers as a vector-scalar binop
			// at parse time, so tryScalarLiteral returns false and the
			// computed-K path's scalar(...) detector also fails.
			name:    "topk K rejects mixed scalar arithmetic",
			query:   `topk(2 + scalar(latency_seconds), up)`,
			wantErr: "must be a scalar literal or scalar(<vector>)",
		},
		{
			name:    "topk K must be non-negative integer",
			query:   `topk(-1, up)`,
			wantErr: "non-negative integer literal",
		},
		{
			name:    "topk K must be > 0",
			query:   `topk(0, up)`,
			wantErr: "K must be > 0",
		},
		{
			name:    "count_values rejects empty label",
			query:   `count_values("", up)`,
			wantErr: "non-empty label name",
		},
		{
			name:    "quantile needs scalar literal phi",
			query:   `quantile(scalar(up), latency_seconds)`,
			wantErr: "scalar literal phi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			_, err = promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
