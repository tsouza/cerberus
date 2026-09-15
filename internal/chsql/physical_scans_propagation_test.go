package chsql

import (
	"strings"
	"testing"
)

// TestPhysicalScans_PropagateThroughEverySpliceSeam pins each seam a physical
// table reference can travel through on its way to the statement's final
// count (Builder.physicalScans's doc), so no future splice path can drop the
// count silently: a nested QueryBuilder rendered via Frag() into its parent,
// one spliced via Spliced (bare, no parens), and a free-standing Builder
// drained into the emitter via splice. Each case also cross-checks against
// the rendered text so the count can never say more or less than what
// ClickHouse will execute.
func TestPhysicalScans_PropagateThroughEverySpliceSeam(t *testing.T) {
	t.Parallel()

	t.Run("nested QueryBuilder.Frag() counts into the parent", func(t *testing.T) {
		t.Parallel()
		inner := NewQuery().Select(Star()).From(physicalTableFrag("otel_logs"))
		outer := NewQuery().Select(Star()).From(aliasedFrag(inner.Frag(), "i"))
		sql, _, err := outer.subquerySQL()
		if err != nil {
			t.Fatal(err)
		}
		if got := outer.physicalScans(); got != 1 {
			t.Fatalf("physicalScans = %d, want 1\nSQL: %s", got, sql)
		}
		if strings.Count(sql, "FROM `otel_logs`") != 1 {
			t.Fatalf("expected one table reference in\n%s", sql)
		}
	})

	t.Run("Spliced carries a pre-rendered statement's count", func(t *testing.T) {
		t.Parallel()
		sub := PreRenderedSQL{SQL: "SELECT `TraceId` FROM `otel_traces`", PhysicalScans: 1}
		q := NewQuery().Select(Star()).From(physicalTableFrag("otel_logs")).Where(In(Col("TraceId"), Spliced(sub)))
		sql, _, err := q.subquerySQL()
		if err != nil {
			t.Fatal(err)
		}
		if got := q.physicalScans(); got != 2 {
			t.Fatalf("physicalScans = %d, want 2 (outer table + spliced statement)\nSQL: %s", got, sql)
		}
	})

	t.Run("emitter.splice drains a Builder's count", func(t *testing.T) {
		t.Parallel()
		e := &emitter{}
		b := NewBuilder()
		physicalTableFrag("otel_metrics_sum")(b)
		if err := e.splice(b); err != nil {
			t.Fatal(err)
		}
		if e.physicalScans != 1 {
			t.Fatalf("emitter.physicalScans = %d after splice, want 1", e.physicalScans)
		}
	})

	// The next two cases prime e.physicalScans to a nonzero value before
	// the call under test, so an emitSelect/splice that OVERWRITES rather
	// than ACCUMULATES the count is distinguishable: renderNode's own
	// emit_node.go:`saveB, saveArgs, saveScans := e.b, e.args, e.physicalScans`
	// save/restore round-trips a caller's earlier count through exactly
	// this field for every nested subquery, so an overwrite would
	// silently drop whichever sibling scan rendered first.

	t.Run("emitter.emitSelect accumulates onto a nonzero count", func(t *testing.T) {
		t.Parallel()
		e := &emitter{physicalScans: 1}
		sb := NewQuery().Select(Star()).From(physicalTableFrag("otel_logs"))
		if err := e.emitSelect(sb); err != nil {
			t.Fatal(err)
		}
		if e.physicalScans != 2 {
			t.Fatalf("emitter.physicalScans = %d after emitSelect from a primed count of 1, want 2 (1 prior + 1 from this SELECT)", e.physicalScans)
		}
	})

	t.Run("emitter.splice accumulates onto a nonzero count", func(t *testing.T) {
		t.Parallel()
		e := &emitter{physicalScans: 1}
		b := NewBuilder()
		physicalTableFrag("otel_metrics_sum")(b)
		if err := e.splice(b); err != nil {
			t.Fatal(err)
		}
		if e.physicalScans != 2 {
			t.Fatalf("emitter.physicalScans = %d after splice from a primed count of 1, want 2 (1 prior + 1 spliced)", e.physicalScans)
		}
	})

	t.Run("countPhysicalScans adds exactly n", func(t *testing.T) {
		t.Parallel()
		b := NewBuilder()
		countPhysicalScans(3, Col("merged"))(b)
		if b.PhysicalScans() != 3 {
			t.Fatalf("PhysicalScans = %d, want 3", b.PhysicalScans())
		}
		if b.String() != "`merged`" {
			t.Fatalf("counting must not alter the rendered text, got %q", b.String())
		}
	})
}
