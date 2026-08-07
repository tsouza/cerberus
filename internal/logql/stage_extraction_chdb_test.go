//go:build chdb

// Class-level coverage for every pipeline stage whose label extraction is
// modelled in SQL.
//
// Each `| json`, `| logfmt`, `| unpack`, … stage lowers to a merge
// constructor that folds an extracted label map onto the running labels
// map. Those constructors are the only place the extraction semantics
// live, and each one was written against upstream Loki's Go
// implementation of the same stage independently. Independently written
// readers diverge on the awkward inputs — nested objects, duplicate keys,
// keys needing sanitisation, non-string values, payloads that do not
// parse — and a divergence in any one of them is invisible until a user
// query returns the wrong label set.
//
// This file drives every modelled stage over ONE shared corpus of those
// awkward lines and pins the exact label map each produces, executed by
// real ClickHouse (chDB) rather than asserted against emitted SQL text.
// A change to any merge constructor that moves a key, a value, or an
// error marker fails here with the two maps printed side by side.
//
// The stage set is closed in both directions:
//
//   - Compilation closes it inwards: an entry names its merge constructor
//     directly, so a renamed or deleted constructor breaks the build.
//   - [TestStageExtraction_RegistryCoversEveryMergeConstructor] closes it
//     outwards: it parses lower.go and fails on any `…MergeLabels`
//     function this registry does not carry. Adding a stage without a row
//     here is not possible, and there is no list of stages exempted from
//     the sweep.
//
// The registry-to-constructor binding is itself checked
// ([TestStageExtraction_RegistryEntriesCallTheirOwnConstructor]): an
// entry keyed `unpackMergeLabels` whose closure quietly calls
// `jsonBareMergeLabels` would otherwise report full coverage while
// testing one stage twice.

package logql

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// stageCorpusLine is one awkward log line, driven through every modelled
// stage. The name is what failures report, so it states the property the
// line carries rather than repeating the body.
type stageCorpusLine struct {
	name string
	body string
}

// stageCorpus is the shared input set. Every line is fed to every stage:
// a line that is meaningless for a stage (a logfmt line through `| json`)
// is exactly where the error-marker semantics get pinned, so the corpus
// is deliberately not partitioned per stage.
var stageCorpus = []stageCorpusLine{
	{name: "empty", body: ``},
	{name: "plain text", body: `not json at all`},
	{name: "logfmt with a duplicate key", body: `level=error msg="hi there" dup=1 dup=2`},
	{name: "packed entry colliding with a stream label", body: `{"_entry":"hi","category":"auth","job":"other"}`},
	{name: "nested object colliding with a flat key", body: `{"user":{"id":"42"},"a":{"b":"nested"},"a_b":"top"}`},
	{name: "keys needing sanitisation, non-string values", body: `{"pod-name":"p1","9lives":"cat","count":42,"nested":{}}`},
	{name: "duplicate keys", body: `{"a":"1","a":"2"}`},
	{name: "object-shaped but malformed", body: `{"_entry":"trunc`},
	{name: "valid JSON that is not an object", body: `["not","an","object"]`},
}

// stageEntry is one modelled stage: the merge constructor that lowers it,
// and the label map it must produce for each corpus line.
type stageEntry struct {
	// name is the merge constructor's identifier. The completeness sweep
	// matches on it, and the binding check requires the closure below to
	// call the function of exactly this name.
	name string

	// query is the LogQL pipeline fragment this constructor lowers,
	// carried for failure messages so a report names the user-visible
	// stage and not just the Go symbol.
	query string

	// build calls the constructor under test with the arguments the
	// lowering passes it.
	build func(prev chplan.Expr, s schema.Logs) (chplan.Expr, error)

	// want maps a corpus line's name to the FULL label map the stage
	// yields for it, stream labels included. Every corpus line must be
	// present: a missing key fails rather than skipping, so the corpus
	// cannot grow past the expectations unnoticed.
	want map[string]map[string]string
}

// stageStreamLabels is the stream label set every seeded row carries. It
// is the `prev` map each constructor merges onto, so it appears in every
// expectation below and is what the `_extracted` collision policy is
// applied against.
var stageStreamLabels = map[string]string{"job": "api", "category": "stream"}

