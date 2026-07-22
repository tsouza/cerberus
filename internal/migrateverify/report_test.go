package migrateverify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// hasAttrib reports whether a candidate of the given category is present.
func hasAttrib(cands []AttributionCandidate, category string) bool {
	for _, c := range cands {
		if c.Category == category {
			return true
		}
	}
	return false
}

// TestIsHotspotExpr covers the robust call-token boundary check: real hotspot
// calls match, while longer identifiers and recording-rule names that merely
// contain a hotspot name do not.
func TestIsHotspotExpr(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{"rate(x[1m])", true},
		{"irate(x[1m])", true},
		{"increase(x[5m])", true},
		{"histogram_quantile(0.9, sum(x))", true},
		{"sum(rate(http_requests_total[1m]))", true},
		{"rate (x[1m])", true}, // whitespace before '(' still a call
		{"up", false},
		{"rate_total", false},         // longer identifier, not a call
		{"job:rate:sum", false},       // recording-rule name, no call
		{"my_irate_helper(x)", false}, // 'irate' inside a longer identifier
		{"sum(node_increase)", false}, // 'increase' inside a longer identifier
		{"histogram_quantiles(x)", false},
	}
	for _, c := range cases {
		if got := isHotspotExpr(c.expr); got != c.want {
			t.Errorf("isHotspotExpr(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// TestAttribution_HotspotVsNonHotspot: a diverging hotspot query carries the
// experimental-ch-feature candidate; a non-hotspot divergence does not. Both
// always carry cerberus-bug (the honest default).
func TestAttribution_HotspotVsNonHotspot(t *testing.T) {
	mk := func(v string) map[string]string {
		return map[string]string{
			"rate(x[1m])": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, v}}}),
			"up":          matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, v}}}),
		}
	}
	refBody, cerBody := mk("1"), mk("2")

	hotspot := runVerifyOne(t, refBody, cerBody, Query{Expr: "rate(x[1m])", Source: "rule:r"})
	if hotspot.Verdict != VerdictDiverge {
		t.Fatalf("hotspot verdict = %q, want diverge", hotspot.Verdict)
	}
	if !hasAttrib(hotspot.Attribution, AttribExperimentalCHFeature) {
		t.Errorf("hotspot divergence must carry the experimental-ch-feature candidate, got %+v", hotspot.Attribution)
	}
	if !hasAttrib(hotspot.Attribution, AttribCerberusBug) {
		t.Errorf("every divergence must carry the cerberus-bug candidate, got %+v", hotspot.Attribution)
	}

	plain := runVerifyOne(t, refBody, cerBody, Query{Expr: "up", Source: "rule:u"})
	if plain.Verdict != VerdictDiverge {
		t.Fatalf("non-hotspot verdict = %q, want diverge", plain.Verdict)
	}
	if hasAttrib(plain.Attribution, AttribExperimentalCHFeature) {
		t.Errorf("non-hotspot divergence must NOT carry the experimental-ch-feature candidate, got %+v", plain.Attribution)
	}
	if !hasAttrib(plain.Attribution, AttribCerberusBug) {
		t.Errorf("every divergence must carry the cerberus-bug candidate, got %+v", plain.Attribution)
	}
}

// TestAttribution_HistogramQuantileHotspot: histogram_quantile is a hotspot too.
func TestAttribution_HistogramQuantileHotspot(t *testing.T) {
	const expr = "histogram_quantile(0.9, x)"
	refBody := map[string]string{expr: matrix(seriesSpec{labels: map[string]string{"le": "1"}, points: []pointSpec{{1_700_000_000, "1"}}})}
	cerBody := map[string]string{expr: matrix(seriesSpec{labels: map[string]string{"le": "1"}, points: []pointSpec{{1_700_000_000, "2"}}})}
	res := runVerifyOne(t, refBody, cerBody, Query{Expr: expr, Source: "panel:p"})
	if !hasAttrib(res.Attribution, AttribExperimentalCHFeature) {
		t.Errorf("histogram_quantile divergence must carry the experimental-ch-feature candidate, got %+v", res.Attribution)
	}
}

// TestAttribution_CoverageGap: a series present on only one backend surfaces the
// data-window-gap + ingest-artifact candidates rather than dialect-semantics.
func TestAttribution_CoverageGap(t *testing.T) {
	refBody := map[string]string{"up": matrix(
		seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}}},
		seriesSpec{labels: map[string]string{"job": "b"}, points: []pointSpec{{1_700_000_000, "1"}}},
	)}
	cerBody := map[string]string{"up": matrix(
		seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}}},
	)}
	res := runVerifyOne(t, refBody, cerBody, Query{Expr: "up", Source: "s"})
	if !hasAttrib(res.Attribution, AttribDataWindowGap) || !hasAttrib(res.Attribution, AttribIngestArtifact) {
		t.Errorf("coverage-gap divergence must carry data-window-gap + ingest-artifact, got %+v", res.Attribution)
	}
	if hasAttrib(res.Attribution, AttribDialectSemantics) {
		t.Errorf("coverage-gap divergence should not carry dialect-semantics, got %+v", res.Attribution)
	}
}

