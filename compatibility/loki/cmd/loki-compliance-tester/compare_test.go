package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logproto"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// compareOne is where a pair of HTTP responses becomes a parity verdict,
// and it carries the harness's only branch-heavy policy: which side's
// emptiness is a pass, which is a harness fault, and which is a real
// diff. Getting one of those arms backwards is invisible in a green run
// — the row simply lands in a different column. The tests below stand up
// two httptest backends and drive every arm.

// stubLoki serves one canned body per query endpoint. status 0 means 200.
type stubLoki struct {
	body    string
	status  int
	lastURL *url.URL
}

func newStubLoki(t *testing.T, s *stubLoki) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := *r.URL
		s.lastURL = &u
		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	streamsBodyOne = `{"status":"success","data":{"resultType":"streams","result":[` +
		`{"stream":{"service_name":"api"},"values":[["1700000000000000001","hello"]]}]}}`
	streamsBodyEmpty  = `{"status":"success","data":{"resultType":"streams","result":[]}}`
	streamsBodyOther  = `{"status":"success","data":{"resultType":"streams","result":[` + `{"stream":{"service_name":"api"},"values":[["1700000000000000001","goodbye"]]}]}}`
	vectorBodyOneItem = `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"level":"info"},"value":[1700000000,"3"]}]}}`
)

func logTestCase(tags ...string) bench.TestCase {
	return bench.TestCase{
		Query:     `{service_name="api"}`,
		Start:     time.Unix(1700000000, 0).UTC(),
		End:       time.Unix(1700000060, 0).UTC(),
		Direction: logproto.BACKWARD,
		QueryDesc: "log case",
		Tags:      tags,
	}
}

func metricTestCase() bench.TestCase {
	return bench.TestCase{
		Query:     `sum(count_over_time({service_name="api"}[5m]))`,
		Start:     time.Unix(1700000000, 0).UTC(),
		End:       time.Unix(1700000060, 0).UTC(),
		Direction: logproto.FORWARD,
		Step:      time.Minute,
		QueryDesc: "metric case",
	}
}

func flagsFor(addr1, addr2 string) flags {
	return flags{addr1: addr1, addr2: addr2, timeout: 5 * time.Second, tolerance: 1e-5, parallelism: 2}
}

// TestCompareOne_Agreement — identical bodies on both endpoints produce a
// clean Result, and the TestCase envelope carries the case's identity.
func TestCompareOne_Agreement(t *testing.T) {
	t.Parallel()
	ref := newStubLoki(t, &stubLoki{body: streamsBodyOne})
	test := newStubLoki(t, &stubLoki{body: streamsBodyOne})

	got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase(), "fast/basic.yaml", false)
	if !got.success() {
		t.Fatalf("identical responses did not pass: %+v", got)
	}
	if got.TestCase.Source != "fast/basic.yaml" || got.TestCase.Kind != "log" {
		t.Fatalf("test case envelope = %+v", got.TestCase)
	}
}

// TestCompareOne_Divergence — differing lines land in Diff, not in
// UnexpectedFailure: a value divergence is a cerberus bug, not a harness
// fault, and the two columns are graded differently downstream.
func TestCompareOne_Divergence(t *testing.T) {
	t.Parallel()
	ref := newStubLoki(t, &stubLoki{body: streamsBodyOne})
	test := newStubLoki(t, &stubLoki{body: streamsBodyOther})

	got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase(), "fast/basic.yaml", false)
	if got.Diff == "" {
		t.Fatal("differing lines produced no diff")
	}
	if got.UnexpectedFailure != "" {
		t.Fatalf("a value divergence must not be reported as a failure: %q", got.UnexpectedFailure)
	}
	if !strings.Contains(got.Diff, "line differs") {
		t.Fatalf("diff = %q, want the line dimension named", got.Diff)
	}
}

