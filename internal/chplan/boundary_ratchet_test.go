package chplan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/sealedscan"
)

// This file is the source-derived boundary ratchet for internal/chplan
// (cerberus issue #3277, step 1): a test-only, zero-behaviour-change lock on
// how far ClickHouse's own operator vocabulary has leaked into what is
// supposed to be a dialect-free logical plan IR.
//
// internal/chplan is meant to be the shared algebra all three query heads
// lower into, with internal/chsql owning everything ClickHouse-specific.
// That split already holds for functions: chplan/fn.go declares an
// engine-agnostic vocabulary and chsql/fnresolution.go resolves it, with
// completeness enforced both ways by go/parser scans
// (fn_completeness_test.go, fnresolution_completeness_test.go). It does not
// hold for operators — node kinds named after ClickHouse aggregate families
// (RangeWindowGridNative, RangeWindowGridNativeInstant,
// RangeWindowGridNativeVectorAgg, RangeBucketGridNative,
// RangeWindowStaleResample) and physical-strategy fields on RangeWindow
// (DownsampleTier, LagAdjacency, SortedSlabOverTime, NativeGroupArray) name
// a ClickHouse operator or aggregate family directly in the logical IR.
//
// # Why this is not an allow-list (invariant 7)
//
// Nothing here is exempted by name. The kind set comes from
// sealedscan.Implementers, re-derived from source on every run — the same
// discipline grid_carrier_completeness_test.go and sealed_kinds_test.go
// already use for this package's other closed sets. A brand-new leak (a new
// planNode kind or field whose name contains one of the identifiers below)
// is caught by the scan with no list to extend and no name to add anywhere.
// The only hand-maintained state is boundaryLeakSeed, one integer — the
// same shape as the coverage-floor ledger (test/coverage-floor/*.json) and
// the rejection-parity divergence ceiling
// (test/rejection-parity/divergence-ceiling.json): a checked-in number that
// must track today's derived truth exactly, tightened by hand as leaks are
// fixed, and never loosened to wave a new one through.
const boundaryLeakSeed = 10

// chBoundaryIdentifiers is the vocabulary of ClickHouse-specific spellings
// that must not appear, outside a comment, on a chplan.Node implementer's
// own type name or field names. Matching is case-insensitive on these
// spellings and ignores spaces on the CH side (see normalizeBoundaryIdent),
// so "LIMIT BY" also matches a hypothetical "LimitBy" Go identifier — Go
// identifiers cannot themselves contain a space.
var chBoundaryIdentifiers = []string{
	"GridNative",
	"StaleResample",
	"DownsampleTier",
	"LagAdjacency",
	"SortedSlab",
	"GroupArray",
	"timeSeries",
	"sumMap",
	"lagInFrame",
	"PREWHERE",
	"LIMIT BY",
}

// boundaryLeak is one site — a plan node kind's own type name, or one of its
// field names — that contains one or more chBoundaryIdentifiers, plus which
// identifier(s) matched there.
type boundaryLeak struct {
	Kind    string
	Site    string
	Matched []string
}

func (l boundaryLeak) String() string {
	return fmt.Sprintf("%s.%s (%s)", l.Kind, l.Site, strings.Join(l.Matched, ", "))
}

// normalizeBoundaryIdent lowercases s and strips spaces, so the scan can
// compare a Go identifier against a spaced ClickHouse keyword like
// "LIMIT BY" on equal footing.
func normalizeBoundaryIdent(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", ""))
}

// matchBoundaryIdentifiers returns the subset of chBoundaryIdentifiers
// contained in name (case-insensitive, space-insensitive on the CH side),
// in chBoundaryIdentifiers' own order.
func matchBoundaryIdentifiers(name string) []string {
	normalized := normalizeBoundaryIdent(name)
	var matched []string
	for _, id := range chBoundaryIdentifiers {
		if strings.Contains(normalized, normalizeBoundaryIdent(id)) {
			matched = append(matched, id)
		}
	}
	return matched
}

