package rejectionparity

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	traceqlast "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

const (
	repoRoot = "../.."
	// catalogueDir is the shard directory (relative to this package's
	// directory), one shard per source file — see catalogue.go's
	// shardPathSeparator doc comment for the naming rule.
	catalogueDir = "catalogue"
)

// TestCatalogueIsRegenerable rescans the three lowering packages and
// diffs the regenerated shards byte-for-byte against the checked-in
// directory — content drift, a missing shard, and a lingering shard
// whose source file lost its last catalogued site are all failures.
// Set CERBERUS_UPDATE_INVENTORY=1 to rewrite the artifact (the same
// update-via-env convention as test/inventory). Regeneration preserves
// the curated fields of surviving sites; new sites land unclassified
// (and then fail TestCatalogueEntriesAreClassified until curated),
// removed sites drop out. Both directions are deliberate, reviewable
// diffs — a rejection site can neither appear nor vanish without the
// catalogue (and therefore the parity corpus) moving in lock-step.
func TestCatalogueIsRegenerable(t *testing.T) {
	t.Parallel()

	prev, err := LoadCatalogue(catalogueDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("load %s: %v", catalogueDir, err)
	}
	cat, err := Generate(repoRoot, prev)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if os.Getenv("CERBERUS_UPDATE_INVENTORY") != "" {
		if err := WriteCatalogue(catalogueDir, cat); err != nil {
			t.Fatalf("write %s: %v", catalogueDir, err)
		}
		t.Logf("rewrote %s (%d entries)", catalogueDir, len(cat.Entries))
		return
	}

	want, err := ShardCatalogue(cat)
	if err != nil {
		t.Fatalf("ShardCatalogue: %v", err)
	}
	diffs, err := DiffShards(catalogueDir, want)
	if err != nil {
		t.Fatalf("DiffShards(%s) (rerun with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", catalogueDir, err)
	}
	if len(diffs) > 0 {
		t.Fatalf("%s is stale relative to the lowering sources — rerun with "+
			"CERBERUS_UPDATE_INVENTORY=1, curate any new entries, and commit the diff.\n%s",
			catalogueDir, strings.Join(diffs, "\n"))
	}
}

// TestShardNamesRoundTrip pins the shard-naming rule: every source file
// the scan reaches maps to a shard name that maps back to the same
// path, and the mapping refuses a path it could not represent
// reversibly. Injectivity is the load-bearing half — two source files
// sharing a shard would let one file's regeneration silently delete the
// other's entries.
func TestShardNamesRoundTrip(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	owners := map[string]string{}
	for _, e := range cat.Entries {
		src, err := siteSourceFile(e.Site.Site)
		if err != nil {
			t.Fatalf("siteSourceFile(%q): %v", e.Site.Site, err)
		}
		name, err := shardName(src)
		if err != nil {
			t.Fatalf("shardName(%q): %v", src, err)
		}
		if prev, ok := owners[name]; ok && prev != src {
			t.Fatalf("shard %s is claimed by both %s and %s — the mapping is not injective, so one "+
				"source file's regeneration would drop the other's entries", name, prev, src)
		}
		owners[name] = src
		back, err := shardSourcePath(name)
		if err != nil {
			t.Fatalf("shardSourcePath(%q): %v", name, err)
		}
		if back != src {
			t.Fatalf("shard %s reverses to %s, want %s", name, back, src)
		}
	}
	if len(owners) == 0 {
		t.Fatal("no shard names derived — the catalogue lost its entries")
	}
}

// TestShardNameRejectsAmbiguousPaths proves the injectivity guard is
// not hollow: a path component carrying the separator, or an empty
// component, is refused rather than aliased onto another file's shard.
func TestShardNameRejectsAmbiguousPaths(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"internal/promql/sub" + shardPathSeparator + "query.go",
		"internal//lower.go",
		"",
	} {
		if name, err := shardName(bad); err == nil {
			t.Errorf("shardName(%q) = %q, want an error — the mapping cannot represent it reversibly", bad, name)
		}
	}
	if _, err := shardSourcePath("internal__promql__lower.go"); err == nil {
		t.Error("shardSourcePath accepted a name with no .json suffix, which is not a shard file")
	}
	if _, err := siteSourceFile("no-colon-here"); err == nil {
		t.Error("siteSourceFile accepted a key with no source-path prefix")
	}
}

