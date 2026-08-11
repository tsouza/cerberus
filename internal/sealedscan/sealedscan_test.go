package sealedscan_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/sealedscan"
)

// fixtureDir is a package under testdata whose shape is pinned by the
// tests below: three Shape implementers, two Colour implementers, and
// four declarations that look like sealing markers but are not.
var fixtureDir = filepath.Join("testdata", "sealedpkg")

func TestMarkers_ReportsSealedInterfacesWithTheirImplementers(t *testing.T) {
	t.Parallel()

	got, err := sealedscan.Markers(fixtureDir)
	if err != nil {
		t.Fatalf("Markers: %v", err)
	}
	want := []sealedscan.Marker{
		{Method: "colourNode", Interface: "Colour", Implementers: []string{"Blue", "Red"}},
		{Method: "shapeNode", Interface: "Shape", Implementers: []string{"Circle", "Square", "Triangle"}},
		// The base itself is absent and its embedders stand in its
		// place, by value and by pointer alike.
		{Method: "soundNode", Interface: "Sound", Implementers: []string{"Bark", "Meow", "Moo"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Markers = %+v, want %+v", got, want)
	}
}

// TestMarkers_RefusesNonMarkerShapes pins what the scanner must NOT
// treat as a sealed set. Each of these would inflate a ratchet's derived
// set with something that is not a member, which is worse than a stale
// count: it fails loudly and wrongly, and the fix is to weaken the
// ratchet.
func TestMarkers_RefusesNonMarkerShapes(t *testing.T) {
	t.Parallel()

	got, err := sealedscan.Markers(fixtureDir)
	if err != nil {
		t.Fatalf("Markers: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range got {
		seen[m.Method] = true
	}
	for _, method := range []string{
		// One implementer is below the floor: nothing can enumerate a
		// one-element set incorrectly.
		"lonelyNode",
		// Exported methods are ordinary API, not a sealing convention.
		"ExportedNode",
		// A method taking an argument carries data, so it is behaviour.
		"paramNode",
		// A non-empty body is behaviour too, however unexported.
		"busyNode",
	} {
		if seen[method] {
			t.Errorf("Markers reported %s() as a sealing marker; it is not one", method)
		}
	}
}

func TestImplementers_ReturnsTheSortedSet(t *testing.T) {
	t.Parallel()

	got, err := sealedscan.Implementers(fixtureDir, "shapeNode")
	if err != nil {
		t.Fatalf("Implementers: %v", err)
	}
	want := []string{"Circle", "Square", "Triangle"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Implementers = %v, want %v", got, want)
	}
}

// TestImplementers_UnknownMarkerIsAnError is the vacuity guard, and it
// is the single most load-bearing behaviour here. Every caller is a
// ratchet of the form "derive the set, then assert something covers it";
// if a renamed marker or a moved directory silently yielded an empty
// set, every one of those ratchets would pass while asserting nothing.
func TestImplementers_UnknownMarkerIsAnError(t *testing.T) {
	t.Parallel()

	_, err := sealedscan.Implementers(fixtureDir, "noSuchMarker")
	if err == nil {
		t.Fatal("Implementers on an unknown marker returned no error; a vacuous set must fail loudly")
	}
	if !strings.Contains(err.Error(), "vacuous") {
		t.Errorf("error = %q, want it to explain the vacuity it is preventing", err)
	}
}

func TestImplementers_MissingDirectoryIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := sealedscan.Implementers(filepath.Join("testdata", "absent"), "shapeNode"); err == nil {
		t.Fatal("Implementers on a missing directory returned no error")
	}
}

// TestMarkers_ScansTheLiveSets is the end-to-end check that the scanner
// still recognises the conventions it exists for, on the real packages
// that use them.
//
// It asserts MEMBERSHIP, not size. Pinning 32 and 23 here would
// reintroduce, inside the scanner's own test, exactly the restatement
// the scanner exists to eliminate — but the obvious alternative, a lower
// bound like "at least two", degrades to "the marker was reported at
// all" and cannot tell a correct set from a plausible wrong one. That is
// not hypothetical: before the embedded-base case was handled, lsyntax's
// Expr set derived to a confident, non-empty, entirely fictional ten
// names, and a size bound saw nothing wrong. Naming members that must be
// present costs nothing to maintain — a type is only removed
// deliberately — and fails loudly when the derivation drifts.
func TestMarkers_ScansTheLiveSets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		dir     string
		method  string
		members []string
	}{
		{"../chplan", "planNode", []string{"Scan", "Filter", "Project", "VectorSetOp"}},
		{"../chplan", "exprNode", []string{"ColumnRef", "Binary", "ScalarSubquery"}},
		// The embedded-base users: every one of these acquires its
		// marker through stageBase rather than declaring it.
		{"../logql/lsyntax", "isStageExpr", []string{"LineFilterExpr", "LabelFilterExpr", "KeepLabelsExpr"}},
		{"../logql/lsyntax", "isExpr", []string{"LineFilterExpr", "MatchersExpr", "BinOpExpr"}},
		{"../logql/lsyntax", "isSampleExpr", []string{"RangeAggregationExpr", "BinOpExpr"}},
		{"../traceql/ast", "isPipelineElement", []string{"Pipeline", "SpansetFilter"}},
	}
	for _, tc := range cases {
		got, err := sealedscan.Implementers(tc.dir, tc.method)
		if err != nil {
			t.Errorf("Implementers(%s, %s): %v", tc.dir, tc.method, err)
			continue
		}
		have := make(map[string]bool, len(got))
		for _, name := range got {
			have[name] = true
		}
		for _, member := range tc.members {
			if !have[member] {
				t.Errorf("scanning %s: %s() set is %v, missing %s — the scanner no longer "+
					"recognises the convention it exists for", tc.dir, tc.method, got, member)
			}
		}
	}
}
