package rejectionparity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// heads maps each lowering package directory (relative to the repo
// root) to its head identifier. The head identifier doubles as the
// required error-message prefix ("promql: ..."), which every error
// site in the three packages carries by convention.
var heads = map[string]string{
	"internal/promql":  "promql",
	"internal/logql":   "logql",
	"internal/traceql": "traceql",
}

// Site is one error-construction site discovered by ScanSites: a
// fmt.Errorf / errors.New call in a lowering package whose format
// string starts with the head prefix.
type Site struct {
	// Head is "promql" / "logql" / "traceql".
	Head string `json:"head"`
	// Site is the stable identifier
	// "internal/<head>/<file>.go:<func>#<hash8>[-<n>]" where <hash8>
	// is the first 8 hex chars of sha256(Message) and <n> is the
	// 1-based ordinal appended only when the same message is
	// constructed more than once inside the same function. Line
	// numbers are deliberately excluded so unrelated edits don't
	// churn the catalogue; hashing the message (rather than counting
	// positionally) keeps keys stable when an unrelated error site is
	// inserted earlier in the function.
	Site string `json:"site"`
	// Message is the raw format string of the error constructor —
	// verbs (%s / %d / %T / %w / ...) included. The exerciser test
	// matches lowering errors against the literal fragments between
	// the verbs (see MessageFragments / ErrorMatchesMessage).
	Message string `json:"message"`
}

// Entry is one catalogued site plus its curated classification.
type Entry struct {
	Site

	// Class is the curated classification:
	//
	//   "rejection"  — reachable from a parseable query: a deliberate
	//                  semantic rejection whose parity against the
	//                  reference backend the compat harnesses verify.
	//                  Requires TriggerQuery (+ Endpoint for traceql
	//                  metrics-pipeline sites); forbids Rationale and
	//                  TrackingIssue.
	//   "internal"   — not reachable from a parseable query through the
	//                  HTTP query endpoints: parser-enforced shapes,
	//                  internal invariants, error-propagation wrappers
	//                  (%w), or paths only reachable via non-wire entry
	//                  points. Requires Rationale; forbids TriggerQuery,
	//                  Endpoint and TrackingIssue.
	//   "divergence" — reachable AND wire-verified to differ from the
	//                  reference backend on purpose: cerberus rejects
	//                  the trigger query, the reference backend answers
	//                  it, and an open GitHub issue tracks closing the
	//                  gap. Requires TriggerQuery (+ Endpoint) exactly
	//                  like "rejection", plus TrackingIssue and Since;
	//                  forbids Rationale. See doc.go for why this is a
	//                  ratchet and not the allow-list the package
	//                  forbids, and for the two mechanisms (a count
	//                  ceiling + an age cap) that keep it that way.
	//
	// The verify test fails on any other value (including ""), so a
	// new rejection site cannot land unclassified.
	Class string `json:"class"`

	// TriggerQuery (class=rejection, class=divergence) is a minimal
	// concrete query that parses with the head's reference parser and
	// fails this site's lowering with this site's message. Pinned by
	// TestRejectionTriggersExerciseSites.
	TriggerQuery string `json:"trigger_query,omitempty"`

	// Endpoint (class=rejection, class=divergence) selects the HTTP
	// endpoint the parity driver sends TriggerQuery to. Empty means
	// the head default (DefaultEndpoint). TraceQL metrics-pipeline
	// rejections set "traceql_metrics" because /api/search does not
	// accept metrics expressions.
	Endpoint string `json:"endpoint,omitempty"`

	// Rationale (class=internal) documents why the site is not a
	// wire-reachable semantic rejection.
	Rationale string `json:"rationale,omitempty"`

	// TrackingIssue (class=divergence) is the number of an open GitHub
	// issue, in this repository, tracking closure of the divergence.
	// The forbid-deferral workflow asserts on every PR/push/merge_group
	// that the issue exists, is open, and is an issue rather than a
	// pull request — the same liveness contract forbid-deferral.mjs
	// already enforces for deferral markers. A closed or missing issue
	// with the entry still present is a failure: the entry must either
	// be deleted (cerberus was fixed) or re-filed against a fresh
	// issue, never left pointing at a closed one.
	TrackingIssue int `json:"tracking_issue,omitempty"`

	// Evidence is the machine-DERIVED reachability evidence for the
	// site: its guard chain and its intra-package reaching set, plus
	// the functions its own rationale names (Cited, which is derived
	// from the prose and never diffed). Generate recomputes it from the
	// go/ast scan on every regeneration and never carries the stored
	// value forward, so it is a fact about the current source rather
	// than a second declaration.
	//
	// It exists to hold class=internal Rationale claims to account.
	// A rationale is prose asserting that no wire query reaches the
	// site; that assertion rests on the guard the site sits behind and
	// on who can call the enclosing function — the two facts Guard and
	// Callers record, both strictly local to the site. When either
	// moves, Generate demotes the entry to unclassified and drops the
	// rationale (reclassifyOnEvidenceDrift), so the claim must be
	// re-made against the new source instead of being inherited. That
	// is the leg #1738 was missing: three lowerHoltWinters rationales
	// asserted a gate that no longer existed, and nothing re-derived
	// the assertion.
	//
	// Claims about a SIBLING interception ("count_values is dispatched
	// to lowerCountValues before buildAggFunc is consulted") are held to
	// account by name instead, through Cited and
	// TestInternalRationaleCitationsStillDispatch — not by storing the
	// caller's neighbourhood, which would make one new function in a
	// central dispatcher demote every rationale in the package.
	//
	// Evidence is recorded for EVERY entry, not just internal ones, so
	// that it is always already present when an entry is reclassified —
	// a field that only appears once a class is chosen would need a
	// bootstrap round-trip, and a bootstrap round-trip is a way to
	// launder a drifted rationale past the gate.
	Evidence *Evidence `json:"evidence,omitempty"`

	// Since (class=divergence) is the date, in divergenceDateLayout
	// ("2026-01-02"), the entry was first classified as a divergence —
	// backdated to the PR that introduced or reclassified it, never
	// reset by later edits. TestDivergenceEntriesRespectAgeCap fails
	// once now minus Since exceeds divergenceStaleAfter, so a
	// divergence cannot sit past its age cap silently: see
	// divergence_ratchet.go and doc.go.
	Since string `json:"since,omitempty"`
}