// TestWriteCataloguePrunesEmptiedShards is the pruning pin: when the
// last catalogued site in a source file goes away, its shard must be
// REMOVED, not left on disk. A lingering shard is a silent correctness
// hole — LoadCatalogue merges whatever it finds, so the deleted site
// would go on being asserted against the reference backend while
// nothing in the tree still constructs it.
func TestWriteCataloguePrunesEmptiedShards(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	before := &Catalogue{Entries: []Entry{
		{Site: Site{Head: "promql", Site: "internal/promql/a.go:fnA#0000aaaa", Message: "promql: a"}, Class: ClassInternal, Rationale: "unit-test fixture"},
		{Site: Site{Head: "promql", Site: "internal/promql/b.go:fnB#0000bbbb", Message: "promql: b"}, Class: ClassInternal, Rationale: "unit-test fixture"},
	}}
	if err := WriteCatalogue(dir, before); err != nil {
		t.Fatalf("WriteCatalogue(before): %v", err)
	}
	gone := filepath.Join(dir, "internal__promql__b.go.json")
	if _, err := os.Stat(gone); err != nil {
		t.Fatalf("shard for internal/promql/b.go was not written: %v", err)
	}

	after := &Catalogue{Entries: before.Entries[:1]}
	if err := WriteCatalogue(dir, after); err != nil {
		t.Fatalf("WriteCatalogue(after): %v", err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want a not-exist error — a shard whose source file lost its last "+
			"catalogued site must be pruned, or LoadCatalogue keeps merging entries no lowering "+
			"constructs any more", gone, err)
	}

	reloaded, err := LoadCatalogue(dir)
	if err != nil {
		t.Fatalf("LoadCatalogue: %v", err)
	}
	if len(reloaded.Entries) != len(after.Entries) {
		t.Fatalf("reloaded %d entries, want %d — the pruned shard is still contributing", len(reloaded.Entries), len(after.Entries))
	}

	// The same emptied-shard state must be a FAILURE for the
	// regenerate-and-diff test, not merely repaired by regeneration.
	if err := os.WriteFile(gone, []byte(`{"entries":[]}`+"\n"), shardFileMode); err != nil {
		t.Fatalf("re-plant stale shard: %v", err)
	}
	want, err := ShardCatalogue(after)
	if err != nil {
		t.Fatalf("ShardCatalogue: %v", err)
	}
	diffs, err := DiffShards(dir, want)
	if err != nil {
		t.Fatalf("DiffShards: %v", err)
	}
	if len(diffs) != 1 || !strings.Contains(diffs[0], "stale shard") {
		t.Fatalf("DiffShards reported %v, want exactly one stale-shard difference", diffs)
	}
}

// TestLoadCatalogueMergesShardsInSiteOrder pins the invariant every
// downstream consumer rests on: the merged value is sorted by site key
// regardless of which shard each entry came from, so BuildCases and the
// classification gate see one flat, ordered catalogue exactly as they
// did when the artifact was a single file.
func TestLoadCatalogueMergesShardsInSiteOrder(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	for i := 1; i < len(cat.Entries); i++ {
		if cat.Entries[i].Site.Site <= cat.Entries[i-1].Site.Site {
			t.Fatalf("entries out of site order at index %d: %q then %q",
				i, cat.Entries[i-1].Site.Site, cat.Entries[i].Site.Site)
		}
	}
	shards, err := listShards(catalogueDir)
	if err != nil {
		t.Fatalf("listShards: %v", err)
	}
	if len(shards) < 2 {
		t.Fatalf("found %d shard(s) in %s — the merge path is not being exercised", len(shards), catalogueDir)
	}
}

