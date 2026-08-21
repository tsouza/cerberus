package chsql

import (
	"context"
	"testing"
)

// TestMutation_RegisterLateMatShape_RequiresBothHalves pins that late
// materialisation is only offered for a table whose schema supplies BOTH the
// unique row key and at least one wide column. The two are a coupled pair, not
// independent hints: the rewrite defers wide columns out of the inner scan and
// joins them back on the row key, so a table with no unique row key would
// multiply rows on the join-back (wrong answers), and a table with no wide
// column would pay for a self-join that defers nothing (slower, same answer).
// Either gap disqualifies the table outright.
//
// Kills the guard's INVERT_LOGICAL (`||` -> `&&`, which registers a table with
// only one half of the pair) and its CONDITIONALS_NEGATION on the wide-column
// emptiness check.
func TestMutation_RegisterLateMatShape_RequiresBothHalves(t *testing.T) {
	t.Parallel()

	const table = "otel_logs"
	wide := []string{"Body"}
	rowKey := []string{"Timestamp", "TraceId"}

	for _, tc := range []struct {
		name      string
		hasRowKey bool
		wide      []string
		want      bool
	}{
		{name: "both halves present", hasRowKey: true, wide: wide, want: true},
		{name: "no unique row key", hasRowKey: false, wide: wide},
		{name: "no wide column", hasRowKey: true, wide: nil},
		{name: "neither half", hasRowKey: false, wide: nil},
	} {
		out := map[string]lateMatShape{}
		registerLateMatShape(out, table, tc.hasRowKey, tc.wide, rowKey)
		if _, ok := out[table]; ok != tc.want {
			t.Errorf("%s: registered=%v, want %v", tc.name, ok, tc.want)
		}
	}
}

// TestMutation_RegisterLateMatShape_CarriesSchemaColumns pins that a registered
// table records the exact wide/row-key column lists it was given — the rewrite
// reads both back to build the inner projection and the join-back key, so a
// registration that drops either list emits a join on nothing.
func TestMutation_RegisterLateMatShape_CarriesSchemaColumns(t *testing.T) {
	t.Parallel()

	const table = "otel_traces"
	out := map[string]lateMatShape{}
	registerLateMatShape(out, table, true, []string{"SpanAttributes"}, []string{"Timestamp", "SpanId"})

	got, ok := out[table]
	if !ok {
		t.Fatalf("%s must be registered", table)
	}
	if len(got.wide) != 1 || got.wide[0] != "SpanAttributes" {
		t.Errorf("wide columns not carried through: %v", got.wide)
	}
	if len(got.rowKey) != 2 || got.rowKey[1] != "SpanId" {
		t.Errorf("row key not carried through: %v", got.rowKey)
	}
}

// TestMutation_WithLateMatShape_RequiresEveryPart defends late_mat.go:114
// (`if table == "" || len(wide) == 0 || len(rowKey) == 0`), the same coupled-pair
// gate registerLateMatShape applies, enforced on the request-threaded shape.
//
// Mutation INVERT_LOGICAL flips ONE of the two `||` to `&&`, and Go's tighter
// `&&` binding regroups the chain rather than negating it:
//
//   - col 17 (first `||`) parses as `(table == "" && len(wide) == 0) || len(rowKey) == 0`,
//     so a nameless table carrying both column lists is WAIVED through and
//     wedged into the context under the "" key.
//   - col 35 (second `||`) parses as `table == "" || (len(wide) == 0 && len(rowKey) == 0)`,
//     so a table missing exactly ONE list — no wide column to defer, or no row
//     key to join back on — is waived through the same way.
//
// Either half-shape reaches resolveLateMatShape as a usable entry and the
// rewrite then defers nothing (no wide column) or joins on nothing (no row
// key). The unusable-half rows below are what separate the mutants: they must
// leave the context untouched.
func TestMutation_WithLateMatShape_RequiresEveryPart(t *testing.T) {
	t.Parallel()

	const table = "otel_logs"
	wide := []string{"Body"}
	rowKey := []string{"Timestamp", "TraceId"}

	for _, tc := range []struct {
		name   string
		table  string
		wide   []string
		rowKey []string
		want   bool
	}{
		{name: "every part present", table: table, wide: wide, rowKey: rowKey, want: true},
		{name: "no table name", wide: wide, rowKey: rowKey},
		{name: "no wide column", table: table, rowKey: rowKey},
		{name: "no row key", table: table, wide: wide},
	} {
		ctx := WithLateMatShape(context.Background(), tc.table, tc.wide, tc.rowKey)
		gotTable, gotShape, ok := lateMatShapeFromCtx(ctx)
		if ok != tc.want {
			t.Errorf("%s: threaded=%v, want %v", tc.name, ok, tc.want)
			continue
		}
		if !ok {
			continue
		}
		if gotTable != tc.table || len(gotShape.wide) != len(tc.wide) || len(gotShape.rowKey) != len(tc.rowKey) {
			t.Errorf("%s: threaded shape (%q, %v, %v) does not match the input", tc.name, gotTable, gotShape.wide, gotShape.rowKey)
		}
	}
}
