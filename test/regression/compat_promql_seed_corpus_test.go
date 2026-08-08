package regression

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// The PromQL compatibility lane diffs cerberus against a reference
// Prometheus over the curated corpus below. Both backends are fed by the
// same seeder, so a query naming a family the seeder never writes gets
// empty from BOTH — and the upstream tester scores "no diff" as PASS. That
// is a hollow green: the case is counted, contributes to the score, and
// tests nothing.
//
// The two tests here close both halves of that failure mode:
//
//   - every metric name the corpus references must be producible by the
//     seeder (TestCompatCorpusReferencesOnlySeededMetrics);
//   - every family the seeder writes to ClickHouse must also be mirrored
//     into the reference Prometheus (TestCompatSeedMirrorsEveryFixture) —
//     a family seeded on one side only is exactly as invisible as one
//     never seeded at all.
const (
	compatSeedSource   = "../../compatibility/prometheus/cmd/seed/main.go"
	compatMirrorSource = "../../compatibility/prometheus/cmd/seed/prom_remote.go"
	compatCorpusPath   = "../../compatibility/prometheus/cerberus-test-queries.yml"

	// absentMetricPrefix marks a family a query references BECAUSE it must
	// not exist (`absent(...)`, empty-result shape pins). It is a contract
	// asserted in both directions, not an allow-list: it names no specific
	// metric and grows no entries.
	absentMetricPrefix = "nonexistent"

	// fixtureInsertsVar and the three mirror-list names are the package-level
	// slices the seeder's two halves are declared in.
	fixtureInsertsVar             = "fixtureInserts"
	fixtureSourcesVar             = "fixtureSources"
	histogramFixtureSourcesVar    = "histogramFixtureSources"
	expHistogramFixtureSourcesVar = "expHistogramFixtureSources"

	insertIntoKeyword = "INSERT INTO "
)

// corpusVariantSubstitutions is the closed set of `{{.var}}` placeholders
// the corpus uses, each mapped to ONE representative expansion. The
// upstream tester expands every variable across its full domain, but metric
// NAMES never vary by variant — only operators, functions and durations do
// — so a single representative expansion is sufficient to recover the
// referenced name set, and keeping the domain closed means a corpus re-sync
// that introduces a new variable fails here instead of silently dropping
// whichever queries use it.
var corpusVariantSubstitutions = map[string]string{
	"arithBinOp":           "+",
	"binOp":                "+",
	"clampFunc":            "clamp_max",
	"compBinOp":            "==",
	"dateFunc":             "year",
	"extrapolatedRateFunc": "rate",
	"instantRateFunc":      "irate",
	"offset":               "5m",
	"quantile":             "0.5",
	"range":                "5m",
	"simpleAggrOp":         "sum",
	"simpleMathFunc":       "abs",
	"simpleTimeAggrOp":     "avg",
	"topBottomOp":          "topk",
}

var corpusVariantRE = regexp.MustCompile(`\{\{\.([a-zA-Z]+)\}\}`)

// seedFixture is one entry of the seeder's fixtureInserts list: the error
// label it carries and the physical table its INSERT targets.
type seedFixture struct {
	name  string
	table string
}

