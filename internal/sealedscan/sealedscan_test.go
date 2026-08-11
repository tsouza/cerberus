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

// TestMarkers_ScansTheLiveChplanSets is the end-to-end check that the
// scanner still recognises the real convention it exists for. The
// assertion is deliberately a lower bound rather than an exact count:
// pinning the numbers here would reintroduce, in the scanner's own test,
// exactly the hand-maintained restatement the scanner exists to
// eliminate.
func TestMarkers_ScansTheLiveChplanSets(t *testing.T) {
	t.Parallel()

	const chplanDir = "../chplan"
	got, err := sealedscan.Markers(chplanDir)
	if err != nil {
		t.Fatalf("Markers: %v", err)
	}
	found := map[string]int{}
	for _, m := range got {
		found[m.Method] = len(m.Implementers)
	}
	for _, method := range []string{"planNode", "exprNode"} {
		if found[method] < 2 {
			t.Errorf("scanning %s found %d %s() implementers, want the live set — the scanner "+
				"no longer recognises the convention it exists for", chplanDir, found[method], method)
		}
	}
}
