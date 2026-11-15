//go:build agpl_oracle

package agpl_oracle

// The copy scan compares the in-house query-language packages, function by
// function, against the pinned Loki and Tempo module sources in the module
// cache, and fails when an in-house function shares its implementation
// with an upstream one.
//
// Method. Each function (or method) is tokenised with go/scanner, which
// drops comments and whitespace. The tokens of one source line form one
// normalised line, and a line holding no identifier, keyword or literal (a
// lone `}` or `)`) is dropped. Three consecutive normalised lines form a
// shingle. Function declarations and the function literals of
// package-level declarations are compared; every non-test Go file under the
// in-house trees is scanned, generated or not, while an upstream file that
// is generated or does not parse on this toolchain is left out and counted
// in the log.
//
// Two passes. The named pass lower-cases identifiers and keeps them, so
// neither spacing nor identifier case takes part in the comparison while
// every name and literal does. The identifier-abstracted pass replaces
// every identifier with one placeholder, except Go's predeclared
// identifiers (`string`, `len`, `nil`, `true`, …), which belong to the
// language as keywords do; it reports a copy whatever its function and
// identifier names. Keywords, operators and literals are kept by both.
//
// Matching. An in-house function matches an upstream function when at
// least minShared of its evidence shingles occur in that upstream function
// and either they make up at least minShare of its evidence shingles (a
// short function copied whole) or they number at least minSection (a
// copied section inside a longer function). The thresholds are per pass
// (passRules).
//
// Evidence. A shingle is evidence unless one of the following holds.
//
//   - It is an interface fact on either side. A reimplementation of a
//     query language shares some text with every other implementation by
//     necessity: the operator and keyword spellings, the enum and wire
//     names, and the error strings matched for parity. A line is an
//     interface fact by its shape alone, never by its location:
//
//   - a spelling table line — a `case` clause, a `key: value,` element
//     or a lone `value,` element whose operands are all names and
//     literals, or a `return` of one literal — is an operator or keyword
//     spelling when every string literal on it is punctuation, and an
//     enum or wire name otherwise;
//
//   - a line that hands a string literal to an error constructor
//     (`errors.New`, `fmt.Errorf`, or a function whose name ends in
//     `error`) is a matched error string.
//
//     A spelling table written as a switch also has frame lines — the
//     `switch v {` header, `default:`, and `return true` / `return false`
//     arms. They join an accepted shingle without making it counted, but
//     a shingle of frame lines alone is counted. A shingle made only of
//     facts and frames is an interface fact; so is any in-house shingle
//     whose normalised text an upstream function states as one, because
//     the kind belongs to the text and, in the abstracted pass, the names
//     that classified it upstream are gone from it.
//
//   - In the abstracted pass, it is a lookup-table shingle: every line is a
//     `case` clause or a `return` statement (or a frame line), as in a
//     switch mapping one enum to another. With names abstracted, what
//     such a shingle keeps is only the alternation every lookup table has.
//
//   - In the abstracted pass, more than copyScanAbstractMaxHolders upstream
//     functions hold it. A statement sequence that two independently
//     written upstream functions both hold is an idiom of the language (a
//     sorted-keys loop, a locked setter), not a trace of one
//     implementation.
//
// The abstracted pass's thresholds are measured, not chosen:
// TestCopyScan_UnrelatedCodeIsNotReported scans every in-house package
// outside the query-language trees — code with no Loki or Tempo
// counterpart, so whatever it reports is chance — and each threshold is
// the first value at which that corpus reports nothing. At the time of
// measurement it held 5296 functions; one step below each constant it
// reported a match (a holder limit of 2: `nonEmpty` 3 of 4; a minimum of
// 2 shared: `qualify` 2 of 2; a section of 5: `concatLiteralSet` 5 of 10),
// and the closest pair under a share rule shared 4 of 7 shingles (57%),
// below copyScanAbstractMinShare. TestCopyScan_DetectsARenamedCopy bounds
// the thresholds from the other side.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"hash/fnv"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// copyScanRoots are the in-house trees the scan covers, relative to the
// repository root.
var copyScanRoots = []string{
	"internal/logql",
	"internal/traceql",
	"internal/drain",
	"internal/api/loki",
	"internal/api/tempo",
}

