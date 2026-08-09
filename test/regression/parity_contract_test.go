package regression

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// parityOraclePkgs are the reference-engine packages whose independence
// from cerberus is the load-bearing property of the whole parity layer.
var parityOraclePkgs = []string{
	"./test/spec/parityoracle/...",
}

// forbiddenOracleDeps are the cerberus packages an oracle may never
// import, directly or transitively.
//
// The list is the SYSTEM UNDER TEST: everything that decides what SQL a
// query becomes. An oracle sharing any of it cannot disagree with cerberus
// about the thing they share, which silently turns the oracle into a
// mirror — and a mirror always agrees, so the parity check would pass
// while proving nothing.
var forbiddenOracleDeps = []string{
	"github.com/tsouza/cerberus/internal/promql",
	"github.com/tsouza/cerberus/internal/logql",
	"github.com/tsouza/cerberus/internal/traceql",
	"github.com/tsouza/cerberus/internal/chplan",
	"github.com/tsouza/cerberus/internal/chsql",
	"github.com/tsouza/cerberus/internal/optimizer",
	"github.com/tsouza/cerberus/internal/schema",
}

// TestParityOracleImportsNoCerberusLowering is the mechanical enforcement
// of the one rule the parity oracle exists under: it must not import the
// system it checks.
//
// This is a `go list -deps -test` scan rather than a source grep because
// the dangerous import is the TRANSITIVE one. A helper that looks
// perfectly innocent — a label-mapping utility, a column-name constant —
// can pull internal/schema in behind it, and nobody reviewing that helper
// would see the oracle lose its independence.
//
// It is also why compatibility/prometheus/cmd/seed's readFixtureSeries is
// NOT reused as the translator even though it does the same CH-rows to
// series conversion: it imports internal/promql and internal/schema. Being
// equal-by-construction is correct for the compat mirror, whose job is to
// put identical data into both backends, and disqualifying for an oracle.
func TestParityOracleImportsNoCerberusLowering(t *testing.T) {
	t.Parallel()

	root := repoRootForParity(t)

	for _, pkg := range parityOraclePkgs {
		// -test includes the test files' own imports, so an oracle cannot
		// smuggle a forbidden dependency in through its _test.go either.
		cmd := exec.Command("go", "list", "-deps", "-test", pkg)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps -test %s: %v", pkg, err)
		}

		deps := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, dep := range deps {
			dep = strings.TrimSpace(dep)
			for _, forbidden := range forbiddenOracleDeps {
				if dep == forbidden || strings.HasPrefix(dep, forbidden+"/") {
					t.Errorf(
						"parity oracle %s transitively imports %s.\n"+
							"An oracle that shares code with the system under test cannot disagree "+
							"with it about the thing they share, so this import silently converts "+
							"the oracle into a mirror — and the parity check would then pass while "+
							"proving nothing. Reimplement whatever was needed inside the oracle "+
							"package instead of importing it.",
						pkg, dep,
					)
				}
			}
		}
	}
}

// TestParitySectionsParse asserts every fixture carrying a `parity:`
// section has a WELL-FORMED one.
//
// LoadParity deliberately errors rather than skipping on a malformed body,
// because a typo'd key would otherwise un-enrol a fixture from its oracle
// while still looking enrolled in the diff — the fixture would keep its
// `-- parity --` header, keep passing, and check nothing. This test is
// what makes that error reachable in the fast, untagged lane rather than
// only in the chdb-tagged one.
func TestParitySectionsParse(t *testing.T) {
	t.Parallel()

	for _, dir := range parityFixtureDirs(t) {
		for _, path := range txtarFilesForParity(t, dir) {
			c, err := spec.Load(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if _, _, err := spec.LoadParity(c); err != nil {
				t.Errorf("%s: %v", filepath.Base(path), err)
			}
		}
	}
}

// TestParityEnrolmentFloor is the ratchet.
//
// Enrolment is incremental — a fixture is enrolled by hand when its seed
// carries what the reference engine needs — so this cannot assert that
// every fixture is enrolled. What it CAN do is make the count monotonic:
// enrolment may only ever grow, and un-enrolling a fixture requires
// editing this constant, which is a visible line in the diff that a
// reviewer must accept.
//
// That is deliberately weaker than the all-or-nothing enrolment a closed
// corpus would allow, and it is the honest trade: PromQL fixtures include
// metadata and exemplar shapes that ask no PromQL expression at all, so
// "every fixture must enrol" is unattainable, and forcing it would require
// a per-fixture exemption vocabulary — precisely the allow-list shape
// invariant 7 forbids.
func TestParityEnrolmentFloor(t *testing.T) {
	t.Parallel()

	// parityEnrolmentFloor is the number of fixtures currently carrying a
	// `parity:` section. It ratchets UP only.
	const parityEnrolmentFloor = 1

	enrolled := 0
	for _, dir := range parityFixtureDirs(t) {
		for _, path := range txtarFilesForParity(t, dir) {
			c, err := spec.Load(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if _, ok, err := spec.LoadParity(c); err == nil && ok {
				enrolled++
			}
		}
	}

	if enrolled < parityEnrolmentFloor {
		t.Errorf(
			"parity enrolment fell to %d, below the committed floor of %d.\n"+
				"A fixture lost its `-- parity --` section, which silently returns it to being "+
				"checked only against cerberus's own regenerated output. If the un-enrolment is "+
				"intentional, lower the floor in this test and say why in the commit message.",
			enrolled, parityEnrolmentFloor,
		)
	}
	if enrolled > parityEnrolmentFloor {
		t.Errorf(
			"parity enrolment rose to %d, above the committed floor of %d.\n"+
				"Raise parityEnrolmentFloor to %d so the gain cannot be silently lost again.",
			enrolled, parityEnrolmentFloor, enrolled,
		)
	}
}

// TestParityVocabulariesAreClosed asserts the accepted values are a closed
// set with no empty members — the property LoadParity's rejection path
// depends on.
func TestParityVocabulariesAreClosed(t *testing.T) {
	t.Parallel()

	keys := spec.ParityKeys()
	if len(keys) == 0 {
		t.Fatal("parity vocabulary is empty; LoadParity would accept any section body")
	}
	for _, key := range keys {
		values := spec.ParityValues(key)
		if len(values) == 0 {
			t.Errorf("parity key %q has no accepted values, so no fixture can ever satisfy it", key)
		}
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				t.Errorf("parity key %q accepts an empty value", key)
			}
		}
	}
}

// --- helpers ---------------------------------------------------------

func repoRootForParity(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// test/regression -> repo root
	return filepath.Dir(filepath.Dir(wd))
}

func parityFixtureDirs(t *testing.T) []string {
	t.Helper()
	root := repoRootForParity(t)
	return []string{
		filepath.Join(root, "test", "spec", "promql"),
		filepath.Join(root, "test", "spec", "logql"),
		filepath.Join(root, "test", "spec", "traceql"),
	}
}

func txtarFilesForParity(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txtar") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}
