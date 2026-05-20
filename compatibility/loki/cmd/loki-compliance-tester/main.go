// Command loki-compliance-tester runs the LogQL compatibility corpus
// (vendored from grafana/loki:pkg/logql/bench/) against two live Loki
// HTTP endpoints, diffs the responses with float tolerance, and emits a
// JSON report whose shape matches compatibility/prometheus's
// promql-compliance-tester (i.e. the `prometheus/compliance` reference
// driver). It also emits a shields.io endpoint-badge score JSON when
// `-score` is set.
//
// This is PR 5 of docs/loki-compliance-plan.md — the cerberus-owned
// replacement for the `go test -tags=remote_correctness -c` approach
// wired in PR 3 (#387). The previous approach piped `go test -v` lines
// into a `.json` file; downstream tooling (informational CI lane, future
// expected-failures triage) needs the same structured report the Prom
// harness ships so a single reconciliation script can consume both.
//
// Per task #68, the driver is report-only: it always exits 0 on the
// parity-drift path. Diffs, unexpected failures, and unexpected
// successes are recorded in the JSON report AND included in the
// compat-score JSON's denominator, but they do not change the exit
// code. Only driver-wide hard errors (corpus load, file write) escalate
// to a non-zero rc. The pre-#68 `os.Exit(1)` on `pass != len(results)`
// was deleted at the same time.
//
// Lifecycle:
//
//  1. Loads the upstream bench package's QueryRegistry + DatasetMetadata
//     resolver. Reuses upstream code so template-variable expansion
//     (`${SELECTOR}` / `${LABEL_NAME}` / `${RANGE}`) tracks Grafana's
//     reference semantics exactly — cerberus-side divergence here would
//     defeat the differential test.
//  2. Loads the optional cerberus-side overlay (cerberus-test-queries.yml)
//     and indexes its `should_skip` + `should_fail` entries by
//     `<suite>/<file>.yaml#<description>` keys.
//  3. For each expanded test case, fans out parallel `/loki/api/v1/query`
//     or `/query_range` calls against both endpoints, decodes the
//     responses into a typed value, normalises ordering, and diffs with
//     a configurable epsilon.
//  4. Writes the report to `-report` (default: stdout) in the Prom-shape
//     JSON envelope (`{totalResults, includePassing, results: [...]}`),
//     with each result carrying `testCase`, `diff`, `unexpectedFailure`,
//     `unexpectedSuccess`, `unsupported`. When `-score` is set, also
//     writes a shields.io endpoint-badge JSON to that path.
//
// The binary imports the vendored upstream/loki-bench/ package; the
// root go.mod marks that path `ignore` so it's excluded from
// `go build ./...` walks, but the package itself is reachable via its
// import path because every transitive dep (`logproto`, `logql/syntax`,
// `yaml.v3`) is already a direct entry in cerberus's go.mod. A plain
// `go build` resolves the binary without needing the `-mod=mod`
// promotion the PR 3 `go test -c` build relied on.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/compatibility/internal/score"
	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("loki-compliance-tester: %v", err)
	}
}

// flags collects all CLI / env knobs in one place.
type flags struct {
	addr1       string
	addr2       string
	corpusDir   string
	metadataDir string
	overlayPath string
	reportPath  string
	scorePath   string
	tolerance   float64
	rangeType   string
	seed        int64
	parallelism int
	includeSkip bool
	timeout     time.Duration
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.addr1, "addr-1", "", "Address of baseline (reference) Loki instance, e.g. http://localhost:23100")
	flag.StringVar(&f.addr2, "addr-2", "", "Address of test (cerberus) Loki-API instance, e.g. http://localhost:29092")
	flag.StringVar(&f.corpusDir, "corpus", "./queries", "Path to the vendored bench/queries/ directory (suite subdirs: fast/, regression/, exhaustive/)")
	flag.StringVar(&f.metadataDir, "metadata-dir", ".", "Directory containing dataset_metadata.json")
	flag.StringVar(&f.overlayPath, "overlay", "", "Optional cerberus-test-queries.yml overlay (skip/fail per source key)")
	flag.StringVar(&f.reportPath, "report", "", "Report output path; empty writes to stdout")
	flag.StringVar(&f.scorePath, "score", "", "shields.io endpoint-badge score JSON output path; empty means do not write")
	flag.Float64Var(&f.tolerance, "tolerance", 1e-5, "Float comparison tolerance (matches upstream remote_test.go default)")
	flag.StringVar(&f.rangeType, "range-type", "range", "Query range type: 'range' or 'instant'")
	flag.Int64Var(&f.seed, "seed", 42, "Random seed for query template resolution (matches upstream default)")
	flag.IntVar(&f.parallelism, "parallelism", 8, "Maximum number of comparison queries to run in parallel")
	flag.BoolVar(&f.includeSkip, "include-skipped", false, "Include queries the upstream YAML marks `skip: true`")
	flag.DurationVar(&f.timeout, "timeout", 30*time.Second, "Per-request HTTP timeout for each Loki endpoint")
	flag.Parse()
	return f
}