// TestCatalogueEntriesAreClassified is the curation gate: every
// scanned site must be classified, rejection/divergence entries must
// carry a trigger query + valid endpoint (divergence additionally
// requires a tracking issue), and internal entries must justify
// themselves with a rationale. An unclassified entry (the regen
// default for a new rejection site) fails here — a new deliberate
// rejection cannot land without a parity-corpus case.
func TestCatalogueEntriesAreClassified(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	seen := map[string]bool{}
	for _, e := range cat.Entries {
		if seen[e.Site.Site] {
			t.Errorf("entry %s is listed twice", e.Site.Site)
		}
		seen[e.Site.Site] = true
		switch e.Class {
		case ClassRejection:
			if strings.TrimSpace(e.TriggerQuery) == "" {
				t.Errorf("rejection entry %s has no trigger query", e.Site.Site)
			}
			ep := e.Endpoint
			if ep == "" {
				ep = DefaultEndpoint(e.Head)
			}
			if !ValidEndpoint(e.Head, ep) {
				t.Errorf("rejection entry %s: endpoint %q invalid for head %s", e.Site.Site, ep, e.Head)
			}
			if e.Rationale != "" {
				t.Errorf("rejection entry %s carries a rationale — rationales belong to internal entries; a rejection justifies itself via its trigger query", e.Site.Site)
			}
			if e.TrackingIssue != 0 {
				t.Errorf("rejection entry %s carries a tracking issue — tracking issues belong to divergence entries", e.Site.Site)
			}
			if e.Since != "" {
				t.Errorf("rejection entry %s carries a since date — since dates belong to divergence entries", e.Site.Site)
			}
		case ClassInternal:
			if e.Evidence == nil {
				t.Errorf("internal entry %s carries no evidence block — a rationale with nothing derived beneath it is an unchecked assertion; regenerate with CERBERUS_UPDATE_INVENTORY=1", e.Site.Site)
			}
			if strings.TrimSpace(e.Rationale) == "" {
				t.Errorf("internal entry %s carries no rationale — every internal classification must justify why the site is not wire-reachable", e.Site.Site)
			}
			if e.TriggerQuery != "" || e.Endpoint != "" {
				t.Errorf("internal entry %s carries trigger query / endpoint — those belong to rejection entries", e.Site.Site)
			}
			if e.TrackingIssue != 0 {
				t.Errorf("internal entry %s carries a tracking issue — tracking issues belong to divergence entries", e.Site.Site)
			}
			if e.Since != "" {
				t.Errorf("internal entry %s carries a since date — since dates belong to divergence entries", e.Site.Site)
			}
		case ClassDivergence:
			if strings.TrimSpace(e.TriggerQuery) == "" {
				t.Errorf("divergence entry %s has no trigger query", e.Site.Site)
			}
			ep := e.Endpoint
			if ep == "" {
				ep = DefaultEndpoint(e.Head)
			}
			if !ValidEndpoint(e.Head, ep) {
				t.Errorf("divergence entry %s: endpoint %q invalid for head %s", e.Site.Site, ep, e.Head)
			}
			if e.Rationale != "" {
				t.Errorf("divergence entry %s carries a rationale — rationales belong to internal entries; a divergence justifies itself via its trigger query + tracking issue", e.Site.Site)
			}
			if e.TrackingIssue <= 0 {
				t.Errorf("divergence entry %s has no tracking issue — every divergence must cite an open issue tracking closure", e.Site.Site)
			}
			if _, err := time.Parse(divergenceDateLayout, e.Since); err != nil {
				t.Errorf("divergence entry %s has an invalid since date %q (want %s): %v — every divergence must record when it was first classified, so the age cap can measure it", e.Site.Site, e.Since, divergenceDateLayout, err)
			}
		default:
			t.Errorf("entry %s is unclassified (class=%q) — classify as rejection (with trigger query), internal (with rationale), or divergence (with trigger query + tracking issue)", e.Site.Site, e.Class)
		}
	}
}

