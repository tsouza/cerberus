package migrateverify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLokiLogStreamDialect_PinsTheReplayEncoding pins every parameter of the
// log-stream probe, because each wrong choice fails SILENTLY — by returning a
// comparable-looking result that answers a different question.
//
//   - no limit / direction: the two backends default them independently, so the
//     truncation boundary is no longer a property of the replay;
//   - a limit above 5000: reference Loki 400s while cerberus clamps, so the two
//     sides answer different questions;
//   - an interval or a step: cerberus's log path parses no interval, so only the
//     reference would honour it;
//   - the categorize-labels header: the two backends emit structurally different
//     third elements under it, manufacturing a difference on every entry.
func TestLokiLogStreamDialect_PinsTheReplayEncoding(t *testing.T) {
	body := logStreamBody(streamSpec{
		labels:  map[string]string{"job": "api"},
		entries: []entrySpec{{tsNano: "1780000000000000001", line: "a"}},
	})
	ref := newLogStreamServer(t, body, http.StatusOK)
	cer := newLogStreamServer(t, body, http.StatusOK)
	p := testParams()
	rep := Verify(context.Background(),
		Corpus{Queries: []Query{logStreamQuery(`{job="api"} |= "boom"`)}},
		logStreamLanes(ref, cer), p)
	if rep.Results[0].Verdict != VerdictMatch {
		t.Fatalf("fixture must produce a comparison, got %q (%s)", rep.Results[0].Verdict, rep.Results[0].Detail)
	}

	for _, side := range []struct {
		name string
		srv  *logStreamServer
	}{{"reference", ref}, {"cerberus", cer}} {
		q := side.srv.params
		if got := q.Get("query"); got != `{job="api"} |= "boom"` {
			t.Errorf("%s: query = %q, want the corpus expression verbatim", side.name, got)
		}
		if got := q.Get("limit"); got != "5000" {
			t.Errorf("%s: limit = %q, want 5000 — upstream Loki's max_entries_limit_per_query default, above which it 400s while cerberus clamps",
				side.name, got)
		}
		if got := q.Get("direction"); got != "backward" {
			t.Errorf("%s: direction = %q, want backward pinned explicitly, so a default change cannot move the truncation boundary",
				side.name, got)
		}
		if got := q.Get("start"); got != p.Start.Format(time.RFC3339Nano) {
			t.Errorf("%s: start = %q, want RFC3339Nano %q", side.name, got, p.Start.Format(time.RFC3339Nano))
		}
		if got := q.Get("end"); got != p.End.Format(time.RFC3339Nano) {
			t.Errorf("%s: end = %q, want RFC3339Nano %q", side.name, got, p.End.Format(time.RFC3339Nano))
		}
		for _, unwanted := range []string{"interval", "step"} {
			if q.Has(unwanted) {
				t.Errorf("%s: %s=%q was sent; cerberus's log path parses no interval, so only the reference would honour it",
					side.name, unwanted, q.Get(unwanted))
			}
		}
		if got := side.srv.header.Get("X-Loki-Response-Encoding-Flags"); got != "" {
			t.Errorf("%s: categorize-labels header %q was sent; under it the two backends emit structurally different third elements",
				side.name, got)
		}
	}
}

// TestVerify_NonMatrixQueryWithoutAKindBackendIsUnconfigured pins that a lane with
// no transport for a shape reports the query as UNCONFIGURED and blocks. The
// alternative failure modes are both worse: a nil dereference, or a silent skip
// that leaves the operator's log panels unjudged behind a green gate.
func TestVerify_NonMatrixQueryWithoutAKindBackendIsUnconfigured(t *testing.T) {
	body := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{{tsNano: "1780000000000000001", line: "a"}}})
	srv := newLogStreamServer(t, body, http.StatusOK)
	// A matrix-only lane: Ref/Cerberus are wired, the kind backends are not.
	lanes := map[string]Lane{HeadLoki: {
		Ref:      NewHTTPBackend(srv.url, WithDialect(LokiDialect())),
		Cerberus: NewHTTPBackend(srv.url, WithDialect(LokiDialect())),
	}}
	rep := Verify(context.Background(), Corpus{Queries: []Query{logStreamQuery(`{job="api"}`)}}, lanes, testParams())

	if rep.Summary.Total != 0 {
		t.Errorf("Total = %d, want 0: a query that was never issued must not inflate the denominator", rep.Summary.Total)
	}
	if len(rep.Unconfigured) != 1 {
		t.Fatalf("Unconfigured = %+v, want the one log-stream query enumerated", rep.Unconfigured)
	}
	if !strings.Contains(rep.Unconfigured[0].Reason, KindLogStream) {
		t.Errorf("Unconfigured reason = %q, want it to name the shape the lane could not fetch", rep.Unconfigured[0].Reason)
	}
	if !rep.Failed() {
		t.Error("a query the gate never issued must block: it cannot be claimed as parity")
	}
}

