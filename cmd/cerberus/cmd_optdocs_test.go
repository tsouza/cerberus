package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

// optdocFixture returns a doc whose generated block is surrounded by
// hand-authored prose on both sides — the arrangement optdocReplaceBlock
// exists to preserve. The prose is deliberately distinctive so a slice that
// took the wrong offset shows up as lost or duplicated text.
func optdocFixture(block string) []byte {
	return []byte("# Heading\n\nPROSE BEFORE THE BLOCK\n\n" +
		block +
		"\n\nPROSE AFTER THE BLOCK\n")
}

// TestOptdocReplaceBlock_PreservesSurroundingProse pins the contract the
// generator's doc comment states: "the generator owns an existing block, it
// does not invent the surrounding section." docs/clickhouse-optimizations.md
// carries hand-authored columns and commentary OUTSIDE the markers, and this
// function rewrites the file in place — so an offset error here silently
// deletes human-written documentation, which no test of the table's contents
// would notice.
func TestOptdocReplaceBlock_PreservesSurroundingProse(t *testing.T) {
	original := optdocFixture(optdocBeginMarker + "\nOLD TABLE\n" + optdocEndMarker)
	replacement := optdocBeginMarker + "\nNEW TABLE\n" + optdocEndMarker

	got, err := optdocReplaceBlock(original, replacement)
	if err != nil {
		t.Fatalf("optdocReplaceBlock: %v", err)
	}

	want := optdocFixture(replacement)
	if !bytes.Equal(got, want) {
		t.Errorf("optdocReplaceBlock =\n%q\nwant\n%q", got, want)
	}
	if bytes.Contains(got, []byte("OLD TABLE")) {
		t.Errorf("result still contains the old block body; want it replaced")
	}
	if !bytes.Contains(got, []byte("PROSE BEFORE THE BLOCK")) {
		t.Errorf("result lost the prose preceding the block")
	}
	if !bytes.Contains(got, []byte("PROSE AFTER THE BLOCK")) {
		t.Errorf("result lost the prose following the block")
	}
	// The END marker must be consumed, not left behind: an endStop that
	// stopped at the marker instead of past it would leave a second, orphan
	// END marker in the file and make the NEXT run's Index find the wrong one.
	if n := bytes.Count(got, []byte(optdocEndMarker)); n != 1 {
		t.Errorf("result contains %d END markers; want exactly 1", n)
	}
	if n := bytes.Count(got, []byte(optdocBeginMarker)); n != 1 {
		t.Errorf("result contains %d BEGIN markers; want exactly 1", n)
	}
}