// TestCompatCorpusReferencesOnlySeededMetrics asserts that every metric name
// the compatibility corpus selects is a name the seeder can actually
// produce, so no case in the lane can score PASS by comparing empty against
// empty.
//
// The containment is deliberately one-way: a seeded family nobody queries is
// legitimate (demo_disk_total_bytes exists so the disk pair has a
// denominator), whereas a queried family nobody seeds is the bug.
func TestCompatCorpusReferencesOnlySeededMetrics(t *testing.T) {
	t.Parallel()

	m := schema.DefaultOTelMetrics()
	fixtures := parseSeedFixtures(t)
	servable := map[string]bool{}
	// bareHistogramNames collects every seeded classic-histogram family's
	// OTel-CH storage name — the spelling with none of the `_bucket` /
	// `_count` / `_sum` wire suffixes. #1483 pins that this spelling must
	// resolve empty even though the seeder DOES write rows under it (the
	// physical storage name and the Prom-wire series name are different
	// things for a classic histogram). A corpus query using one is
	// therefore neither "servable" (per the loop above) nor a hollow
	// comparison against an unseeded name: it is a real regression pin,
	// so it gets its own exemption arm rather than being forced through
	// the unrelated absentMetricPrefix convention (which a seeded family's
	// synthetic names are themselves forbidden from carrying, below).
	bareHistogramNames := map[string]bool{}
	// expHistogramNames collects every seeded exponential-histogram family.
	// Unlike the classic layout the BARE name IS the wire series — a native
	// histogram carries its whole distribution in the sample — so it is
	// servable rather than exempt.
	expHistogramNames := map[string]bool{}
	sawBucketExpansion := false
	for _, f := range fixtures {
		switch f.table {
		case m.HistogramTable:
			synthetic := promql.HistogramSyntheticNames(f.name, m)
			if len(synthetic) == 0 {
				t.Fatalf("fixture %q inserts into the histogram table but yields no "+
					"Prom-wire series names", f.name)
			}
			for _, name := range synthetic {
				// The BARE base name must NOT be servable: the OTel-CH
				// layout stores the row under it, but cerberus's lowering
				// never serves it, so treating it as servable would let a
				// query select a name that returns empty forever.
				if name == f.name {
					t.Fatalf("HistogramSyntheticNames(%q) contains the bare base name; "+
						"cerberus does not serve it", f.name)
				}
				if strings.HasSuffix(name, "_bucket") {
					sawBucketExpansion = true
				}
				servable[name] = true
			}
			bareHistogramNames[f.name] = true
		case m.ExpHistogramTable:
			if !m.IsExpHistogramMetric(f.name) {
				t.Fatalf("fixture %q inserts into the exponential-histogram table but its "+
					"name does not carry the %q routing suffix, so cerberus would read it "+
					"from a different table than the seeder writes it to",
					f.name, m.ExpHistogramSuffix)
			}
			servable[f.name] = true
			expHistogramNames[f.name] = true
			// Cerberus also serves `<name>_count` / `<name>_sum` companions
			// off an exp-histogram row, and those are deliberately NOT
			// servable here. Prometheus has no companion series for a native
			// histogram at all — the count and the sum are reached through
			// histogram_count() / histogram_sum() — so a corpus query on one
			// would compare cerberus's populated result against a structurally
			// empty reference, a divergence the fixture invented rather than
			// one either backend has.
		case m.GaugeTable, m.SumTable:
			servable[f.name] = true
		default:
			t.Fatalf("fixture %q inserts into %q: teach this test how that table's "+
				"rows surface as PromQL series names", f.name, f.table)
		}
	}
	if len(servable) == 0 {
		t.Fatal("no servable metric names extracted from the seeder; this test would compare nothing")
	}
	if !sawBucketExpansion {
		t.Fatal("no seeded family expanded to a `_bucket` series; the histogram arm of " +
			"this gate never fired, so it proves nothing about classic histograms")
	}
	if len(expHistogramNames) == 0 {
		t.Fatal("no seeded family lands in the exponential-histogram table; the " +
			"native-histogram arm of this gate never fired, and the corpus's " +
			"histogram_count / histogram_sum / histogram_quantile cases would " +
			"compare the empty vector against the empty vector")
	}
	for name := range servable {
		if strings.HasPrefix(name, absentMetricPrefix) {
			t.Errorf("seeded metric %q carries the %q prefix, which the corpus reserves "+
				"for families that must NOT exist", name, absentMetricPrefix)
		}
	}

	referenced, usedVariants := parseCorpusMetricNames(t)
	if len(referenced) == 0 {
		t.Fatal("no metric names extracted from the corpus; this test would compare nothing")
	}
	for variable := range corpusVariantSubstitutions {
		if !usedVariants[variable] {
			t.Errorf("substitution table declares %q but no corpus query uses it; "+
				"drop the entry so the table can't rot", variable)
		}
	}

	sawAbsentReference := false
	sawBareHistogramReference := false
	sawExpHistogramReference := false
	var missing []string
	for _, name := range sortedSetKeys(referenced) {
		if strings.HasPrefix(name, absentMetricPrefix) {
			sawAbsentReference = true
			continue
		}
		if bareHistogramNames[name] {
			sawBareHistogramReference = true
			continue
		}
		if expHistogramNames[name] {
			sawExpHistogramReference = true
		}
		if !servable[name] {
			missing = append(missing, name)
		}
	}
	if !sawAbsentReference {
		t.Error("no corpus query references a name with the " + absentMetricPrefix +
			" prefix; the exemption arm of this gate is dead and should be removed")
	}
	if !sawBareHistogramReference {
		t.Error("no corpus query references a seeded classic-histogram family's bare " +
			"(non-suffixed) name; the #1483 bare-name exemption arm of this gate is dead " +
			"and should be removed")
	}
	if !sawExpHistogramReference {
		t.Error("no corpus query references a seeded exponential-histogram family; the " +
			"native-histogram value functions (histogram_count / histogram_sum / " +
			"histogram_avg / histogram_stddev / histogram_fraction / histogram_quantile) " +
			"would then be exercised only against float series, which the reference " +
			"skips — an empty-vs-empty comparison reporting parity it never established")
	}
	if len(missing) > 0 {
		t.Errorf("corpus queries reference metric families the seeder cannot produce: %v\n"+
			"Either seed the family in %s (and mirror it in %s), or — if the query needs "+
			"the family to be ABSENT — rename it with the %q prefix.",
			missing, compatSeedSource, compatMirrorSource, absentMetricPrefix)
	}
}