// streamLabelSQL spells stageStreamLabels as a literal CH map() call. A
// literal rather than a Go-map-driven render keeps the physical key order
// fixed: a CH Map compares positionally, so an unstable order would make
// the collision expectations turn on Go map iteration order.
const streamLabelSQL = `map('job', 'api', 'category', 'stream')`

// stageRegistry is the closed set of modelled stages. Adding a merge
// constructor to lower.go without adding a row here fails
// TestStageExtraction_RegistryCoversEveryMergeConstructor.
var stageRegistry = []stageEntry{
	{
		name:  "logfmtMergeLabels",
		query: `| logfmt`,
		build: func(prev chplan.Expr, s schema.Logs) (chplan.Expr, error) {
			return logfmtMergeLabels(prev, s), nil
		},
		want: map[string]map[string]string{
			// logfmt reads `key=value` pairs and nothing else. A JSON
			// document scans as one bare token with no value, and a
			// valueless key is dropped rather than kept empty — which is
			// what upstream's `--keep-empty` flag exists to turn off.
			"empty":      stageLabels(nil),
			"plain text": stageLabels(nil),
			"logfmt with a duplicate key": stageLabels(map[string]string{
				"level": "error", "msg": "hi there",
				// Last write wins, as upstream's per-pair assignment does.
				"dup": "2",
			}),
			"packed entry colliding with a stream label":   stageLabels(nil),
			"nested object colliding with a flat key":      stageLabels(nil),
			"keys needing sanitisation, non-string values": stageLabels(nil),
			"duplicate keys":                   stageLabels(nil),
			"object-shaped but malformed":      stageLabels(nil),
			"valid JSON that is not an object": stageLabels(nil),
		},
	},
	{
		name:  "jsonBareMergeLabels",
		query: `| json`,
		build: func(prev chplan.Expr, s schema.Logs) (chplan.Expr, error) {
			return jsonBareMergeLabels(prev, s), nil
		},
		want: map[string]map[string]string{
			"empty":                       stageLabels(nil),
			"plain text":                  stageLabels(nil),
			"logfmt with a duplicate key": stageLabels(nil),
			"packed entry colliding with a stream label": stageLabels(map[string]string{
				// `| json` has no notion of `_entry`: it is an ordinary
				// member. The two keys that collide with a stream label
				// are suffixed rather than overwriting it.
				"_entry": "hi", "category_extracted": "auth", "job_extracted": "other",
			}),
			"nested object colliding with a flat key": stageLabels(map[string]string{
				// `user.id` flattens to `user_id`; `a.b` flattens onto
				// the same name as the top-level `a_b`, which is stated
				// later in the document and therefore wins.
				"user_id": "42", "a_b": "top",
			}),
			"keys needing sanitisation, non-string values": stageLabels(map[string]string{
				"pod_name": "p1", "_9lives": "cat",
				// Numbers are labels; an object with no scalar leaf
				// contributes nothing.
				"count": "42",
			}),
			"duplicate keys":                   stageLabels(map[string]string{"a": "2"}),
			"object-shaped but malformed":      stageLabels(nil),
			"valid JSON that is not an object": stageLabels(nil),
		},
	},
	{
		name:  "unpackMergeLabels",
		query: `| unpack`,
		build: func(prev chplan.Expr, s schema.Logs) (chplan.Expr, error) {
			return unpackMergeLabels(prev, s), nil
		},
		want: map[string]map[string]string{
			// An empty line is not an error — upstream returns it
			// untouched — but anything else that is not a readable JSON
			// object is.
			"empty":                       stageLabels(nil),
			"plain text":                  stageLabels(unpackNotAnObject),
			"logfmt with a duplicate key": stageLabels(unpackNotAnObject),
			"packed entry colliding with a stream label": stageLabels(map[string]string{
				// `_entry` becomes the line, never a label; the rest are
				// labels, suffixed where they collide with the stream.
				"category_extracted": "auth", "job_extracted": "other",
			}),
			// A readable object with no `_entry` member is not packed:
			// upstream discards its whole buffer, labels and error alike.
			"nested object colliding with a flat key":      stageLabels(nil),
			"keys needing sanitisation, non-string values": stageLabels(nil),
			"duplicate keys": stageLabels(nil),
			// Object-shaped but unparseable. The detail text is the Go
			// JSON reader's own parse-position message, which no CH
			// expression derives, so SQL stamps the error label and
			// [internal/api/loki.unpackParseDetailStep] fills the detail
			// in on the log-stream path.
			"object-shaped but malformed": stageLabels(map[string]string{
				syntax.ErrorLabel:        JSONParserErrValue,
				syntax.ErrorDetailsLabel: "",
			}),
			"valid JSON that is not an object": stageLabels(unpackNotAnObject),
		},
	},
	{
		name:  "jsonExpressionMergeLabels",
		query: `| json uid="user.id", ab="a.b"`,
		build: func(prev chplan.Expr, s schema.Logs) (chplan.Expr, error) {
			return jsonExpressionMergeLabels(prev, s, jsonExtractionExprs)
		},
		want: map[string]map[string]string{
			"empty":                       stageLabels(jsonExpressionMisses),
			"plain text":                  stageLabels(jsonExpressionMisses),
			"logfmt with a duplicate key": stageLabels(jsonExpressionMisses),
			"packed entry colliding with a stream label": stageLabels(jsonExpressionMisses),
			"nested object colliding with a flat key": stageLabels(map[string]string{
				// Both paths address a nested member directly, so the
				// top-level `a_b` sibling is not what `a.b` resolves to.
				"uid": "42", "ab": "nested",
			}),
			"keys needing sanitisation, non-string values": stageLabels(jsonExpressionMisses),
			"duplicate keys":                   stageLabels(jsonExpressionMisses),
			"object-shaped but malformed":      stageLabels(jsonExpressionMisses),
			"valid JSON that is not an object": stageLabels(jsonExpressionMisses),
		},
	},
	{
		name:  "logfmtExpressionMergeLabels",
		query: `| logfmt lvl="level", dup="dup"`,
		build: func(prev chplan.Expr, s schema.Logs) (chplan.Expr, error) {
			return logfmtExpressionMergeLabels(prev, s, logfmtExtractionExprs)
		},
		want: map[string]map[string]string{
			"empty":      stageLabels(logfmtExpressionMisses),
			"plain text": stageLabels(logfmtExpressionMisses),
			"logfmt with a duplicate key": stageLabels(map[string]string{
				// The same duplicate resolution the bare form applies:
				// both read one reduced pair set, so they cannot drift.
				"lvl": "error", "dup": "2",
			}),
			"packed entry colliding with a stream label":   stageLabels(logfmtExpressionMisses),
			"nested object colliding with a flat key":      stageLabels(logfmtExpressionMisses),
			"keys needing sanitisation, non-string values": stageLabels(logfmtExpressionMisses),
			"duplicate keys":                   stageLabels(logfmtExpressionMisses),
			"object-shaped but malformed":      stageLabels(logfmtExpressionMisses),
			"valid JSON that is not an object": stageLabels(logfmtExpressionMisses),
		},
	},
	{
		name:  "regexpMergeLabels",
		query: `| regexp "` + stageRegexpPattern + `"`,
		build: func(prev chplan.Expr, s schema.Logs) (chplan.Expr, error) {
			return regexpMergeLabels(prev, s, stageRegexpPattern)
		},
		want: map[string]map[string]string{
			"empty":      stageLabels(regexpMisses),
			"plain text": stageLabels(regexpMisses),
			"logfmt with a duplicate key": stageLabels(map[string]string{
				// The leftmost match, and only the first: `| regexp`
				// names one capture per label.
				"head": "level", "tail": "error",
			}),
			"packed entry colliding with a stream label":   stageLabels(regexpMisses),
			"nested object colliding with a flat key":      stageLabels(regexpMisses),
			"keys needing sanitisation, non-string values": stageLabels(regexpMisses),
			"duplicate keys":                   stageLabels(regexpMisses),
			"object-shaped but malformed":      stageLabels(regexpMisses),
			"valid JSON that is not an object": stageLabels(regexpMisses),
		},
	},
}