// Catalogue is the merged, in-memory view of the checked-in artifact
// (the shard directory test/rejection-parity/catalogue/), sorted by
// site key. Its mechanical half is a go/ast scan of the
// fmt.Errorf/errors.New sites in internal/{promql,logql,traceql}
// (non-test files); the classification and trigger queries are curated
// by hand and pinned by this package's meta-tests.
//
// Consumers see this one flat value regardless of how many shards it
// was assembled from — sharding is an on-disk concurrency measure, not
// a semantic split.
type Catalogue struct {
	Entries []Entry `json:"entries"`
}

// catalogueShard is the on-disk shape of one shard file: the entries
// whose site keys name a single source file, sorted by site key. There
// is deliberately no other field — anything a shard could carry about
// the catalogue as a whole would be duplicated 30 times and would
// itself become a merge conflict.
type catalogueShard struct {
	Entries []Entry `json:"entries"`
}

// Shard-file naming.
//
// The catalogue is stored as one shard per SOURCE FILE, so two PRs
// fixing guards in different lowering files never write the same file.
// A shard's name is its source path with every "/" replaced by
// shardPathSeparator, plus a ".json" suffix:
//
//	internal/promql/subquery.go  ->  internal__promql__subquery.go.json
//
// The mapping is injective and reversible — split the name's stem on
// shardPathSeparator to recover the source path — as long as no path
// component itself contains the separator. shardName rejects such a
// path rather than silently aliasing two source files onto one shard.
const (
	shardPathSeparator = "__"
	shardExt           = ".json"
)

// shardFileMode / shardDirMode are the permissions a regenerated shard
// and its parent directory get.
const (
	shardFileMode = 0o644
	shardDirMode  = 0o755
)

// siteSourceSeparator splits a site key into its source-file half and
// the "<func>#<hash>" half. Source paths never contain it.
const siteSourceSeparator = ":"

// Endpoint identifiers consumed by compatibility/cmd/rejection-parity.
const (
	EndpointPromInstant    = "promql_instant"
	EndpointLogQLRange     = "logql_range"
	EndpointTraceQLSearch  = "traceql_search"
	EndpointTraceQLMetrics = "traceql_metrics"
)

// Class identifiers — see the Entry.Class doc comment for the full
// semantics of each.
const (
	ClassRejection  = "rejection"
	ClassInternal   = "internal"
	ClassDivergence = "divergence"
)