// TestCompatSeedMirrorsEveryFixture pins the CH-side INSERT list against the
// Prom-side remote_write list. The seeder's own comment says to keep them in
// lock-step; nothing enforced it, and a family written to ClickHouse but
// never remote-written makes every query over it diff against an empty
// reference Prometheus.
func TestCompatSeedMirrorsEveryFixture(t *testing.T) {
	t.Parallel()

	inserted := parseSeedFixtures(t)
	mirrored := parseMirrorSources(t)

	if len(inserted) == 0 {
		t.Fatal("no INSERT fixtures extracted from the seeder; this test would compare nothing")
	}
	if len(mirrored) == 0 {
		t.Fatal("no remote_write sources extracted from the mirror; this test would compare nothing")
	}

	if got, want := renderFixtures(mirrored), renderFixtures(inserted); got != want {
		t.Errorf("the CH-side INSERT list and the Prom-side remote_write list disagree\n"+
			"%s writes:        %s\n%s mirrors:      %s",
			compatSeedSource, want, compatMirrorSource, got)
	}
}

func renderFixtures(fs []seedFixture) string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.name+"@"+f.table)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseSeedFixtures reads the seeder's fixtureInserts list out of the Go
// source and returns the (metric name, target table) pair each entry
// declares. Reading the AST rather than executing the seeder keeps this a
// default-lane test with no ClickHouse dependency.
func parseSeedFixtures(t *testing.T) []seedFixture {
	t.Helper()

	lit := findSliceLiteral(t, compatSeedSource, fixtureInsertsVar)
	out := make([]seedFixture, 0, len(lit.Elts))
	for _, elt := range lit.Elts {
		entry, ok := elt.(*ast.CompositeLit)
		if !ok {
			t.Fatalf("%s: %s element is %T, want a composite literal", compatSeedSource, fixtureInsertsVar, elt)
		}
		fields := map[string]string{}
		for _, f := range entry.Elts {
			kv, ok := f.(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("%s: %s entry has a positional field %T, want key: value",
					compatSeedSource, fixtureInsertsVar, f)
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				t.Fatalf("%s: %s entry field key is %T, want an identifier",
					compatSeedSource, fixtureInsertsVar, kv.Key)
			}
			fields[key.Name] = basicLitValue(t, compatSeedSource, kv.Value)
		}
		name, sql := fields["name"], fields["sql"]
		if name == "" || sql == "" {
			t.Fatalf("%s: %s entry is missing name or sql (name=%q, sql len=%d)",
				compatSeedSource, fixtureInsertsVar, name, len(sql))
		}
		// namedStmt.name is documented as an error label. Pinning it to the
		// MetricName literal the SQL actually writes stops the label drifting
		// away from the data, which would make every assertion below reason
		// about a family that isn't there.
		if !strings.Contains(sql, "'"+name+"'") {
			t.Errorf("%s: fixture labelled %q never writes that literal MetricName; "+
				"the label and the data have drifted apart", compatSeedSource, name)
		}
		out = append(out, seedFixture{name: name, table: insertTargetTable(t, name, sql)})
	}
	return out
}