// unpackNotAnObject is the marker pair `| unpack` stamps on a payload
// that is not a JSON object at all. Upstream's sentinel text, byte for
// byte.
var unpackNotAnObject = map[string]string{
	syntax.ErrorLabel:        JSONParserErrValue,
	syntax.ErrorDetailsLabel: UnexpectedJSONObjectDetail,
}

// jsonExpressionMisses / logfmtExpressionMisses / regexpMisses are what
// the three statically-keyed stages yield when their paths, keys or
// captures do not resolve: the label is present and empty rather than
// absent, which is the same thing to every consumer, because a label
// carrying no value is dropped from the emitted set.
var (
	jsonExpressionMisses   = map[string]string{"uid": "", "ab": ""}
	logfmtExpressionMisses = map[string]string{"lvl": "", "dup": ""}
	regexpMisses           = map[string]string{"head": "", "tail": ""}
)

// stageSeedTable is the corpus table. Body is the line under test and the
// stream labels are materialised so every constructor reads the same
// `prev` map without the seed repeating it per row.
const stageSeedTable = `CREATE OR REPLACE TABLE stage_corpus (
    idx UInt32,
    Body String,
    ResourceAttributes Map(String, String) MATERIALIZED ` + streamLabelSQL + `,
    LogAttributes Map(String, String) MATERIALIZED map()
) ENGINE = Memory;`

