package dispatchscan_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/dispatchscan"
)

// fixtureDir is the package the scan reads. It holds every shape the
// scan must recognise beside the near-misses it must not, so a scan that
// stops recognising and a scan that starts over-recognising both fail
// here.
var fixtureDir = filepath.Join("testdata", "dispatchpkg")

// sealedFixture names the fixture's sealed interface the way a caller
// walking the tree would qualify it.
func sealedFixture(t *testing.T) map[string]bool {
	t.Helper()
	pkg, err := dispatchscan.PackageName(fixtureDir)
	if err != nil {
		t.Fatalf("PackageName: %v", err)
	}
	if pkg != "dispatchpkg" {
		t.Fatalf("PackageName = %q, want dispatchpkg", pkg)
	}
	return map[string]bool{dispatchscan.QualifyInterface(pkg, "Node"): true}
}

func TestClassifiers(t *testing.T) {
	t.Parallel()

	found, err := dispatchscan.Classifiers(fixtureDir, sealedFixture(t))
	if err != nil {
		t.Fatalf("Classifiers: %v", err)
	}

	got := map[string][]string{}
	for _, c := range found {
		if c.Interface != "dispatchpkg.Node" {
			t.Errorf("%s: Interface = %q, want dispatchpkg.Node", c.Func, c.Interface)
		}
		if c.Package != "dispatchpkg" {
			t.Errorf("%s: Package = %q, want dispatchpkg", c.Func, c.Package)
		}
		if c.Line == 0 || !strings.HasSuffix(c.File, "pkg.go") {
			t.Errorf("%s: File/Line = %s:%d, want a position in pkg.go", c.Func, c.File, c.Line)
		}
		got[c.Func] = c.Arms
	}

	// The two classifier shapes, with the `nil` case dropped from the
	// arms: it names no node kind, so it says nothing about coverage.
	wantArms := map[string][]string{
		"isDerived":  {"*Filter", "*Scan"},
		"keyedTwice": {"*Project"},
	}
	for fn, want := range wantArms {
		arms, ok := got[fn]
		if !ok {
			t.Errorf("%s was not recognised as a classifier", fn)
			continue
		}
		if strings.Join(arms, ",") != strings.Join(want, ",") {
			t.Errorf("%s: arms = %v, want %v", fn, arms, want)
		}
	}

	// The near-misses. Each is a function the rule must stay away from,
	// and each fails for a different reason.
	for _, fn := range []string{"unwrap", "isEmpty", "switchesOnLocal"} {
		if _, ok := got[fn]; ok {
			t.Errorf("%s was recognised as a classifier, but it is a near-miss the scan "+
				"must not read", fn)
		}
	}
}

// TestClassifiersIgnoresUnsealedInterfaces pins that the sealed set is
// what bounds the scan. Passing an empty set must find nothing rather
// than every type switch in the package.
func TestClassifiersIgnoresUnsealedInterfaces(t *testing.T) {
	t.Parallel()

	found, err := dispatchscan.Classifiers(fixtureDir, map[string]bool{})
	if err != nil {
		t.Fatalf("Classifiers: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("Classifiers over an empty sealed set found %d classifiers, want 0", len(found))
	}
}

func TestSwitchArms(t *testing.T) {
	t.Parallel()

	arms, err := dispatchscan.SwitchArms(fixtureDir, "dispatchVocabulary")
	if err != nil {
		t.Fatalf("SwitchArms: %v", err)
	}
	// Sorted and deduplicated: the fixture names "rate" twice and lists
	// two names in one clause.
	want := "irate,rate,sum_over_time"
	if got := strings.Join(arms, ","); got != want {
		t.Errorf("SwitchArms = %q, want %q", got, want)
	}
}

// TestSwitchArmsRefusesShapesItCannotDerive pins the vacuity guards. Each
// case is a dispatch this scan must NOT report an answer for, because a
// silently empty or half-read vocabulary makes every ratchet built on it
// pass while asserting nothing.
func TestSwitchArmsRefusesShapesItCannotDerive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fn     string
		reason string
	}{
		{fn: "noSuchFunction", reason: "the function is gone"},
		{fn: "twoSwitches", reason: "the dispatch was reshaped into several switches"},
		{fn: "intSwitch", reason: "the cases are not a string vocabulary"},
		{fn: "noSwitch", reason: "the switch is gone"},
	}
	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			if _, err := dispatchscan.SwitchArms(fixtureDir, tc.fn); err == nil {
				t.Errorf("SwitchArms(%s) returned no error, but %s — a ratchet built on it "+
					"would pass while asserting nothing", tc.fn, tc.reason)
			}
		})
	}
}

func TestScanErrorsOnMissingDirectory(t *testing.T) {
	t.Parallel()

	if _, err := dispatchscan.Classifiers(filepath.Join("testdata", "nope"), nil); err == nil {
		t.Error("Classifiers on a missing directory returned no error")
	}
	if _, err := dispatchscan.PackageName(filepath.Join("testdata", "nope")); err == nil {
		t.Error("PackageName on a missing directory returned no error")
	}
}