// scanBoundaryLeaksInSource parses one Go source file's bytes and reports a
// boundaryLeak for every struct type named in kinds whose own type name, or
// any of whose field names, contains a ClickHouse identifier.
//
// This only ever sees identifiers: go/parser attaches comment text to
// *ast.CommentGroup nodes reached separately from the *ast.TypeSpec /
// *ast.Field nodes this walk inspects, so a ClickHouse name mentioned only
// in a doc comment — the overwhelming majority of the hits in this
// package's source, describing physical rendering strategy in prose — never
// matches. That is the "non-comment positions" scope the ratchet is
// specified over, and TestBoundaryRatchet_DetectsANewLeak's synthetic
// source below exercises exactly that boundary.
func scanBoundaryLeaksInSource(filename string, src []byte, kinds map[string]bool) ([]boundaryLeak, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}
	var leaks []boundaryLeak
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || !kinds[ts.Name.Name] {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		if matched := matchBoundaryIdentifiers(ts.Name.Name); len(matched) > 0 {
			leaks = append(leaks, boundaryLeak{Kind: ts.Name.Name, Site: ts.Name.Name, Matched: matched})
		}
		if st.Fields != nil {
			for _, field := range st.Fields.List {
				for _, fieldName := range field.Names {
					if matched := matchBoundaryIdentifiers(fieldName.Name); len(matched) > 0 {
						leaks = append(leaks, boundaryLeak{Kind: ts.Name.Name, Site: fieldName.Name, Matched: matched})
					}
				}
			}
		}
		return true
	})
	return leaks, nil
}

// scanPackageBoundaryLeaks runs scanBoundaryLeaksInSource over every
// non-test .go file in dir for the given kind set (fn.go is included in the
// walk but contributes nothing: it declares no planNode-implementing
// struct, so it is out of scope by construction rather than by exemption),
// and returns every leak found, sorted for a deterministic report.
func scanPackageBoundaryLeaks(dir string, kinds []string) ([]boundaryLeak, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read package directory: %w", err)
	}
	kindSet := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		kindSet[k] = true
	}
	var leaks []boundaryLeak
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path) //nolint:gosec // repo-relative package source path
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		found, err := scanBoundaryLeaksInSource(path, src, kindSet)
		if err != nil {
			return nil, err
		}
		leaks = append(leaks, found...)
	}
	sort.Slice(leaks, func(i, j int) bool {
		if leaks[i].Kind != leaks[j].Kind {
			return leaks[i].Kind < leaks[j].Kind
		}
		return leaks[i].Site < leaks[j].Site
	})
	return leaks, nil
}

// renderBoundaryLeaks formats leaks as one bullet per site, for a failure
// message a human can act on directly.
func renderBoundaryLeaks(leaks []boundaryLeak) string {
	if len(leaks) == 0 {
		return " (none)"
	}
	lines := make([]string, 0, len(leaks))
	for _, l := range leaks {
		lines = append(lines, "  - "+l.String())
	}
	return "\n" + strings.Join(lines, "\n")
}

// checkBoundaryLeakSeed compares the derived leak count against seed and
// returns the failure message (and ok=false) if they disagree. It is a pure
// function, separate from *testing.T, precisely so
// TestBoundaryRatchet_DetectsANewLeak below can drive it directly with a
// synthetic leak list and inspect the message text, instead of only
// checking that a t.Errorf happened somewhere.
//
// The two directions are both failures, not just the "never raise it"
// half: a count BELOW seed means a leak was fixed without the committed
// number following it down, which would let the ratchet silently tolerate
// slack instead of pinning the improvement — the same discipline the
// coverage-floor and divergence-ceiling ledgers already apply.
func checkBoundaryLeakSeed(leaks []boundaryLeak, seed int) (message string, ok bool) {
	got := len(leaks)
	switch {
	case got > seed:
		return fmt.Sprintf(
			"internal/chplan carries %d ClickHouse-identifier boundary leaks, up from the committed "+
				"seed of %d (boundaryLeakSeed in boundary_ratchet_test.go). Derived leak list:%s\n"+
				"This constant may never be raised to wave a new leak through — rename the offending "+
				"node/field so it no longer names a ClickHouse operator or aggregate family, or move the "+
				"physical rendering strategy it encodes into internal/chsql where it belongs.",
			got, seed, renderBoundaryLeaks(leaks),
		), false
	case got < seed:
		return fmt.Sprintf(
			"internal/chplan carries %d ClickHouse-identifier boundary leaks, down from the committed "+
				"seed of %d (boundaryLeakSeed in boundary_ratchet_test.go). Derived leak list:%s\n"+
				"Lower boundaryLeakSeed to %d in this same change — the ratchet must track today's real "+
				"count exactly so it keeps pinning the improvement instead of drifting stale.",
			got, seed, renderBoundaryLeaks(leaks), got,
		), false
	default:
		return "", true
	}
}