// DefaultEndpoint returns the per-head endpoint used when an entry
// does not override it.
func DefaultEndpoint(head string) string {
	switch head {
	case "promql":
		return EndpointPromInstant
	case "logql":
		return EndpointLogQLRange
	case "traceql":
		return EndpointTraceQLSearch
	}
	return ""
}

// ValidEndpoint reports whether ep is a recognised endpoint for head.
func ValidEndpoint(head, ep string) bool {
	switch head {
	case "promql":
		return ep == EndpointPromInstant
	case "logql":
		return ep == EndpointLogQLRange
	case "traceql":
		return ep == EndpointTraceQLSearch || ep == EndpointTraceQLMetrics
	}
	return false
}

// Case is one parity-corpus case derived from a rejection or
// divergence entry. The driver sends Query to Endpoint on both
// backends; the expected verdict depends on Class — see
// compatibility/cmd/rejection-parity's runCase.
type Case struct {
	// Name is the catalogue site key — stable, unique, and greppable
	// straight back to the error-construction site.
	Name string `json:"name"`
	Head string `json:"head"`
	// Endpoint is resolved (entry override or head default).
	Endpoint string `json:"endpoint"`
	Query    string `json:"query"`
	// Class is the entry's classification (ClassRejection or
	// ClassDivergence — BuildCases never emits ClassInternal cases).
	// The driver branches its verdict logic on this field: a
	// rejection case asserts BOTH backends reject; a divergence case
	// asserts cerberus rejects AND the reference backend answers.
	Class string `json:"class"`
}

// BuildCases derives the parity corpus for one head from the
// catalogue: exactly one case per class=rejection or class=divergence
// entry, no more, no fewer. The 1:1 derivation is the "corpus-case
// count == catalogue count" leg of the ratchet — there is no separate
// corpus file to drift. Divergence entries are included deliberately:
// being checked on every compat run — with an inverted expected
// verdict — is the entire point of the class (see doc.go); silently
// dropping them from the corpus would turn "divergence" into exactly
// the allow-list the package forbids.
func BuildCases(cat *Catalogue, head string) ([]Case, error) {
	var out []Case
	for _, e := range cat.Entries {
		if e.Head != head || (e.Class != ClassRejection && e.Class != ClassDivergence) {
			continue
		}
		if strings.TrimSpace(e.TriggerQuery) == "" {
			return nil, fmt.Errorf("%s entry %s has no trigger query", e.Class, e.Site.Site)
		}
		ep := e.Endpoint
		if ep == "" {
			ep = DefaultEndpoint(head)
		}
		if !ValidEndpoint(head, ep) {
			return nil, fmt.Errorf("%s entry %s: endpoint %q invalid for head %s", e.Class, e.Site.Site, ep, head)
		}
		out = append(out, Case{Name: e.Site.Site, Head: head, Endpoint: ep, Query: e.TriggerQuery, Class: e.Class})
	}
	return out, nil
}

// ScanSites walks the three lowering packages under repoRoot and
// returns every prefixed error-construction site, sorted by site key.
// Test files and testdata are excluded — they construct errors for
// assertions, not for the wire.
func ScanSites(repoRoot string) ([]Site, error) {
	sc, err := scanLowerings(repoRoot)
	if err != nil {
		return nil, err
	}
	return sc.sites, nil
}

// scanLowerings parses all three lowering packages once and returns
// both halves of the scan: the mechanical site inventory and the
// derived evidence for each site. They come out of a single parse
// deliberately — a guard chain and the reaching set that surrounds it
// must always describe the same revision of the source.
func scanLowerings(repoRoot string) (*pkgScan, error) {
	out := &pkgScan{evidence: map[string]*Evidence{}, scopes: map[string]*astScope{}}
	for dir, head := range heads {
		one, err := scanDir(filepath.Join(repoRoot, dir), dir, head)
		if err != nil {
			return nil, err
		}
		out.sites = append(out.sites, one.sites...)
		for k, v := range one.evidence {
			out.evidence[k] = v
		}
		out.scopes[head] = one.scopes[head]
	}
	sort.Slice(out.sites, func(i, j int) bool { return out.sites[i].Site < out.sites[j].Site })
	return out, nil
}