// TestCompareOne_ReferenceFailure — a broken baseline is a harness
// glitch: reported as UnexpectedFailure, never as Unsupported (which is
// reserved for cerberus answering "not implemented").
func TestCompareOne_ReferenceFailure(t *testing.T) {
	t.Parallel()
	ref := newStubLoki(t, &stubLoki{body: "boom", status: http.StatusInternalServerError})
	test := newStubLoki(t, &stubLoki{body: streamsBodyOne})

	got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase(), "fast/basic.yaml", false)
	if !strings.Contains(got.UnexpectedFailure, "reference (-addr-1) failed") {
		t.Fatalf("UnexpectedFailure = %q, want the reference named", got.UnexpectedFailure)
	}
	if got.Unsupported {
		t.Fatal("a reference-side failure must not be flagged Unsupported")
	}
}

// TestCompareOne_TestEndpointUnsupported — a 501 from cerberus is both a
// failure AND flagged Unsupported, which is what separates "cannot yet
// answer" from "answered wrongly" in the report.
func TestCompareOne_TestEndpointUnsupported(t *testing.T) {
	t.Parallel()
	ref := newStubLoki(t, &stubLoki{body: streamsBodyOne})
	test := newStubLoki(t, &stubLoki{body: "not implemented", status: http.StatusNotImplemented})

	got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase(), "fast/basic.yaml", false)
	if got.UnexpectedFailure == "" {
		t.Fatal("a 501 from the test endpoint produced no failure")
	}
	if !got.Unsupported {
		t.Fatalf("501 not flagged Unsupported: %q", got.UnexpectedFailure)
	}
}

// TestCompareOne_TestEndpointErrorIsNotUnsupported — a 500 is a plain
// failure. Without this arm the Unsupported flag could be hard-wired on
// and every failure would read as a feature gap.
func TestCompareOne_TestEndpointErrorIsNotUnsupported(t *testing.T) {
	t.Parallel()
	ref := newStubLoki(t, &stubLoki{body: streamsBodyOne})
	test := newStubLoki(t, &stubLoki{body: "internal", status: http.StatusInternalServerError})

	got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase(), "fast/basic.yaml", false)
	if got.UnexpectedFailure == "" {
		t.Fatal("a 500 from the test endpoint produced no failure")
	}
	if got.Unsupported {
		t.Fatalf("500 wrongly flagged Unsupported: %q", got.UnexpectedFailure)
	}
}