func run() error {
	f := parseFlags()
	if f.addr1 == "" || f.addr2 == "" {
		return errors.New("both -addr-1 and -addr-2 must be set")
	}
	isInstant := f.rangeType == "instant"

	overlay, err := loadOverlay(f.overlayPath)
	if err != nil {
		return fmt.Errorf("loading overlay: %w", err)
	}

	cases, err := loadCases(f, isInstant)
	if err != nil {
		return fmt.Errorf("loading cases: %w", err)
	}

	results := compareAll(cases, f, overlay, isInstant)

	report := Report{
		TotalResults:   len(results),
		IncludePassing: true,
		Results:        results,
	}
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling report: %w", err)
	}

	if err := writeReport(f.reportPath, out); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}

	// Summary stderr line: mirrors the Prom run script's jq summary so
	// the run script can `tee | jq` regardless of which tester it ran.
	pass, diffs, unfail, unsupp := summarise(results)
	fmt.Fprintf(os.Stderr, "==> summary: total=%d passed=%d diffs=%d unexpected_failures=%d unsupported=%d\n",
		len(results), pass, diffs, unfail, unsupp)

	// Per task #68, the driver is report-only: parity drift is captured
	// in the JSON report + compat-score.json, but never in the exit
	// code. Hard errors (corpus load failure, file write failure) still
	// return a non-zero rc — those are infrastructure failures, not
	// parity drift. A previous revision called os.Exit(1) when any case
	// diff'd; that semantic moved out of the driver and is now derived
	// from compat-score.json by downstream tooling.
	if f.scorePath != "" {
		// The score's denominator includes every case the driver
		// attempted that wasn't an overlay-driven skip. Diffs,
		// unexpected failures (including baseline failures + unsupported
		// markers), and unexpected successes all contribute to total
		// but not to passed. Overlay-skipped cases are excluded entirely:
		// they're documented as out-of-scope rather than as parity
		// gaps, so counting them would penalise the score for
		// intentional exclusions.
		passed, total := scoreCounts(results)
		s := score.Compute("LogQL compat", passed, total)
		if err := score.Write(f.scorePath, s); err != nil {
			return fmt.Errorf("writing score: %w", err)
		}
		fmt.Fprintf(os.Stderr, "==> score: passed=%d total=%d percent=%.2f color=%s -> %s\n",
			passed, total, s.Percent, s.Color, f.scorePath)
	}
	return nil
}

// scoreCounts derives (passed, total) for the compat-score JSON.
// Overlay-driven skips (cases the cerberus-test-queries.yml flagged as
// out-of-scope) are excluded from both counts so they don't penalise
// the score; everything else (passes, diffs, unexpected failures /
// successes) contributes to the denominator.
func scoreCounts(results []Result) (passed, total int) {
	for _, r := range results {
		if r.SkipReason != "" {
			continue
		}
		total++
		if r.success() {
			passed++
		}
	}
	return passed, total
}

// Overlay mirrors the cerberus-test-queries.yml schema documented in
// compatibility/loki/cerberus-test-queries.yml. The runner
// consults `ShouldSkip` to decide whether to include a case (when
// `-include-skipped=false`); the `ShouldFail` slot is plumbed for
// future use (analogue of prometheus/compliance's `should_fail` —
// expected hard-failure entries flip `unexpectedSuccess` semantics).
type Overlay struct {
	ShouldSkip []OverlayEntry `yaml:"should_skip"`
	ShouldFail []OverlayEntry `yaml:"should_fail"`
}

// OverlayEntry is a single overlay row. `Source` is the lookup key in
// the form `<suite>/<file>.yaml#<description>`; `Reason` documents
// rationale; the `Since` / `Jira` fields are for drift audits.
type OverlayEntry struct {
	Source string `yaml:"source"`
	Reason string `yaml:"reason"`
	Since  string `yaml:"since,omitempty"`
	Jira   string `yaml:"jira,omitempty"`
}