// TestStageExtraction_ModelledStagesOverSharedCorpus executes every
// registered stage's extraction against every corpus line and pins the
// resulting label map.
func TestStageExtraction_ModelledStagesOverSharedCorpus(t *testing.T) {
	db := openStageCorpus(t)
	defer db.Close()

	s := schema.DefaultOTelLogs()
	for _, entry := range stageRegistry {
		t.Run(entry.name, func(t *testing.T) {
			labels, err := entry.build(&chplan.ColumnRef{Name: s.ResourceAttributesColumn}, s)
			if err != nil {
				t.Fatalf("%s: build extraction: %v", entry.name, err)
			}
			got := runStageExtraction(t, db, labels)
			for i, line := range stageCorpus {
				want, ok := entry.want[line.name]
				if !ok {
					// Report what the stage produced. A new corpus line
					// or a new stage is pinned by reading this and
					// deciding whether the map is right, which is a
					// different act from copying it in.
					t.Errorf("%s (%s) has no expectation for corpus line %q (body %q) — every line must be pinned; it produced: %s",
						entry.name, entry.query, line.name, line.body, renderLabelMap(got[i]))
					continue
				}
				if diff := diffLabelMaps(want, got[i]); diff != "" {
					t.Errorf("%s (%s) on %q (body %q):\n%s",
						entry.name, entry.query, line.name, line.body, diff)
				}
			}
		})
	}
}

// openStageCorpus opens a chDB session and seeds the corpus table.
func openStageCorpus(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	if _, err := db.Exec(stageSeedTable); err != nil {
		db.Close()
		t.Fatalf("seed table: %v", err)
	}
	for i, line := range stageCorpus {
		if _, err := db.Exec("INSERT INTO stage_corpus (idx, Body) VALUES (?, ?)", i, line.body); err != nil {
			db.Close()
			t.Fatalf("seed %q: %v", line.name, err)
		}
	}
	return db
}

// runStageExtraction emits labels as SQL, runs it over the corpus and
// returns one decoded label map per corpus line, indexed as stageCorpus
// is.
func runStageExtraction(t *testing.T, db *sql.DB, labels chplan.Expr) []map[string]string {
	t.Helper()
	b := chsql.NewBuilder()
	if err := b.Expr(labels); err != nil {
		t.Fatalf("emit extraction expression: %v", err)
	}
	exprSQL, args, err := b.Build()
	if err != nil {
		t.Fatalf("build extraction expression: %v", err)
	}
	// toJSONString renders the Map as an object so the assertion reads
	// ClickHouse's answer directly rather than through the driver's
	// Map-column type mapping.
	query := "SELECT `idx`, toJSONString(" + exprSQL + ") FROM `stage_corpus` ORDER BY `idx`"
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("run %s with args %v: %v", query, args, err)
	}
	defer rows.Close()

	out := make([]map[string]string, len(stageCorpus))
	for rows.Next() {
		var idx int
		var encoded string
		if err := rows.Scan(&idx, &encoded); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if idx < 0 || idx >= len(out) {
			t.Fatalf("row index %d outside the corpus", idx)
		}
		decoded, err := decodeLabelMap(encoded)
		if err != nil {
			t.Fatalf("corpus line %q: %v", stageCorpus[idx].name, err)
		}
		out[idx] = decoded
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	for i, got := range out {
		if got == nil {
			t.Fatalf("corpus line %q produced no row", stageCorpus[i].name)
		}
	}
	return out
}