// insertTargetTable pulls the physical table out of an `INSERT INTO <table>`
// statement.
func insertTargetTable(t *testing.T, name, sql string) string {
	t.Helper()

	idx := strings.Index(sql, insertIntoKeyword)
	if idx < 0 {
		t.Fatalf("fixture %q has no %q clause", name, strings.TrimSpace(insertIntoKeyword))
	}
	fields := strings.Fields(sql[idx+len(insertIntoKeyword):])
	if len(fields) == 0 {
		t.Fatalf("fixture %q names no table after %q", name, strings.TrimSpace(insertIntoKeyword))
	}
	return fields[0]
}

// parseMirrorSources reads the remote_write source lists out of the mirror's
// Go source. All are declared with composite literals whose first two
// fields are (metric name, table) — positional (fixtureSources' original
// 2-field shape) or keyed (fixtureSources.accumulateToCumulative's optional
// 3rd field, which this parser ignores: it only needs to know a family IS
// mirrored, not how). Either way basicLitValue reads the metric-name and
// table string literals directly off the first two field values, keyed or
// positional.
func parseMirrorSources(t *testing.T) []seedFixture {
	t.Helper()

	var out []seedFixture
	for _, varName := range []string{
		fixtureSourcesVar, histogramFixtureSourcesVar, expHistogramFixtureSourcesVar,
	} {
		lit := findSliceLiteral(t, compatMirrorSource, varName)
		for _, elt := range lit.Elts {
			entry, ok := elt.(*ast.CompositeLit)
			if !ok {
				t.Fatalf("%s: %s element is %T, want a composite literal", compatMirrorSource, varName, elt)
			}
			if len(entry.Elts) < 2 {
				t.Fatalf("%s: %s entry has %d fields, want at least (metric name, table)",
					compatMirrorSource, varName, len(entry.Elts))
			}
			out = append(out, seedFixture{
				name:  basicLitValue(t, compatMirrorSource, mirrorFieldValue(t, entry.Elts[0])),
				table: basicLitValue(t, compatMirrorSource, mirrorFieldValue(t, entry.Elts[1])),
			})
		}
	}
	return out
}

// mirrorFieldValue unwraps a composite-literal field expr to its value,
// whether the field was written positionally (expr IS the value) or keyed
// (expr is a `field: value` ast.KeyValueExpr, whose .Value is what
// basicLitValue needs).
func mirrorFieldValue(t *testing.T, expr ast.Expr) ast.Expr {
	t.Helper()

	if kv, ok := expr.(*ast.KeyValueExpr); ok {
		return kv.Value
	}
	return expr
}

// findSliceLiteral locates `var <name> = []T{...}` in a Go source file.
func findSliceLiteral(t *testing.T, path, name string) *ast.CompositeLit {
	t.Helper()

	file, err := goparser.ParseFile(token.NewFileSet(), path, nil, goparser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var found *ast.CompositeLit
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s: %s is %T, want a slice literal", path, name, vs.Values[i])
				}
				found = lit
			}
		}
	}
	if found == nil {
		t.Fatalf("%s: no `var %s = []...{}` declaration found", path, name)
	}
	return found
}