// copyScanUnrelatedParents are the repository directories whose packages,
// outside copyScanRoots and copyScanCopiedTree, make up the corpus of
// unrelated in-house code.
var copyScanUnrelatedParents = []string{"internal", "cmd"}

// copyScanCopiedTree holds sources copied from upstream under their own
// licence, so it is neither query-language code nor unrelated code.
const copyScanCopiedTree = "internal/thirdparty"

// copyScanUpstreamModules are the upstream modules the scan compares
// against, at the versions (and replacements) go.mod pins.
var copyScanUpstreamModules = []string{
	"github.com/grafana/loki/v3",
	"github.com/grafana/tempo",
}

// controlPackage is one upstream package the positive controls disguise or
// rename and compare against its own source.
type controlPackage struct {
	module int    // index into copyScanUpstreamModules
	dir    string // package directory inside the module
}

// copyScanControlPackages are the upstream packages the positive controls
// use: Loki's pattern package and Tempo's TraceQL package.
var copyScanControlPackages = []controlPackage{
	{module: 0, dir: "pkg/logql/log/pattern"},
	{module: 1, dir: "pkg/traceql"},
}

const (
	// copyScanShingleLines is the number of consecutive normalised lines
	// in one shingle.
	copyScanShingleLines = 3

	// copyScanMinShared is the named pass's fewest shared shingles that
	// make a match.
	copyScanMinShared = 2
	// copyScanMinShare is the named pass's smallest fraction of a
	// function's evidence shingles that, shared with one upstream
	// function, makes a match.
	copyScanMinShare = 0.5
	// copyScanMinSection is the named pass's number of shared shingles
	// that makes a match whatever fraction of the function they are.
	copyScanMinSection = 5

	// copyScanAbstractMinShared, copyScanAbstractMinShare and
	// copyScanAbstractMinSection are the abstracted pass's counterparts,
	// measured on unrelated in-house code (see the file comment).
	copyScanAbstractMinShared  = 3
	copyScanAbstractMinShare   = 0.75
	copyScanAbstractMinSection = 6
	// copyScanAbstractMaxHolders is the most upstream functions that may
	// hold a shingle for it to count as evidence in the abstracted pass.
	copyScanAbstractMaxHolders = 1
)

// scanPass names how a pass of the scan normalises identifiers.
type scanPass int

const (
	// namedPass lower-cases identifiers and keeps them, so a match shares
	// names as well as statements.
	namedPass scanPass = iota
	// abstractPass replaces every identifier but the predeclared ones
	// with identPlaceholder, so a match shares a statement sequence
	// whatever its names.
	abstractPass
	scanPassCount
)

func (p scanPass) String() string {
	if p == abstractPass {
		return "identifier-abstracted"
	}
	return "named"
}

// identPlaceholder stands for every identifier in the abstracted pass.
const identPlaceholder = "_"

// passRule is one pass's match thresholds.
type passRule struct {
	minShared  int
	minShare   float64
	minSection int
	maxHolders int
}

var passRules = [scanPassCount]passRule{
	namedPass: {
		minShared:  copyScanMinShared,
		minShare:   copyScanMinShare,
		minSection: copyScanMinSection,
		maxHolders: math.MaxInt,
	},
	abstractPass: {
		minShared:  copyScanAbstractMinShared,
		minShare:   copyScanAbstractMinShare,
		minSection: copyScanAbstractMinSection,
		maxHolders: copyScanAbstractMaxHolders,
	},
}

// overlapKind names a kind of interface fact.
type overlapKind string

const (
	kindNone        overlapKind = ""
	kindSpelling    overlapKind = "operator or keyword spelling"
	kindEnumName    overlapKind = "enum or wire name"
	kindErrorString overlapKind = "error string matched for parity"
	// kindTableFrame is a line that frames a spelling table written as a
	// switch: its `switch v {` header, its `default:` and a `return true`
	// or `return false` arm. It states no fact of its own, so a shingle
	// made only of frame lines is counted; it never turns an accepted
	// shingle into a counted one.
	kindTableFrame overlapKind = "table frame"
	// kindLookupTable is an abstracted-pass shingle of switch arms.
	kindLookupTable overlapKind = "lookup table"
	// kindIdiom is an abstracted-pass shingle held by more than
	// copyScanAbstractMaxHolders upstream functions.
	kindIdiom overlapKind = "idiom held by several upstream functions"
)