// TestBoundaryRatchet_ChplanIsDialectFree is the ratchet itself. It derives
// the full chplan.Node kind set from source (sealedscan, never a hand-kept
// list — see nodeMarkerMethod in sealed_kinds_test.go), scans every kind's
// own struct declaration for the ClickHouse vocabulary in
// chBoundaryIdentifiers, and asserts the count matches boundaryLeakSeed
// exactly. It would have flagged RangeWindowGridNative and
// RangeWindowStaleResample on the day they landed.
func TestBoundaryRatchet_ChplanIsDialectFree(t *testing.T) {
	kinds, err := sealedscan.Implementers(".", nodeMarkerMethod)
	if err != nil {
		t.Fatalf("derive the chplan.Node kind set: %v", err)
	}
	leaks, err := scanPackageBoundaryLeaks(".", kinds)
	if err != nil {
		t.Fatalf("scan chplan sources for ClickHouse-identifier boundary leaks: %v", err)
	}
	if message, ok := checkBoundaryLeakSeed(leaks, boundaryLeakSeed); !ok {
		t.Error(message)
	}
}

// TestBoundaryRatchet_DetectsANewLeak is the vacuity guard: a gate's first
// green proves nothing unless it can be shown to fail. It exercises both
// halves of the mechanism independently of this package's real, and
// changing, source tree:
//
//  1. scanBoundaryLeaksInSource, driven directly on a synthetic snippet
//     containing exactly one leaky field and one CH mention that is ONLY in
//     a comment, proving the scan fires on the field name and ignores the
//     comment.
//  2. checkBoundaryLeakSeed, driven on a fabricated leak list padded out to
//     exactly boundaryLeakSeed (green) and then with the synthetic leak
//     added on top (red), proving the ratchet's own pass/fail path — not
//     merely the scanner in isolation — crosses from green to red and
//     names the new leak in its message.
func TestBoundaryRatchet_DetectsANewLeak(t *testing.T) {
	const syntheticSource = `package chplan

// FakeLeaky is a synthetic plan node that exists only in this test, used to
// prove the boundary scan actually fires on a new leak.
type FakeLeaky struct {
	Input Node

	// A mention of groupArray here, in a doc comment, must NOT count as a
	// leak — only the field's own name below may.
	SortedSlabOverTime bool
}

func (*FakeLeaky) planNode() {}
`

	newLeaks, err := scanBoundaryLeaksInSource("synthetic_leak.go", []byte(syntheticSource), map[string]bool{"FakeLeaky": true})
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	if len(newLeaks) != 1 {
		t.Fatalf("scanning the synthetic source found %d leaks, want exactly 1 (the comment must not count): %v",
			len(newLeaks), newLeaks)
	}
	got := newLeaks[0]
	if got.Kind != "FakeLeaky" || got.Site != "SortedSlabOverTime" {
		t.Fatalf("leak = %+v, want Kind=FakeLeaky Site=SortedSlabOverTime", got)
	}
	wantIdent := "SortedSlab"
	found := false
	for _, m := range got.Matched {
		if m == wantIdent {
			found = true
		}
	}
	if !found {
		t.Fatalf("leak matched identifiers = %v, want to include %q", got.Matched, wantIdent)
	}

	// Pad a fake "current" leak list out to exactly the committed seed —
	// this must be green on its own, or the padding itself would be
	// confounding the result below.
	padded := make([]boundaryLeak, boundaryLeakSeed)
	for i := range padded {
		padded[i] = boundaryLeak{Kind: "Filler", Site: fmt.Sprintf("Field%d", i), Matched: []string{"GridNative"}}
	}
	if message, ok := checkBoundaryLeakSeed(padded, boundaryLeakSeed); !ok {
		t.Fatalf("a leak list padded to exactly boundaryLeakSeed unexpectedly failed: %s", message)
	}

	// One leak beyond the seed must fail, and must name it.
	withNewLeak := append(append([]boundaryLeak{}, padded...), got)
	message, ok := checkBoundaryLeakSeed(withNewLeak, boundaryLeakSeed)
	if ok {
		t.Fatal("adding one leak beyond boundaryLeakSeed did not fail the ratchet — the gate is vacuous")
	}
	if !strings.Contains(message, "FakeLeaky.SortedSlabOverTime") {
		t.Fatalf("failure message does not name the new leak by site: %s", message)
	}
	if !strings.Contains(message, "may never be raised") {
		t.Fatalf("failure message does not state the never-raise rule: %s", message)
	}

	// One FEWER leak than the seed must also fail, telling the reader to
	// lower the constant rather than silently tolerating the slack.
	message, ok = checkBoundaryLeakSeed(padded[:boundaryLeakSeed-1], boundaryLeakSeed)
	if ok {
		t.Fatal("one fewer leak than boundaryLeakSeed did not fail the ratchet — a fixed leak would go unpinned")
	}
	if !strings.Contains(message, fmt.Sprintf("Lower boundaryLeakSeed to %d", boundaryLeakSeed-1)) {
		t.Fatalf("failure message does not instruct lowering the constant: %s", message)
	}
}
