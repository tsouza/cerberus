package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/compatibility/loki/corpus"
)

// TestIsExpectedEmptyCase pins the corpus-tag contract that powers
// `compareOne`'s empty-result branch. A drift in the upstream YAML tag
// vocabulary (e.g. renaming the tag) would silently flip the failing
// `fast/basic-selectors.yaml#Log query with impossible filter` case
// back into a `baseline returned empty` row, so we anchor the predicate
// against the tag literal here.
func TestIsExpectedEmptyCase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags []string
		want bool
	}{
		{"tagged-empty-result", []string{"line-filter", "empty-result", "cache-test"}, true},
		{"tag-set-without-empty", []string{"line-filter", "regex"}, false},
		{"nil-tags", nil, false},
		{"empty-tags", []string{}, false},
		{"only-empty-result", []string{"empty-result"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isExpectedEmptyCase(corpus.Case{Tags: tc.tags})
			if got != tc.want {
				t.Fatalf("isExpectedEmptyCase(tags=%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

// TestScoreCounts asserts the score denominator includes every case
// the driver attempted, with the numerator being only the cases that
// passed. There is no allow-list / overlay exclusion: the harness
// carries no `should_skip` consumer code.
func TestScoreCounts(t *testing.T) {
	t.Parallel()
	results := []Result{
		{},                        // pass
		{},                        // pass
		{Diff: "x"},               // diff
		{UnexpectedFailure: "y"},  // unexpected failure
		{UnexpectedSuccess: true}, // unexpected success
	}
	passed, total := scoreCounts(results)
	if total != 5 {
		t.Fatalf("total = %d, want 5 (every attempted case counts)", total)
	}
	if passed != 2 {
		t.Fatalf("passed = %d, want 2", passed)
	}
}

func TestScoreCounts_AllPassing(t *testing.T) {
	t.Parallel()
	results := []Result{{}, {}, {}}
	passed, total := scoreCounts(results)
	if passed != 3 || total != 3 {
		t.Fatalf("(passed, total) = (%d, %d), want (3, 3)", passed, total)
	}
}

func TestScoreCounts_Empty(t *testing.T) {
	t.Parallel()
	passed, total := scoreCounts(nil)
	if passed != 0 || total != 0 {
		t.Fatalf("(passed, total) = (%d, %d), want (0, 0)", passed, total)
	}
}

// TestCheckExpansion pins the zero-expansion rail: a definition that
// expands to zero cases is a hard error naming the definition, while
// any positive expansion passes. The rail exists because the vendored
// registry once defaulted Directions before Kind, expanding eight
// metric-shaped exhaustive/aggregations.yaml definitions to zero cases
// — invisible to both the score denominator and the skip baseline.
func TestCheckExpansion(t *testing.T) {
	t.Parallel()
	def := corpus.Definition{
		Description: "ghost query",
		Source:      "exhaustive/aggregations.yaml:177",
		Kind:        "log",
	}
	err := checkExpansion(def, 0)
	if err == nil {
		t.Fatalf("checkExpansion(n=0) = nil, want hard error")
	}
	for _, want := range []string{"ghost query", "exhaustive/aggregations.yaml:177", "zero test cases"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
	for _, n := range []int{1, 2} {
		if err := checkExpansion(def, n); err != nil {
			t.Fatalf("checkExpansion(n=%d) = %v, want nil", n, err)
		}
	}
}

// The expanded-case identity of the full corpus — upstream plus
// cerberus-queries/, seed 42, against the committed dataset metadata —
// as caseIdentity computes it, one digest per lane. These digests were
// recorded from the loader the harness used before this one, so they
// pin that the current loader expands the corpus into exactly the same
// cases: same queries, windows, directions, steps, kinds, sources,
// descriptions and tags.
const (
	fullCorpusRangeCases       = 99
	fullCorpusRangeIdentity    = "bae65080643e2c0c36babcb31a73d0b448e72465653414b61fd315c59f5761a7"
	fullCorpusInstantCases     = 71
	fullCorpusInstantIdentity  = "a57eea66427951ac042100b205678e5d0ec42adbdda698061eba4e7580e09e3f"
	fullCorpusIdentitySeed     = 42
	kindlessMetricDefinitions  = 8
	kindlessMetricSourceFile   = "exhaustive/aggregations.yaml"
	kindlessMetricExpectedStep = time.Minute
)

// kindlessMetricKeys are the skip-baseline-style opaque keys of the
// definitions in exhaustive/aggregations.yaml that declare no `kind:`
// and are metric-shaped. Each must expand to one forward metric case.
var kindlessMetricKeys = []string{
	"upstream:a2fe120755aa3ea1",
	"upstream:135c2024b7b833cf",
	"upstream:93ddc31178a37475",
	"upstream:43e79f1e8ce3fd23",
	"upstream:61c90f8ea068118b",
	"upstream:d3c1f81481677bf2",
	"upstream:52e7891e0757e4ca",
	"upstream:f6de3f370e99021d",
}

// caseIdentity is the SHA-256 of the sorted wire form of every expanded
// case: the JSON of the report's TestCase, one per line.
func caseIdentity(t *testing.T, cases []loadedCase, isInstant bool) string {
	t.Helper()
	lines := make([]string, 0, len(cases))
	for _, lc := range cases {
		if lc.expandErr != nil {
			t.Fatalf("expansion error for %s: %v", lc.def.Source, lc.expandErr)
		}
		b, err := json.Marshal(newTestCase(lc.tc, lc.def.File(), isInstant))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		lines = append(lines, string(b))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// fullCorpusFlags points the loader at the real corpora: the upstream
// one resolved from the module cache (corpusDir left empty, exactly as
// the driver runs) and the committed cerberus-queries/ and metadata.
func fullCorpusFlags() flags {
	return flags{
		cerberusQueriesDir: filepath.Join("..", "..", "cerberus-queries"),
		metadataDir:        filepath.Join("..", ".."),
		seed:               fullCorpusIdentitySeed,
	}
}

// TestLoadCases_FullCorpusExpansion loads the upstream corpus from the
// module cache, the cerberus-queries/ additive corpus and the dataset
// metadata through the same path the driver uses, and requires the
// recorded case count and identity digest on both lanes. It also pins
// the kind inference directly: the eight kind-less metric-shaped
// definitions in exhaustive/aggregations.yaml each contribute exactly
// one forward metric case carrying the YAML's 1m step — a loader that
// defaulted their directions before their kind expanded them to zero
// cases.
func TestLoadCases_FullCorpusExpansion(t *testing.T) {
	t.Parallel()
	for _, lane := range []struct {
		instant  bool
		count    int
		identity string
	}{
		{false, fullCorpusRangeCases, fullCorpusRangeIdentity},
		{true, fullCorpusInstantCases, fullCorpusInstantIdentity},
	} {
		cases, err := loadCases(fullCorpusFlags(), lane.instant)
		if err != nil {
			t.Fatalf("loadCases(instant=%v): %v", lane.instant, err)
		}
		if len(cases) != lane.count {
			t.Fatalf("instant=%v: corpus expanded to %d cases, want %d", lane.instant, len(cases), lane.count)
		}
		if got := caseIdentity(t, cases, lane.instant); got != lane.identity {
			t.Fatalf("instant=%v: expanded-case identity = %s, want %s", lane.instant, got, lane.identity)
		}
	}

	cases, err := loadCases(fullCorpusFlags(), false)
	if err != nil {
		t.Fatalf("loadCases: %v", err)
	}
	byKey := make(map[string][]corpus.Case)
	for _, lc := range cases {
		if lc.def.File() == kindlessMetricSourceFile {
			byKey[baselineKey(lc.def)] = append(byKey[baselineKey(lc.def)], lc.tc)
		}
	}
	if len(kindlessMetricKeys) != kindlessMetricDefinitions {
		t.Fatalf("kindlessMetricKeys lists %d keys, want %d", len(kindlessMetricKeys), kindlessMetricDefinitions)
	}
	for _, key := range kindlessMetricKeys {
		tcs := byKey[key]
		if len(tcs) != 1 {
			t.Fatalf("definition %s expanded to %d cases, want exactly 1 (metric, forward)", key, len(tcs))
		}
		tc := tcs[0]
		if got := tc.Kind(); got != corpus.KindMetric {
			t.Fatalf("definition %s resolved to kind %q, want metric (query=%s)", key, got, tc.Query)
		}
		if tc.Direction != corpus.Forward {
			t.Fatalf("definition %s direction = %v, want FORWARD", key, tc.Direction)
		}
		if tc.Step != kindlessMetricExpectedStep {
			t.Fatalf("definition %s step = %v, want %v (from the YAML time_range.step)", key, tc.Step, kindlessMetricExpectedStep)
		}
	}
}

// TestCaseSet_UpstreamCasesAreOpaque pins the ledger contract: an
// upstream corpus case enters compat-cases.json under its opaque ID with
// its readable identity as the label, and a cerberus-owned case keeps
// its readable ID and carries no label.
func TestCaseSet_UpstreamCasesAreOpaque(t *testing.T) {
	t.Parallel()
	upstream := TestCase{Source: "fast/a.yaml", Kind: "log", Direction: "FORWARD", Query: `{a="b"}`, Description: "d", upstream: true}
	owned := TestCase{Source: "regression/own.yaml", Kind: "log", Direction: "FORWARD", Query: `{a="b"}`, Description: "d"}
	got := caseSet([]Result{{TestCase: upstream}, {TestCase: owned, Diff: "x"}})
	if len(got) != 2 {
		t.Fatalf("caseSet returned %d cases, want 2", len(got))
	}
	if got[0].ID != corpus.OpaqueID(upstream.id()) || got[0].Label != upstream.id() || !got[0].Passed {
		t.Fatalf("upstream case = %+v, want ID %q labelled %q, passed", got[0], corpus.OpaqueID(upstream.id()), upstream.id())
	}
	if got[1].ID != owned.id() || got[1].Label != "" || got[1].Passed {
		t.Fatalf("cerberus-owned case = %+v, want readable ID %q, no label, failed", got[1], owned.id())
	}
}

// TestLoadCases_MarksOnlyUpstreamCases — every case expanded from the
// upstream corpus is marked upstream, and no cerberus-queries/ case is,
// so the opaque-ID split in caseSet follows provenance exactly.
func TestLoadCases_MarksOnlyUpstreamCases(t *testing.T) {
	t.Parallel()
	upstreamDefs, err := loadUpstreamQueries(fullCorpusFlags())
	if err != nil {
		t.Fatalf("loadUpstreamQueries: %v", err)
	}
	upstreamSources := make(map[string]bool, len(upstreamDefs))
	for _, def := range upstreamDefs {
		upstreamSources[def.Source] = true
	}
	cases, err := loadCases(fullCorpusFlags(), false)
	if err != nil {
		t.Fatalf("loadCases: %v", err)
	}
	var upstream, owned int
	for _, lc := range cases {
		if lc.upstream {
			upstream++
		} else {
			owned++
		}
		if lc.upstream != upstreamSources[lc.def.Source] {
			t.Fatalf("case from %s marked upstream=%v, but the upstream corpus %s that source", lc.def.Source, lc.upstream, map[bool]string{true: "has", false: "does not have"}[upstreamSources[lc.def.Source]])
		}
	}
	if upstream == 0 || owned == 0 {
		t.Fatalf("upstream cases = %d, cerberus-owned cases = %d; both corpora must contribute", upstream, owned)
	}
}

// TestBaselineKey pins the committed key of a skipped definition: the
// opaque ID of `<suite>/<file>#<description>`, with the line suffix
// stripped so a reordering of the YAML does not move the key, and with
// no corpus text left in it.
func TestBaselineKey(t *testing.T) {
	t.Parallel()
	withLine := corpus.Definition{Source: "fast/a.yaml:12", Description: "first definition"}
	moved := corpus.Definition{Source: "fast/a.yaml:40", Description: "first definition"}
	other := corpus.Definition{Source: "fast/a.yaml:12", Description: "second definition"}
	key := baselineKey(withLine)
	if key != corpus.OpaqueID("fast/a.yaml#first definition") {
		t.Fatalf("baselineKey = %q, want the opaque ID of the readable key", key)
	}
	if strings.Contains(key, "first definition") || strings.Contains(key, "a.yaml") {
		t.Fatalf("baselineKey %q carries corpus text", key)
	}
	if baselineKey(moved) != key {
		t.Fatalf("moving a definition within its file changed its key")
	}
	if baselineKey(other) == key {
		t.Fatalf("two descriptions in one file share the key %q", key)
	}
}

// TestReadSkipBaseline_IgnoresCommentsAndBlanks pins the parser
// contract — comment + blank lines stay out of the resulting set so
// the file can carry rationale prose.
func TestReadSkipBaseline_IgnoresCommentsAndBlanks(t *testing.T) {
	t.Parallel()
	body := "# leading comment\n" +
		"# another comment\n" +
		"\n" +
		"fast/foo.yaml#alpha\n" +
		"\n" +
		"   # indented comment\n" +
		"regression/bar.yaml#beta\n" +
		"exhaustive/baz.yaml#gamma\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	got, err := readSkipBaseline(path)
	if err != nil {
		t.Fatalf("readSkipBaseline: %v", err)
	}
	want := []string{
		"exhaustive/baz.yaml#gamma",
		"fast/foo.yaml#alpha",
		"regression/bar.yaml#beta",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCheckSkipBaseline_MatchPasses confirms an exact set match returns
// nil — the no-drift path.
func TestCheckSkipBaseline_MatchPasses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	defs := []corpus.Definition{
		{Source: "fast/a.yaml:3", Description: "one"},
		{Source: "regression/b.yaml:7", Description: "two"},
	}
	body := baselineKey(defs[0]) + "\n" + baselineKey(defs[1]) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := checkSkipBaseline(path, defs); err != nil {
		t.Fatalf("checkSkipBaseline returned err: %v", err)
	}
}

// TestCheckSkipBaseline_DetectsAddition pins the trip-wire: a new
// upstream entry not in the baseline fails with the new key named.
func TestCheckSkipBaseline_DetectsAddition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	defs := []corpus.Definition{
		{Source: "fast/a.yaml:3", Description: "one"},
		{Source: "regression/new.yaml:11", Description: "fresh entry"},
	}
	body := baselineKey(defs[0]) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	err := checkSkipBaseline(path, defs)
	if err == nil {
		t.Fatalf("checkSkipBaseline = nil, want drift error")
	}
	// The added entry is named by its key and, for the operator, by its
	// readable identity; the removed direction can only name the key.
	for _, want := range []string{baselineKey(defs[1]), "regression/new.yaml#fresh entry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestCheckSkipBaseline_DetectsRemoval mirrors the addition case for
// the other direction — an upstream re-enable surfaces.
func TestCheckSkipBaseline_DetectsRemoval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	gone := baselineKey(corpus.Definition{Source: "regression/gone.yaml:5", Description: "two"})
	defs := []corpus.Definition{
		{Source: "fast/a.yaml:3", Description: "one"},
	}
	body := baselineKey(defs[0]) + "\n" + gone + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	err := checkSkipBaseline(path, defs)
	if err == nil {
		t.Fatalf("checkSkipBaseline = nil, want drift error")
	}
	if !strings.Contains(err.Error(), gone) {
		t.Fatalf("error %q missing removed key", err.Error())
	}
}

// TestWriteSkipBaseline_RoundTripsThroughRead confirms the writer
// emits a file the reader parses back to the same sorted set, so
// -regen-baseline + the diff path are self-consistent.
func TestWriteSkipBaseline_RoundTripsThroughRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	defs := []corpus.Definition{
		{Source: "regression/b.yaml:7", Description: "two"},
		{Source: "fast/a.yaml:3", Description: "one"},
		{Source: "exhaustive/c.yaml:99", Description: "three"},
	}
	if err := writeSkipBaseline(path, defs); err != nil {
		t.Fatalf("writeSkipBaseline: %v", err)
	}
	got, err := readSkipBaseline(path)
	if err != nil {
		t.Fatalf("readSkipBaseline: %v", err)
	}
	want := []string{baselineKey(defs[0]), baselineKey(defs[1]), baselineKey(defs[2])}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCompareAll_CarriesProvenanceIntoTheLedger — compareAll hands each
// case's provenance to its result on both paths (an expansion failure
// and a case that reached the wire), so caseSet keys an upstream case
// opaquely and a cerberus-owned case readably.
func TestCompareAll_CarriesProvenanceIntoTheLedger(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wire := func(desc string) corpus.Case {
		return corpus.Case{Query: `{a="b"}`, Start: start, End: start.Add(time.Hour), Direction: corpus.Forward, Source: "fast/a.yaml:1", Description: desc}
	}
	cases := []loadedCase{
		{def: corpus.Definition{Source: "fast/a.yaml:1", Description: "upstream wire"}, tc: wire("upstream wire"), upstream: true},
		{def: corpus.Definition{Source: "fast/a.yaml:5", Description: "upstream expansion", Query: "${SELECTOR}"}, upstream: true, expandErr: errors.New("no selector")},
		{def: corpus.Definition{Source: "regression/own.yaml:1", Description: "owned wire"}, tc: wire("owned wire")},
	}
	f := flags{addr1: srv.URL, addr2: srv.URL, timeout: 5 * time.Second, parallelism: 2}
	results := compareAll(cases, f, false)
	for i, want := range []bool{true, true, false} {
		if results[i].TestCase.upstream != want {
			t.Fatalf("result %d (%s) upstream = %v, want %v", i, results[i].TestCase.Description, results[i].TestCase.upstream, want)
		}
	}
	var opaque, readable []string
	for _, c := range caseSet(results) {
		if strings.HasPrefix(c.ID, "upstream:") {
			opaque = append(opaque, c.ID)
		} else {
			readable = append(readable, c.ID)
		}
	}
	if len(opaque) != 2 || len(readable) != 1 || !strings.Contains(readable[0], "owned wire") {
		t.Fatalf("ledger IDs: opaque %v, readable %v; want the two upstream cases opaque and only the owned case readable", opaque, readable)
	}
}
