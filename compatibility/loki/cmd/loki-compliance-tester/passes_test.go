package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// The driver-level fan-out: the detected-fields differential, the
// status-parity differential, and the corpus split that feeds the
// upstream-skip baseline. Each decides what lands in the report without
// any per-case oracle behind it, so a wrong branch here is a wrong
// parity verdict nothing downstream re-checks.

// TestCompareDetectedFieldsOne_Arms drives the per-service fields
// verdict. The two emptiness arms matter most: an empty list on either
// side is a harness/seed problem, and reporting it as a clean pass would
// make a service with no seeded data look like agreement.
func TestCompareDetectedFieldsOne_Arms(t *testing.T) {
	t.Parallel()
	start := time.Unix(1700000000, 0).UTC()
	end := time.Unix(1700086400, 0).UTC()

	cases := []struct {
		name        string
		ref         *fieldValuesStub
		test        *fieldValuesStub
		wantPass    bool
		wantDiff    string
		wantFailure string
	}{
		{
			name:     "agreement",
			ref:      &fieldValuesStub{fieldsBody: fieldsBodyTwo},
			test:     &fieldValuesStub{fieldsBody: fieldsBodyTwo},
			wantPass: true,
		},
		{
			name:     "cardinality-divergence",
			ref:      &fieldValuesStub{fieldsBody: fieldsBodyTwo},
			test:     &fieldValuesStub{fieldsBody: `{"fields":[{"label":"status","type":"int","cardinality":4},{"label":"path","type":"string","cardinality":9}],"limit":1000}`},
			wantDiff: `field "status" cardinality`,
		},
		{
			name:        "empty-baseline",
			ref:         &fieldValuesStub{fieldsBody: `{"fields":[],"limit":1000}`},
			test:        &fieldValuesStub{fieldsBody: fieldsBodyTwo},
			wantFailure: "baseline returned empty",
		},
		{
			name:        "empty-test-endpoint",
			ref:         &fieldValuesStub{fieldsBody: fieldsBodyTwo},
			test:        &fieldValuesStub{fieldsBody: `{"fields":[],"limit":1000}`},
			wantFailure: "test endpoint returned empty",
		},
		{
			name:        "reference-side-failure",
			ref:         &fieldValuesStub{status: http.StatusInternalServerError},
			test:        &fieldValuesStub{fieldsBody: fieldsBodyTwo},
			wantFailure: "reference (-addr-1) failed",
		},
		{
			name:        "test-side-failure",
			ref:         &fieldValuesStub{fieldsBody: fieldsBodyTwo},
			test:        &fieldValuesStub{status: http.StatusInternalServerError},
			wantFailure: "status=500",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := flagsFor(newFieldValuesStub(t, tc.ref), newFieldValuesStub(t, tc.test))
			got := compareDetectedFieldsOne(&http.Client{Timeout: 5 * time.Second}, f, "api", `{service_name="api"}`, start, end)
			if got.TestCase.Kind != "detected_fields" || !strings.Contains(got.TestCase.Description, "service=api") {
				t.Fatalf("envelope = %+v", got.TestCase)
			}
			switch {
			case tc.wantPass:
				if !got.success() {
					t.Fatalf("want a pass, got %+v", got)
				}
			case tc.wantDiff != "":
				if !strings.Contains(got.Diff, tc.wantDiff) {
					t.Fatalf("Diff = %q, want it to contain %q (failure=%q)", got.Diff, tc.wantDiff, got.UnexpectedFailure)
				}
			default:
				if !strings.Contains(got.UnexpectedFailure, tc.wantFailure) {
					t.Fatalf("UnexpectedFailure = %q, want it to contain %q", got.UnexpectedFailure, tc.wantFailure)
				}
			}
		})
	}
}

