package migrateverify

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestValuesEqual_Infinities pins the fix for the infinity false-divergence: two
// EQUAL infinities must match (math.Abs(+Inf - +Inf) is NaN, and NaN <= tol is
// false, so the pre-fix abs-diff path reported byte-identical +Inf as divergent).
// Sign-aware exact equality gets every direction right.
func TestValuesEqual_Infinities(t *testing.T) {
	posInf, negInf := math.Inf(1), math.Inf(-1)
	cases := []struct {
		name string
		a, b float64
		want bool
	}{
		{"equal +Inf", posInf, posInf, true},
		{"equal -Inf", negInf, negInf, true},
		{"+Inf vs -Inf", posInf, negInf, false},
		{"-Inf vs +Inf", negInf, posInf, false},
		{"+Inf vs finite", posInf, 1e9, false},
		{"finite vs +Inf", 42, posInf, false},
		{"-Inf vs finite", negInf, -1, false},
		{"NaN vs NaN still equal", math.NaN(), math.NaN(), true},
		{"NaN vs +Inf", math.NaN(), posInf, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := valuesEqual(tc.a, tc.b, DefaultTolerance); got != tc.want {
				t.Errorf("valuesEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestValuesEqual_RelativeToleranceAtLargeMagnitude pins the relative-tolerance
// fix: a fixed absolute 1e-9 epsilon cannot express last-ULP equality near 1e9
// (a float64 ULP there is ~2e-7), so two backends agreeing to the last digit
// would false-diverge. The combined limit accepts a diff within the intrinsic
// float granularity while still rejecting a genuine large difference.
func TestValuesEqual_RelativeToleranceAtLargeMagnitude(t *testing.T) {
	const base = 1e9
	oneULP := math.Nextafter(base, math.Inf(1)) - base // ~1.19e-7, far above 1e-9
	if oneULP <= DefaultTolerance {
		t.Fatalf("test premise broken: 1 ULP at %g is %g, not above the absolute tolerance %g", base, oneULP, DefaultTolerance)
	}
	if !valuesEqual(base, base+oneULP, DefaultTolerance) {
		t.Errorf("two values 1 ULP apart at magnitude %g must match under the relative tolerance", base)
	}
	// A difference well beyond the relative granularity is still a divergence.
	if valuesEqual(base, base*1.01, DefaultTolerance) {
		t.Errorf("a 1%% difference at magnitude %g must still diverge", base)
	}
}

// TestVerify_InfinityMatchEndToEnd exercises the infinity fix through the full
// replay path: both backends return byte-identical +Inf (the 1/0 shape) and the
// query must MATCH, not diverge.
func TestVerify_InfinityMatchEndToEnd(t *testing.T) {
	body := map[string]string{
		"1/0": matrix(seriesSpec{
			labels: map[string]string{"job": "a"},
			points: []pointSpec{{1_700_000_000, "+Inf"}, {1_700_000_060, "+Inf"}},
		}),
	}
	res := runVerifyOne(t, body, body, Query{Expr: "1/0", Source: "rule:inf"})
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (first-diff: %+v)", res.Verdict, res.FirstDiff)
	}
}

// statusServer answers every request with a fixed status code and body, so a
// half-broken backend (e.g. cerberus whose ClickHouse is down, 503 on every
// query) can be exercised.
func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVerify_Cerberus5xxIsBlockingError pins the honesty fix: a cerberus that
// 503s every query (its ClickHouse is down) must be classed VerdictError
// (BLOCKING), not the non-blocking VerdictUnsupported that would let
// "VERIFICATION PASSED" ship over a dead backend.
func TestVerify_Cerberus5xxIsBlockingError(t *testing.T) {
	refBody := map[string]string{
		"up": matrix(seriesSpec{
			labels: map[string]string{"job": "a"},
			points: []pointSpec{{1_700_000_000, "1"}},
		}),
	}
	ref := NewHTTPBackend(matrixServer(t, refBody).URL)
	cer := NewHTTPBackend(statusServer(t, http.StatusServiceUnavailable, `{"status":"error"}`).URL)

	rep := Verify(context.Background(), Corpus{PromQL: []Query{{Expr: "up", Source: "rule:up"}}}, ref, cer, testParams())
	res := rep.Results[0]
	if res.Verdict != VerdictError {
		t.Fatalf("verdict = %q, want error for a cerberus 503 (detail: %s)", res.Verdict, res.Detail)
	}
	if !strings.Contains(res.Detail, "status=503") {
		t.Errorf("error detail should name the 503 status, got %q", res.Detail)
	}
	if rep.Summary.Error != 1 {
		t.Errorf("summary error count = %d, want 1", rep.Summary.Error)
	}
	if !rep.Failed() {
		t.Error("a cerberus 5xx must FAIL the gate (Failed() == true)")
	}
}

// TestVerify_Cerberus4xxStaysUnsupported guards that the 5xx tightening did NOT
// re-class an honest 4xx rejection: a 400 is still the non-blocking
// VerdictUnsupported.
func TestVerify_Cerberus4xxStaysUnsupported(t *testing.T) {
	refBody := map[string]string{
		"up": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
	}
	// matrixServer answers a 400 (non-matrix) for any query it has no entry for.
	res := runVerifyOne(t, refBody, map[string]string{}, Query{Expr: "up", Source: "rule:up"})
	if res.Verdict != VerdictUnsupported {
		t.Fatalf("verdict = %q, want unsupported for a cerberus 400", res.Verdict)
	}
}

// TestVerify_RecordsTolerance pins that the resolved tolerance is recorded in the
// gate-consumed Report, so a verify.json produced with a loosened tolerance can
// no longer be blessed blind.
func TestVerify_RecordsTolerance(t *testing.T) {
	const tol = 0.25
	body := map[string]string{
		"up": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}}}),
	}
	ref := NewHTTPBackend(matrixServer(t, body).URL)
	cer := NewHTTPBackend(matrixServer(t, body).URL)
	p := testParams()
	p.Tolerance = tol
	rep := Verify(context.Background(), Corpus{PromQL: []Query{{Expr: "up", Source: "s"}}}, ref, cer, p)
	if rep.Params.Tolerance != tol {
		t.Errorf("Report.Params.Tolerance = %v, want %v", rep.Params.Tolerance, tol)
	}
}

