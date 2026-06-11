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
	//   "rejection" — reachable from a parseable query: a deliberate
	//                 semantic rejection whose parity against the
	//                 reference backend the compat harnesses verify.
	//                 Requires TriggerQuery (+ Endpoint for traceql
	//                 metrics-pipeline sites).
	//   "internal"  — not reachable from a parseable query through the
	//                 HTTP query endpoints: parser-enforced shapes,
	//                 internal invariants, error-propagation wrappers
	//                 (%w), or paths only reachable via non-wire entry
	//                 points. Requires Rationale.
	//
	// The verify test fails on any other value (including ""), so a
	// new rejection site cannot land unclassified.
	Class string `json:"class"`

	// TriggerQuery (class=rejection) is a minimal concrete query that
	// parses with the head's reference parser and fails this site's
	// lowering with this site's message. Pinned by
	// TestRejectionTriggersExerciseSites.
	TriggerQuery string `json:"trigger_query,omitempty"`

	// Endpoint (class=rejection) selects the HTTP endpoint the parity
	// driver sends TriggerQuery to. Empty means the head default
	// (DefaultEndpoint). TraceQL metrics-pipeline rejections set
	// "traceql_metrics" because /api/search does not accept metrics
	// expressions.
	Endpoint string `json:"endpoint,omitempty"`

	// Rationale (class=internal) documents why the site is not a
	// wire-reachable semantic rejection.
	Rationale string `json:"rationale,omitempty"`
}

// Catalogue is the checked-in JSON artifact shape
// (test/rejection-parity/catalogue.json).
type Catalogue struct {
	Source  string  `json:"source"`
	Entries []Entry `json:"entries"`
}

// catalogueSource documents how the mechanical half is derived.
const catalogueSource = "go/ast scan of fmt.Errorf/errors.New sites in " +
	"internal/{promql,logql,traceql} (non-test files); classification + " +
	"trigger queries are curated and pinned by test/rejection-parity"

// Endpoint identifiers consumed by compatibility/cmd/rejection-parity.
const (
	EndpointPromInstant    = "promql_instant"
	EndpointLogQLRange     = "logql_range"
	EndpointTraceQLSearch  = "traceql_search"
	EndpointTraceQLMetrics = "traceql_metrics"
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

// Case is one parity-corpus case derived from a rejection entry. The
// driver sends Query to Endpoint on both backends and asserts both
// reject (4xx).
type Case struct {
	// Name is the catalogue site key — stable, unique, and greppable
	// straight back to the error-construction site.
	Name string `json:"name"`
	Head string `json:"head"`
	// Endpoint is resolved (entry override or head default).
	Endpoint string `json:"endpoint"`
	Query    string `json:"query"`
}

// BuildCases derives the parity corpus for one head from the
// catalogue: exactly one case per class=rejection entry, no more, no
// fewer. The 1:1 derivation is the "corpus-case count == catalogue
// count" leg of the ratchet — there is no separate corpus file to
// drift.
func BuildCases(cat *Catalogue, head string) ([]Case, error) {
	var out []Case
	for _, e := range cat.Entries {
		if e.Head != head || e.Class != "rejection" {
			continue
		}
		if strings.TrimSpace(e.TriggerQuery) == "" {
			return nil, fmt.Errorf("rejection entry %s has no trigger query", e.Site.Site)
		}
		ep := e.Endpoint
		if ep == "" {
			ep = DefaultEndpoint(head)
		}
		if !ValidEndpoint(head, ep) {
			return nil, fmt.Errorf("rejection entry %s: endpoint %q invalid for head %s", e.Site.Site, ep, head)
		}
		out = append(out, Case{Name: e.Site.Site, Head: head, Endpoint: ep, Query: e.TriggerQuery})
	}
	return out, nil
}

// ScanSites walks the three lowering packages under repoRoot and
// returns every prefixed error-construction site, sorted by site key.
// Test files and testdata are excluded — they construct errors for
// assertions, not for the wire.
func ScanSites(repoRoot string) ([]Site, error) {
	var out []Site
	for dir, head := range heads {
		sites, err := scanDir(filepath.Join(repoRoot, dir), dir, head)
		if err != nil {
			return nil, err
		}
		out = append(out, sites...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site < out[j].Site })
	return out, nil
}

func scanDir(absDir, relDir, head string) ([]Site, error) {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", absDir, err)
	}
	var out []Site
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sites, err := scanFile(filepath.Join(absDir, name), relDir+"/"+name, head)
		if err != nil {
			return nil, err
		}
		out = append(out, sites...)
	}
	return out, nil
}

// scanFile parses one source file and extracts the prefixed
// error-construction sites, assigning per-(func, message) ordinals.
func scanFile(absPath, relPath, head string) ([]Site, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, absPath, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", absPath, err)
	}
	prefix := head + ": "
	var out []Site
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Body != nil {
			out = append(out, scanFunc(fn, relPath, head, prefix)...)
		}
	}
	return out, nil
}

// scanFunc walks one function body for error constructors. The site
// key embeds a hash of the message so it stays stable when an
// unrelated error site is inserted earlier in the function; a repeat
// ordinal is appended only for the rare case of the same message
// constructed twice in the same function.
func scanFunc(fn *ast.FuncDecl, relPath, head, prefix string) []Site {
	var out []Site
	seen := map[string]int{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
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
		return true
	})
	return out
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
// (class / trigger query / endpoint / rationale); new sites land with
// an empty class so the verify test demands curation; sites that
// disappeared from the source are dropped. Shrink and growth are both
// therefore deliberate, reviewable diffs.
func Generate(repoRoot string, prev *Catalogue) (*Catalogue, error) {
	sites, err := ScanSites(repoRoot)
	if err != nil {
		return nil, err
	}
	prevByKey := map[string]Entry{}
	if prev != nil {
		for _, e := range prev.Entries {
			prevByKey[e.Site.Site] = e
		}
	}
	out := &Catalogue{Source: catalogueSource}
	for _, s := range sites {
		e := Entry{Site: s}
		if p, ok := prevByKey[s.Site]; ok {
			e.Class = p.Class
			e.TriggerQuery = p.TriggerQuery
			e.Endpoint = p.Endpoint
			e.Rationale = p.Rationale
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// LoadCatalogue reads + parses the checked-in artifact.
func LoadCatalogue(path string) (*Catalogue, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative artifact path
	if err != nil {
		return nil, err
	}
	var cat Catalogue
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cat, nil
}

// MarshalCatalogue renders the canonical on-disk JSON form (2-space
// indent + trailing newline) so the regenerate-and-diff test compares
// byte-for-byte. Mirrors test/inventory.MarshalInventory.
func MarshalCatalogue(cat *Catalogue) ([]byte, error) {
	b, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
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