// TestOptdocReplaceBlock_RejectsMalformedMarkers pins the three refusals.
// Each is a case where writing SOMETHING would corrupt the doc: with no
// markers there is no block to own, and with the markers reversed the byte
// range between them is negative, so a function that sliced anyway would
// either panic or silently truncate the file it is about to overwrite.
func TestOptdocReplaceBlock_RejectsMalformedMarkers(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{"no markers at all", "# Heading\n\njust prose\n", "BEGIN marker"},
		{"BEGIN only", "# Heading\n" + optdocBeginMarker + "\n", "END marker"},
		{"END only", "# Heading\n" + optdocEndMarker + "\n", "BEGIN marker"},
		{"markers reversed", "# Heading\n" + optdocEndMarker + "\nbody\n" + optdocBeginMarker + "\n", "precedes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := optdocReplaceBlock([]byte(tc.doc), "NEW")
			if err == nil {
				t.Fatalf("optdocReplaceBlock returned no error; want one (got %q)", got)
			}
			if got != nil {
				t.Errorf("optdocReplaceBlock returned %q alongside an error; want nil so a caller cannot write a corrupted doc", got)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q; want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestOptdocRenderMinVersion pins the special case its own comment calls out:
// the AlwaysAvailable zero floor must read "none", because a literal "0.0"
// in the published table would tell an operator there is a version
// requirement to check when there is none.
func TestOptdocRenderMinVersion(t *testing.T) {
	if got := optdocRenderMinVersion(chopt.AlwaysAvailable); got != "none" {
		t.Errorf("optdocRenderMinVersion(AlwaysAvailable) = %q; want %q", got, "none")
	}
	v := chopt.Version{Major: 25, Minor: 8}
	if got, want := optdocRenderMinVersion(v), v.String(); got != want {
		t.Errorf("optdocRenderMinVersion(%v) = %q; want %q", v, got, want)
	}
	if got := optdocRenderMinVersion(chopt.Version{Major: 25, Minor: 8}); got == "none" {
		t.Errorf("a real version floor rendered as %q; want the version, not the no-floor sentinel", "none")
	}
}

// TestOptdocRenderStabilityAndAutoSelect pins the two display vocabularies.
// Both are read by operators deciding whether to enable a feature, and both
// are booleans-in-disguise where an inverted mapping is invisible to
// everything except a test that names the expected word.
func TestOptdocRenderStabilityAndAutoSelect(t *testing.T) {
	if got := optdocRenderStability(chopt.Stable); got != "stable" {
		t.Errorf("optdocRenderStability(Stable) = %q; want %q", got, "stable")
	}
	if got := optdocRenderStability(chopt.Experimental); got != "experimental" {
		t.Errorf("optdocRenderStability(Experimental) = %q; want %q", got, "experimental")
	}
	if got := optdocRenderAutoSelect(true); got != "yes" {
		t.Errorf("optdocRenderAutoSelect(true) = %q; want %q", got, "yes")
	}
	if got := optdocRenderAutoSelect(false); got != "no" {
		t.Errorf("optdocRenderAutoSelect(false) = %q; want %q", got, "no")
	}
}

// TestOptdocWidthsFor pins the column sizing that keeps the emitted table
// MD060-aligned: each width is the max of the header label and every cell in
// that column, and the columns are sized independently. A width taken from
// the wrong column, or one that ignored the header, emits a table markdownlint
// rejects — which fails a lint gate rather than this generator, a long way
// from the cause.
func TestOptdocWidthsFor(t *testing.T) {
	t.Run("headers floor the widths", func(t *testing.T) {
		w := optdocWidthsFor(nil)
		if w.ID != len("id") || w.MinVersion != len("minVersion") ||
			w.Stability != len("stability") || w.AutoSelect != len("autoSelect") {
			t.Errorf("optdocWidthsFor(nil) = %+v; want each column at its header's length", w)
		}
	})

	t.Run("longest cell wins, per column", func(t *testing.T) {
		// Each cell is longer than its own header and a DIFFERENT length from
		// every other cell, so a width copied from the wrong column is visible.
		rows := []optdocRow{
			{ID: "aaaaa", MinVersion: "bbbbbbbbbbbb", Stability: "ccccccccccccc", AutoSelect: "dddddddddddddd"},
			{ID: "aa", MinVersion: "bb", Stability: "cc", AutoSelect: "dd"},
		}
		w := optdocWidthsFor(rows)
		if w.ID != 5 {
			t.Errorf("ID width = %d; want 5", w.ID)
		}
		if w.MinVersion != 12 {
			t.Errorf("MinVersion width = %d; want 12", w.MinVersion)
		}
		if w.Stability != 13 {
			t.Errorf("Stability width = %d; want 13", w.Stability)
		}
		if w.AutoSelect != 14 {
			t.Errorf("AutoSelect width = %d; want 14", w.AutoSelect)
		}
	})
}

// TestOptdocSpacesAndDashes pins the two padding primitives, and in
// particular their disagreement at n <= 0: spaces collapse to the empty
// string (a cell already at or past the column width needs no padding),
// while dashes floor at ONE, because a markdown separator row with a
// zero-width cell is not a table at all and the whole block would stop
// rendering.
func TestOptdocSpacesAndDashes(t *testing.T) {
	if got := optdocSpaces(3); got != "   " {
		t.Errorf("optdocSpaces(3) = %q; want three spaces", got)
	}
	if got := optdocSpaces(0); got != "" {
		t.Errorf("optdocSpaces(0) = %q; want the empty string", got)
	}
	if got := optdocSpaces(-2); got != "" {
		t.Errorf("optdocSpaces(-2) = %q; want the empty string", got)
	}
	if got := optdocDashes(3); got != "---" {
		t.Errorf("optdocDashes(3) = %q; want three dashes", got)
	}
	if got := optdocDashes(0); got != "-" {
		t.Errorf("optdocDashes(0) = %q; want a single dash — a zero-width separator cell breaks the table", got)
	}
	if got := optdocDashes(-2); got != "-" {
		t.Errorf("optdocDashes(-2) = %q; want a single dash", got)
	}
}

// TestOptdocRenderBlock_ShapeAndRegistryCoverage pins the generated block
// against the registry it is derived from: every registered feature gets a
// row, the block is marker-delimited, and the separator row is present. The
// per-feature check is what makes `-check` drift detection meaningful — a
// generator that silently dropped features would keep the doc "current"
// while the table under-reports what cerberus supports.
func TestOptdocRenderBlock_ShapeAndRegistryCoverage(t *testing.T) {
	block, err := optdocRenderBlock()
	if err != nil {
		t.Fatalf("optdocRenderBlock: %v", err)
	}
	if !strings.HasPrefix(block, optdocBeginMarker) {
		t.Errorf("block does not start with the BEGIN marker")
	}
	if !strings.HasSuffix(block, optdocEndMarker) {
		t.Errorf("block does not end with the END marker")
	}
	if !strings.Contains(block, "| id ") {
		t.Errorf("block has no header row:\n%s", block)
	}

	features := chopt.Registry()
	if len(features) == 0 {
		t.Fatal("chopt.Registry() is empty; this test's premise no longer holds")
	}
	for _, f := range features {
		if !strings.Contains(block, "`"+f.ID+"`") {
			t.Errorf("feature %q has no row in the generated block", f.ID)
		}
	}
	// Every table line begins a new line with `|`: the header row, the
	// separator row, and exactly one data row per registered feature. A
	// generator that emitted a feature twice, or skipped one, lands a
	// different count here even if every id still appears somewhere.
	if got, want := strings.Count(block, "\n|"), len(features)+2; got != want {
		t.Errorf("block has %d table lines; want %d (header + separator + %d features)", got, want, len(features))
	}
}

// TestOptdocsGenerate_WritesCheckAndDriftDetection pins the three outcomes
// `optdocsGenerate` produces, which together are the CI doc-drift gate:
// a stale doc is rewritten in write mode, an up-to-date doc is left byte-
// identical, and `-check` reports drift WITHOUT touching the file. The last
// is the one that matters most: a -check that wrote would make the gate
// self-healing on the runner and it would never fail, so the file's bytes
// are compared before and after.
func TestOptdocsGenerate_WritesCheckAndDriftDetection(t *testing.T) {
	block, err := optdocRenderBlock()
	if err != nil {
		t.Fatalf("optdocRenderBlock: %v", err)
	}
	stale := optdocFixture(optdocBeginMarker + "\n| stale | table |\n" + optdocEndMarker)
	current := optdocFixture(block)

	t.Run("check reports drift and does not write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "doc.md")
		if err := os.WriteFile(path, stale, 0o600); err != nil {
			t.Fatal(err)
		}
		err := optdocsGenerate(path, true)
		if err == nil {
			t.Fatal("optdocsGenerate(check) on a stale doc = nil; want a drift error")
		}
		if !strings.Contains(err.Error(), "stale") {
			t.Errorf("drift error = %q; want it to say the doc is stale", err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(after, stale) {
			t.Errorf("-check rewrote the doc; want it left untouched so the gate can actually fail")
		}
	})

	t.Run("write mode refreshes a stale doc", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "doc.md")
		if err := os.WriteFile(path, stale, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := optdocsGenerate(path, false); err != nil {
			t.Fatalf("optdocsGenerate(write): %v", err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(after, current) {
			t.Errorf("write mode produced a doc that does not match the rendered block")
		}
		// Regenerating a fresh doc must be a no-op, and -check must now pass:
		// a generator whose output is not a fixed point would make the gate
		// fail forever no matter how often it is run.
		if err := optdocsGenerate(path, true); err != nil {
			t.Errorf("-check on a just-generated doc = %v; want nil (the generator is not idempotent)", err)
		}
	})

	t.Run("missing file is an error, not a silent create", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.md")
		if err := optdocsGenerate(path, false); err == nil {
			t.Error("optdocsGenerate on a missing doc = nil; want a read error rather than a doc invented from nothing")
		}
	})
}

// TestOptdocsRun_FlagParsing pins the single-dash flag surface the
// `go:generate` directive in internal/chopt/registry.go and `just
// gen-opt-docs` invoke. Cobra's flag parsing is disabled for this command
// precisely so those historical spellings keep working, which means nothing
// else in the CLI test suite exercises them.
func TestOptdocsRun_FlagParsing(t *testing.T) {
	block, err := optdocRenderBlock()
	if err != nil {
		t.Fatalf("optdocRenderBlock: %v", err)
	}

	t.Run("-doc and -check are honoured", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "doc.md")
		if err := os.WriteFile(path, optdocFixture(block), 0o600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if err := optdocsRun([]string{"-doc", path, "-check"}, &stderr); err != nil {
			t.Errorf("optdocsRun on a current doc = %v; want nil", err)
		}
	})

	t.Run("drift is reported under the optdocs prefix", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "doc.md")
		staleDoc := optdocFixture(optdocBeginMarker + "\n| stale |\n" + optdocEndMarker)
		if err := os.WriteFile(path, staleDoc, 0o600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		err := optdocsRun([]string{"-doc", path, "-check"}, &stderr)
		if err == nil {
			t.Fatal("optdocsRun on a stale doc = nil; want a drift error")
		}
		if !strings.HasPrefix(err.Error(), "optdocs: ") {
			t.Errorf("error = %q; want the legacy `optdocs: ` prefix the go:generate caller reports under", err)
		}
	})

	t.Run("an unknown flag is refused", func(t *testing.T) {
		var stderr bytes.Buffer
		if err := optdocsRun([]string{"-nope"}, &stderr); err == nil {
			t.Error("optdocsRun with an unknown flag = nil; want a parse error rather than a silent regeneration of the default doc")
		}
	})
}