// scanLine is one normalised source line.
type scanLine struct {
	text string
	kind overlapKind
	arm  bool // a `case` clause header or a `return` statement
}

// shingleSet is one function's shingles under one pass.
type shingleSet struct {
	counted  map[uint64]string
	accepted map[uint64]overlapKind
}

// scanFunc is one function's shingles under every pass.
type scanFunc struct {
	id     string // path:Receiver.Name
	passes [scanPassCount]shingleSet
}

func TestCopyScan_NoUpstreamImplementationMatch(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	var inHouse []string
	for _, r := range copyScanRoots {
		inHouse = append(inHouse, filepath.Join(root, r))
	}
	ours, _ := scanTrees(t, root, inHouse, inHouseTree)
	if len(ours) == 0 {
		t.Fatal("the scan found no in-house function")
	}
	assertNoMatch(t, ours, upstreamCorpus(t))
}

// TestCopyScan_UnrelatedCodeIsNotReported is the scan's negative control
// and the measurement behind the abstracted pass's thresholds: in-house
// code outside the query-language trees implements nothing Loki or Tempo
// implement, so a match there is chance, and neither pass may report one.
func TestCopyScan_UnrelatedCodeIsNotReported(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	trees := unrelatedTrees(t, root)
	unrelated, _ := scanTrees(t, root, trees, inHouseTree)
	if len(unrelated) == 0 {
		t.Fatal("the scan found no unrelated in-house function")
	}
	assertNoMatch(t, unrelated, upstreamCorpus(t))
}

// assertNoMatch fails for every function in ours that either pass reports.
func assertNoMatch(t *testing.T, ours []*scanFunc, up *upstream) {
	t.Helper()
	t.Logf("scanned %d in-house and %d upstream functions (%d generated or unparsable upstream files left out)",
		len(ours), len(up.funcs), up.leftOut)
	for pass := range scanPassCount {
		matches, accepted := compareFuncs(ours, up, pass)
		kinds := make([]string, 0, len(accepted))
		for k, n := range accepted {
			kinds = append(kinds, fmt.Sprintf("%s: %d", k, n))
		}
		sort.Strings(kinds)
		t.Logf("%s pass: accepted overlap by kind: %s", pass, strings.Join(kinds, ", "))
		for _, m := range matches {
			t.Errorf("%s pass: implementation match: %s", pass, m)
		}
	}
}

// unrelatedTrees lists every directory and Go file under
// copyScanUnrelatedParents that is neither inside nor above a scanned root
// nor inside copyScanCopiedTree; a directory above a root is descended.
func unrelatedTrees(t *testing.T, root string) []string {
	t.Helper()
	excluded := append([]string{copyScanCopiedTree}, copyScanRoots...)
	var trees []string
	var visit func(rel string)
	visit = func(rel string) {
		entries, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, e := range entries {
			child := filepath.ToSlash(filepath.Join(rel, e.Name()))
			switch {
			case !e.IsDir():
				if strings.HasSuffix(child, ".go") {
					trees = append(trees, filepath.Join(root, child))
				}
			case slices.Contains(excluded, child):
			case slices.ContainsFunc(excluded, func(x string) bool { return isWithin(x, child) }):
				visit(child)
			default:
				trees = append(trees, filepath.Join(root, child))
			}
		}
	}
	for _, parent := range copyScanUnrelatedParents {
		visit(parent)
	}
	return trees
}

// isWithin reports whether the slash path p lies strictly inside dir.
func isWithin(p, dir string) bool {
	return strings.HasPrefix(p, dir+"/")
}

