// Internal-package (`package chplan`) companion to
// canonical_series_keys_test.go. It pins what [seriesIdentityKeys] and
// [seriesIdentityKeyAliases] answer for a [RangeWindowNative] that defers its
// label shaping, which the external chplan_test package cannot reach directly.
//
// Why it is worth its own test rather than being left to the whole-plan
// no-op assertion: both arms fail SILENTLY. Answering with GroupBy makes the
// walk see a raw attribute Map as the node's output identity and splice a
// second shaping layer over an already-shaped result; answering with a
// non-nil alias list on the two-level shape rebinds a name the node never
// projects. Neither raises an error, and both present as a shifted label set.
package chplan

import (
	"testing"
	"time"
)

// deferredShapingNode is the hoisted shape: GroupBy carries the RAW columns
// the inner state level groups on, and Recollapse carries the shaping tower
// the merge level re-collapses them by, under the OUTPUT column name.
func deferredShapingNode(recollapse []Projection) *RangeWindowNative {
	return &RangeWindowNative{
		Input:           &Scan{Table: "otel_metrics_sum", Columns: []string{"Attributes", "Value"}},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		Start:           time.Unix(1000, 0).UTC(),
		End:             time.Unix(4600, 0).UTC(),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy: []Expr{
			&ColumnRef{Name: "MetricName"},
			&ColumnRef{Name: "Attributes"},
		},
		Recollapse: recollapse,
	}
}

func shapedAttributes() Projection {
	return Projection{
		Expr:  &FuncCall{Name: CanonicalMapFunc, Args: []Expr{&ColumnRef{Name: "Attributes"}}},
		Alias: "Attributes",
	}
}

func TestSeriesIdentityKeys_DeferredShapingAnswersPassThroughThenRecollapse(t *testing.T) {
	t.Parallel()

	shaped := shapedAttributes()
	n := deferredShapingNode([]Projection{shaped})

	keys := seriesIdentityKeys(n)

	// GroupBy is [MetricName, Attributes] and the tower reads Attributes, so
	// MetricName is the pass-through key and Attributes the shaping input.
	want := []Expr{n.GroupBy[0], shaped.Expr}
	if len(keys) != len(want) {
		t.Fatalf("want the pass-through GroupBy keys then the Recollapse expressions (%d), got %d keys",
			len(want), len(keys))
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("key %d = %#v, want %#v — dropping the pass-through keys lets a pass-through "+
				"attribute Map reach the state and merge levels raw, splitting one logical series "+
				"into two positional groups; including the shaping INPUT instead reports the raw "+
				"Map the deferred tower has already canonicalised", i, keys[i], want[i])
		}
	}
	for _, k := range keys {
		if k == n.GroupBy[1] {
			t.Fatalf("the shaping input %#v is consumed below the merge and must not be reported "+
				"as this node's output identity", n.GroupBy[1])
		}
	}
}

func TestSeriesIdentityKeys_TwoLevelShapeAnswersGroupBy(t *testing.T) {
	t.Parallel()

	n := deferredShapingNode(nil)

	keys := seriesIdentityKeys(n)

	if len(keys) != len(n.GroupBy) {
		t.Fatalf("without Recollapse the keys are GroupBy (%d), got %d", len(n.GroupBy), len(keys))
	}
	for i := range keys {
		if keys[i] != n.GroupBy[i] {
			t.Fatalf("key %d = %#v, want GroupBy[%d] = %#v", i, keys[i], i, n.GroupBy[i])
		}
	}
}

func TestSeriesIdentityKeyAliases_DeferredShapingAnswersRecollapseAliases(t *testing.T) {
	t.Parallel()

	n := deferredShapingNode([]Projection{shapedAttributes()})

	aliases := seriesIdentityKeyAliases(n)

	// A pass-through key is emitted verbatim, so its alias is its own column
	// name; the shaped keys follow under their output names.
	want := []string{"MetricName", "Attributes"}
	if len(aliases) != len(want) {
		t.Fatalf("want %d alias(es) %q, got %d (%q)", len(want), want, len(aliases), aliases)
	}
	for i := range want {
		if aliases[i] != want[i] {
			t.Fatalf("alias %d = %q, want %q — the alias list is read POSITIONALLY against "+
				"seriesIdentityKeys, so a misaligned entry resolves the wrong expression", i, aliases[i], want[i])
		}
	}
}

func TestSeriesIdentityKeyAliases_TwoLevelShapeAnswersNil(t *testing.T) {
	t.Parallel()

	if aliases := seriesIdentityKeyAliases(deferredShapingNode(nil)); aliases != nil {
		t.Fatalf("the two-level shape projects GroupBy unaliased, so it must keep the nil "+
			"fall-through; got %q", aliases)
	}
}