// scanDir parses every non-test source file of one lowering package
// with a shared FileSet, builds the intra-package call graph over them,
// and then extracts the error sites plus their evidence. The whole
// package is parsed before any site is emitted because the reaching set
// of a function in one file is written by callers in the others.
func scanDir(absDir, relDir, head string) (*pkgScan, error) {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", absDir, err)
	}
	fset := token.NewFileSet()
	var (
		files []*ast.File
		rels  []string
	)
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		absPath := filepath.Join(absDir, name)
		f, err := parser.ParseFile(fset, absPath, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", absPath, err)
		}
		files = append(files, f)
		rels = append(rels, relDir+"/"+name)
	}
	declared, funcs, calls, callers := callGraph(files)
	sc := &astScope{fset: fset, declared: declared, funcs: funcs, calls: calls, callers: callers}

	out := &pkgScan{evidence: map[string]*Evidence{}, scopes: map[string]*astScope{head: sc}}
	prefix := head + ": "
	for i, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			sites, ev := scanFunc(sc, fn, rels[i], head, prefix)
			out.sites = append(out.sites, sites...)
			for k, v := range ev {
				out.evidence[k] = v
			}
		}
	}
	return out, nil
}

// scanFunc walks one function body for error constructors. The site
// key embeds a hash of the message so it stays stable when an
// unrelated error site is inserted earlier in the function; a repeat
// ordinal is appended only for the rare case of the same message
// constructed twice in the same function.
//
// The walk keeps an ancestor stack so each site's guard chain — the
// conditions that must hold for the error to be constructed — falls
// out of the same traversal that discovers it.
func scanFunc(sc *astScope, fn *ast.FuncDecl, relPath, head, prefix string) ([]Site, map[string]*Evidence) {
	var out []Site
	evidence := map[string]*Evidence{}
	seen := map[string]int{}
	callers := sc.callers[fn.Name.Name]
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if !isErrorConstructor(call.Fun) {
			return true
		}
		msg, ok := stringLiteral(call.Args[0])
		if !ok || !strings.HasPrefix(msg, prefix) {
			return true
		}
		seen[msg]++
		sum := sha256.Sum256([]byte(msg))
		key := fmt.Sprintf("%s:%s#%s", relPath, fn.Name.Name, hex.EncodeToString(sum[:4]))
		if seen[msg] > 1 {
			key = fmt.Sprintf("%s-%d", key, seen[msg])
		}
		out = append(out, Site{Head: head, Site: key, Message: msg})
		evidence[key] = &Evidence{
			Guard:   guardChain(sc.fset, stack),
			Callers: callers,
		}
		return true
	})
	return out, evidence
}

// isErrorConstructor matches fmt.Errorf and errors.New selector calls.
func isErrorConstructor(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch {
	case pkg.Name == "fmt" && sel.Sel.Name == "Errorf":
		return true
	case pkg.Name == "errors" && sel.Sel.Name == "New":
		return true
	}
	return false
}

// stringLiteral folds an expression into its constant string value:
// a plain string literal or a `+` concatenation of string literals.
func stringLiteral(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := stringLiteral(v.X)
		if !ok {
			return "", false
		}
		r, ok := stringLiteral(v.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	case *ast.ParenExpr:
		return stringLiteral(v.X)
	}
	return "", false
}