// TestCopyScan_DetectsACopy is the named pass's positive control: each
// control package's hand-written files, disguised the way the method
// claims not to be fooled by (comments dropped, every identifier
// upper-cased and every line re-spaced) and scanned as an in-house tree,
// must match function for function, so a scan that silently stopped
// detecting a copy fails here rather than passing the test above.
func TestCopyScan_DetectsACopy(t *testing.T) {
	t.Parallel()

	up := upstreamCorpus(t)
	for _, pkg := range copyScanControlPackages {
		dir := writeControl(t, up, pkg, func(src []byte) []byte {
			return respell(src, strings.ToUpper)
		})
		copied, _ := scanTrees(t, dir, []string{dir}, inHouseTree)
		checked, missed := detected(copied, up, namedPass)
		for _, id := range missed {
			t.Errorf("%s, a disguised copy of %s, is not reported as a match", id, pkg.dir)
		}
		if checked == 0 {
			t.Errorf("the control package %s has no function large enough to compare", pkg.dir)
		}
	}
}

// TestCopyScan_DetectsARenamedCopy is the abstracted pass's positive
// control: each control package's hand-written files, with every
// identifier renamed (all but the predeclared ones and `_`, each name
// replaced consistently with a fresh one) and scanned as an in-house
// tree, must be reported function for function. The named pass must miss some of them, so the rename defeats the
// names this control means to take away.
func TestCopyScan_DetectsARenamedCopy(t *testing.T) {
	t.Parallel()

	up := upstreamCorpus(t)
	for _, pkg := range copyScanControlPackages {
		var names renamer
		dir := writeControl(t, up, pkg, func(src []byte) []byte {
			return respell(src, names.rename)
		})
		renamed, _ := scanTrees(t, dir, []string{dir}, inHouseTree)
		checked, missed := detected(renamed, up, abstractPass)
		for _, id := range missed {
			t.Errorf("%s, a renamed copy of %s, is not reported as a match", id, pkg.dir)
		}
		if checked == 0 {
			t.Errorf("the control package %s has no function large enough to compare", pkg.dir)
		}
		if namedChecked, namedMissed := detected(renamed, up, namedPass); len(namedMissed) == 0 {
			t.Errorf("the named pass reports all %d renamed functions of %s, so the rename hides no name", namedChecked, pkg.dir)
		}
	}
}

