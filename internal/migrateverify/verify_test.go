package migrateverify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// matrixServer returns an httptest server that answers /api/v1/query_range with
// the supplied per-query matrix JSON. A query with no entry gets a 400 with a
// non-matrix body, so a test can exercise the cerberus-unsupported path.
func matrixServer(t *testing.T, byQuery map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != queryRangePath {
			t.Errorf("unexpected path %q, want %q", got, queryRangePath)
		}
		q := r.URL.Query().Get("query")
		body, ok := byQuery[q]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"unknown query"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// matrix builds a Prometheus matrix response body from a small spec. Each series
// is a label set plus [ts,"value"] points.
func matrix(series ...seriesSpec) string {
	type point [2]any
	type rawSeries struct {
		Metric map[string]string `json:"metric"`
		Values []point           `json:"values"`
	}
	out := struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string      `json:"resultType"`
			Result     []rawSeries `json:"result"`
		} `json:"data"`
	}{Status: "success"}
	out.Data.ResultType = "matrix"
	for _, s := range series {
		rs := rawSeries{Metric: s.labels}
		for _, p := range s.points {
			rs.Values = append(rs.Values, point{p.ts, p.val})
		}
		out.Data.Result = append(out.Data.Result, rs)
	}
	b, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type seriesSpec struct {
	labels map[string]string
	points []pointSpec
}

type pointSpec struct {
	ts  float64
	val string
}

// testParams is a fixed, deterministic window (relative parsing is exercised
// separately).
func testParams() Params {
	return Params{
		Start:     time.Unix(1_700_000_000, 0).UTC(),
		End:       time.Unix(1_700_000_600, 0).UTC(),
		Step:      60 * time.Second,
		Tolerance: DefaultTolerance,
	}
}

func runVerifyOne(t *testing.T, refBody, cerBody map[string]string, q Query) QueryResult {
	t.Helper()
	ref := NewHTTPBackend(matrixServer(t, refBody).URL)
	cer := NewHTTPBackend(matrixServer(t, cerBody).URL)
	rep := Verify(context.Background(), Corpus{PromQL: []Query{q}}, ref, cer, testParams())
	if len(rep.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(rep.Results))
	}
	return rep.Results[0]
}

// TestVerify_Match: identical matrices from both backends → match, no first-diff,
// gate passes.
func TestVerify_Match(t *testing.T) {
	body := map[string]string{
		"up": matrix(seriesSpec{
			labels: map[string]string{"__name__": "up", "job": "api"},
			points: []pointSpec{{1_700_000_000, "1"}, {1_700_000_060, "1"}},
		}),
	}
	res := runVerifyOne(t, body, body, Query{Expr: "up", Source: "rule:up"})
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (detail: %s)", res.Verdict, res.Detail)
	}
	if res.FirstDiff != nil {
		t.Fatalf("match must have no first-diff, got %+v", res.FirstDiff)
	}
}

// TestVerify_ValueDivergence: same series, one differing value → diverge, with
// the first differing point captured exactly, and the gate fails.
func TestVerify_ValueDivergence(t *testing.T) {
	refBody := map[string]string{
		"rate(x[1m])": matrix(seriesSpec{
			labels: map[string]string{"job": "api"},
			points: []pointSpec{{1_700_000_000, "1"}, {1_700_000_060, "2"}, {1_700_000_120, "3"}},
		}),
	}
	cerBody := map[string]string{
		"rate(x[1m])": matrix(seriesSpec{
			labels: map[string]string{"job": "api"},
			points: []pointSpec{{1_700_000_000, "1"}, {1_700_000_060, "2.5"}, {1_700_000_120, "3"}},
		}),
	}
	res := runVerifyOne(t, refBody, cerBody, Query{Expr: "rate(x[1m])", Source: "rule:r"})
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge", res.Verdict)
	}
	fd := res.FirstDiff
	if fd == nil {
		t.Fatal("diverge must carry a first-diff")
	}
	if fd.Timestamp != 1_700_000_060 {
		t.Errorf("first-diff ts = %v, want 1700000060 (the first mismatching step)", fd.Timestamp)
	}
	if fd.RefValue != "2" || fd.CerberusValue != "2.5" {
		t.Errorf("first-diff values = ref %q / cerberus %q, want 2 / 2.5", fd.RefValue, fd.CerberusValue)
	}
	rep := Report{Summary: Summary{Diverge: 1}}
	if !rep.Failed() {
		t.Error("a divergence must fail the gate")
	}
}

