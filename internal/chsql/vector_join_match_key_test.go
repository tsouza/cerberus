package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// matchKeyJoin builds a one-to-one VectorJoin over the canonical 4-column
// arm shape so the emitted SQL differs between match modifiers only in the
// match key itself.
func matchKeyJoin(m chplan.VectorMatch) *chplan.VectorJoin {
	return &chplan.VectorJoin{
		Left:             narySetOpArm("a"),
		Right:            narySetOpArm("b"),
		Op:               chplan.OpMul,
		Match:            m,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

// matchKeySetOp builds a two-arm `and` VectorSetOp carrying the given
// match modifier — the shape whose match key flows through the GROUP BY /
// PARTITION BY / IN-subquery signature rather than the JOIN ON predicate.
func matchKeySetOp(m chplan.VectorMatch) *chplan.VectorSetOp {
	s := setOpPair(chplan.VectorSetAnd, false)
	s.Match = m
	return s
}

func emitJoin(t *testing.T, j *chplan.VectorJoin) string {
	t.Helper()
	sql, _, err := chsql.Emit(context.Background(), j)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sql
}

// TestMatchKeyFrags_CanonicaliseMapKeys pins the order-insensitivity of
// every Map-valued V-V match key, and the deliberate absence of the
// wrapper on the two branches whose key is not a Map.
//
// A ClickHouse Map compares and groups positionally over its (keys,
// values) arrays, so `map('job','api','dc','eu')` and
// `map('dc','eu','job','api')` are unequal despite carrying the same
// label set. Every match key built from a Map — the raw Attributes
// column, the `on(labels)` mapFilter, the `ignoring(labels)` mapFilter —
// is therefore wrapped in `mapSort` at the point of comparison.
//
// `on()` with no labels collapses onto a constant tuple and the
// `on(labels)` JOIN ON predicate compares per-label `Attributes[?]`
// subscripts; neither is a Map, so neither is wrapped. Asserting their
// absence pins those no-change decisions alongside the fix.
func TestMatchKeyFrags_CanonicaliseMapKeys(t *testing.T) {
	t.Parallel()

	const canonical = "mapSort("

	// Map-valued keys — the wrapper must be present, and the raw
	// (unwrapped) shape must be gone.
	t.Run("join_default_on_predicate", func(t *testing.T) {
		t.Parallel()
		sql := emitJoin(t, matchKeyJoin(chplan.VectorMatch{}))
		wantOn := "ON mapSort(L.`Attributes`) = mapSort(R.`Attributes`)"
		if !strings.Contains(sql, wantOn) {
			t.Errorf("default-matching join ON: missing %q; sql=%s", wantOn, sql)
		}
		if strings.Contains(sql, "ON L.`Attributes` = R.`Attributes`") {
			t.Errorf("default-matching join ON: still compares raw Map values; sql=%s", sql)
		}
	})

	t.Run("join_ignoring_on_predicate", func(t *testing.T) {
		t.Parallel()
		sql := emitJoin(t, matchKeyJoin(chplan.VectorMatch{Labels: []string{"env"}}))
		wantOn := "ON mapSort(mapFilter((k, v) -> NOT (k IN (?)), L.`Attributes`)) = " +
			"mapSort(mapFilter((k, v) -> NOT (k IN (?)), R.`Attributes`))"
		if !strings.Contains(sql, wantOn) {
			t.Errorf("ignoring() join ON: missing %q; sql=%s", wantOn, sql)
		}
		// The roleOne per-side aggregation groups by the same key.
		wantGroup := "GROUP BY mapSort(mapFilter((k, v) -> NOT (k IN (?)), `Attributes`))"
		if !strings.Contains(sql, wantGroup) {
			t.Errorf("ignoring() join GROUP BY: missing %q; sql=%s", wantGroup, sql)
		}
	})

	t.Run("setop_default_signature", func(t *testing.T) {
		t.Parallel()
		sql := emitSetOp(t, matchKeySetOp(chplan.VectorMatch{}))
		want := "WHERE mapSort(`Attributes`) IN ((SELECT DISTINCT mapSort(`Attributes`) FROM"
		if !strings.Contains(sql, want) {
			t.Errorf("default-matching set op: missing %q; sql=%s", want, sql)
		}
		if strings.Contains(sql, "WHERE `Attributes` IN ((SELECT DISTINCT `Attributes` FROM") {
			t.Errorf("default-matching set op: still keys on the raw Map value; sql=%s", sql)
		}
	})

	t.Run("setop_on_labels_signature", func(t *testing.T) {
		t.Parallel()
		sql := emitSetOp(t, matchKeySetOp(chplan.VectorMatch{Labels: []string{"job", "dc"}, On: true}))
		want := "WHERE mapSort(mapFilter((k, v) -> k IN (?, ?), `Attributes`)) IN " +
			"((SELECT DISTINCT mapSort(mapFilter((k, v) -> k IN (?, ?), `Attributes`)) FROM"
		if !strings.Contains(sql, want) {
			t.Errorf("on(labels) set op: missing %q; sql=%s", want, sql)
		}
	})

	t.Run("setop_ignoring_signature", func(t *testing.T) {
		t.Parallel()
		sql := emitSetOp(t, matchKeySetOp(chplan.VectorMatch{Labels: []string{"env"}}))
		want := "WHERE mapSort(mapFilter((k, v) -> NOT (k IN (?)), `Attributes`)) IN " +
			"((SELECT DISTINCT mapSort(mapFilter((k, v) -> NOT (k IN (?)), `Attributes`)) FROM"
		if !strings.Contains(sql, want) {
			t.Errorf("ignoring() set op: missing %q; sql=%s", want, sql)
		}
	})

	// Non-Map keys — the wrapper must be absent.
	t.Run("setop_on_no_labels_is_not_a_map", func(t *testing.T) {
		t.Parallel()
		sql := emitSetOp(t, matchKeySetOp(chplan.VectorMatch{On: true}))
		if !strings.Contains(sql, "WHERE tuple() IN ((SELECT DISTINCT tuple() FROM") {
			t.Errorf("on() set op: expected the constant-tuple key; sql=%s", sql)
		}
		if strings.Contains(sql, canonical) {
			t.Errorf("on() set op: wrapped a non-Map key in mapSort; sql=%s", sql)
		}
	})

	t.Run("join_on_labels_predicate_is_per_label_subscript", func(t *testing.T) {
		t.Parallel()
		sql := emitJoin(t, matchKeyJoin(chplan.VectorMatch{Labels: []string{"job", "dc"}, On: true}))
		wantOn := "ON L.`Attributes`[?] = R.`Attributes`[?] AND L.`Attributes`[?] = R.`Attributes`[?]"
		if !strings.Contains(sql, wantOn) {
			t.Errorf("on(labels) join ON: missing per-label subscript equality %q; sql=%s", wantOn, sql)
		}
		// The roleOne GROUP BY on the same plan IS Map-valued and must
		// still be canonicalised; the ON predicate itself must not be.
		onClause := sql[strings.LastIndex(sql, ") AS R ON "):]
		if strings.Contains(onClause, canonical) {
			t.Errorf("on(labels) join ON: wrapped a non-Map key in mapSort; on=%s", onClause)
		}
	})

	t.Run("join_on_no_labels_predicate_is_constant_true", func(t *testing.T) {
		t.Parallel()
		sql := emitJoin(t, matchKeyJoin(chplan.VectorMatch{On: true}))
		if !strings.Contains(sql, "ON 1 = 1") {
			t.Errorf("on() join ON: expected the constant-TRUE predicate; sql=%s", sql)
		}
		onClause := sql[strings.LastIndex(sql, ") AS R ON "):]
		if strings.Contains(onClause, canonical) {
			t.Errorf("on() join ON: wrapped a non-Map key in mapSort; on=%s", onClause)
		}
	})
}