// writeControl writes the hand-written files of a control package,
// rewritten by respellFile, into a fresh directory.
func writeControl(t *testing.T, up *upstream, pkg controlPackage, respellFile func([]byte) []byte) string {
	t.Helper()
	control := filepath.Join(up.dirs[pkg.module], filepath.FromSlash(pkg.dir))
	out := t.TempDir()
	entries, err := os.ReadDir(control)
	if err != nil {
		t.Fatalf("read the control package: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(control, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		header, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ParseComments|parser.PackageClauseOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if ast.IsGenerated(header) {
			continue // the upstream corpus leaves generated files out
		}
		rewritten := respellFile(src)
		if bytes.Equal(rewritten, src) {
			t.Fatalf("rewriting %s left it unchanged", name)
		}
		if err := os.WriteFile(filepath.Join(out, name), rewritten, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return out
}

// detected compares a control copy with upstream under one pass and
// returns how many of its functions carry enough evidence to be compared,
// and the ids of those not reported as a match.
func detected(copied []*scanFunc, up *upstream, pass scanPass) (checked int, missed []string) {
	reported := map[string]bool{}
	matches, _ := compareFuncs(copied, up, pass)
	for _, m := range matches {
		reported[m.ours] = true
	}
	for _, f := range copied {
		if len(up.evidence(f.passes[pass], pass)) < passRules[pass].minShared {
			continue
		}
		checked++
		if !reported[f.id] {
			missed = append(missed, f.id)
		}
	}
	return checked, missed
}

// renamer gives every identifier it is shown a fresh name, the same one
// each time.
type renamer struct {
	mu sync.Mutex
	to map[string]string
}

// rename returns the fresh name for id, leaving the predeclared
// identifiers and `_` as they are.
func (r *renamer) rename(id string) string {
	if id == "_" || types.Universe.Lookup(id) != nil {
		return id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.to == nil {
		r.to = map[string]string{}
	}
	fresh, ok := r.to[id]
	if !ok {
		fresh = fmt.Sprintf("renamed%d", len(r.to))
		r.to[id] = fresh
	}
	return fresh
}

// respell re-emits Go source with its comments dropped, every identifier
// passed through ident and the tokens of each line separated by single
// spaces, line breaks kept so the result still parses.
func respell(src []byte, ident func(string) string) []byte {
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	var out bytes.Buffer
	line := 1
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		for ; line < file.PositionFor(pos, false).Line; line++ {
			out.WriteByte('\n')
		}
		switch {
		case tok == token.IDENT:
			out.WriteString(ident(lit))
		case lit != "":
			out.WriteString(lit)
			line += strings.Count(lit, "\n")
		default:
			out.WriteString(tok.String())
		}
		out.WriteByte(' ')
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// match is one in-house function reported as sharing its implementation
// with an upstream one.
type match struct {
	ours, theirs  string
	shared, total int
	sample        []string
}

func (m match) String() string {
	return fmt.Sprintf("%s shares %d of %d shingles (%.0f%%) with upstream %s:\n%s",
		m.ours, m.shared, m.total, float64(m.shared)*100/float64(m.total), m.theirs,
		strings.Join(m.sample, "\n    ---\n"))
}

// shingleIndex maps the shingles of the upstream corpus under one pass to
// the functions holding them.
type shingleIndex struct {
	counted  map[uint64][]int       // counted shingle -> indexes into upstream.funcs
	accepted map[uint64]overlapKind // accepted shingle -> its kind
}

// upstream is the scanned upstream corpus with its shingle indexes.
type upstream struct {
	dirs    []string
	funcs   []*scanFunc
	index   [scanPassCount]shingleIndex
	leftOut int // generated or unparsable files left out
}

var (
	upstreamOnce sync.Once
	upstreamScan *upstream
)

// upstreamCorpus scans the upstream modules once for every test.
func upstreamCorpus(t *testing.T) *upstream {
	t.Helper()
	upstreamOnce.Do(func() {
		up := &upstream{}
		up.dirs = upstreamModuleDirs(t, repoRoot(t))
		for _, dir := range up.dirs {
			funcs, left := scanTrees(t, dir, []string{dir}, upstreamTree)
			up.funcs = append(up.funcs, funcs...)
			up.leftOut += left
		}
		for pass := range scanPassCount {
			idx := shingleIndex{counted: map[uint64][]int{}, accepted: map[uint64]overlapKind{}}
			for i, f := range up.funcs {
				for h := range f.passes[pass].counted {
					idx.counted[h] = append(idx.counted[h], i)
				}
				for h, kind := range f.passes[pass].accepted {
					idx.accepted[h] = kind
				}
			}
			up.index[pass] = idx
		}
		upstreamScan = up
	})
	if upstreamScan == nil || len(upstreamScan.funcs) == 0 {
		t.Fatal("the scan found no upstream function")
	}
	return upstreamScan
}

// evidence returns the shingles of f that count towards a match under
// pass: those no upstream function states as an interface fact and, under
// the pass's holder limit, no idiom.
func (up *upstream) evidence(f shingleSet, pass scanPass) []uint64 {
	idx := up.index[pass]
	var out []uint64
	for h := range f.counted {
		if _, fact := idx.accepted[h]; fact || len(idx.counted[h]) > passRules[pass].maxHolders {
			continue
		}
		out = append(out, h)
	}
	return out
}

// compareFuncs reports every function in ours that matches an upstream
// function under one pass, and counts by kind the accepted shingles ours
// shares with upstream.
func compareFuncs(ours []*scanFunc, up *upstream, pass scanPass) ([]match, map[overlapKind]int) {
	idx := up.index[pass]
	rule := passRules[pass]
	accepted := map[overlapKind]int{}
	var matches []match
	for _, of := range ours {
		f := of.passes[pass]
		for h, kind := range f.accepted {
			if _, ok := idx.accepted[h]; ok {
				accepted[kind]++
			}
		}
		for h := range f.counted {
			if kind, ok := idx.accepted[h]; ok {
				accepted[kind]++
			} else if len(idx.counted[h]) > rule.maxHolders {
				accepted[kindIdiom]++
			}
		}
		evidence := up.evidence(f, pass)
		if len(evidence) < rule.minShared {
			continue
		}
		shared := map[int][]uint64{}
		for _, h := range evidence {
			for _, i := range idx.counted[h] {
				shared[i] = append(shared[i], h)
			}
		}
		best := -1
		for i, hs := range shared {
			if best < 0 || len(hs) > len(shared[best]) || (len(hs) == len(shared[best]) && up.funcs[i].id < up.funcs[best].id) {
				best = i
			}
		}
		if best < 0 {
			continue
		}
		n := len(shared[best])
		share := float64(n) / float64(len(evidence))
		if n < rule.minShared || (share < rule.minShare && n < rule.minSection) {
			continue
		}
		m := match{ours: of.id, theirs: up.funcs[best].id, shared: n, total: len(evidence)}
		for _, h := range shared[best] {
			m.sample = append(m.sample, "      "+strings.ReplaceAll(f.counted[h], "\n", "\n      "))
		}
		sort.Strings(m.sample)
		matches = append(matches, m)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ours < matches[j].ours })
	return matches, accepted
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// upstreamModuleDirs resolves each upstream module to its source directory
// in the module cache, downloading it when it is not there yet.
func upstreamModuleDirs(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"mod", "download", "-json"}, copyScanUpstreamModules...)...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod download: %v\n%s", err, out)
	}
	var dirs []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var m struct{ Path, Version, Dir, Error string }
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode go mod download output: %v", err)
		}
		if m.Error != "" || m.Dir == "" {
			t.Fatalf("module %s@%s has no source directory: %s", m.Path, m.Version, m.Error)
		}
		t.Logf("upstream source: %s@%s", m.Path, m.Version)
		dirs = append(dirs, m.Dir)
	}
	if len(dirs) != len(copyScanUpstreamModules) {
		t.Fatalf("resolved %d upstream modules, want %d", len(dirs), len(copyScanUpstreamModules))
	}
	return dirs
}