func loadOverlay(path string) (*Overlay, error) {
	if path == "" {
		return &Overlay{}, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // CLI-supplied overlay path; harness tool
	if err != nil {
		return nil, err
	}
	var o Overlay
	if err := yaml.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// skipKey indexes an overlay entry by its (suite, file, description)
// triple. The bench package stores `Source` as `<suite>/<file>.yaml:<line>`
// — we re-derive the description-shaped key (`<suite>/<file>.yaml#<desc>`)
// at lookup time to match the overlay's documented schema.
func (o *Overlay) skipKey(suiteFile, description string) (string, bool) {
	key := suiteFile + "#" + description
	for _, e := range o.ShouldSkip {
		if e.Source == key {
			return e.Reason, true
		}
	}
	return "", false
}

// loadCases reuses the upstream bench loader so template expansion
// stays in lock-step with grafana/loki:pkg/logql/bench/. Instant mode
// mirrors remote_test.go: keep only metric queries, collapse range to
// a point.
//
// Expansion errors are recorded as `loadErr` against the originating
// QueryDefinition rather than bubbled up — a single corpus-wide failure
// (e.g. one query template referencing a label the seeded fixture
// doesn't carry) would otherwise abort the whole run. The caller
// converts each loadErr into an `UnexpectedFailure` result so the
// report still emits and the operator can triage per-query.
func loadCases(f flags, isInstant bool) ([]loadedCase, error) {
	metadata, err := bench.LoadMetadata(f.metadataDir)
	if err != nil {
		return nil, fmt.Errorf("LoadMetadata(%s): %w", f.metadataDir, err)
	}

	registry := bench.NewQueryRegistry(f.corpusDir)
	suites := []bench.Suite{bench.SuiteFast, bench.SuiteRegression, bench.SuiteExhaustive}
	if err := registry.Load(suites...); err != nil {
		return nil, fmt.Errorf("registry.Load: %w", err)
	}

	resolver := bench.NewMetadataVariableResolver(metadata, f.seed)
	defs := registry.GetQueries(f.includeSkip, suites...)

	var cases []loadedCase
	for _, def := range defs {
		expanded, err := registry.ExpandQuery(def, resolver, isInstant)
		if err != nil {
			cases = append(cases, loadedCase{
				def:       def,
				expandErr: fmt.Errorf("expand %q: %w", def.Description, err),
			})
			continue
		}
		for _, tc := range expanded {
			cases = append(cases, loadedCase{def: def, tc: tc})
		}
	}

	if isInstant {
		filtered := cases[:0]
		for _, lc := range cases {
			if lc.expandErr != nil || lc.tc.Kind() == "metric" {
				if lc.expandErr == nil {
					lc.tc.Start = lc.tc.End
					lc.tc.Step = 0
				}
				filtered = append(filtered, lc)
			}
		}
		cases = filtered
	}
	return cases, nil
}

// loadedCase carries either a fully-expanded TestCase or an expansion
// failure (with the originating QueryDefinition for context). Carrying
// both via the same slot lets compareAll emit a result row even for
// queries that never reached the wire.
type loadedCase struct {
	def       bench.QueryDefinition
	tc        bench.TestCase
	expandErr error
}

// summarise tallies the four headline counters reported on stderr.
func summarise(results []Result) (pass, diffs, unfail, unsupp int) {
	for _, r := range results {
		switch {
		case r.UnexpectedFailure != "":
			unfail++
			if r.Unsupported {
				unsupp++
			}
		case r.Diff != "":
			diffs++
		case r.UnexpectedSuccess:
			// Counted as a diff for summary purposes (the case was
			// expected to fail but didn't).
			diffs++
		default:
			pass++
		}
	}
	return pass, diffs, unfail, unsupp
}

func writeReport(path string, payload []byte) error {
	if path == "" {
		_, err := os.Stdout.Write(payload)
		if err == nil {
			fmt.Println()
		}
		return err
	}
	return os.WriteFile(path, payload, 0o600)
}

// compareAll runs every test case in parallel and collects results.
// Concurrency is capped at `parallelism` via a buffered work-token
// channel — same shape as the Prom tester's main.go.
//
// Note: this used to maintain an `allSuccess` flag for the
// "exit non-zero on any diff" semantic the driver shipped before task
// #68. Under report-only, the driver always returns 0 from run() on
// the parity-drift path; the score JSON + the per-result fields carry
// the signal. The pre-#68 `allSuccess atomic.Bool` was removed at the
// same time the exit-on-diff branch was deleted.
func compareAll(cases []loadedCase, f flags, overlay *Overlay, isInstant bool) []Result {
	httpClient := &http.Client{Timeout: f.timeout}
	results := make([]Result, len(cases))

	workCh := make(chan struct{}, max(1, f.parallelism))
	var wg sync.WaitGroup

	for i, lc := range cases {
		i, lc := i, lc

		// Resolve the overlay key using the upstream QueryDefinition.
		// Both expanded and expansion-failed cases share the same
		// (Source, Description) pair; resolving up-front means a
		// skip entry can suppress either kind of failure without
		// having to know the expansion outcome.
		suiteFile := stripSourceLine(lc.def.Source)
		reason, isSkipped := overlay.skipKey(suiteFile, lc.def.Description)

		// Expansion failures: emit a synthetic TestCase from the
		// upstream QueryDefinition (no time range available) and
		// surface the error as UnexpectedFailure unless an overlay
		// entry has explicitly skipped this case.
		if lc.expandErr != nil {
			tc := TestCase{
				Query:       lc.def.Query,
				Source:      suiteFile,
				Description: lc.def.Description,
				Kind:        lc.def.Kind,
				Direction:   string(lc.def.Directions),
			}
			if isSkipped {
				results[i] = Result{TestCase: tc, SkipReason: reason}
				continue
			}
			results[i] = Result{TestCase: tc, UnexpectedFailure: "expansion: " + lc.expandErr.Error()}
			continue
		}

		// Apply overlay skip — skipped cases never hit the wire.
		if isSkipped {
			results[i] = Result{
				TestCase: newTestCase(lc.tc, suiteFile, isInstant),
				// `Diff` empty + `UnexpectedFailure` empty +
				// `UnexpectedSuccess=false` means "passed"; we use
				// `SkipReason` to surface the documented rationale.
				// The shape stays compatible with the Prom envelope
				// by not flipping any of the failure flags.
				SkipReason: reason,
			}
			continue
		}

		wg.Add(1)
		workCh <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-workCh }()

			results[i] = compareOne(httpClient, f, lc.tc, suiteFile, isInstant)
		}()
	}

	wg.Wait()
	return results
}

