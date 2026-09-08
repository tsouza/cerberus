//go:build chdb

// Coverage ledger for TestPropertyOptimizerSemanticEquivalence.
//
// The property is only as good as the plans it draws, and its original
// grammar drew three node kinds. A generator that silently stops
// covering a kind looks exactly like a generator that never covered it,
// so this file derives the roster the property owes coverage for from
// the source rather than restating it by hand, and fails when a kind on
// that roster is neither round-tripped nor accounted for.
//
// The roster is not "every kind in chplan". A kind no optimizer rule
// can see passes through the driver untouched, so generating one would
// only re-test the tree walk — coverage theatre that makes the ledger
// long and its failures uninformative. The roster is instead every
// chplan node kind internal/optimizer's own non-test sources name: those
// are the kinds a rule can match, rewrite, or reason about, and
// therefore the kinds where a rule can change an answer. Both halves are
// parsed out of the tree, so adding a rule that touches a new kind, or
// adding a kind to chplan and wiring a rule to it, shows up here as a
// gap rather than as silence.
package optimizer_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// uncoveredOptimizerKinds explains, per kind, why the generator does not
// produce it. An entry is required to be a kind the roster contains and
// one the property does NOT round-trip, so a reason cannot outlive the
// gap it describes.
//
// These are reasons, not permissions: each names the concrete thing that
// would have to exist for the kind to be generated, so the next person
// to widen the generator knows what the work is rather than having to
// rediscover it.
var uncoveredOptimizerKinds = map[string]string{
	"NaryVectorSetOp": "produced by FlattenVectorSetOp, never by a lowering, so it is not an " +
		"input shape a generator can draw. It IS round-tripped, as the optimized side of the " +
		"VectorSetOp chains generateSetOpChain draws — which is the only way a plan ever " +
		"contains one.",
	"NestedSetAnnotate": "reached only by RequireScanResourceBound, a verify-only analyzer that " +
		"panics on a broken lowering invariant rather than rewriting anything. There is no " +
		"rewrite for a round-trip to disagree about, and the invariant it checks is pinned " +
		"directly by test/spec's traces resource-bound lane.",
	"StructuralJoin": "a TraceQL span-to-span join over the otel_traces layout — parent/child " +
		"span IDs, trace IDs and a nested-set window. This harness seeds the OTel metrics " +
		"layouts only, so generating one needs a spans seed table first.",
	"TopK": "its ranking is not a function of the row set alone once values tie: the optimized " +
		"plan is a different query, so ClickHouse may legitimately break a tie the other way " +
		"and the property would flake rather than report. Generating it needs a seed whose " +
		"values are distinct within every partition the generator can draw.",
	"VectorJoin": "an arithmetic vector-vector join, which needs two operands matched on series " +
		"identity AND a cardinality modifier that makes the match well-defined. The gauge seed " +
		"has one series per (MetricName, host) pair, so a generated join would be one-to-one " +
		"only by accident; it needs a seed built for the join rather than for the leaf grammar.",
}

// assertOptimizerKindsCovered is called at the end of the property run
// with the kinds it actually round-tripped.
func assertOptimizerKindsCovered(t *testing.T, verified map[string]bool) {
	t.Helper()

	roster := optimizerNodeKindRoster(t)
	if len(roster) == 0 {
		t.Fatal("derived an empty roster of optimizer-visible chplan kinds — the scan lost its " +
			"grip on the source shape, so this ledger is vacuous")
	}

	var missing []string
	for kind := range roster {
		if verified[kind] {
			continue
		}
		if reason := uncoveredOptimizerKinds[kind]; reason != "" {
			continue
		}
		missing = append(missing, kind)
	}
	sort.Strings(missing)
	for _, kind := range missing {
		t.Errorf("internal/optimizer names chplan.%s, so a rule can act on it, but the property "+
			"never round-tripped one: generate it in property_stage_gen_test.go, or add an entry "+
			"to uncoveredOptimizerKinds saying what would have to exist first", kind)
	}

	for kind, reason := range uncoveredOptimizerKinds {
		switch {
		case reason == "":
			t.Errorf("uncoveredOptimizerKinds[%q] has an empty reason; an unexplained gap is the "+
				"thing this ledger exists to prevent", kind)
		case !roster[kind]:
			t.Errorf("uncoveredOptimizerKinds names chplan.%s, which internal/optimizer no longer "+
				"references — drop the entry", kind)
		case verified[kind]:
			t.Errorf("uncoveredOptimizerKinds says chplan.%s cannot be generated, but the property "+
				"round-tripped one — drop the entry, the gap it describes is closed", kind)
		}
	}
}

// recordVerifiedKinds walks a plan and records every node kind in it.
func recordVerifiedKinds(into map[string]bool, n chplan.Node) {
	if n == nil {
		return
	}
	into[reflect.TypeOf(n).Elem().Name()] = true
	for _, c := range n.Children() {
		recordVerifiedKinds(into, c)
	}
}

// optimizerNodeKindRoster returns the chplan node kinds internal/optimizer's
// non-test sources name — the intersection of "declares planNode()" and
// "written as chplan.X somewhere a rule can reach".
func optimizerNodeKindRoster(t *testing.T) map[string]bool {
	t.Helper()

	declared := declaredPlanNodeKinds(t, filepath.Join("..", "chplan"))
	referenced := referencedChplanNames(t, ".")

	roster := map[string]bool{}
	for name := range declared {
		if referenced[name] {
			roster[name] = true
		}
	}
	return roster
}

// declaredPlanNodeKinds returns the name of every type in dir declaring
// the chplan.Node marker method.
func declaredPlanNodeKinds(t *testing.T, dir string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	forEachGoFile(t, dir, func(f *ast.File) {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "planNode" || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
	})
	if len(out) == 0 {
		t.Fatalf("found no type declaring planNode() under %s — the scan lost its grip on the "+
			"source shape, so this ledger is vacuous", dir)
	}
	return out
}

// referencedChplanNames returns every `chplan.X` selector written in
// dir's non-test Go sources.
func referencedChplanNames(t *testing.T, dir string) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	forEachGoFile(t, dir, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "chplan" {
				out[sel.Sel.Name] = true
			}
			return true
		})
	})
	if len(out) == 0 {
		t.Fatalf("found no chplan.X reference under %s — the scan lost its grip on the source "+
			"shape, so this ledger is vacuous", dir)
	}
	return out
}

func forEachGoFile(t *testing.T, dir string, visit func(*ast.File)) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		visit(f)
	}
}