// treeKind says whose source a scanned tree holds.
type treeKind bool

const (
	// inHouseTree is cerberus's own source: every non-test file is
	// scanned, and a file that does not parse fails the scan.
	inHouseTree treeKind = false
	// upstreamTree is an upstream module: a generated file, or one that
	// does not parse on this toolchain, is left out and counted.
	upstreamTree treeKind = true
)

// scanTrees collects the shingles of every function in the non-test Go
// files under trees, and the number of files left out (see treeKind).
// Function ids are relative to base.
func scanTrees(t *testing.T, base string, trees []string, kind treeKind) ([]*scanFunc, int) {
	t.Helper()
	var paths []string
	for _, tree := range trees {
		err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); path != tree && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}

	var (
		mu    sync.Mutex
		funcs []*scanFunc
		wg    sync.WaitGroup
		work  = make(chan string)
		fails []string
		left  int
	)
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for path := range work {
				fs, leftOut, err := scanFile(base, path, kind)
				mu.Lock()
				if err != nil {
					fails = append(fails, err.Error())
				}
				if leftOut {
					left++
				}
				funcs = append(funcs, fs...)
				mu.Unlock()
			}
		})
	}
	for _, p := range paths {
		work <- p
	}
	close(work)
	wg.Wait()
	if len(fails) > 0 {
		sort.Strings(fails)
		t.Fatalf("scan: %s", strings.Join(fails, "\n"))
	}
	sort.Slice(funcs, func(i, j int) bool { return funcs[i].id < funcs[j].id })
	return funcs, left
}