// TestVerify_MissingSeries: cerberus omits a series the reference returned → that
// is itself a divergence (never a silent skip).
func TestVerify_MissingSeries(t *testing.T) {
	refBody := map[string]string{
		"q": matrix(
			seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}}},
			seriesSpec{labels: map[string]string{"job": "b"}, points: []pointSpec{{1_700_000_000, "2"}}},
		),
	}
	cerBody := map[string]string{
		"q": matrix(
			seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}}},
		),
	}
	res := runVerifyOne(t, refBody, cerBody, Query{Expr: "q", Source: "rule:q"})
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge", res.Verdict)
	}
	if res.FirstDiff == nil || !strings.Contains(res.FirstDiff.Reason, "reference only") {
		t.Fatalf("first-diff should flag the missing series as reference-only, got %+v", res.FirstDiff)
	}
	if res.FirstDiff.CerberusValue != "<missing series>" {
		t.Errorf("missing-series diff should mark cerberus side missing, got %q", res.FirstDiff.CerberusValue)
	}
}

// TestVerify_EpsilonBoundary: a diff exactly at tolerance matches; a diff just
// beyond it diverges.
func TestVerify_EpsilonBoundary(t *testing.T) {
	// tol and the sample values are exactly representable in float64 (halves), so
	// the boundary comparison is not clouded by decimal-to-binary rounding.
	const tol = 0.5
	mk := func(v string) map[string]string {
		return map[string]string{"q": matrix(seriesSpec{
			labels: map[string]string{"job": "a"},
			points: []pointSpec{{1_700_000_000, v}},
		})}
	}
	p := testParams()
	p.Tolerance = tol

	// Exactly at the boundary: |0 - 0.5| == tol → within tolerance → match.
	ref := NewHTTPBackend(matrixServer(t, mk("0")).URL)
	cerAtBoundary := NewHTTPBackend(matrixServer(t, mk("0.5")).URL)
	rep := Verify(context.Background(), Corpus{PromQL: []Query{{Expr: "q", Source: "s"}}}, ref, cerAtBoundary, p)
	if rep.Results[0].Verdict != VerdictMatch {
		t.Errorf("diff exactly at tolerance should match, got %q", rep.Results[0].Verdict)
	}

	// Just beyond the boundary: |0 - 0.75| > tol → diverge.
	cerBeyond := NewHTTPBackend(matrixServer(t, mk("0.75")).URL)
	rep = Verify(context.Background(), Corpus{PromQL: []Query{{Expr: "q", Source: "s"}}}, ref, cerBeyond, p)
	if rep.Results[0].Verdict != VerdictDiverge {
		t.Errorf("diff beyond tolerance should diverge, got %q", rep.Results[0].Verdict)
	}
}

// TestVerify_NaNEqual: both backends report NaN at the same step → equal, match.
func TestVerify_NaNEqual(t *testing.T) {
	body := map[string]string{
		"q": matrix(seriesSpec{
			labels: map[string]string{"job": "a"},
			points: []pointSpec{{1_700_000_000, "NaN"}, {1_700_000_060, "5"}},
		}),
	}
	res := runVerifyOne(t, body, body, Query{Expr: "q", Source: "s"})
	if res.Verdict != VerdictMatch {
		t.Fatalf("NaN==NaN should be treated as equal, got %q (diff %+v)", res.Verdict, res.FirstDiff)
	}
}