// compareOne performs the per-case differential: fans out concurrent
// requests to both endpoints, decodes, normalises, diffs.
func compareOne(c *http.Client, f flags, tc bench.TestCase, suiteFile string, isInstant bool) Result {
	tcOut := newTestCase(tc, suiteFile, isInstant)
	result := Result{TestCase: tcOut}

	type fetched struct {
		value typedResult
		err   error
	}
	out := make([]fetched, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	for idx, addr := range []string{f.addr1, f.addr2} {
		idx, addr := idx, addr
		go func() {
			defer wg.Done()
			v, err := queryOne(c, addr, tc, isInstant)
			out[idx] = fetched{value: v, err: err}
		}()
	}
	wg.Wait()

	refErr, testErr := out[0].err, out[1].err
	switch {
	case refErr != nil:
		// Baseline failure isn't a cerberus regression — treat as harness
		// glitch. We surface it as UnexpectedFailure so the operator
		// sees it but flag Unsupported=false (it's not a 501).
		result.UnexpectedFailure = fmt.Sprintf("reference (-addr-1) failed: %v", refErr)
		return result
	case testErr != nil:
		result.UnexpectedFailure = testErr.Error()
		result.Unsupported = isUnsupportedErr(testErr)
		return result
	}

	expected := normaliseTypedResult(out[0].value)
	actual := normaliseTypedResult(out[1].value)
	// Cases tagged `empty-result` in the upstream YAML are designed to
	// return no rows (e.g. `${SELECTOR} |= "this will not hit any line"`
	// in fast/basic-selectors.yaml — the filter literal can never match
	// a seeded log line by construction). For those cases a baseline-
	// empty response is the expected outcome, so we treat
	// baseline-empty + actual-empty as a parity pass and flip the
	// diff direction: actual-non-empty means cerberus returned rows
	// reference Loki did not, which is a real shape mismatch.
	if isExpectedEmptyCase(tc) {
		switch {
		case expected.isEmpty() && actual.isEmpty():
			return result
		case expected.isEmpty() && !actual.isEmpty():
			result.Diff = "baseline empty (expected) but test endpoint returned rows"
			return result
		case !expected.isEmpty() && actual.isEmpty():
			result.UnexpectedFailure = "test endpoint returned empty"
			return result
		}
		// Fall through to the diff path when both sides have rows —
		// the case may carry the tag for the cache exercise reason
		// even though the seeded data happens to flow through.
	}
	if expected.isEmpty() {
		// Same convention as upstream `assertResultNotEmpty`: we don't
		// flip a comparison failure on an empty baseline because the
		// upstream test framework treats it as a setup error. Report
		// it explicitly so the overlay author can suppress.
		result.UnexpectedFailure = "baseline returned empty"
		return result
	}
	if actual.isEmpty() {
		result.UnexpectedFailure = "test endpoint returned empty"
		return result
	}
	if diff := diffTyped(expected, actual, f.tolerance); diff != "" {
		result.Diff = diff
	}
	return result
}

// isExpectedEmptyCase returns true when the upstream YAML tags this
// case as intentionally empty. The fast/basic-selectors.yaml entry
// `Log query with impossible filter ...` is the canonical example —
// the corpus author plugs in a filter literal that cannot match any
// seeded line. Without this signal the harness would flag every such
// case as `baseline returned empty` and force a should_skip overlay,
// turning an honest differential ("both endpoints agree on empty")
// into a silenced row.
func isExpectedEmptyCase(tc bench.TestCase) bool {
	return slices.Contains(tc.Tags, "empty-result")
}

// stripSourceLine strips the trailing `:<line>` from a bench source path.
// Upstream stores `Source` as `fast/basic-selectors.yaml:6`; the overlay
// keys + the report's `testCase.source` field use the line-less form so
// they stay stable across upstream re-orderings.
func stripSourceLine(src string) string {
	if i := strings.LastIndex(src, ":"); i >= 0 {
		// Validate the suffix is a number; otherwise leave the string
		// untouched. Source paths shouldn't carry a colon other than
		// the line suffix, so this is purely defensive.
		if _, err := strconv.Atoi(src[i+1:]); err == nil {
			return src[:i]
		}
	}
	return src
}

func newTestCase(tc bench.TestCase, suiteFile string, isInstant bool) TestCase {
	out := TestCase{
		Query:       tc.Query,
		Source:      suiteFile,
		Description: tc.QueryDesc,
		Kind:        tc.Kind(),
		Direction:   tc.Direction.String(),
		Start:       tc.Start.UTC().Format(time.RFC3339Nano),
		End:         tc.End.UTC().Format(time.RFC3339Nano),
		Instant:     isInstant,
		Tags:        tc.Tags,
	}
	if tc.Step > 0 {
		out.Step = tc.Step.String()
	}
	return out
}

// isUnsupportedErr classifies a Loki error as "feature not supported"
// for the report's Unsupported flag. The Prom analogue keys on HTTP 501;
// Loki uses a mix of 400-with-"not implemented" + 501 + 400-with-parser
// errors. We keep the predicate conservative — anything not clearly a
// shape mismatch is treated as supported.
func isUnsupportedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "status=501") ||
		strings.Contains(s, "not implemented") ||
		strings.Contains(s, "unsupported")
}