// scanFile collects the shingles of one file's function declarations and
// of the function literals in its package-level declarations. leftOut
// reports an upstream file left out (see treeKind).
func scanFile(base, path string, kind treeKind) (funcs []*scanFunc, leftOut bool, err error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		if kind == upstreamTree {
			return nil, true, nil
		}
		return nil, false, err
	}
	if kind == upstreamTree && ast.IsGenerated(file) {
		return nil, true, nil
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return nil, false, err
	}
	scan := func(id string, n ast.Node) *scanFunc {
		text := src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset]
		f := &scanFunc{id: id}
		for pass := range scanPassCount {
			f.passes[pass] = shingle(normaliseLines(text, pass), pass)
		}
		return f
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				funcs = append(funcs, scan(rel+":"+funcName(d), d))
			}
		case *ast.GenDecl:
			literal := 0
			ast.Inspect(d, func(n ast.Node) bool {
				lit, ok := n.(*ast.FuncLit)
				if !ok {
					return true
				}
				literal++
				funcs = append(funcs, scan(fmt.Sprintf("%s:func literal %d at line %d", rel, literal, fset.Position(lit.Pos()).Line), lit))
				return false
			})
		}
	}
	return funcs, false, nil
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	for {
		switch r := recv.(type) {
		case *ast.StarExpr:
			recv = r.X
			continue
		case *ast.IndexExpr:
			recv = r.X
			continue
		case *ast.IndexListExpr:
			recv = r.X
			continue
		case *ast.Ident:
			return r.Name + "." + fd.Name.Name
		}
		return fd.Name.Name
	}
}

// lineToken is one scanned token of a line.
type lineToken struct {
	tok token.Token
	lit string
}

// normaliseLines tokenises src (one function) into its normalised lines
// under pass, dropping comments, automatic semicolons and lines with no
// identifier, keyword or literal.
func normaliseLines(src []byte, pass scanPass) []scanLine {
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)

	var lines []scanLine
	var cur []lineToken
	curLine := 0
	flush := func() {
		if l, ok := renderLine(cur, pass); ok {
			lines = append(lines, l)
		}
		cur = cur[:0]
	}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		if line := file.PositionFor(pos, false).Line; line != curLine {
			flush()
			curLine = line
		}
		cur = append(cur, lineToken{tok: tok, lit: lit})
	}
	flush()
	return lines
}

// renderLine joins a line's tokens under pass and classifies it; ok is
// false for a line that carries nothing but punctuation.
func renderLine(toks []lineToken, pass scanPass) (scanLine, bool) {
	var parts []string
	meaningful := false
	for _, tk := range toks {
		switch {
		case tk.tok == token.IDENT && pass == abstractPass && types.Universe.Lookup(tk.lit) == nil:
			parts = append(parts, identPlaceholder)
			meaningful = true
		case tk.tok == token.IDENT:
			parts = append(parts, strings.ToLower(tk.lit))
			meaningful = true
		case tk.tok.IsLiteral():
			parts = append(parts, tk.lit)
			meaningful = true
		case tk.tok.IsKeyword():
			parts = append(parts, tk.tok.String())
			meaningful = true
		default:
			parts = append(parts, tk.tok.String())
		}
	}
	if !meaningful {
		return scanLine{}, false
	}
	return scanLine{text: strings.Join(parts, " "), kind: classifyLine(toks), arm: isSwitchArm(toks)}, true
}

// isSwitchArm recognises a `case …:` clause header and a `return`
// statement.
func isSwitchArm(toks []lineToken) bool {
	first, last := toks[0].tok, toks[len(toks)-1].tok
	return first == token.RETURN || (first == token.CASE && last == token.COLON)
}

// classifyLine names the kind of interface fact a line states, kindTableFrame
// for a switch-table frame line, or kindNone.
func classifyLine(toks []lineToken) overlapKind {
	if isTableFrame(toks) {
		return kindTableFrame
	}
	if kind := tableLineKind(toks); kind != kindNone {
		return kind
	}
	if handsStringToErrorConstructor(toks) {
		return kindErrorString
	}
	return kindNone
}

// isTableFrame recognises `switch v {`, `default:`, `return true` and
// `return false`.
func isTableFrame(toks []lineToken) bool {
	switch {
	case len(toks) >= 2 && toks[0].tok == token.SWITCH && toks[len(toks)-1].tok == token.LBRACE:
		return true
	case len(toks) == 2 && toks[0].tok == token.DEFAULT && toks[1].tok == token.COLON:
		return true
	case len(toks) == 2 && toks[0].tok == token.RETURN && toks[1].tok == token.IDENT:
		return toks[1].lit == "true" || toks[1].lit == "false"
	}
	return false
}

