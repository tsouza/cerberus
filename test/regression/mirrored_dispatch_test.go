package regression

// Class-closing ratchet for the PRODUCTION half of the "declared
// duplicate of a derivable fact" family (#2031).
//
// sealed_marker_derivation_test.go closes the TEST half: a number or a
// hand list restating a sealed set, in a `_test.go`, matched against the
// markers declared beside it. Its own scope note names what it cannot
// see — "a dispatch mirrored between two PRODUCTION files" — which is the
// shape #1504 actually recorded: internal/api/prom hand-mirrored a 3-kind
// derived-shape classifier while internal/chsql covered 5 for the same
// question, with a docstring on each asserting they agreed. That one was
// closed by unifying on chplan.IsDerivedShape, but by unification, not by
// a gate, so nothing stopped it coming back.
//
// Two rules run here, one per shape the production mirror takes.
//
//	Rule C — TWO CLASSIFIERS, ONE NAME. A bool-returning function that
//	answers a question about an IR node by switching on the node's
//	dynamic type is a classification. Two of them over the same sealed
//	interface, in different packages, under the same name, are one
//	question with two answers, and the answers drift: the copy is made
//	by copying, so it starts identical and then only one side learns
//	about a new node kind.
//
//	Rule D — TWO VOCABULARIES, ONE DISPATCH. Where a head gates on the
//	set of names a dispatch accepts, the gate and the dispatch must be
//	re-derived from each other rather than both hand-written. The
//	emitter's arms bind to unexported methods, so the arm set is not a
//	value any test can read — it is derived from source
//	(internal/dispatchscan) and diffed against the vocabulary the head
//	reads.
//
// SCOPE, stated plainly. Rule C matches on the NAME, not on the arm
// sets, and that is a deliberate narrowing rather than the whole target.
// The arm-set formulations were measured against this tree first: "two
// classifiers over one interface whose arms differ" and "…whose arms are
// in a subset relation" flag 18 pairs today, essentially all of them
// legitimate — chsql.isCheapPredicate and traceql.isNumericExpr share an
// arm set the way any two predicates over one IR do, and a gate that
// fires on those would be a gate nobody can keep green. Name identity
// flagged exactly one pair, and that pair was a real mirror whose
// docstring said so ("Mirrors prom's isMatrixRangeWindow"). The rule
// therefore catches the copy that keeps its name, which is what a copy
// does; it does not claim to catch a mirror written from scratch under a
// different name. Its remedy is the honest pair: collapse the two onto
// one classifier the way IsDerivedShape was, or — when they answer
// genuinely different questions, as the PromQL and LogQL matrix walks
// turned out to — name them for the questions they answer, so the next
// reader is not invited to fix one by copying the other.

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/dispatchscan"
	"github.com/tsouza/cerberus/internal/sealedscan"
)

// internalRoot is the tree Rule C walks. Production dispatch over the
// shared IR lives under internal/; cmd/ wires it together and declares no
// classifiers.
const internalRoot = repoRoot + "/internal"

// rangeWindowEmitDir and rangeWindowEmitFunc name the dispatch Rule D
// re-derives: the SQL emitter's range-window switch, whose arms are the
// reducer names the windowed-array emitters handle.
const (
	rangeWindowEmitDir  = repoRoot + "/internal/chsql"
	rangeWindowEmitFunc = "emitRangeWindow"
)

// TestProductionDispatchIsNotMirrored is Rule C over the live tree.
func TestProductionDispatchIsNotMirrored(t *testing.T) {
	t.Parallel()

	dirs := goPackageDirs(t, internalRoot)
	sealed := sealedInterfaces(t, dirs)
	if len(sealed) == 0 {
		t.Fatal("found no sealed marker interfaces under internal/ — the scan lost its " +
			"grip on the source shape, so this ratchet is vacuous")
	}

	var all []dispatchscan.Classifier
	for _, dir := range dirs {
		found, err := dispatchscan.Classifiers(dir, sealed)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		all = append(all, found...)
	}
	if len(all) == 0 {
		t.Fatal("found no bool classifiers over any sealed interface — the scan lost its " +
			"grip on the source shape, so this ratchet is vacuous")
	}

	for _, v := range mirroredClassifiers(all) {
		t.Errorf("%s:%d: %s", v.path, v.line, v.msg)
	}
}