// ----- HTTP layer ----------------------------------------------------

// queryOne issues a single instant or range query against `addr` and
// returns the decoded result. Instant queries hit /loki/api/v1/query
// only when the TestCase represents one (metric query collapsed to a
// point); everything else routes to /query_range, matching the
// upstream test's queryRemote() function.
func queryOne(c *http.Client, addr string, tc bench.TestCase, isInstant bool) (typedResult, error) {
	const limit = 1000
	base := strings.TrimRight(addr, "/")

	if isInstant && tc.Kind() == "metric" && tc.Start.Equal(tc.End) {
		u := base + "/loki/api/v1/query"
		params := url.Values{}
		params.Set("query", tc.Query)
		params.Set("time", strconv.FormatInt(tc.End.UnixNano(), 10))
		params.Set("limit", strconv.Itoa(limit))
		params.Set("direction", tc.Direction.String())
		return doQuery(c, u, params)
	}

	u := base + "/loki/api/v1/query_range"
	params := url.Values{}
	params.Set("query", tc.Query)
	params.Set("start", strconv.FormatInt(tc.Start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(tc.End.UnixNano(), 10))
	params.Set("limit", strconv.Itoa(limit))
	params.Set("direction", tc.Direction.String())
	if tc.Step > 0 {
		params.Set("step", strconv.FormatFloat(tc.Step.Seconds(), 'f', -1, 64))
	}
	return doQuery(c, u, params)
}

func doQuery(c *http.Client, u string, params url.Values) (typedResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
	if err != nil {
		return typedResult{}, fmt.Errorf("new request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return typedResult{}, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		return typedResult{}, fmt.Errorf("read body: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		// Truncate the error body so a wall-of-text upstream stack
		// trace doesn't dominate the diff column.
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 400 {
			snippet = snippet[:400] + "…"
		}
		return typedResult{}, fmt.Errorf("status=%d body=%s", resp.StatusCode, snippet)
	}

	return decodeResponse(body)
}

// ----- response decoder ---------------------------------------------

// typedResult is a tagged union over the four Loki/PromQL result types.
// We model it as an opaque struct rather than a sealed interface to
// keep the decoder allocation-light and the diff loop branch-free.
type typedResult struct {
	kind     string // "streams" | "vector" | "matrix" | "scalar" | ""
	streams  []decodedStream
	vector   []decodedSample
	matrix   []decodedSeries
	scalar   decodedSample
	hasValue bool
}

func (t typedResult) isEmpty() bool {
	switch t.kind {
	case "streams":
		return len(t.streams) == 0
	case "vector":
		return len(t.vector) == 0
	case "matrix":
		// Mirror upstream: a matrix with series-but-no-points is empty.
		if len(t.matrix) == 0 {
			return true
		}
		for _, s := range t.matrix {
			if len(s.Floats) > 0 {
				return false
			}
		}
		return true
	case "scalar":
		return !t.hasValue
	}
	return true
}

type decodedStream struct {
	Labels  map[string]string // canonical label set (sorted at compare time)
	Entries []logEntry
}

type logEntry struct {
	Timestamp int64 // unix nanos
	Line      string
}

type decodedSample struct {
	Metric map[string]string
	T      int64 // unix ms (matches Prom's promql.Sample.T)
	F      float64
}

type decodedSeries struct {
	Metric map[string]string
	Floats []decodedPoint
}

type decodedPoint struct {
	T int64
	F float64
}

// decodeResponse parses Loki's `{status, data: {resultType, result}}`
// envelope into a typedResult. The `result` decoder is type-driven by
// `resultType` since each shape has its own JSON layout.
func decodeResponse(body []byte) (typedResult, error) {
	var env struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return typedResult{}, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Status != "" && env.Status != "success" {
		return typedResult{}, fmt.Errorf("loki status=%q", env.Status)
	}

	switch env.Data.ResultType {
	case "streams":
		return decodeStreams(env.Data.Result)
	case "vector":
		return decodeVector(env.Data.Result)
	case "matrix":
		return decodeMatrix(env.Data.Result)
	case "scalar":
		return decodeScalar(env.Data.Result)
	default:
		return typedResult{}, fmt.Errorf("unknown resultType %q", env.Data.ResultType)
	}
}

func decodeStreams(raw json.RawMessage) (typedResult, error) {
	var arr []struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return typedResult{}, fmt.Errorf("decode streams: %w", err)
	}
	out := typedResult{kind: "streams"}
	for _, s := range arr {
		stream := decodedStream{Labels: s.Stream}
		for _, v := range s.Values {
			ts, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				return typedResult{}, fmt.Errorf("decode stream timestamp %q: %w", v[0], err)
			}
			stream.Entries = append(stream.Entries, logEntry{Timestamp: ts, Line: v[1]})
		}
		out.streams = append(out.streams, stream)
	}
	return out, nil
}