// TestWriteText_VerdictBanner: the text report LEADS with an unmistakable verdict
// banner — FAILED on a diverging run, PASSED on a clean one.
func TestWriteText_VerdictBanner(t *testing.T) {
	failing := Report{Summary: Summary{Total: 3, Match: 1, Diverge: 1, Error: 1}}
	var fb strings.Builder
	if err := failing.WriteText(&fb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	first := strings.SplitN(fb.String(), "\n", 2)[0]
	if !strings.HasPrefix(first, "VERIFICATION FAILED") {
		t.Errorf("failing report must lead with VERIFICATION FAILED, got first line: %q", first)
	}
	if !strings.Contains(first, "1 diverged") || !strings.Contains(first, "1 errored") ||
		!strings.Contains(first, "1 matched") || !strings.Contains(first, "of 3") {
		t.Errorf("failure banner must carry the counts, got: %q", first)
	}

	passing := Report{Summary: Summary{Total: 5, Match: 5}}
	var pb strings.Builder
	if err := passing.WriteText(&pb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	firstPass := strings.SplitN(pb.String(), "\n", 2)[0]
	if !strings.HasPrefix(firstPass, "VERIFICATION PASSED") || !strings.Contains(firstPass, "all 5") {
		t.Errorf("passing report must lead with VERIFICATION PASSED — all 5, got: %q", firstPass)
	}
}

// TestWriteTextGuided_BugReport: a failing report ends with the "Report this to
// cerberus" section carrying the issues URL and the supplied repro command.
func TestWriteTextGuided_BugReport(t *testing.T) {
	rep := Report{
		Summary: Summary{Total: 1, Diverge: 1},
		Results: []QueryResult{{
			Source: "rule:r", Expr: "rate(x[1m])", Verdict: VerdictDiverge,
			Attribution: attributeDivergence("rate(x[1m])", &FirstDiff{Reason: "value differs beyond tolerance"}),
		}},
	}
	const repro = "migrate verify --corpus c.json --ref http://ref --cerberus http://cer --report verify-report.json"
	var b strings.Builder
	if err := rep.WriteTextGuided(&b, TextGuidance{ReproCommand: repro}); err != nil {
		t.Fatalf("WriteTextGuided: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "Report this to cerberus") {
		t.Errorf("failing report must carry the bug-report section, got:\n%s", out)
	}
	if !strings.Contains(out, IssuesURL) {
		t.Errorf("bug-report section must carry the issues URL %q, got:\n%s", IssuesURL, out)
	}
	if !strings.Contains(out, repro) {
		t.Errorf("bug-report section must carry the repro command, got:\n%s", out)
	}
	if !strings.Contains(out, "candidate-cause [experimental-ch-feature]") {
		t.Errorf("failing report must print the per-divergence candidate causes, got:\n%s", out)
	}
	if !strings.Contains(out, ExperimentalNote) {
		t.Errorf("report header must carry the one-time experimental-feature note, got:\n%s", out)
	}
}

// TestVerifyReport_JSON: the --report diagnostic marshals to valid, parseable
// JSON carrying the schema version, tool version, timestamp, run params, summary,
// and per-query verdicts (with attribution on a divergence).
func TestVerifyReport_JSON(t *testing.T) {
	refBody := map[string]string{
		"up":          matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
		"rate(x[1m])": matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
	}
	cerBody := map[string]string{
		"up":          matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
		"rate(x[1m])": matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "9"}}}),
	}
	ref := NewHTTPBackend(matrixServer(t, refBody).URL)
	cer := NewHTTPBackend(matrixServer(t, cerBody).URL)
	corpus := Corpus{PromQL: []Query{{Expr: "up", Source: "s1"}, {Expr: "rate(x[1m])", Source: "s2"}}}
	rep := Verify(context.Background(), corpus, ref, cer, testParams())

	params := VerifyReportParams{
		RefURL: "http://ref", CerberusURL: "http://cer",
		Start: "2023-11-14T22:13:20Z", End: "2023-11-14T22:23:20Z",
		Step: "1m0s", Tolerance: DefaultTolerance, Corpus: "corpus.json",
	}
	genAt := time.Unix(1_700_000_600, 0).UTC()
	diag := NewVerifyReport(rep, params, "v1.2.3", genAt)

	var buf strings.Builder
	if err := diag.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// It must be valid, parseable JSON.
	var back VerifyReport
	if err := json.Unmarshal([]byte(buf.String()), &back); err != nil {
		t.Fatalf("diagnostic must be parseable JSON: %v\n%s", err, buf.String())
	}
	if back.SchemaVersion != VerifyReportVersion {
		t.Errorf("schema_version = %d, want %d", back.SchemaVersion, VerifyReportVersion)
	}
	if back.ToolVersion != "v1.2.3" {
		t.Errorf("tool_version = %q, want v1.2.3", back.ToolVersion)
	}
	if back.GeneratedAt != genAt.Format(time.RFC3339) {
		t.Errorf("generated_at = %q, want %q", back.GeneratedAt, genAt.Format(time.RFC3339))
	}
	if back.Note != ExperimentalNote {
		t.Errorf("note = %q, want the experimental-feature note", back.Note)
	}
	if back.Params != params {
		t.Errorf("params = %+v, want %+v", back.Params, params)
	}
	if back.Summary != rep.Summary {
		t.Errorf("summary = %+v, want %+v", back.Summary, rep.Summary)
	}
	if len(back.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(back.Results))
	}

	// The diverging result must carry its attribution through the JSON round-trip.
	var diverged QueryResult
	for _, r := range back.Results {
		if r.Verdict == VerdictDiverge {
			diverged = r
		}
	}
	if diverged.Source == "" {
		t.Fatal("expected a diverged result in the diagnostic")
	}
	if !hasAttrib(diverged.Attribution, AttribExperimentalCHFeature) {
		t.Errorf("diverged rate() result must carry experimental-ch-feature attribution, got %+v", diverged.Attribution)
	}

	// Deterministic marshaling: the same inputs produce byte-identical JSON.
	var buf2 strings.Builder
	if err := NewVerifyReport(rep, params, "v1.2.3", genAt).WriteJSON(&buf2); err != nil {
		t.Fatalf("WriteJSON (second): %v", err)
	}
	if buf.String() != buf2.String() {
		t.Errorf("diagnostic marshaling must be deterministic; got two different encodings")
	}
}