// TestRangeWindowVocabularyIsDerived is Rule D over the live tree.
//
// The PromQL head gates whether a function accepts a subquery argument on
// chplan's range-window vocabulary. That gate was a hand-written
// `rangeVectorFn` set in internal/promql, pointing at the emitter's
// switch and asserting it agreed with it; the emitter's switch pointed
// back. Neither could see the other, so the vocabulary was two facts.
func TestRangeWindowVocabularyIsDerived(t *testing.T) {
	t.Parallel()

	emitted, err := dispatchscan.SwitchArms(rangeWindowEmitDir, rangeWindowEmitFunc)
	if err != nil {
		t.Fatalf("derive the emitter's range-window arms: %v", err)
	}
	vocabulary := chplan.RangeWindowFuncNames()
	if len(vocabulary) == 0 {
		t.Fatal("chplan.RangeWindowFuncNames is empty — the ratchet would pass while " +
			"asserting nothing")
	}

	inVocabulary := make(map[string]bool, len(vocabulary))
	for _, name := range vocabulary {
		inVocabulary[name] = true
	}
	emits := make(map[string]bool, len(emitted))
	for _, name := range emitted {
		emits[name] = true
	}
	for _, name := range vocabulary {
		if !emits[name] {
			t.Errorf("chplan's range-window vocabulary names %q but %s has no arm for it: "+
				"a head lowers a plan carrying it and the emit then fails with ErrUnsupported "+
				"instead of the query being rejected at lowering", name, rangeWindowEmitFunc)
		}
	}
	for _, name := range emitted {
		if !inVocabulary[name] {
			t.Errorf("%s emits range function %q but chplan's vocabulary omits it: no head "+
				"can build a plan that reaches the arm, and the PromQL subquery gate — which "+
				"derives from that vocabulary — rejects a query the SQL could have served",
				rangeWindowEmitFunc, name)
		}
	}
}