// decodeLabelMap decodes ClickHouse's JSON rendering of a label map, and
// rejects one that carries the same key twice.
//
// A CH Map retains duplicate entries: a stage whose extraction lets a
// repeated source key through produces a Map of length 3 for two labels,
// and every lookup against it resolves to the FIRST of the pair while
// upstream resolves to the last. Decoding straight into a Go map would
// silently keep whichever came last and hide exactly that bug, so the
// decode walks the token stream instead.
func decodeLabelMap(encoded string) (map[string]string, error) {
	dec := json.NewDecoder(strings.NewReader(encoded))
	open, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decode label map %q: %v", encoded, err)
	}
	if delim, ok := open.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("label map %q does not open with an object", encoded)
	}
	out := map[string]string{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decode label map %q: %v", encoded, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("label map %q has a non-string key", encoded)
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode value of %q in %q: %v", key, encoded, err)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("label map %q carries %q twice: a lookup against it resolves to the "+
				"FIRST entry, where upstream's assignment leaves the LAST standing", encoded, key)
		}
		out[key] = value
	}
	return out, nil
}

// stageLabels renders one expectation: the stream label set with the
// stage's extracted labels applied on top, which is what a merge
// constructor produces when nothing collides.
func stageLabels(extracted map[string]string) map[string]string {
	out := make(map[string]string, len(stageStreamLabels)+len(extracted))
	for k, v := range stageStreamLabels {
		out[k] = v
	}
	for k, v := range extracted {
		out[k] = v
	}
	return out
}

// renderLabelMap renders a label map in a stable order.
func renderLabelMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q: %q", k, m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// diffLabelMaps renders the difference between two label maps, or "" when
// they match. Sorted so a failure reads the same on every run.
func diffLabelMaps(want, got map[string]string) string {
	keys := map[string]struct{}{}
	for k := range want {
		keys[k] = struct{}{}
	}
	for k := range got {
		keys[k] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	var sb strings.Builder
	for _, k := range ordered {
		w, inWant := want[k]
		g, inGot := got[k]
		switch {
		case inWant && !inGot:
			fmt.Fprintf(&sb, "  missing %q (want %q)\n", k, w)
		case !inWant && inGot:
			fmt.Fprintf(&sb, "  unexpected %q = %q\n", k, g)
		case w != g:
			fmt.Fprintf(&sb, "  %q: want %q, got %q\n", k, w, g)
		}
	}
	return sb.String()
}

// mergeConstructorSuffix is the naming convention every stage's label
// merge constructor follows in lower.go. The completeness sweep below
// enumerates the stage set from the source by it, so the registry cannot
// silently fall behind a newly added stage.
const mergeConstructorSuffix = "MergeLabels"

// TestStageExtraction_RegistryCoversEveryMergeConstructor parses lower.go
// and fails on any label merge constructor missing from stageRegistry.
//
// This is the half compilation cannot check. Without it, a new stage
// lowers, ships, and is simply never driven over the awkward-line corpus
// — which is how the four already-fixed divergences reached users.
func TestStageExtraction_RegistryCoversEveryMergeConstructor(t *testing.T) {
	registered := map[string]bool{}
	for _, entry := range stageRegistry {
		registered[entry.name] = true
	}
	declared := mergeConstructorNames(t)
	if len(declared) == 0 {
		t.Fatal("scanned lower.go and found no merge constructors — the scan is broken, not the code")
	}
	for _, name := range declared {
		if !registered[name] {
			t.Errorf("%s lowers a stage's labels but stageRegistry has no row for it: "+
				"add one with its expected label map for every corpus line", name)
		}
	}
	for name := range registered {
		if !contains(declared, name) {
			t.Errorf("stageRegistry names %q, which lower.go does not declare", name)
		}
	}
}

// mergeConstructorNames returns every `…MergeLabels` function declared in
// lower.go.
func mergeConstructorNames(t *testing.T) []string {
	t.Helper()
	file := parseGoFile(t, "lower.go")
	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		if strings.HasSuffix(fn.Name.Name, mergeConstructorSuffix) {
			names = append(names, fn.Name.Name)
		}
	}
	sort.Strings(names)
	return names
}