func decodeVector(raw json.RawMessage) (typedResult, error) {
	var arr []struct {
		Metric map[string]string  `json:"metric"`
		Value  [2]json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return typedResult{}, fmt.Errorf("decode vector: %w", err)
	}
	out := typedResult{kind: "vector"}
	for _, s := range arr {
		ts, f, err := decodeSamplePair(s.Value)
		if err != nil {
			return typedResult{}, err
		}
		out.vector = append(out.vector, decodedSample{Metric: s.Metric, T: ts, F: f})
	}
	return out, nil
}

func decodeMatrix(raw json.RawMessage) (typedResult, error) {
	var arr []struct {
		Metric map[string]string    `json:"metric"`
		Values [][2]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return typedResult{}, fmt.Errorf("decode matrix: %w", err)
	}
	out := typedResult{kind: "matrix"}
	for _, s := range arr {
		series := decodedSeries{Metric: s.Metric}
		for _, pair := range s.Values {
			ts, f, err := decodeSamplePair(pair)
			if err != nil {
				return typedResult{}, err
			}
			series.Floats = append(series.Floats, decodedPoint{T: ts, F: f})
		}
		out.matrix = append(out.matrix, series)
	}
	return out, nil
}

func decodeScalar(raw json.RawMessage) (typedResult, error) {
	var pair [2]json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil {
		return typedResult{}, fmt.Errorf("decode scalar: %w", err)
	}
	ts, f, err := decodeSamplePair(pair)
	if err != nil {
		return typedResult{}, err
	}
	return typedResult{kind: "scalar", scalar: decodedSample{T: ts, F: f}, hasValue: true}, nil
}

// decodeSamplePair parses a [<ts>, <value>] pair where `ts` is a Loki
// timestamp (number or string-of-number) and `value` is a string-encoded
// float (Prom convention). NaN / +Inf / -Inf round-trip correctly via
// strconv.ParseFloat.
func decodeSamplePair(pair [2]json.RawMessage) (int64, float64, error) {
	var ts int64
	// Timestamp can come back as either a JSON number or a string;
	// try string first since that's the Loki convention.
	var tsStr string
	if err := json.Unmarshal(pair[0], &tsStr); err == nil {
		// Loki returns floating-point seconds; convert to unix ms so
		// the diff comparator can deal in integers.
		secs, err := strconv.ParseFloat(tsStr, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("ts parse %q: %w", tsStr, err)
		}
		ts = int64(secs * 1000)
	} else {
		var tsNum float64
		if err := json.Unmarshal(pair[0], &tsNum); err != nil {
			return 0, 0, fmt.Errorf("ts decode: %w", err)
		}
		ts = int64(tsNum * 1000)
	}

	var fStr string
	if err := json.Unmarshal(pair[1], &fStr); err != nil {
		return 0, 0, fmt.Errorf("value decode: %w", err)
	}
	f, err := strconv.ParseFloat(fStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("value parse %q: %w", fStr, err)
	}
	return ts, f, nil
}