// TestVerify_CerberusUnsupported: cerberus returns a 400 (non-matrix) → the query
// is classified unsupported, the reference is untouched, and the gate does NOT
// fail on an unsupported query alone.
func TestVerify_CerberusUnsupported(t *testing.T) {
	refBody := map[string]string{
		"histogram_quantile(0.9, x)": matrix(seriesSpec{
			labels: map[string]string{"job": "a"},
			points: []pointSpec{{1_700_000_000, "1"}},
		}),
	}
	// cerBody has no entry for the query → matrixServer answers 400.
	res := runVerifyOne(t, refBody, map[string]string{}, Query{Expr: "histogram_quantile(0.9, x)", Source: "panel:x"})
	if res.Verdict != VerdictUnsupported {
		t.Fatalf("verdict = %q, want unsupported", res.Verdict)
	}
	if !strings.Contains(res.Detail, "status=400") {
		t.Errorf("unsupported detail should name the cerberus status, got %q", res.Detail)
	}
	rep := Report{Summary: Summary{Unsupported: 1}}
	if rep.Failed() {
		t.Error("an unsupported query alone must not fail the gate")
	}
}

// TestVerify_ReferenceError: the reference returns non-200 → error verdict (no
// baseline to compare), which DOES fail the gate.
func TestVerify_ReferenceError(t *testing.T) {
	cerBody := map[string]string{
		"q": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
	}
	res := runVerifyOne(t, map[string]string{}, cerBody, Query{Expr: "q", Source: "s"})
	if res.Verdict != VerdictError {
		t.Fatalf("verdict = %q, want error", res.Verdict)
	}
	rep := Report{Summary: Summary{Error: 1}}
	if !rep.Failed() {
		t.Error("an error verdict must fail the gate")
	}
}

// TestVerify_SummaryAndJSON: a mixed run rolls up the right counts and the JSON
// report round-trips.
func TestVerify_SummaryAndJSON(t *testing.T) {
	refBody := map[string]string{
		"good": matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
		"bad":  matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
	}
	cerBody := map[string]string{
		"good": matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
		"bad":  matrix(seriesSpec{labels: map[string]string{"j": "a"}, points: []pointSpec{{1_700_000_000, "9"}}}),
	}
	ref := NewHTTPBackend(matrixServer(t, refBody).URL)
	cer := NewHTTPBackend(matrixServer(t, cerBody).URL)
	corpus := Corpus{
		PromQL:         []Query{{Expr: "good", Source: "s1"}, {Expr: "bad", Source: "s2"}},
		OutOfScope:     []OutOfScopeEntry{{Source: "panel:logs", Lang: "logql"}},
		HarvestSkipped: []HarvestSkippedEntry{{Source: "rule:broken.yml", Reason: "rule has an empty expr"}},
	}
	rep := Verify(context.Background(), corpus, ref, cer, testParams())

	if rep.Summary.Total != 2 || rep.Summary.Match != 1 || rep.Summary.Diverge != 1 ||
		rep.Summary.OutOfScope != 1 || rep.Summary.HarvestSkipped != 1 {
		t.Fatalf("summary = %+v, want total 2 / match 1 / diverge 1 / oos 1 / harvest-skipped 1", rep.Summary)
	}
	if !rep.Failed() {
		t.Error("a run with a divergence must fail the gate")
	}

	var buf strings.Builder
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var back Report
	if err := json.Unmarshal([]byte(buf.String()), &back); err != nil {
		t.Fatalf("JSON report should round-trip: %v", err)
	}
	if back.Summary != rep.Summary {
		t.Errorf("round-tripped summary = %+v, want %+v", back.Summary, rep.Summary)
	}
	if len(back.HarvestSkipped) != 1 || back.HarvestSkipped[0].Source != "rule:broken.yml" {
		t.Errorf("round-tripped harvest-skipped = %+v, want the one broken-rule skip", back.HarvestSkipped)
	}

	var text strings.Builder
	if err := rep.WriteText(&text); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(text.String(), "FAIL:") {
		t.Errorf("text report of a failing run must announce FAIL, got:\n%s", text.String())
	}
	if !strings.Contains(text.String(), "out of scope") {
		t.Errorf("text report must account for out-of-scope entries, got:\n%s", text.String())
	}
	if !strings.Contains(text.String(), "harvest-skipped") ||
		!strings.Contains(text.String(), "rule:broken.yml") {
		t.Errorf("text report must account for harvest-skipped entries, got:\n%s", text.String())
	}
}