// mirroredClassifiers implements Rule C: classifiers over one sealed
// interface, sharing a name, declared in more than one package.
//
// The name is compared case-insensitively so that an exported copy of an
// unexported classifier is not a way around it.
//
// It RETURNS findings rather than reporting through *testing.T for the
// same reason the sealed-marker rules do: a scan-and-assert-nothing gate
// whose detector has no positive test goes green either because the tree
// is clean or because the detector broke, and nothing distinguishes the
// two. TestMirroredDispatchRules_FireOnTheDefectShapes drives it against
// fixtures that hold the defect.
func mirroredClassifiers(all []dispatchscan.Classifier) []finding {
	type key struct{ iface, name string }
	groups := map[key][]dispatchscan.Classifier{}
	for _, c := range all {
		k := key{iface: c.Interface, name: strings.ToLower(c.Func)}
		groups[k] = append(groups[k], c)
	}

	var out []finding
	for k, group := range groups {
		pkgs := map[string]bool{}
		for _, c := range group {
			pkgs[c.Package] = true
		}
		if len(pkgs) < 2 {
			continue
		}
		for _, c := range group {
			out = append(out, finding{
				path: c.File,
				line: c.Line,
				msg: fmt.Sprintf(
					"%s.%s classifies %s here, and %s does the same under the same name (%s). "+
						"Two classifiers over one sealed interface are one question with two "+
						"answers: collapse them onto a single classifier in the interface's own "+
						"package the way chplan.IsDerivedShape was, or — if they answer "+
						"different questions — name them for the questions they answer so the "+
						"next reader does not fix one by copying the other",
					c.Package, c.Func, k.iface, strings.Join(otherPackages(group, c.Package), ", "),
					k.name,
				),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].line < out[j].line
	})
	return out
}

// otherPackages names the sorted packages in group other than self.
func otherPackages(group []dispatchscan.Classifier, self string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range group {
		if c.Package == self || seen[c.Package] {
			continue
		}
		seen[c.Package] = true
		out = append(out, c.Package+"."+c.Func)
	}
	sort.Strings(out)
	return out
}

// sealedInterfaces derives the qualified names of every sealed marker
// interface declared under the given directories.
func sealedInterfaces(t *testing.T, dirs []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range dirs {
		markers, err := sealedscan.Markers(dir)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		if len(markers) == 0 {
			continue
		}
		pkg, err := dispatchscan.PackageName(dir)
		if err != nil {
			t.Fatalf("package name for %s: %v", dir, err)
		}
		for _, m := range markers {
			out[dispatchscan.QualifyInterface(pkg, m.Interface)] = true
		}
	}
	return out
}

// TestMirroredDispatchRules_FireOnTheDefectShapes is the guard on the
// guard. Each fixture tree under testdata holds the defect its rule must
// catch alongside the near-misses it must NOT catch, and the expectation
// is the exact set of flagged lines — so a rule that stops detecting, and
// a rule that starts over-detecting, both fail here.
func TestMirroredDispatchRules_FireOnTheDefectShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		dir string
		// positive says the fixture holds at least one line the rule
		// must flag. Without it a fixture whose classifiers were all
		// deleted would expect nothing, match a rule that detects
		// nothing, and pass — the dead-rule case this test prevents.
		positive bool
	}{
		// One classifier copied into a second package under its own
		// name, having already drifted by an arm — beside the local
		// helper that shares the name but not the interface.
		{dir: "mirrored", positive: true},
		// The same two packages classifying the same interface under
		// names that state their different questions, plus a same-named
		// pair that returns a node rather than a bool (an unwrapper,
		// whose arms legitimately differ per caller) and a same-named
		// pair inside ONE package.
		{dir: "distinct"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			t.Parallel()

			root := filepath.Join("testdata", "mirroreddispatch", tc.dir)
			dirs := goPackageDirs(t, root)
			if len(dirs) < 2 {
				t.Fatalf("fixture %s holds %d package directories, want at least 2 — a "+
					"cross-package rule cannot be exercised by one package", root, len(dirs))
			}
			sealed := sealedInterfaces(t, dirs)
			if len(sealed) == 0 {
				t.Fatalf("fixture %s declares no sealed interface, so there is nothing to "+
					"classify", root)
			}

			var all []dispatchscan.Classifier
			for _, dir := range dirs {
				found, err := dispatchscan.Classifiers(dir, sealed)
				if err != nil {
					t.Fatalf("scan %s: %v", dir, err)
				}
				all = append(all, found...)
			}
			if len(all) == 0 {
				t.Fatalf("fixture %s holds no classifiers, so the rule has nothing to "+
					"read", root)
			}

			gotLines := []int{}
			for _, v := range mirroredClassifiers(all) {
				gotLines = append(gotLines, v.line)
			}
			sort.Ints(gotLines)
			want := markedSourceLines(t, root)
			if tc.positive != (len(want) > 0) {
				t.Fatalf("fixture %s marks %d lines with %s, but positive=%v — the fixture "+
					"no longer states the case it is named for", root, len(want), wantMarker,
					tc.positive)
			}
			if !sameInts(gotLines, want) {
				t.Errorf("fixture %s: rule flagged lines %v, want %v (the lines marked %s)",
					root, gotLines, want, wantMarker)
			}
		})
	}
}

// TestDispatchScanVacuityGuards pins that the derivations fail loudly
// rather than returning an empty answer when they lose their grip on the
// source shape. A ratchet whose scan silently finds nothing passes
// forever while asserting nothing, which is the defect this whole file
// exists to prevent — so the guards themselves need a test.
func TestDispatchScanVacuityGuards(t *testing.T) {
	t.Parallel()

	if _, err := dispatchscan.SwitchArms(rangeWindowEmitDir, "noSuchDispatchFunction"); err == nil {
		t.Error("SwitchArms found no such function and returned no error — a ratchet built " +
			"on it would derive an empty vocabulary and pass while asserting nothing")
	}
	// A function with no expression switch at all: the derivation must
	// refuse rather than report an empty arm set.
	if _, err := dispatchscan.SwitchArms(repoRoot+"/internal/chplan", "RangeWindowFuncNames"); err == nil {
		t.Error("SwitchArms accepted a function holding no expression switch and returned " +
			"no error — the dispatch it is aimed at could be reshaped away unnoticed")
	}
}

// markedSourceLines returns the sorted fixture lines marked with
// wantMarker across a fixture TREE's non-test Go files.
//
// It is the production-shaped sibling of wantMarkedLines, which reads
// `_test.go` files in one directory: these fixtures are production
// dispatch spread over several packages, and dispatchscan deliberately
// ignores test files.
func markedSourceLines(t *testing.T, root string) []int {
	t.Helper()
	lines := []int{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.TrimSpace(c.Text) == wantMarker {
					lines = append(lines, fset.Position(c.Pos()).Line)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Ints(lines)
	return lines
}