// ----- normalise + diff ---------------------------------------------

// normaliseTypedResult applies the same ordering the upstream test does
// (sortVector / sortMatrix / sortStreams). Diff is order-sensitive so
// canonicalisation has to happen before comparison.
func normaliseTypedResult(t typedResult) typedResult {
	switch t.kind {
	case "vector":
		sort.SliceStable(t.vector, func(i, j int) bool {
			if c := labelsCmp(t.vector[i].Metric, t.vector[j].Metric); c != 0 {
				return c < 0
			}
			return t.vector[i].T < t.vector[j].T
		})
	case "matrix":
		sort.SliceStable(t.matrix, func(i, j int) bool {
			return labelsCmp(t.matrix[i].Metric, t.matrix[j].Metric) < 0
		})
	case "streams":
		sort.SliceStable(t.streams, func(i, j int) bool {
			return labelsCmp(t.streams[i].Labels, t.streams[j].Labels) < 0
		})
		for i := range t.streams {
			entries := t.streams[i].Entries
			sort.SliceStable(entries, func(a, b int) bool {
				if entries[a].Timestamp != entries[b].Timestamp {
					return entries[a].Timestamp < entries[b].Timestamp
				}
				return entries[a].Line < entries[b].Line
			})
		}
	}
	return t
}