// basicLitValue unquotes a string literal expression, handling both the
// interpreted and raw-string forms the seeder mixes.
func basicLitValue(t *testing.T, path string, expr ast.Expr) string {
	t.Helper()

	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("%s: expected a string literal, got %T", path, expr)
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("%s: unquote %.40s...: %v", path, lit.Value, err)
	}
	return v
}

// corpusCase is the subset of the upstream compliance test-case shape this
// gate needs.
type corpusCase struct {
	Query      string `yaml:"query"`
	ShouldFail bool   `yaml:"should_fail"`
}

// parseCorpusMetricNames returns every metric name the corpus selects, plus
// the set of `{{.var}}` placeholders it actually used.
func parseCorpusMetricNames(t *testing.T) (referenced, usedVariants map[string]bool) {
	t.Helper()

	raw, err := os.ReadFile(compatCorpusPath)
	if err != nil {
		t.Fatalf("read %s: %v", compatCorpusPath, err)
	}
	var corpus struct {
		TestCases []corpusCase `yaml:"test_cases"`
	}
	if err := yaml.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse %s: %v", compatCorpusPath, err)
	}
	if len(corpus.TestCases) == 0 {
		t.Fatalf("%s decoded to zero test cases", compatCorpusPath)
	}

	parser := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	referenced = map[string]bool{}
	usedVariants = map[string]bool{}
	parsed := 0
	for i, c := range corpus.TestCases {
		if c.Query == "" {
			t.Fatalf("%s: test case %d has an empty query; the decoder and the file shape "+
				"have drifted apart", compatCorpusPath, i)
		}
		query := expandCorpusVariants(t, c.Query, usedVariants)
		expr, err := parser.ParseExpr(query)
		if err != nil {
			// A query the corpus itself declares as failing may legitimately
			// be unparseable. Anything else means this gate silently stopped
			// inspecting a case, so it fails loudly.
			if c.ShouldFail {
				continue
			}
			t.Fatalf("%s: test case %d (%q) does not parse and is not marked should_fail: %v",
				compatCorpusPath, i, query, err)
		}
		parsed++
		collectSelectorNames(expr, referenced)
	}
	if parsed == 0 {
		t.Fatalf("%s: no test case parsed; this test would inspect nothing", compatCorpusPath)
	}
	return referenced, usedVariants
}

// expandCorpusVariants substitutes one representative value per `{{.var}}`
// placeholder, recording which variables were seen.
func expandCorpusVariants(t *testing.T, query string, usedVariants map[string]bool) string {
	t.Helper()

	return corpusVariantRE.ReplaceAllStringFunc(query, func(match string) string {
		variable := corpusVariantRE.FindStringSubmatch(match)[1]
		value, ok := corpusVariantSubstitutions[variable]
		if !ok {
			t.Fatalf("%s: query %q uses the unknown variant variable %q; "+
				"add it to corpusVariantSubstitutions", compatCorpusPath, query, variable)
		}
		usedVariants[variable] = true
		return value
	})
}

// collectSelectorNames walks a parsed PromQL expression and records the
// metric name of every vector selector that pins one. A selector whose
// `__name__` matcher is a regex contributes no name — correctly, since it
// selects whatever happens to exist rather than naming a family the seeder
// would have to provide.
func collectSelectorNames(expr promparser.Expr, into map[string]bool) {
	promparser.Inspect(expr, func(node promparser.Node, _ []promparser.Node) error {
		vs, ok := node.(*promparser.VectorSelector)
		if !ok {
			return nil
		}
		if vs.Name != "" {
			into[vs.Name] = true
			return nil
		}
		for _, matcher := range vs.LabelMatchers {
			if matcher.Name == model.MetricNameLabel && matcher.Type == labels.MatchEqual {
				into[matcher.Value] = true
			}
		}
		return nil
	})
}