// TestInternalRationalesRestOnCurrentEvidence is the drift gate for the
// 161 class=internal rationales. A rationale is prose asserting that no
// wire query reaches a site; the assertion always rests on the site's
// guard chain, on which functions can reach the one that constructs the
// error, or on a sibling dispatch that intercepts first. Those three
// facts are re-derived from the current lowering source here and
// compared against the evidence stored beside the rationale, so a
// change that moves any of them fails NAMING the site instead of
// leaving the claim standing unexamined — which is what #1738 was: three
// lowerHoltWinters rationales asserting a gate that did not exist, with
// nothing in the tree able to tell.
//
// The fix for a failure here is never to regenerate and move on: the
// same regeneration DEMOTES the entry to unclassified and drops its
// rationale (see reclassifyOnEvidenceDrift), so
// TestCatalogueEntriesAreClassified fails next and the claim has to be
// re-made against the source as it now stands — or the entry
// reclassified as the wire-reachable rejection it has become.
func TestInternalRationalesRestOnCurrentEvidence(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	scan, err := scanLowerings(repoRoot)
	if err != nil {
		t.Fatalf("scan lowerings: %v", err)
	}
	checked := 0
	for _, e := range cat.Entries {
		derived, ok := scan.evidence[e.Site.Site]
		if !ok {
			t.Errorf("site %s is catalogued but the scan no longer finds it — regenerate with CERBERUS_UPDATE_INVENTORY=1", e.Site.Site)
			continue
		}
		if e.Evidence.Equal(derived) {
			checked++
			continue
		}
		if e.Class == ClassInternal {
			t.Errorf("site %s: the source facts its rationale rests on have moved.\n  rationale: %s\n  %s\n"+
				"Re-verify the claim against the site as it now stands, then regenerate with "+
				"CERBERUS_UPDATE_INVENTORY=1 and re-state the rationale (regeneration clears it).",
				e.Site.Site, e.Rationale, strings.Join(e.Evidence.Diff(derived), "\n  "))
			continue
		}
		t.Errorf("site %s (class %s): stored evidence is stale.\n  %s\nRegenerate with CERBERUS_UPDATE_INVENTORY=1.",
			e.Site.Site, e.Class, strings.Join(e.Evidence.Diff(derived), "\n  "))
	}
	if checked == 0 {
		t.Fatal("no entry's evidence was re-derived — the gate is inert")
	}
}

// TestEvidenceDriftDemotesInternalEntries proves the demotion is real
// rather than a comment: an internal entry whose evidence moved comes
// out of regeneration unclassified and rationale-less (so the
// classification gate fails on it by name), an entry whose evidence is
// unchanged is untouched, and a rejection entry — which makes no
// unreachability claim and is re-proved every run by its trigger query
// — keeps its curation regardless.
func TestEvidenceDriftDemotesInternalEntries(t *testing.T) {
	t.Parallel()

	derived := &Evidence{Guard: []string{"if len(c.Args) != 2"}, Callers: []string{"lowerCall"}}
	for _, tc := range []struct {
		name          string
		class         string
		prev          *Evidence
		wantClass     string
		wantRationale string
	}{
		{"internal, guard moved", ClassInternal, &Evidence{Guard: []string{"if len(c.Args) != 3"}, Callers: []string{"lowerCall"}}, "", ""},
		{"internal, new caller", ClassInternal, &Evidence{Guard: []string{"if len(c.Args) != 2"}, Callers: []string{"lowerCall", "threadOuterRangeFnScalars"}}, "", ""},
		{"internal, dispatch moved", ClassInternal, &Evidence{Guard: []string{"if len(c.Args) != 2"}, Callers: []string{"lowerCall"}, CallerDispatch: []string{"lowerCountValues"}}, "", ""},
		{"internal, no stored evidence", ClassInternal, nil, "", ""},
		{"internal, unchanged", ClassInternal, &Evidence{Guard: []string{"if len(c.Args) != 2"}, Callers: []string{"lowerCall"}}, ClassInternal, "because"},
		{"rejection, guard moved", ClassRejection, &Evidence{Guard: []string{"if len(c.Args) != 3"}}, ClassRejection, "because"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := Entry{Class: tc.class, Rationale: "because", Evidence: derived}
			reclassifyOnEvidenceDrift(&e, tc.prev)
			if e.Class != tc.wantClass {
				t.Errorf("class = %q, want %q", e.Class, tc.wantClass)
			}
			if e.Rationale != tc.wantRationale {
				t.Errorf("rationale = %q, want %q", e.Rationale, tc.wantRationale)
			}
		})
	}
}