// TestVerify_QueryWithNoComparatorIsAnError pins that an unroutable shape fails
// loudly. Scoring it as anything else would let a query reach the report as
// judged when no definition of equality was ever applied to it.
func TestVerify_QueryWithNoComparatorIsAnError(t *testing.T) {
	body := logStreamBody()
	srv := newLogStreamServer(t, body, http.StatusOK)
	lanes := logStreamLanes(srv, srv)
	q := Query{Expr: `{ span.a = "b" }`, Source: "panel:x", Head: HeadLoki, Lang: "traceql", Kind: KindTraceSearch}

	rep := Verify(context.Background(), Corpus{Queries: []Query{q}}, lanes, testParams())
	res := rep.Results[0]
	if res.Verdict != VerdictError {
		t.Fatalf("verdict = %q, want error: no comparator judged this shape", res.Verdict)
	}
	if !strings.Contains(res.Detail, KindTraceSearch) {
		t.Errorf("detail = %q, want it to name the shape that has no comparator", res.Detail)
	}
	if !rep.Failed() {
		t.Error("a query no comparator judged must block")
	}
}

// TestVerify_HealthyFamilyDoesNotVouchForADeadSiblingFamily pins the masking rule
// one level below the per-head split: a single loki lane that compared 12 log
// entries and zero series has NOT proved its metric queries. Keyed on the head's
// summed evidence, this run reads green.
func TestVerify_HealthyFamilyDoesNotVouchForADeadSiblingFamily(t *testing.T) {
	logBody := logStreamBody(streamSpec{
		labels: map[string]string{"job": "api"},
		entries: []entrySpec{
			{tsNano: "1780000000000000001", line: "boot"},
			{tsNano: "1780000000000000002", line: "serve"},
		},
	})
	// The metric query answers an EMPTY matrix on both sides: all-match, zero
	// series compared — exactly the shape a verdict-count guard clears.
	emptyMatrix := matrix()
	const metricExpr = `sum(rate({job="api"}[5m]))`

	srv := newDualLokiServer(t, map[string]string{metricExpr: emptyMatrix}, logBody)
	backend := NewHTTPBackend(srv.URL, WithDialect(LokiDialect()), WithKindDialects(LokiLogStreamDialect()))
	lanes := map[string]Lane{HeadLoki: {Ref: backend, Cerberus: backend, RefKind: backend, CerberusKind: backend}}

	rep := Verify(context.Background(), Corpus{Queries: []Query{
		{Expr: metricExpr, Source: "panel:lograte", Head: HeadLoki, Lang: "logql", Kind: KindMetricMatrix},
		logStreamQuery(`{job="api"}`),
	}}, lanes, testParams())

	if rep.Summary.Match != 2 {
		t.Fatalf("summary = %+v, want both queries matching — the shape the family split exists to see through", rep.Summary)
	}
	if rep.Summary.ComparedUnits == 0 {
		t.Fatalf("ComparedUnits = 0: the head DID compare something, which is why only a per-family rule can catch this")
	}
	dead := rep.DeadFamilies()
	if len(dead) != 1 {
		t.Fatalf("DeadFamilies = %+v, want exactly the metric-matrix family", dead)
	}
	if dead[0].Head != HeadLoki || dead[0].Kind != KindMetricMatrix || dead[0].Unit != UnitSeries {
		t.Errorf("dead family = %+v, want loki/metric-matrix counted in series", dead[0])
	}
	if !rep.Failed() {
		t.Error("a lane whose metric family compared nothing must block, however many log entries its sibling family diffed")
	}
	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	// The per-head row must split, or the two families' evidence is summed back
	// into one number and the masking returns in the human report.
	for _, want := range []string{
		"log-stream     1 replayed",
		"metric-matrix  1 replayed",
		"2 log entries compared",
		"0 series compared",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the per-head table must break the lane down per shape (%q), got:\n%s", want, out.String())
		}
	}
}