// Generate scans repoRoot and merges the result with the previous
// catalogue: sites present in prev keep their curated classification
// (class / trigger query / endpoint / rationale / tracking issue);
// new sites land with an empty class so the verify test demands
// curation; sites that disappeared from the source are dropped.
// Shrink and growth are both therefore deliberate, reviewable diffs.
// Evidence is never carried forward: it is rederived from the scan on
// every call, and a class=internal entry whose evidence MOVED loses its
// classification and its rationale (see reclassifyOnEvidenceDrift), so
// an unreachability claim can never outlive the source facts it was
// made about.
func Generate(repoRoot string, prev *Catalogue) (*Catalogue, error) {
	scan, err := scanLowerings(repoRoot)
	if err != nil {
		return nil, err
	}
	prevByKey := map[string]Entry{}
	if prev != nil {
		for _, e := range prev.Entries {
			prevByKey[e.Site.Site] = e
		}
	}
	out := &Catalogue{}
	for _, s := range scan.sites {
		e := Entry{Site: s, Evidence: scan.evidence[s.Site]}
		if p, ok := prevByKey[s.Site]; ok {
			e.Class = p.Class
			e.TriggerQuery = p.TriggerQuery
			e.Endpoint = p.Endpoint
			e.Rationale = p.Rationale
			e.TrackingIssue = p.TrackingIssue
			e.Since = p.Since
			reclassifyOnEvidenceDrift(&e, p.Evidence)
			e.Evidence.Cited = scan.citationsOf(s, e.Rationale, p.Evidence.citedNames())
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// reclassifyOnEvidenceDrift demotes a class=internal entry back to
// unclassified when the evidence its rationale was written against no
// longer matches the source. Regeneration therefore cannot be used to
// wave a stale unreachability claim through: the entry comes out of it
// with no class and no rationale, TestCatalogueEntriesAreClassified
// fails on it by name, and a human has to look at the site again and
// either re-state why it is still unreachable or reclassify it as the
// wire-reachable rejection it has become.
//
// Only class=internal is demoted. A rejection or divergence entry
// makes no unreachability claim to falsify — its trigger query is
// re-executed against the lowering on every run of
// TestRejectionTriggersExerciseSites, which is a stronger check than
// any evidence diff and is unaffected by where the guard moved.
func reclassifyOnEvidenceDrift(e *Entry, prevEvidence *Evidence) {
	if e.Class != ClassInternal || e.Evidence.Equal(prevEvidence) {
		return
	}
	e.Class = ""
	e.Rationale = ""
}

// siteSourceFile returns the source-file half of a site key —
// "internal/promql/subquery.go:lowerSubquery#0a1b2c3d" yields
// "internal/promql/subquery.go". It is the shard key.
func siteSourceFile(site string) (string, error) {
	src, _, ok := strings.Cut(site, siteSourceSeparator)
	if !ok || src == "" {
		return "", fmt.Errorf("site key %q carries no %q-terminated source path — every key is "+
			"\"<file>.go:<func>#<hash8>\"", site, siteSourceSeparator)
	}
	return src, nil
}

// shardName maps a source path to the shard file that owns its
// entries. See the shardPathSeparator doc comment for the rule; a path
// component containing the separator is rejected rather than aliased.
func shardName(srcPath string) (string, error) {
	parts := strings.Split(srcPath, "/")
	for _, p := range parts {
		if p == "" || strings.Contains(p, shardPathSeparator) {
			return "", fmt.Errorf("source path %q has a component that is empty or contains %q — the "+
				"shard-name mapping cannot represent it reversibly", srcPath, shardPathSeparator)
		}
	}
	return strings.Join(parts, shardPathSeparator) + shardExt, nil
}

// shardSourcePath reverses shardName.
func shardSourcePath(name string) (string, error) {
	stem, ok := strings.CutSuffix(name, shardExt)
	if !ok || stem == "" {
		return "", fmt.Errorf("shard file %q is not a %q-suffixed shard name", name, shardExt)
	}
	return strings.ReplaceAll(stem, shardPathSeparator, "/"), nil
}

// LoadCatalogue reads every shard in dir and merges them into one
// catalogue sorted by site key — byte-for-byte the same in-memory
// value the single-file artifact used to produce, so nothing
// downstream of this function knows the artifact is sharded. A missing
// directory is returned as-is (os.IsNotExist holds) so the regen path
// can bootstrap from nothing.
func LoadCatalogue(dir string) (*Catalogue, error) {
	names, err := listShards(dir)
	if err != nil {
		return nil, err
	}
	cat := &Catalogue{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path) //nolint:gosec // repo-relative artifact path
		if err != nil {
			return nil, err
		}
		var shard catalogueShard
		if err := json.Unmarshal(raw, &shard); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		cat.Entries = append(cat.Entries, shard.Entries...)
	}
	sortEntries(cat.Entries)
	return cat, nil
}

// listShards returns the shard file names in dir, sorted.
func listShards(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), shardExt) {
			continue
		}
		out = append(out, de.Name())
	}
	sort.Strings(out)
	return out, nil
}

// sortEntries orders entries by site key — the catalogue's only order,
// applied both to the merged in-memory value and within each shard.
func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Site.Site < entries[j].Site.Site })
}