// TestBuildParams_ToleranceValidation pins that a fat-fingered tolerance is
// rejected loudly rather than riding through into a clean-looking verify.json.
func TestBuildParams_ToleranceValidation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	bad := []struct {
		name string
		tol  float64
	}{
		{"negative", -1},
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"absurd 1000", 1000},
		{"at the cap", maxVerifyTolerance},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildParams("-1h", "now", "60s", tc.tol, now); err == nil {
				t.Errorf("BuildParams accepted tolerance %v, want rejection", tc.tol)
			}
		})
	}
	// A sane tolerance below the cap is still accepted.
	if _, err := BuildParams("-1h", "now", "60s", 0.5, now); err != nil {
		t.Errorf("BuildParams rejected a sane tolerance 0.5: %v", err)
	}
}

// TestReadCappedBody pins the response-size cap: a body within the limit reads
// through, one past it errors instead of buffering an unbounded stream.
func TestReadCappedBody(t *testing.T) {
	const limit = 16
	if _, err := readCappedBody(strings.NewReader(strings.Repeat("x", limit)), limit); err != nil {
		t.Errorf("a body exactly at the limit must read, got %v", err)
	}
	if _, err := readCappedBody(strings.NewReader(strings.Repeat("x", limit+1)), limit); err == nil {
		t.Error("a body past the limit must error, not truncate silently")
	}
}

// TestCompareSeries_PerStepCoverageGap exercises the per-step coverage-gap
// branch in compareSeries: two matched series where one backend is missing a
// sample at a step the other has. Each side's absence is its own divergence,
// never a silent skip.
func TestCompareSeries_PerStepCoverageGap(t *testing.T) {
	key := "{job=\"a\"}"
	full := Series{Samples: []Sample{{T: 1, V: 1}, {T: 2, V: 2}, {T: 3, V: 3}}}
	gappy := Series{Samples: []Sample{{T: 1, V: 1}, {T: 3, V: 3}}} // missing T=2

	// cerberus (second arg) missing a step the reference has.
	fd := compareSeries(key, full, gappy, DefaultTolerance)
	if fd == nil || !strings.Contains(fd.Reason, "cerberus has no sample") {
		t.Fatalf("want a cerberus-missing-step diff at T=2, got %+v", fd)
	}
	if fd.Timestamp != 2 {
		t.Errorf("diff timestamp = %v, want 2", fd.Timestamp)
	}

	// reference (first arg) missing a step cerberus has.
	fd = compareSeries(key, gappy, full, DefaultTolerance)
	if fd == nil || !strings.Contains(fd.Reason, "reference has no sample") {
		t.Fatalf("want a reference-missing-step diff at T=2, got %+v", fd)
	}
}

// TestRedactURL pins that basic-auth userinfo is stripped from a URL before it
// can reach any artifact, while a credential-free URL is returned unchanged.
func TestRedactURL(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"user and pass", "https://user:s3cret@prom.example:9090", "https://REDACTED@prom.example:9090"},
		{"user only", "http://admin@prom.local", "http://REDACTED@prom.local"},
		{"no creds unchanged", "https://prom.example:9090/api", "https://prom.example:9090/api"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "s3cret") {
				t.Errorf("RedactURL leaked the password: %q", got)
			}
		})
	}
}