// TestCompareDetectedFieldsAll_OnePerQueryableService — one row per
// seeded service in sorted service order, and a service carrying no
// selector contributes none rather than a bogus empty-query row.
func TestCompareDetectedFieldsAll_OnePerQueryableService(t *testing.T) {
	t.Parallel()
	md := fieldValuesMetadata()
	md.ByServiceName["zulu"] = []string{`{service_name="zulu"}`}
	md.ByServiceName["ghost"] = nil

	f := flagsFor(
		newFieldValuesStub(t, &fieldValuesStub{fieldsBody: fieldsBodyTwo}),
		newFieldValuesStub(t, &fieldValuesStub{fieldsBody: fieldsBodyTwo}),
	)
	results := compareDetectedFieldsAll(&http.Client{Timeout: 5 * time.Second}, f, md)
	if len(results) != 2 {
		t.Fatalf("results = %d, want one per queryable service", len(results))
	}
	if !strings.Contains(results[0].TestCase.Description, "service=api") ||
		!strings.Contains(results[1].TestCase.Description, "service=zulu") {
		t.Fatalf("service order = [%q, %q], want it sorted", results[0].TestCase.Description, results[1].TestCase.Description)
	}
	for _, r := range results {
		if !r.success() {
			t.Fatalf("agreeing backends produced a non-passing row: %+v", r)
		}
	}
}

// TestCompareStatusParityAll_RunsEveryFixedCase — the fan-out over the
// fixed case table. Every case must reach the report: a loop that
// dropped one would shrink the denominator invisibly, the same failure
// mode the zero-expansion rail exists to prevent on the corpus side.
func TestCompareStatusParityAll_RunsEveryFixedCase(t *testing.T) {
	ref := statusServer(t, http.StatusBadRequest)
	test := statusServer(t, http.StatusBadRequest)

	results := compareStatusParityAll(&http.Client{Timeout: 5 * time.Second}, flags{addr1: ref.URL, addr2: test.URL})
	want := statusParityCases()
	if len(results) != len(want) {
		t.Fatalf("results = %d, want one per fixed case (%d)", len(results), len(want))
	}
	for i, r := range results {
		if !r.success() {
			t.Fatalf("agreeing rejection produced a non-passing row: %+v", r)
		}
		if r.TestCase.Source != statusParitySource || r.TestCase.Kind != "status_parity" {
			t.Fatalf("row %d envelope = %+v", i, r.TestCase)
		}
		if r.TestCase.Description != want[i].desc {
			t.Fatalf("row %d description = %q, want %q", i, r.TestCase.Description, want[i].desc)
		}
	}
}

// TestErrorBodySnippet — every fetch path quotes a failing body through
// this one helper, so its bound is the single thing standing between an
// upstream HTML error page and the report's diff column.
func TestErrorBodySnippet(t *testing.T) {
	t.Parallel()
	if got := errorBodySnippet("  short  "); got != "short" {
		t.Fatalf("snippet = %q, want the body trimmed and otherwise unchanged", got)
	}
	atBound := strings.Repeat("y", errorBodySnippetMaxLen)
	if got := errorBodySnippet(atBound); got != atBound {
		t.Fatalf("a body exactly at the bound was truncated (len=%d)", len(got))
	}
	got := errorBodySnippet(atBound + "overflow")
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("the truncated snippet does not end in an ellipsis: %q", got[len(got)-5:])
	}
	if trimmed := strings.TrimSuffix(got, "…"); trimmed != atBound {
		t.Fatalf("the truncated snippet carries %d chars of body, want %d", len(trimmed), errorBodySnippetMaxLen)
	}
}

// TestLoadAllQueriesAndSplit_PartitionsTheRealCorpus runs the real
// vendored corpus through the split that feeds the upstream-skip
// baseline. Both partitions must be non-empty and disjoint: a split that
// dumped everything into one side would make the baseline rail either
// vacuous or a whole-corpus dump, and neither is visible from the
// baseline file alone.
func TestLoadAllQueriesAndSplit_PartitionsTheRealCorpus(t *testing.T) {
	t.Parallel()
	f := flags{corpusDir: filepath.Join("..", "..", "upstream", "loki-bench", "queries")}
	runnable, skipped, err := loadAllQueriesAndSplit(f)
	if err != nil {
		t.Fatalf("loadAllQueriesAndSplit: %v", err)
	}
	if len(runnable) == 0 {
		t.Fatal("no runnable definitions; the compare path would have nothing to do")
	}
	if len(skipped) == 0 {
		t.Fatal("no upstream-skipped definitions; the baseline rail would be vacuous")
	}
	seen := make(map[string]struct{}, len(runnable))
	for _, def := range runnable {
		if def.Skip {
			t.Fatalf("definition %q is marked Skip but landed in the runnable partition", baselineKey(def))
		}
		seen[baselineKey(def)] = struct{}{}
	}
	for _, def := range skipped {
		if !def.Skip {
			t.Fatalf("definition %q is not marked Skip but landed in the skipped partition", baselineKey(def))
		}
		if _, dup := seen[baselineKey(def)]; dup {
			t.Fatalf("definition %q appears in both partitions", baselineKey(def))
		}
	}

	// The committed baseline is what the skipped partition is diffed
	// against, so the two must agree on the real corpus.
	if err := checkSkipBaseline(filepath.Join("..", "..", "upstream-skip-baseline.txt"), skipped); err != nil {
		t.Fatalf("the corpus's skip set has drifted from the committed baseline: %v", err)
	}
}