// labelsCmp gives a stable ordering between two label sets. We sort the
// keys, then compare key-by-key (k1 vs k2, v1 vs v2). This matches the
// `labels.Compare` semantics the upstream test uses (canonical sorted
// pairs).
func labelsCmp(a, b map[string]string) int {
	akeys := sortedKeys(a)
	bkeys := sortedKeys(b)
	n := len(akeys)
	if len(bkeys) < n {
		n = len(bkeys)
	}
	for i := 0; i < n; i++ {
		if c := strings.Compare(akeys[i], bkeys[i]); c != 0 {
			return c
		}
		if c := strings.Compare(a[akeys[i]], b[bkeys[i]]); c != 0 {
			return c
		}
	}
	return len(akeys) - len(bkeys)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diffTyped runs a value-aware comparison and returns a human-readable
// diff string (empty means equal). The string is structured enough for
// triage but stops short of the cmp.Diff verbosity — the goal is the
// pass/fail signal, with reproducer reference back to the corpus YAML.
func diffTyped(expected, actual typedResult, tolerance float64) string {
	if expected.kind != actual.kind {
		return fmt.Sprintf("resultType differs: expected=%s actual=%s", expected.kind, actual.kind)
	}
	switch expected.kind {
	case "vector":
		return diffVector(expected.vector, actual.vector, tolerance)
	case "matrix":
		return diffMatrix(expected.matrix, actual.matrix, tolerance)
	case "scalar":
		return diffScalar(expected.scalar, actual.scalar, tolerance)
	case "streams":
		return diffStreams(expected.streams, actual.streams)
	}
	return ""
}

func diffVector(e, a []decodedSample, tol float64) string {
	if len(e) != len(a) {
		return fmt.Sprintf("vector length: expected=%d actual=%d", len(e), len(a))
	}
	for i := range e {
		if d := labelsCmp(e[i].Metric, a[i].Metric); d != 0 {
			return fmt.Sprintf("vector[%d] metric differs: expected=%v actual=%v", i, e[i].Metric, a[i].Metric)
		}
		if e[i].T != a[i].T {
			return fmt.Sprintf("vector[%d] timestamp differs: expected=%d actual=%d", i, e[i].T, a[i].T)
		}
		if !floatEqual(e[i].F, a[i].F, tol) {
			return fmt.Sprintf("vector[%d] value differs: expected=%v actual=%v", i, e[i].F, a[i].F)
		}
	}
	return ""
}

func diffMatrix(e, a []decodedSeries, tol float64) string {
	if len(e) != len(a) {
		return fmt.Sprintf("matrix length: expected=%d actual=%d", len(e), len(a))
	}
	for i := range e {
		if d := labelsCmp(e[i].Metric, a[i].Metric); d != 0 {
			return fmt.Sprintf("matrix[%d] metric differs: expected=%v actual=%v", i, e[i].Metric, a[i].Metric)
		}
		if len(e[i].Floats) != len(a[i].Floats) {
			return fmt.Sprintf("matrix[%d] series length: expected=%d actual=%d", i, len(e[i].Floats), len(a[i].Floats))
		}
		for j := range e[i].Floats {
			if e[i].Floats[j].T != a[i].Floats[j].T {
				return fmt.Sprintf("matrix[%d].points[%d] timestamp differs: expected=%d actual=%d", i, j, e[i].Floats[j].T, a[i].Floats[j].T)
			}
			if !floatEqual(e[i].Floats[j].F, a[i].Floats[j].F, tol) {
				return fmt.Sprintf("matrix[%d].points[%d] value differs: expected=%v actual=%v", i, j, e[i].Floats[j].F, a[i].Floats[j].F)
			}
		}
	}
	return ""
}

func diffScalar(e, a decodedSample, tol float64) string {
	if e.T != a.T {
		return fmt.Sprintf("scalar timestamp differs: expected=%d actual=%d", e.T, a.T)
	}
	if !floatEqual(e.F, a.F, tol) {
		return fmt.Sprintf("scalar value differs: expected=%v actual=%v", e.F, a.F)
	}
	return ""
}

func diffStreams(e, a []decodedStream) string {
	if len(e) != len(a) {
		return fmt.Sprintf("streams length: expected=%d actual=%d", len(e), len(a))
	}
	for i := range e {
		if d := labelsCmp(e[i].Labels, a[i].Labels); d != 0 {
			return fmt.Sprintf("streams[%d] labels differ: expected=%v actual=%v", i, e[i].Labels, a[i].Labels)
		}
		if len(e[i].Entries) != len(a[i].Entries) {
			return fmt.Sprintf("streams[%d] entry count: expected=%d actual=%d", i, len(e[i].Entries), len(a[i].Entries))
		}
		for j := range e[i].Entries {
			if e[i].Entries[j].Timestamp != a[i].Entries[j].Timestamp {
				return fmt.Sprintf("streams[%d].entries[%d] timestamp differs: expected=%d actual=%d", i, j, e[i].Entries[j].Timestamp, a[i].Entries[j].Timestamp)
			}
			if e[i].Entries[j].Line != a[i].Entries[j].Line {
				return fmt.Sprintf("streams[%d].entries[%d] line differs: expected=%q actual=%q", i, j, e[i].Entries[j].Line, a[i].Entries[j].Line)
			}
		}
	}
	return ""
}

// floatEqual mirrors the upstream `require.InDelta` semantics: NaNs
// compare equal (both backends commonly produce NaN for empty bucket
// reductions), and within-tolerance values match.
func floatEqual(a, b, tol float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	if tol > 0 {
		return math.Abs(a-b) <= tol
	}
	return a == b
}

// ----- report shape --------------------------------------------------

// Report mirrors the prometheus/compliance JSON shape so cerberus-side
// tooling (informational CI lane today, the future PR 6 expected-failures
// reconciliation script) can consume both harness reports with a single
// schema. See compatibility/prometheus/upstream/promql/output/json.go.
type Report struct {
	TotalResults   int      `json:"totalResults"`
	IncludePassing bool     `json:"includePassing"`
	Results        []Result `json:"results"`
	// QueryTweaks is reserved for future per-query tolerance / label-drop
	// adjustments. The Prom harness uses it for the same purpose.
	QueryTweaks []any `json:"queryTweaks,omitempty"`
}

// Result is the per-test-case outcome. The four flag fields keep parity
// with the Prom harness; `SkipReason` is the cerberus-side extension
// for overlay-driven skips (the Prom corpus stitches these into the
// upstream YAML directly).
type Result struct {
	TestCase          TestCase `json:"testCase"`
	Diff              string   `json:"diff"`
	UnexpectedFailure string   `json:"unexpectedFailure"`
	UnexpectedSuccess bool     `json:"unexpectedSuccess"`
	Unsupported       bool     `json:"unsupported"`
	SkipReason        string   `json:"skipReason,omitempty"`
}

// success replicates `comparer.Result.Success` so the runtime can know
// when to flag the run as failed without re-deriving the predicate.
func (r Result) success() bool {
	return r.Diff == "" && !r.UnexpectedSuccess && r.UnexpectedFailure == ""
}

// TestCase is the wire surface of a fully-expanded compliance case. We
// embed the fields the Prom shape carries (query/start/end/resolution)
// plus the Loki-specific ones (direction/source/kind/instant/tags) the
// reviewer needs to triage. Field names follow JSON-camelCase to match
// the Prom report.
type TestCase struct {
	Query       string   `json:"query"`
	Source      string   `json:"source"`
	Description string   `json:"description,omitempty"`
	Kind        string   `json:"kind"`
	Direction   string   `json:"direction"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Step        string   `json:"step,omitempty"`
	Instant     bool     `json:"instant"`
	Tags        []string `json:"tags,omitempty"`
}