// tableLineKind recognises a spelling-table line: `case A, B:`, `K: V,`,
// `V,` or `return V`, where every operand is a name or a literal (a
// qualified name such as `pkg.Name` included).
func tableLineKind(toks []lineToken) overlapKind {
	body := toks
	switch {
	case len(body) > 0 && body[0].tok == token.CASE:
		if body[len(body)-1].tok != token.COLON {
			return kindNone
		}
		body = body[1 : len(body)-1]
	case len(body) > 0 && body[0].tok == token.RETURN:
		body = body[1:]
		if len(body) != 1 || !body[0].tok.IsLiteral() {
			return kindNone
		}
	default:
		if len(body) == 0 || body[len(body)-1].tok != token.COMMA {
			return kindNone
		}
		body = body[:len(body)-1]
	}
	if len(body) == 0 {
		return kindNone
	}
	sawString, allPunct := false, true
	for _, tk := range body {
		switch tk.tok {
		case token.IDENT, token.PERIOD, token.COMMA, token.COLON, token.INT, token.FLOAT, token.CHAR:
		case token.STRING:
			sawString = true
			if !isPunctuationLiteral(tk.lit) {
				allPunct = false
			}
		default:
			return kindNone
		}
	}
	if sawString && allPunct {
		return kindSpelling
	}
	return kindEnumName
}

// isPunctuationLiteral reports whether a string literal spells only
// punctuation and symbols, as an operator does.
func isPunctuationLiteral(lit string) bool {
	body := strings.Trim(lit, "\"`")
	if body == "" {
		return false
	}
	for _, r := range body {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// handsStringToErrorConstructor reports whether the line calls an error
// constructor with a string literal argument.
func handsStringToErrorConstructor(toks []lineToken) bool {
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].tok != token.IDENT || toks[i+1].tok != token.LPAREN || toks[i+2].tok != token.STRING {
			continue
		}
		name := strings.ToLower(toks[i].lit)
		qualifier := ""
		if i >= 2 && toks[i-1].tok == token.PERIOD && toks[i-2].tok == token.IDENT {
			qualifier = toks[i-2].lit
		}
		if (qualifier == "errors" && name == "new") || (qualifier == "fmt" && name == "errorf") ||
			strings.HasSuffix(name, "error") {
			return true
		}
	}
	return false
}

// shingle builds a function's shingle sets under pass from its normalised
// lines.
func shingle(lines []scanLine, pass scanPass) shingleSet {
	f := shingleSet{counted: map[uint64]string{}, accepted: map[uint64]overlapKind{}}
	for i := 0; i+copyScanShingleLines <= len(lines); i++ {
		window := lines[i : i+copyScanShingleLines]
		texts := make([]string, len(window))
		for j, l := range window {
			texts[j] = l.text
		}
		kind := shingleKind(window)
		if kind == kindNone && pass == abstractPass && isLookupTable(window) {
			kind = kindLookupTable
		}
		text := strings.Join(texts, "\n")
		h := fnv.New64a()
		h.Write([]byte(text))
		sum := h.Sum64()
		if kind != kindNone {
			f.accepted[sum] = kind
			continue
		}
		f.counted[sum] = text
	}
	return f
}

// shingleKind is the interface-fact kind of a shingle whose every line is
// an interface fact or a table frame and at least one is a fact, and
// kindNone for any other shingle.
func shingleKind(window []scanLine) overlapKind {
	kind := kindNone
	for _, l := range window {
		switch {
		case l.kind == kindNone:
			return kindNone
		case l.kind == kindTableFrame:
		case kind == kindNone:
			kind = l.kind
		case !strings.Contains(string(kind), string(l.kind)):
			parts := strings.Split(string(kind), kindJoiner)
			parts = append(parts, string(l.kind))
			sort.Strings(parts)
			kind = overlapKind(strings.Join(parts, kindJoiner))
		}
	}
	return kind
}

// isLookupTable reports whether every line of a shingle is a switch arm or
// a table frame line, and at least one is an arm.
func isLookupTable(window []scanLine) bool {
	arm := false
	for _, l := range window {
		switch {
		case l.arm:
			arm = true
		case l.kind != kindTableFrame:
			return false
		}
	}
	return arm
}

// kindJoiner joins the kinds of a shingle whose lines state facts of more
// than one kind.
const kindJoiner = " + "