// TestCompareOne_EmptyResultTagArms pins the three-way branch behind the
// upstream `empty-result` tag. Both-empty is the pass the tag exists
// for; cerberus-returning-rows flips into a Diff (a shape mismatch the
// corpus author says cannot happen); cerberus-empty-while-reference-has-
// rows stays a failure.
func TestCompareOne_EmptyResultTagArms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		refBody     string
		testBody    string
		wantPass    bool
		wantDiff    string
		wantFailure string
	}{
		{
			name:     "both-empty-passes",
			refBody:  streamsBodyEmpty,
			testBody: streamsBodyEmpty,
			wantPass: true,
		},
		{
			name:     "test-returned-rows-is-a-diff",
			refBody:  streamsBodyEmpty,
			testBody: streamsBodyOne,
			wantDiff: "baseline empty (expected) but test endpoint returned rows",
		},
		{
			name:        "test-empty-while-reference-has-rows",
			refBody:     streamsBodyOne,
			testBody:    streamsBodyEmpty,
			wantFailure: "test endpoint returned empty",
		},
		{
			// Both sides carry rows: the tag does not short-circuit, the
			// normal value diff runs. The corpus attaches `empty-result`
			// to cache-exercise cases too, so a tagged case with data
			// must still be compared rather than auto-passed.
			name:     "both-non-empty-falls-through-to-the-value-diff",
			refBody:  streamsBodyOne,
			testBody: streamsBodyOther,
			wantDiff: "line differs",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := newStubLoki(t, &stubLoki{body: tc.refBody})
			test := newStubLoki(t, &stubLoki{body: tc.testBody})
			got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase("empty-result"), "fast/basic.yaml", false)
			switch {
			case tc.wantPass:
				if !got.success() {
					t.Fatalf("want pass, got %+v", got)
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

// TestCompareOne_UntaggedEmptyBaselineIsAFailure — without the tag, an
// empty baseline is a seed/config problem and is reported as one rather
// than passing as "both agree on nothing".
func TestCompareOne_UntaggedEmptyBaselineIsAFailure(t *testing.T) {
	t.Parallel()
	ref := newStubLoki(t, &stubLoki{body: streamsBodyEmpty})
	test := newStubLoki(t, &stubLoki{body: streamsBodyEmpty})

	got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase(), "fast/basic.yaml", false)
	if got.UnexpectedFailure != "baseline returned empty" {
		t.Fatalf("UnexpectedFailure = %q, want \"baseline returned empty\"", got.UnexpectedFailure)
	}
}

// TestCompareOne_UntaggedEmptyTestSideIsAFailure — the mirror arm: the
// reference has rows and cerberus does not.
func TestCompareOne_UntaggedEmptyTestSideIsAFailure(t *testing.T) {
	t.Parallel()
	ref := newStubLoki(t, &stubLoki{body: streamsBodyOne})
	test := newStubLoki(t, &stubLoki{body: streamsBodyEmpty})

	got := compareOne(&http.Client{Timeout: 5 * time.Second}, flagsFor(ref, test), logTestCase(), "fast/basic.yaml", false)
	if got.UnexpectedFailure != "test endpoint returned empty" {
		t.Fatalf("UnexpectedFailure = %q, want \"test endpoint returned empty\"", got.UnexpectedFailure)
	}
}

// TestQueryOne_RangeRoute pins the wire contract of the range call: the
// /query_range path, nanosecond start/end, the direction, and the step
// rendered in SECONDS (Loki rejects a Go duration string here).
func TestQueryOne_RangeRoute(t *testing.T) {
	t.Parallel()
	stub := &stubLoki{body: vectorBodyOneItem}
	addr := newStubLoki(t, stub)

	tc := metricTestCase()
	if _, err := queryOne(&http.Client{Timeout: 5 * time.Second}, addr+"/", tc, false); err != nil {
		t.Fatalf("queryOne: %v", err)
	}
	if stub.lastURL.Path != "/loki/api/v1/query_range" {
		t.Fatalf("path = %q, want /loki/api/v1/query_range", stub.lastURL.Path)
	}
	q := stub.lastURL.Query()
	if q.Get("query") != tc.Query {
		t.Fatalf("query param = %q", q.Get("query"))
	}
	if q.Get("start") != "1700000000000000000" || q.Get("end") != "1700000060000000000" {
		t.Fatalf("window params = (%q, %q), want unix nanos", q.Get("start"), q.Get("end"))
	}
	if q.Get("step") != "60" {
		t.Fatalf("step = %q, want 60 (seconds)", q.Get("step"))
	}
	if q.Get("direction") != "FORWARD" {
		t.Fatalf("direction = %q, want FORWARD", q.Get("direction"))
	}
	if q.Get("time") != "" {
		t.Fatalf("range call must not carry a `time` param, got %q", q.Get("time"))
	}
}

// TestQueryOne_InstantRoute — an instant metric case (start == end)
// routes to /query with a `time` anchor and no step.
func TestQueryOne_InstantRoute(t *testing.T) {
	t.Parallel()
	stub := &stubLoki{body: vectorBodyOneItem}
	addr := newStubLoki(t, stub)

	tc := metricTestCase()
	tc.Start = tc.End
	tc.Step = 0
	if _, err := queryOne(&http.Client{Timeout: 5 * time.Second}, addr, tc, true); err != nil {
		t.Fatalf("queryOne: %v", err)
	}
	if stub.lastURL.Path != "/loki/api/v1/query" {
		t.Fatalf("path = %q, want /loki/api/v1/query", stub.lastURL.Path)
	}
	q := stub.lastURL.Query()
	if q.Get("time") != "1700000060000000000" {
		t.Fatalf("time = %q, want the end anchor in unix nanos", q.Get("time"))
	}
	if q.Get("start") != "" || q.Get("step") != "" {
		t.Fatalf("instant call carried range params: start=%q step=%q", q.Get("start"), q.Get("step"))
	}
}

// TestQueryOne_LogCaseStaysOnRangeEvenInInstantMode — a log query has no
// instant flavour, so the instant lane must still route it to
// /query_range. This mirrors upstream's queryRemote().
func TestQueryOne_LogCaseStaysOnRangeEvenInInstantMode(t *testing.T) {
	t.Parallel()
	stub := &stubLoki{body: streamsBodyOne}
	addr := newStubLoki(t, stub)

	tc := logTestCase()
	tc.Start = tc.End
	if _, err := queryOne(&http.Client{Timeout: 5 * time.Second}, addr, tc, true); err != nil {
		t.Fatalf("queryOne: %v", err)
	}
	if stub.lastURL.Path != "/loki/api/v1/query_range" {
		t.Fatalf("path = %q, want /loki/api/v1/query_range for a log case", stub.lastURL.Path)
	}
}

// TestDoQuery_NonOKCarriesStatusAndTruncatedBody — the error text is what
// isUnsupportedErr keys on, so it must carry the status code; and the
// body is truncated so an upstream stack trace cannot swamp the report.
func TestDoQuery_NonOKCarriesStatusAndTruncatedBody(t *testing.T) {
	t.Parallel()
	const bodyLen = 5000
	stub := &stubLoki{body: strings.Repeat("x", bodyLen), status: http.StatusBadRequest}
	addr := newStubLoki(t, stub)

	_, err := doQuery(&http.Client{Timeout: 5 * time.Second}, addr+"/loki/api/v1/query_range", url.Values{})
	if err == nil {
		t.Fatal("doQuery over a 400 returned no error")
	}
	if !strings.Contains(err.Error(), "status=400") {
		t.Fatalf("error %q missing the status code", err.Error())
	}
	if len(err.Error()) >= bodyLen {
		t.Fatalf("error body was not truncated (len=%d)", len(err.Error()))
	}
	if !strings.HasSuffix(err.Error(), "…") {
		t.Fatalf("truncated error should end in an ellipsis: %q", err.Error()[max(0, len(err.Error())-20):])
	}
}

// TestDoQuery_UnreachableHost — a transport failure is wrapped rather
// than surfacing as a zero-value result that would read as "empty".
func TestDoQuery_UnreachableHost(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing listens on this port any more

	_, err := doQuery(&http.Client{Timeout: 2 * time.Second}, addr+"/loki/api/v1/query_range", url.Values{})
	if err == nil {
		t.Fatal("doQuery against a closed listener returned no error")
	}
	if !strings.Contains(err.Error(), "http call") {
		t.Fatalf("error %q should be the wrapped transport failure", err.Error())
	}
}

// TestCompareAll_ExpansionErrorsBypassTheWire — a case that failed to
// expand never reaches an endpoint; it still contributes a result row,
// built from the QueryDefinition, so the denominator does not shrink.
func TestCompareAll_ExpansionErrorsBypassTheWire(t *testing.T) {
	t.Parallel()
	// compareAll fans out, and compareOne calls both endpoints
	// concurrently, so the handler counter is shared across goroutines.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(streamsBodyOne))
	}))
	t.Cleanup(srv.Close)

	cases := []loadedCase{
		{
			def: bench.QueryDefinition{
				Query:       `{bad="selector"}`,
				Source:      "fast/broken.yaml:9",
				Description: "unexpandable",
				Kind:        "log",
				Directions:  "both",
			},
			expandErr: errUnexpandable{},
		},
		{def: bench.QueryDefinition{Source: "fast/basic.yaml:1"}, tc: logTestCase()},
	}
	results := compareAll(cases, flagsFor(srv.URL, srv.URL), false)
	if len(results) != 2 {
		t.Fatalf("results = %d, want one per loaded case", len(results))
	}
	if !strings.HasPrefix(results[0].UnexpectedFailure, "expansion: ") {
		t.Fatalf("expansion failure = %q, want the `expansion: ` prefix", results[0].UnexpectedFailure)
	}
	if results[0].TestCase.Source != "fast/broken.yaml" {
		t.Fatalf("source = %q, want the line suffix stripped", results[0].TestCase.Source)
	}
	if results[0].TestCase.Direction != "both" {
		t.Fatalf("direction = %q, want the definition's Directions", results[0].TestCase.Direction)
	}
	if !results[1].success() {
		t.Fatalf("the expandable case did not pass: %+v", results[1])
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("endpoint hits = %d, want 2 (one case × two endpoints); the unexpandable case must not reach the wire", got)
	}
}