// TestGuardChainRecordsFallThroughArmInventory pins the derivation
// shape the largest rationale family depends on. "Every op the grammar
// produces is mapped, so the default arm is unreachable" is a claim
// about the SIBLING arms of a switch, not about the error site, and the
// site itself carries no lexical condition at all when the mapper is
// written as a switch of returning cases followed by a trailing
// `return fmt.Errorf(...)`. Both shapes must put the arm inventory in
// the guard chain, or deleting a case would make the site reachable
// while the evidence — and therefore the rationale — sat still.
func TestGuardChainRecordsFallThroughArmInventory(t *testing.T) {
	t.Parallel()

	const src = `package p

import "fmt"

func mapOp(op string) (int, error) {
	switch op {
	case "add":
		return 1, nil
	case "sub":
		return 2, nil
	}
	return 0, fmt.Errorf("promql: unknown op %s", op)
}

func mapKind(k any) (int, error) {
	switch v := k.(type) {
	case int:
		return v, nil
	default:
		return 0, fmt.Errorf("promql: unsupported kind %T", k)
	}
}

func unwrap(v any) (int, error) {
	if i, ok := v.(int); ok {
		return i, nil
	}
	return 0, fmt.Errorf("promql: not an int %T", v)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	declared, calls, callers := callGraph([]*ast.File{f})
	sc := &astScope{fset: fset, declared: declared, calls: calls, callers: callers}

	got := map[string][]string{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		_, ev := scanFunc(sc, fn, "internal/promql/p.go", "promql", "promql: ")
		for _, e := range ev {
			got[fn.Name.Name] = e.Guard
		}
	}

	want := map[string][]string{
		"mapOp":   {`past exhaustive switch op over [case "add"; case "sub"]`},
		"mapKind": {`default of switch v := k.(type) over [case int; default]`},
		"unwrap":  {`past returning if i, ok := v.(int); ok`},
	}
	for fn, wantGuard := range want {
		if !equalStrings(got[fn], wantGuard) {
			t.Errorf("%s guard chain = %q, want %q", fn, got[fn], wantGuard)
		}
	}
}

// TestRejectionTriggersExerciseSites is the centrepiece pin: for every
// class=rejection or class=divergence entry, the trigger query (a)
// parses with the head's reference parser — proving the rejection is
// semantic, not a parse error — and (b) fails the head's lowering with
// an error matching the site's message — proving the trigger actually
// exercises the catalogued site, so the parity corpus diffs the claim
// the site makes and not some other failure. Divergence entries share
// this reachability proof with rejection entries — the two classes
// only disagree about what the REFERENCE backend does with the same
// trigger, never about whether cerberus itself rejects it.
func TestRejectionTriggersExerciseSites(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	for _, e := range cat.Entries {
		if e.Class != ClassRejection && e.Class != ClassDivergence {
			continue
		}
		e := e
		t.Run(e.Site.Site, func(t *testing.T) {
			t.Parallel()
			errStr := lowerTrigger(t, e)
			if !ErrorMatchesMessage(errStr, e.Message) {
				t.Fatalf("trigger %q failed lowering with %q, which does not match the catalogued message %q — the trigger exercises a different site",
					e.TriggerQuery, errStr, e.Message)
			}
		})
	}
}

// TestParityCorpusMatchesCatalogue pins the third leg of the ratchet:
// the parity-corpus case set the compat driver runs is derived 1:1
// from the rejection + divergence entries — same count, same site
// keys, same class tags — for every head. BuildCases is the same
// function the driver binary calls, so the corpus cannot drift from
// the catalogue, and a divergence entry cannot be silently dropped
// from the set that gets checked.
func TestParityCorpusMatchesCatalogue(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	for _, head := range []string{"promql", "logql", "traceql"} {
		cases, err := BuildCases(cat, head)
		if err != nil {
			t.Fatalf("BuildCases(%s): %v", head, err)
		}
		var wantNames, wantClasses []string
		for _, e := range cat.Entries {
			if e.Head == head && (e.Class == ClassRejection || e.Class == ClassDivergence) {
				wantNames = append(wantNames, e.Site.Site)
				wantClasses = append(wantClasses, e.Class)
			}
		}
		if len(cases) != len(wantNames) {
			t.Fatalf("head %s: %d parity cases for %d rejection/divergence entries", head, len(cases), len(wantNames))
		}
		for i, c := range cases {
			if c.Name != wantNames[i] {
				t.Fatalf("head %s: case %d is %s, want %s", head, i, c.Name, wantNames[i])
			}
			if c.Class != wantClasses[i] {
				t.Fatalf("head %s: case %d (%s) has class %s, want %s", head, i, c.Name, c.Class, wantClasses[i])
			}
		}
		if len(cases) == 0 {
			t.Fatalf("head %s: zero parity cases — the catalogue lost its rejection/divergence entries", head)
		}
	}
}

// lowerTrigger parses + lowers one trigger query through the same
// entrypoints the HTTP handlers use and returns the lowering error
// string. Parse failures and lowering successes are both fatal — a
// trigger must be a valid-language query that the lowering rejects.
func lowerTrigger(t *testing.T, e Entry) string {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	switch e.Head {
	case "promql":
		p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
		expr, err := p.ParseExpr(e.TriggerQuery)
		if err != nil {
			t.Fatalf("trigger %q does not parse as PromQL: %v", e.TriggerQuery, err)
		}
		// Instant query shape: the prom Lang adapter always lowers via
		// LowerAtRange; /api/v1/query leaves Step at zero.
		_, lerr := promql.LowerAtRange(ctx, expr, schema.DefaultOTelMetrics(), end, end, 0)
		if lerr == nil {
			t.Fatalf("trigger %q lowered successfully — the catalogued rejection is unreachable via this query", e.TriggerQuery)
		}
		return lerr.Error()
	case "logql":
		// ParseExprPermissive is the parse entrypoint the wire path
		// uses (internal/logql/lang.go Parse); mirror it so the
		// exerciser proves wire-level reachability.
		expr, err := logql.ParseExprPermissive(e.TriggerQuery)
		if err != nil {
			t.Fatalf("trigger %q does not parse as LogQL: %v", e.TriggerQuery, err)
		}
		_, lerr := logql.LowerAtRange(ctx, expr, schema.DefaultOTelLogs(), start, end, 30*time.Second)
		if lerr == nil {
			t.Fatalf("trigger %q lowered successfully — the catalogued rejection is unreachable via this query", e.TriggerQuery)
		}
		return lerr.Error()
	case "traceql":
		expr, err := traceqlast.Parse(e.TriggerQuery)
		if err != nil {
			t.Fatalf("trigger %q does not parse as TraceQL: %v", e.TriggerQuery, err)
		}
		_, lerr := traceql.Lower(ctx, expr, schema.DefaultOTelTraces())
		if lerr == nil {
			t.Fatalf("trigger %q lowered successfully — the catalogued rejection is unreachable via this query", e.TriggerQuery)
		}
		return lerr.Error()
	}
	t.Fatalf("entry %s has unknown head %q", e.Site.Site, e.Head)
	return ""
}

func loadCatalogue(t *testing.T) *Catalogue {
	t.Helper()
	cat, err := LoadCatalogue(catalogueDir)
	if err != nil {
		t.Fatalf("load %s (rerun TestCatalogueIsRegenerable with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", catalogueDir, err)
	}
	if len(cat.Entries) == 0 {
		t.Fatalf("%s is empty", catalogueDir)
	}
	return cat
}