// TestLoadAllQueriesAndSplit_MissingCorpusErrors — a bad -corpus path is
// a hard error, not an empty (and therefore vacuously clean) split.
func TestLoadAllQueriesAndSplit_MissingCorpusErrors(t *testing.T) {
	t.Parallel()
	_, _, err := loadAllQueriesAndSplit(flags{corpusDir: filepath.Join(t.TempDir(), "nope")})
	if err == nil {
		t.Fatal("loadAllQueriesAndSplit over a missing directory returned no error")
	}
	if !strings.Contains(err.Error(), "registry.Load") {
		t.Fatalf("error %q should name the failing load", err.Error())
	}
}

// TestLoadCerberusQueries_MissingDirErrors — the additive corpus is
// mandatory once configured; a typo'd path must fail rather than
// silently contributing zero extra cases to the score.
func TestLoadCerberusQueries_MissingDirErrors(t *testing.T) {
	t.Parallel()
	suites := []bench.Suite{bench.SuiteFast, bench.SuiteRegression, bench.SuiteExhaustive}
	_, err := loadCerberusQueries(flags{cerberusQueriesDir: filepath.Join(t.TempDir(), "nope")}, suites)
	if err == nil {
		t.Fatal("loadCerberusQueries over a missing directory returned no error")
	}
	if !strings.Contains(err.Error(), "cerberus query registry.Load") {
		t.Fatalf("error %q should name the cerberus registry", err.Error())
	}
}

// TestLoadCases_MissingMetadataErrors — without dataset metadata the
// template resolver cannot expand a single query, so the load fails
// instead of producing a silently empty corpus.
func TestLoadCases_MissingMetadataErrors(t *testing.T) {
	t.Parallel()
	f := flags{
		corpusDir:          filepath.Join("..", "..", "upstream", "loki-bench", "queries"),
		cerberusQueriesDir: filepath.Join("..", "..", "cerberus-queries"),
		metadataDir:        filepath.Join(t.TempDir(), "nope"),
	}
	if _, err := loadCases(f, false); err == nil {
		t.Fatal("loadCases without dataset metadata returned no error")
	}
}

// TestLoadCases_InstantModeKeepsMetricQueriesOnly mirrors upstream's
// remote_test.go: the instant lane drops log queries and collapses each
// surviving metric case to a point. A lane that kept log queries would
// send them to /query_range anyway and double-count them in the score.
func TestLoadCases_InstantModeKeepsMetricQueriesOnly(t *testing.T) {
	t.Parallel()
	f := flags{
		corpusDir:          filepath.Join("..", "..", "upstream", "loki-bench", "queries"),
		cerberusQueriesDir: filepath.Join("..", "..", "cerberus-queries"),
		metadataDir:        filepath.Join("..", ".."),
		seed:               1,
	}
	rangeCases, err := loadCases(f, false)
	if err != nil {
		t.Fatalf("loadCases(range): %v", err)
	}
	instantCases, err := loadCases(f, true)
	if err != nil {
		t.Fatalf("loadCases(instant): %v", err)
	}
	if len(instantCases) == 0 {
		t.Fatal("the instant lane produced no cases")
	}
	if len(instantCases) >= len(rangeCases) {
		t.Fatalf("instant cases = %d, range cases = %d; the instant lane must drop the log queries", len(instantCases), len(rangeCases))
	}
	for _, lc := range instantCases {
		if lc.expandErr != nil {
			continue
		}
		if lc.tc.Kind() != "metric" {
			t.Fatalf("the instant lane kept a %q case: %s", lc.tc.Kind(), lc.tc.Query)
		}
		if !lc.tc.Start.Equal(lc.tc.End) {
			t.Fatalf("instant case %q was not collapsed to a point: [%v, %v]", lc.tc.Query, lc.tc.Start, lc.tc.End)
		}
		if lc.tc.Step != 0 {
			t.Fatalf("instant case %q kept step %v", lc.tc.Query, lc.tc.Step)
		}
	}
}