type errUnexpandable struct{}

func (errUnexpandable) Error() string { return "no such label in the dataset" }

// TestSummarise pins the four headline counters, including the two
// places a naive tally goes wrong: an unsupported case counts in BOTH
// the failure and the unsupported column, and an unexpected success
// counts as a diff rather than a pass.
func TestSummarise(t *testing.T) {
	t.Parallel()
	results := []Result{
		{},
		{},
		{Diff: "values differ"},
		{UnexpectedSuccess: true},
		{UnexpectedFailure: "boom"},
		{UnexpectedFailure: "status=501", Unsupported: true},
		// A failure outranks a diff: a case that could not be answered
		// is not evidence about values.
		{Diff: "values differ", UnexpectedFailure: "boom"},
	}
	pass, diffs, unfail, unsupp := summarise(results)
	if pass != 2 {
		t.Fatalf("pass = %d, want 2", pass)
	}
	if diffs != 2 {
		t.Fatalf("diffs = %d, want 2 (one real diff + one unexpected success)", diffs)
	}
	if unfail != 3 {
		t.Fatalf("unfail = %d, want 3", unfail)
	}
	if unsupp != 1 {
		t.Fatalf("unsupp = %d, want 1", unsupp)
	}
}

// TestCaseSet pins the roster the parity ratchet reads: one entry per
// result, in order, carrying the case identity and its verdict.
// Diverging cases stay in — the ratchet gates on WHICH cases failed.
func TestCaseSet(t *testing.T) {
	t.Parallel()
	results := []Result{
		{TestCase: TestCase{Source: "fast/a.yaml", Kind: "log", Direction: "FORWARD", Query: "{a=\"b\"}"}},
		{TestCase: TestCase{Source: "fast/b.yaml", Kind: "metric", Direction: "FORWARD", Query: "sum(x)"}, Diff: "boom"},
	}
	cases := caseSet(results)
	if len(cases) != 2 {
		t.Fatalf("cases = %d, want 2 (a failing case still appears)", len(cases))
	}
	if !cases[0].Passed {
		t.Fatal("cases[0] should be passing")
	}
	if cases[1].Passed {
		t.Fatal("cases[1] diffed and must not be marked passed")
	}
	if cases[0].ID != results[0].TestCase.id() {
		t.Fatalf("cases[0].ID = %q, want the TestCase identity", cases[0].ID)
	}
	if cases[0].ID == cases[1].ID {
		t.Fatal("two distinct cases collapsed to one identity")
	}
}