// TestStageExtraction_RegistryEntriesCallTheirOwnConstructor pins the
// registry's key-to-closure binding: the closure of an entry named X must
// call X.
//
// Without this, an entry could name one constructor and exercise another,
// and both completeness checks above would still pass while a stage went
// untested.
func TestStageExtraction_RegistryEntriesCallTheirOwnConstructor(t *testing.T) {
	file := parseGoFile(t, "stage_extraction_chdb_test.go")
	called := registryEntryCalls(t, file)
	for _, entry := range stageRegistry {
		if !called[entry.name] {
			t.Errorf("stageRegistry entry %q does not call %s in its build closure", entry.name, entry.name)
		}
	}
	if len(called) != len(stageRegistry) {
		t.Errorf("scanned %d registry entries in the source but the registry holds %d — the scan is stale",
			len(called), len(stageRegistry))
	}
}

// registryEntryCalls maps each stageRegistry entry's declared name to
// whether its own build closure calls the function of that name.
func registryEntryCalls(t *testing.T, file *ast.File) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, elt := range registryLiteral(t, file).Elts {
		lit, ok := elt.(*ast.CompositeLit)
		if !ok {
			t.Fatalf("stageRegistry element is %T, want a composite literal", elt)
		}
		name, build := "", ast.Expr(nil)
		for _, field := range lit.Elts {
			kv, ok := field.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "name":
				basic, ok := kv.Value.(*ast.BasicLit)
				if !ok {
					t.Fatalf("stageRegistry entry name is %T, want a string literal", kv.Value)
				}
				name = strings.Trim(basic.Value, `"`)
			case "build":
				build = kv.Value
			}
		}
		if name == "" || build == nil {
			t.Fatal("stageRegistry entry is missing its name or build field")
		}
		out[name] = callsFunction(build, name)
	}
	return out
}

// registryLiteral returns the composite literal stageRegistry is declared
// with, failing when the declaration is not found or is not a literal.
func registryLiteral(t *testing.T, file *ast.File) *ast.CompositeLit {
	t.Helper()
	var found *ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "stageRegistry" || len(spec.Values) != 1 {
			return true
		}
		lit, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatalf("stageRegistry is declared as %T, want a composite literal", spec.Values[0])
		}
		found = lit
		return false
	})
	if found == nil {
		t.Fatal("stageRegistry declaration not found in this file")
	}
	return found
}

// callsFunction reports whether node contains a call to the named
// function.
func callsFunction(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// parseGoFile parses a file from this package's directory.
func parseGoFile(t *testing.T, name string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// stageExtractionExprs keeps the label-extraction expression arguments
// the expression-parser stages take, spelled once so the registry rows
// read as the stage they model.
var (
	jsonExtractionExprs = []syntax.LabelExtractionExpr{
		syntax.NewLabelExtractionExpr("uid", "user.id"),
		syntax.NewLabelExtractionExpr("ab", "a.b"),
	}
	logfmtExtractionExprs = []syntax.LabelExtractionExpr{
		syntax.NewLabelExtractionExpr("lvl", "level"),
		syntax.NewLabelExtractionExpr("dup", "dup"),
	}
)

// stageRegexpPattern is the pattern the `| regexp` row models: two named
// captures, the second of which does not participate on most corpus
// lines, so both the hit and the miss shape are exercised.
const stageRegexpPattern = `(?P<head>[a-z]+)=(?P<tail>[a-z]+)`
