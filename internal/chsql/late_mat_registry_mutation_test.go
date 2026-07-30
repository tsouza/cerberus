package chsql

import "testing"

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