// TestTestCaseID pins the identity the roster keys on. Start / End are
// deliberately absent (they identify the RUN), while the lane, step and
// description are present — an instant and a range flavour of the same
// query are different cases.
func TestTestCaseID(t *testing.T) {
	t.Parallel()
	base := TestCase{
		Query:       `{service_name="api"}`,
		Source:      "fast/basic.yaml",
		Kind:        "log",
		Direction:   "BACKWARD",
		Start:       "2023-11-14T22:13:20Z",
		End:         "2023-11-14T22:14:20Z",
		Description: "basic selector",
	}
	id := base.id()
	for _, want := range []string{"fast/basic.yaml", "log", "range", "BACKWARD", `{service_name="api"}`, "basic selector"} {
		if !strings.Contains(id, want) {
			t.Fatalf("id %q missing %q", id, want)
		}
	}
	for _, unwanted := range []string{base.Start, base.End} {
		if strings.Contains(id, unwanted) {
			t.Fatalf("id %q carries the wall-clock field %q; the identity must be run-independent", id, unwanted)
		}
	}

	instant := base
	instant.Instant = true
	if instant.id() == id {
		t.Fatal("instant and range flavours produced the same identity")
	}
	if !strings.Contains(instant.id(), "instant") {
		t.Fatalf("instant id %q does not name the lane", instant.id())
	}

	stepped := base
	stepped.Step = "1m0s"
	if stepped.id() == id {
		t.Fatal("a step-carrying case produced the same identity as a step-less one")
	}
	if !strings.Contains(stepped.id(), "step=1m0s") {
		t.Fatalf("stepped id %q does not carry the step", stepped.id())
	}

	undescribed := base
	undescribed.Description = ""
	if strings.HasSuffix(undescribed.id(), " | ") {
		t.Fatalf("an empty description left a dangling separator: %q", undescribed.id())
	}
}