// ShardCatalogue renders the canonical on-disk form: shard file name ->
// file bytes (2-space indent + trailing newline), entries partitioned
// by source file and sorted by site key inside each shard. A catalogue
// with no entries for a source file yields no shard for it, which is
// what makes pruning in WriteCatalogue a total operation rather than a
// guess.
func ShardCatalogue(cat *Catalogue) (map[string][]byte, error) {
	byShard := map[string][]Entry{}
	for _, e := range cat.Entries {
		src, err := siteSourceFile(e.Site.Site)
		if err != nil {
			return nil, err
		}
		name, err := shardName(src)
		if err != nil {
			return nil, err
		}
		byShard[name] = append(byShard[name], e)
	}
	out := make(map[string][]byte, len(byShard))
	for name, entries := range byShard {
		sortEntries(entries)
		b, err := json.MarshalIndent(catalogueShard{Entries: entries}, "", "  ")
		if err != nil {
			return nil, err
		}
		out[name] = append(b, '\n')
	}
	return out, nil
}

// WriteCatalogue writes the sharded form into dir and REMOVES shards
// that carry no entries any more. Pruning is not housekeeping: a shard
// left behind after the last guard in its source file was deleted
// keeps feeding stale entries into LoadCatalogue, so the catalogue
// would go on asserting rejections that no longer exist while the
// regenerate-and-diff test — which only ever compared the files it
// wrote — reported green.
func WriteCatalogue(dir string, cat *Catalogue) error {
	shards, err := ShardCatalogue(cat)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, shardDirMode); err != nil {
		return err
	}
	for name, body := range shards {
		if err := os.WriteFile(filepath.Join(dir, name), body, shardFileMode); err != nil {
			return err
		}
	}
	existing, err := listShards(dir)
	if err != nil {
		return err
	}
	for _, name := range existing {
		if _, keep := shards[name]; keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// DiffShards compares the rendered shards in want against the files in
// dir, returning one human-readable line per difference (a shard that
// is missing, one that lingers with no entries left, or one whose
// bytes drifted). An empty result means the checked-in directory is
// byte-for-byte what regeneration would write.
func DiffShards(dir string, want map[string][]byte) ([]string, error) {
	existing, err := listShards(dir)
	if err != nil {
		return nil, err
	}
	onDisk := map[string]bool{}
	for _, name := range existing {
		onDisk[name] = true
	}

	var diffs []string
	for _, name := range sortedKeys(want) {
		path := filepath.Join(dir, name)
		if !onDisk[name] {
			diffs = append(diffs, fmt.Sprintf("%s: missing — the source file gained its first catalogued site", path))
			continue
		}
		got, err := os.ReadFile(path) //nolint:gosec // repo-relative artifact path
		if err != nil {
			return nil, err
		}
		if string(got) != string(want[name]) {
			diffs = append(diffs, fmt.Sprintf("%s: stale — want %d bytes, got %d bytes", path, len(want[name]), len(got)))
		}
	}
	for _, name := range existing {
		if _, keep := want[name]; !keep {
			diffs = append(diffs, fmt.Sprintf("%s: stale shard — no catalogued site names that source file any more", filepath.Join(dir, name)))
		}
	}
	return diffs, nil
}

// sortedKeys returns m's keys in lexical order, so every diff this
// package reports is deterministic.
func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MessageFragments splits a fmt format string into its literal
// fragments — the chunks between %-verbs — trimmed of whitespace.
// Empty fragments are dropped.
func MessageFragments(format string) []string {
	var frags []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			frags = append(frags, s)
		}
		cur.Reset()
	}
	runes := []rune(format)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '%' {
			cur.WriteRune(runes[i])
			continue
		}
		if i+1 < len(runes) && runes[i+1] == '%' {
			cur.WriteRune('%')
			i++
			continue
		}
		// Skip the verb: flags / width / precision then the verb rune.
		flush()
		j := i + 1
		for j < len(runes) && strings.ContainsRune("+-# 0123456789.[]*", runes[j]) {
			j++
		}
		i = j // consume the verb rune itself via the loop increment
	}
	flush()
	return frags
}

// ErrorMatchesMessage reports whether errStr contains every literal
// fragment of the format string, in order. This is the comparison the
// exerciser test uses to attribute a lowering error to a catalogue
// site without being brittle about the interpolated values.
func ErrorMatchesMessage(errStr, format string) bool {
	rest := errStr
	for _, frag := range MessageFragments(format) {
		idx := strings.Index(rest, frag)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(frag):]
	}
	return true
}