// newDualLokiServer answers BOTH Loki contracts on the one query_range endpoint:
// a metric expression gets its matrix body, anything else gets the streams body.
// One server, because one backend serves both shapes in production too.
func newDualLokiServer(t *testing.T, byMetricExpr map[string]string, streams string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != lokiQueryRangePath {
			t.Errorf("probe hit path %q, want %q", r.URL.Path, lokiQueryRangePath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if body, ok := byMetricExpr[r.URL.Query().Get("query")]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(streams))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVerify_MatrixFirstDiffCarriesItsShapeAndReasonCode pins that the matrix
// comparator's diffs are stamped with the vocabulary other code switches on. The
// candidate-cause attribution reads the reason CODE, so a diff that lost it would
// silently drop its data-window and ingest hints.
func TestVerify_MatrixFirstDiffCarriesItsShapeAndReasonCode(t *testing.T) {
	point := []pointSpec{{1_700_000_000, "1"}}
	cases := []struct {
		name             string
		refBody, cerBody map[string]string
		wantCode         string
		wantGap          bool
	}{
		{
			name:     "value differs",
			refBody:  map[string]string{"q": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: point})},
			cerBody:  map[string]string{"q": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "2"}}})},
			wantCode: ReasonValueDiffers,
		},
		{
			name:    "series missing",
			refBody: map[string]string{"q": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: point})},
			cerBody: map[string]string{"q": matrix()},
			// A series only one backend has is a coverage gap, not a maths difference.
			wantCode: ReasonSeriesMissing, wantGap: true,
		},
		{
			name:     "no sample at step",
			refBody:  map[string]string{"q": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}, {1_700_000_060, "1"}}})},
			cerBody:  map[string]string{"q": matrix(seriesSpec{labels: map[string]string{"job": "a"}, points: point})},
			wantCode: ReasonNoSampleAtStep, wantGap: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runVerifyOne(t, tc.refBody, tc.cerBody, promQuery("q", "rule:q"))
			if res.Verdict != VerdictDiverge {
				t.Fatalf("verdict = %q, want diverge", res.Verdict)
			}
			if res.Kind != KindMetricMatrix {
				t.Errorf("result kind = %q, want %q", res.Kind, KindMetricMatrix)
			}
			if res.ComparedUnits != res.ComparedSeries {
				t.Errorf("ComparedUnits = %d / ComparedSeries = %d: the matrix lane's two counters must agree",
					res.ComparedUnits, res.ComparedSeries)
			}
			fd := res.FirstDiff
			if fd == nil {
				t.Fatal("a divergence must carry a first-diff")
				return
			}
			if fd.Kind != KindMetricMatrix {
				t.Errorf("first-diff kind = %q, want %q", fd.Kind, KindMetricMatrix)
			}
			if fd.ReasonCode != tc.wantCode {
				t.Errorf("first-diff code = %q, want %q (reason: %q)", fd.ReasonCode, tc.wantCode, fd.Reason)
			}
			if isCoverageGap(fd.ReasonCode) != tc.wantGap {
				t.Errorf("isCoverageGap(%q) = %v, want %v: the attribution hints hang off this classification",
					fd.ReasonCode, isCoverageGap(fd.ReasonCode), tc.wantGap)
			}
			if fd.TimestampNano != "" || fd.Stream != "" {
				t.Errorf("first-diff = %+v: a matrix diff must not borrow the log lane's anchors", fd)
			}
		})
	}
}

// TestMergeLimitations_IsCodeSortedAndSums pins the roll-up's determinism and its
// arithmetic. Limitations arrive in comparator order, which varies with the
// corpus, so a merge that preserved arrival order would make two identical runs
// produce different artifacts.
func TestMergeLimitations_IsCodeSortedAndSums(t *testing.T) {
	got := mergeLimitations(
		[]Limitation{limitation(LimitLogStructuredMeta, 3), limitation(LimitLogEntryOrder, 3)},
		[]Limitation{limitation(LimitLogEntryOrder, 4), limitation(LimitLogTruncationBand, 2)},
	)
	want := []Limitation{
		limitation(LimitLogEntryOrder, 7),
		limitation(LimitLogStructuredMeta, 3),
		limitation(LimitLogTruncationBand, 2),
	}
	if len(got) != len(want) {
		t.Fatalf("merged = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("merged[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A per-query limitation may carry that query's specifics; the merged one must
	// not, or a roll-up would quote one query's boundary as if it held for all.
	perQuery := Limitation{Code: LimitLogTruncationBand, Count: 2, Detail: limitationDetails[LimitLogTruncationBand] + " The boundary was timestamp 5 ns, at limit=5000."}
	merged := mergeLimitations(nil, []Limitation{perQuery})
	if merged[0].Detail != limitationDetails[LimitLogTruncationBand] {
		t.Errorf("merged detail = %q, want the canonical per-code explanation", merged[0].Detail)
	}
	if mergeLimitations(nil, nil) != nil {
		t.Error("merging nothing must yield nil, so an omitempty field round-trips to itself")
	}
}