// TestNewTestCase pins the wire envelope: RFC3339Nano UTC timestamps, the
// derived kind, the instant flag, and a step that is present only when
// the case has one.
func TestNewTestCase(t *testing.T) {
	t.Parallel()
	tc := metricTestCase()
	tc.Tags = []string{"aggregation"}
	out := newTestCase(tc, "fast/metrics.yaml", true)

	if out.Kind != "metric" {
		t.Fatalf("Kind = %q, want metric (derived from the query)", out.Kind)
	}
	if out.Start != "2023-11-14T22:13:20Z" || out.End != "2023-11-14T22:14:20Z" {
		t.Fatalf("window = (%q, %q), want RFC3339Nano UTC", out.Start, out.End)
	}
	if out.Step != "1m0s" {
		t.Fatalf("Step = %q, want 1m0s", out.Step)
	}
	if !out.Instant {
		t.Fatal("Instant flag not propagated")
	}
	if out.Direction != "FORWARD" || out.Source != "fast/metrics.yaml" || out.Description != "metric case" {
		t.Fatalf("envelope = %+v", out)
	}
	if len(out.Tags) != 1 || out.Tags[0] != "aggregation" {
		t.Fatalf("Tags = %v", out.Tags)
	}

	stepless := logTestCase()
	if got := newTestCase(stepless, "fast/basic.yaml", false); got.Step != "" {
		t.Fatalf("a step-less case rendered Step = %q, want empty", got.Step)
	}
}

// TestIsUnsupportedErr pins the conservative predicate: only the three
// documented markers classify as unsupported, and a nil error never
// does.
func TestIsUnsupportedErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"501", errText("status=501 body=nope"), true},
		{"not-implemented", errText("status=400 body=not implemented: ip()"), true},
		{"unsupported", errText("unsupported aggregation"), true},
		{"plain-400", errText("status=400 body=parse error at line 1"), false},
		{"500", errText("status=500 body=internal"), false},
		{"transport", errText("http call: connection refused"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isUnsupportedErr(tc.err); got != tc.want {
				t.Fatalf("isUnsupportedErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type errText string

func (e errText) Error() string { return string(e) }

// TestWriteReport_ToFile — the report path writes the exact payload, so a
// downstream consumer parses what the driver built.
func TestWriteReport_ToFile(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(Report{TotalResults: 2, IncludePassing: true, Results: []Result{{}, {Diff: "d"}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeReport(path, payload); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // test-local temp path
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var back Report
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("the written file is not the payload we handed in: %v", err)
	}
	if back.TotalResults != 2 || len(back.Results) != 2 || back.Results[1].Diff != "d" {
		t.Fatalf("round-tripped report = %+v", back)
	}
}

// TestWriteReport_UnwritablePathErrors — a write failure must surface, or
// a CI run would report success having produced no artefact.
func TestWriteReport_UnwritablePathErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "no-such-dir", "report.json")
	if err := writeReport(path, []byte("{}")); err == nil {
		t.Fatal("writeReport into a missing directory returned nil error")
	}
}
