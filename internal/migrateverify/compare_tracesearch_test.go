package migrateverify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// spanSpec / traceSpec / searchMetricsSpec build a Tempo /api/search body from a
// small spec. The two nanosecond fields are written as the decimal STRINGS both
// backends emit for 64-bit proto fields, and durationMs as the bare number
// proto3 JSON leaves a 32-bit field, so a decoder that handled only one of the
// two encodings could not pass these fixtures.
type spanSpec struct {
	id            string
	name          string
	startNano     string
	durationNanos string
}

// traceSpec is one trace summary. matched == 0 renders no `matched` key at all,
// exactly as proto3 JSON omits a zero, so the fixture exercises the reading that
// keeps an uncapped set uncapped.
type traceSpec struct {
	id         string
	service    string
	name       string
	startNano  string
	durationMs int
	matched    int
	spans      []spanSpec
	// legacySpanSet also renders the deprecated single `spanSet` field next to
	// `spanSets`, which is what cerberus's Tempo head actually emits.
	legacySpanSet bool
}

// searchMetricsSpec is the per-search block. The reference fills the job
// counters; cerberus reports its own aggregates as hard zeros.
type searchMetricsSpec struct {
	totalJobs       int
	completedJobs   int
	inspectedTraces int
}

func (m searchMetricsSpec) render() string {
	return fmt.Sprintf(`{"inspectedTraces":%d,"inspectedBytes":0,"totalBlocks":0,"totalJobs":%d,"completedJobs":%d}`,
		m.inspectedTraces, m.totalJobs, m.completedJobs)
}

func (s spanSpec) render() string {
	return fmt.Sprintf(`{"spanID":%q,"name":%q,"startTimeUnixNano":%q,"durationNanos":%q}`,
		s.id, s.name, s.startNano, s.durationNanos)
}

func (t traceSpec) renderSpanSet() string {
	var b strings.Builder
	b.WriteString(`{"spans":[`)
	for i, sp := range t.spans {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(sp.render())
	}
	b.WriteString("]")
	if t.matched > 0 {
		fmt.Fprintf(&b, `,"matched":%d`, t.matched)
	}
	b.WriteString("}")
	return b.String()
}

func (t traceSpec) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"traceID":%q,"rootServiceName":%q,"rootTraceName":%q,"startTimeUnixNano":%q,"durationMs":%d`,
		t.id, t.service, t.name, t.startNano, t.durationMs)
	set := t.renderSpanSet()
	b.WriteString(`,"spanSets":[` + set + `]`)
	if t.legacySpanSet {
		b.WriteString(`,"spanSet":` + set)
	}
	b.WriteString("}")
	return b.String()
}

// traceSearchBody renders the /api/search envelope both backends return. Cerberus's
// Tempo head mirrors tempopb.SearchResponse's JSON, so one builder stands in for
// both sides and a difference in a fixture is a difference in the DATA, never in
// the encoding.
func traceSearchBody(m searchMetricsSpec, traces ...traceSpec) string {
	var b strings.Builder
	b.WriteString(`{"traces":[`)
	for i, t := range traces {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(t.render())
	}
	b.WriteString(`],"metrics":` + m.render() + "}")
	return b.String()
}

// traceSearchServer answers Tempo's search endpoint with a fixed body, AND
// /api/traces/{id} for every trace that body mentions (see
// traceByIDBodiesFromSearch) — a trace-search comparison derives a
// trace-by-id fetch for every trace ID both sides return, so a fake server
// that only answered the search path would leave every derived probe 404ing
// into a dead trace-by-id family. It records the request it was asked, so a
// test can assert both the comparison and the wire encoding that produced it.
type traceSearchServer struct {
	url    string
	params url.Values
	header http.Header
}

func newTraceSearchServer(t *testing.T, body string, status int) *traceSearchServer {
	t.Helper()
	rec := &traceSearchServer{}
	byID := traceByIDBodiesFromSearch(t, body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := strings.CutPrefix(r.URL.Path, tempoTraceByIDPathPrefix); ok {
			traceByIDBody, ok := byID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(traceByIDBody))
			return
		}
		if r.URL.Path != tempoSearchPath {
			t.Errorf("trace-search probe hit path %q, want %q or %q", r.URL.Path, tempoSearchPath, tempoTraceByIDPathPrefix)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rec.params = r.URL.Query()
		rec.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	rec.url = srv.URL
	return rec
}

// traceByIDBodiesFromSearch decodes a rendered /api/search body with the SAME
// production decoder the comparator uses, and renders one trace-by-id body per
// trace it mentions — so a derived fetch always gets an answer that agrees
// with whatever this same search response already claimed about that trace's
// spans. A search body with zero traces yields no bodies at all, which is
// correct: no common trace ID, no derived fetch, nothing to serve.
func traceByIDBodiesFromSearch(t *testing.T, searchBody string) map[string]string {
	t.Helper()
	decoded, err := decodeTraceSearch([]byte(searchBody))
	if err != nil {
		t.Fatalf("traceByIDBodiesFromSearch: decode fixture search body: %v", err)
	}
	res, ok := decoded.(traceSearchResult)
	if !ok {
		t.Fatalf("traceByIDBodiesFromSearch: decodeTraceSearch returned %T, want traceSearchResult", decoded)
	}
	out := make(map[string]string, len(res.Traces))
	for _, tr := range res.Traces {
		var b strings.Builder
		b.WriteString(`{"batches":[{"resource":{"attributes":{}},"spans":[`)
		for i, sp := range tr.Spans {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"traceId":%q,"spanId":%q,"name":%q,"startTimeUnixNano":%q,"durationNanos":%d}`,
				tr.TraceID, sp.SpanID, sp.Name, fmt.Sprintf("%d", sp.StartTimeUnixNano), sp.DurationNanos)
		}
		b.WriteString(`]}]}`)
		out[tr.TraceID] = b.String()
	}
	return out
}

// traceSearchLanes wires one tempo lane whose matrix, trace-search, AND
// trace-by-id contracts are served by the SAME backend pair, exactly as the
// CLI builds it.
func traceSearchLanes(ref, cerberus *traceSearchServer) map[string]Lane {
	mk := func(u string) *HTTPBackend {
		return NewHTTPBackend(u, WithDialect(TempoDialect()),
			WithKindDialects(TempoSearchDialect(), TempoTraceByIDDialect()))
	}
	r, c := mk(ref.url), mk(cerberus.url)
	return map[string]Lane{HeadTempo: {Ref: r, Cerberus: c, RefKind: r, CerberusKind: c}}
}

// searchQuery is one corpus entry routed to the trace-search comparator.
func searchQuery(expr string) Query {
	return Query{Expr: expr, Source: "panel:traces", Head: HeadTempo, Lang: "traceql", Kind: KindTraceSearch}
}

// runTraceSearch replays one trace-search query against two fixed bodies and
// returns the whole report, so a test can assert the verdict AND the evidence
// the report attributes to the trace-search family. It also drives the
// trace-search comparator's derived trace-by-id fetches for every trace ID
// both bodies mention (see traceByIDBodiesFromSearch), so a positive fixture's
// trace-by-id family is never left dead by an unanswered derived probe.
func runTraceSearch(t *testing.T, refBody, cerBody string) Report {
	t.Helper()
	ref := newTraceSearchServer(t, refBody, http.StatusOK)
	cer := newTraceSearchServer(t, cerBody, http.StatusOK)
	return Verify(context.Background(),
		Corpus{Queries: []Query{searchQuery(`{ span.http.status_code = 500 }`)}},
		traceSearchLanes(ref, cer), testParams())
}

// searchFamily returns the tempo head's trace-search family row, failing if the
// report carries none: a run that judged a search and reported no family for it
// would leave the dead-family rule nothing to check.
func searchFamily(t *testing.T, rep Report) FamilySummary {
	t.Helper()
	for _, h := range rep.Heads {
		if h.Head != HeadTempo {
			continue
		}
		for _, f := range h.Families {
			if f.Kind == KindTraceSearch {
				return f
			}
		}
	}
	t.Fatalf("report carries no tempo trace-search family: %+v", rep.Heads)
	return FamilySummary{}
}

// traceByIDFamily is searchFamily's sibling for the DERIVED trace-by-id
// family.
func traceByIDFamily(t *testing.T, rep Report) FamilySummary {
	t.Helper()
	for _, h := range rep.Heads {
		if h.Head != HeadTempo {
			continue
		}
		for _, f := range h.Families {
			if f.Kind == KindTraceByID {
				return f
			}
		}
	}
	t.Fatalf("report carries no tempo trace-by-id family: %+v", rep.Heads)
	return FamilySummary{}
}

// limitationByCode returns the limitation carrying code, and fails when the
// comparison did not state it. A missing limitation is the hollow-green failure
// this whole mechanism exists to prevent, so its absence is never tolerated.
func limitationByCode(t *testing.T, got []Limitation, code string) Limitation {
	t.Helper()
	for _, l := range got {
		if l.Code == code {
			return l
		}
	}
	t.Fatalf("limitations = %+v, want one carrying %q: a dimension that was not judged must be stated", got, code)
	return Limitation{}
}

// completeMetrics is a reference that finished every job it started, which is the
// only shape in which its set membership is evidence of anything.
var completeMetrics = searchMetricsSpec{totalJobs: 4, completedJobs: 4, inspectedTraces: 9}

// cerberusMetrics is what cerberus's own Tempo head reports: an aggregate block of
// hard zeros, with no job accounting at all.
var cerberusMetrics = searchMetricsSpec{}

// hexTraceID renders the nth fixture trace's canonical 32-hex identifier.
func hexTraceID(n int) string { return fmt.Sprintf("%032x", n) }

// hexSpanID renders the nth fixture span's canonical 16-hex identifier.
func hexSpanID(n int) string { return fmt.Sprintf("%016x", n) }

// fixtureTrace is the shape every positive fixture reuses: one trace with one
// uncapped matched span.
func fixtureTrace(n int) traceSpec {
	return traceSpec{
		id:         hexTraceID(n),
		service:    "checkout",
		name:       "GET /cart",
		startNano:  fmt.Sprintf("178000000000000%04d", n),
		durationMs: 40 + n,
		matched:    1,
		spans: []spanSpec{{
			id:            hexSpanID(n),
			startNano:     fmt.Sprintf("178000000000000%04d", n),
			durationNanos: fmt.Sprintf("%d", 1_000_000*(40+n)),
		}},
		legacySpanSet: true,
	}
}

// TestVerify_TraceSearchAgreementCountsEveryTrace pins the positive case with its
// EVIDENCE, not just its verdict: two empty responses also score a match, so a
// test that asserted only "match" would pass over a comparator that compared
// nothing at all.
func TestVerify_TraceSearchAgreementCountsEveryTrace(t *testing.T) {
	traces := []traceSpec{fixtureTrace(1), fixtureTrace(2)}
	rep := runTraceSearch(t,
		traceSearchBody(completeMetrics, traces...),
		traceSearchBody(cerberusMetrics, traces...))

	// One search result plus one DERIVED trace-by-id fetch per trace both
	// backends returned (2 here, under maxDerivedTraceFetchesPerSearch): the
	// trace-search comparator spawns a structural follow-up for every trace ID
	// it found in common, and Verify's second phase replays them through the
	// same accounting — see compareTraceSearch's Derived and Verify's two-phase
	// runBatch.
	const wantDerivedFetches = 2
	if len(rep.Results) != 1+wantDerivedFetches {
		t.Fatalf("want 1 search result + %d derived trace-by-id results, got %+v", wantDerivedFetches, rep.Results)
	}
	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (detail: %s, first-diff: %+v)", res.Verdict, res.Detail, res.FirstDiff)
	}
	if res.Kind != KindTraceSearch {
		t.Errorf("Kind = %q, want %q", res.Kind, KindTraceSearch)
	}
	const wantTraces = 2
	if res.ComparedUnits != wantTraces {
		t.Errorf("ComparedUnits = %d, want %d: every trace both backends returned must be counted", res.ComparedUnits, wantTraces)
	}
	if res.ComparedSeries != 0 {
		t.Errorf("ComparedSeries = %d, want 0: the series counter is matrix-only and must not silently count traces", res.ComparedSeries)
	}
	// A complete, uncapped, unlimited comparison judged every dimension it names,
	// so it must claim NO limitation. A stray one here would tell an operator
	// something went unchecked when it did not.
	if res.Limitations != nil {
		t.Errorf("limitations = %+v, want none: a complete search compared every dimension", res.Limitations)
	}
	f := searchFamily(t, rep)
	if f.Unit != UnitTraces || f.Compared != wantTraces || f.Match != 1 {
		t.Errorf("trace-search family = %+v, want %d traces compared in %q on one match", f, wantTraces, UnitTraces)
	}
	// The trace-search comparator's own DERIVATION must actually have judged
	// something: two derived trace-by-id fetches that all matched, each on the
	// one span fixtureTrace gives it.
	byID := traceByIDFamily(t, rep)
	if byID.Total != wantDerivedFetches || byID.Match != wantDerivedFetches || byID.Compared == 0 {
		t.Errorf("trace-by-id family = %+v, want %d replayed and matched with real evidence compared", byID, wantDerivedFetches)
	}
	for _, res := range rep.Results[1:] {
		if res.Kind != KindTraceByID || res.Verdict != VerdictMatch {
			t.Errorf("derived result = %+v, want a matching trace-by-id fetch", res)
		}
	}
	if rep.Failed() {
		t.Errorf("a trace-search lane that compared %d traces (and matched every derived trace-by-id fetch) must pass, got %+v", wantTraces, rep.Summary)
	}
}

// TestVerify_EmptyTraceSearchIsNotProvenParity is the failure this whole evidence
// mechanism exists to prevent, in its most dangerous form on this endpoint: the
// reference OMITS the traces key when it has no matches while cerberus always
// emits an empty array, so both sides decode to zero traces and naive set equality
// scores a match having compared nothing.
func TestVerify_EmptyTraceSearchIsNotProvenParity(t *testing.T) {
	// The reference's own encoding of "no matches": no traces key at all.
	const refEmpty = `{"metrics":{"totalJobs":4,"completedJobs":4}}`
	rep := runTraceSearch(t, refEmpty, traceSearchBody(cerberusMetrics))

	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: two empty results do agree — the point is that agreeing proves nothing", res.Verdict)
	}
	if res.ComparedUnits != 0 {
		t.Fatalf("ComparedUnits = %d, want 0: nothing was diffed", res.ComparedUnits)
	}
	f := searchFamily(t, rep)
	if f.Compared != 0 || f.Total != 1 {
		t.Errorf("trace-search family = %+v, want one replay that compared nothing", f)
	}
	dead := rep.DeadFamilies()
	if len(dead) != 1 || dead[0].Head != HeadTempo || dead[0].Kind != KindTraceSearch || dead[0].Unit != UnitTraces {
		t.Fatalf("DeadFamilies = %+v, want the tempo trace-search family named", dead)
	}
	if !rep.Failed() {
		t.Error("a trace-search family that compared no traces must block: an all-match run that judged nothing is not parity")
	}
}

// truncatedPair builds two full-page results that OVERLAP but are not equal: the
// reference returns traces [lo, lo+limit) and cerberus returns [lo+skew,
// lo+skew+limit). Both are exactly at the replay limit, so both are prefixes of a
// ranking neither backend's wire contract fixes.
func truncatedPair(skew int, mutate func(t *traceSpec)) (string, string) {
	refTraces := make([]traceSpec, 0, traceSearchReplayLimit)
	cerTraces := make([]traceSpec, 0, traceSearchReplayLimit)
	for i := range traceSearchReplayLimit {
		refTraces = append(refTraces, fixtureTrace(1+i))
		cer := fixtureTrace(1 + i + skew)
		if mutate != nil {
			mutate(&cer)
		}
		cerTraces = append(cerTraces, cer)
	}
	return traceSearchBody(completeMetrics, refTraces...), traceSearchBody(cerberusMetrics, cerTraces...)
}

// TestVerify_TruncatedTraceSearchIsUndecidableNotMatched is the central case of
// the trace-search lane: both backends legitimately returned DIFFERENT SUBSETS.
//
// Each side answered with exactly the replay limit of summaries, so each result is
// a prefix of a ranking that is not a wire contract — cerberus ranks newest-first
// by trace start time, reference Tempo returns what its block search reached
// before the limit. Three traces arrived only from the reference and three only
// from cerberus, and NOTHING in either wire format says which of those is a defect.
//
// Scoring that as a match would claim parity over a membership difference nobody
// established; scoring it as a divergence would cry wolf at a ranking difference
// and send an operator chasing a phantom bug. The honest answer is a verdict of
// its own, with both side counts and the exact residue named — and with every
// trace both sides DID return still field-diffed, which is what keeps a real bug
// from hiding behind the truncation.
func TestVerify_TruncatedTraceSearchIsUndecidableNotMatched(t *testing.T) {
	const skew = 3
	refBody, cerBody := truncatedPair(skew, nil)
	rep := runTraceSearch(t, refBody, cerBody)

	res := rep.Results[0]
	if res.Verdict != VerdictUndecidable {
		t.Fatalf("verdict = %q, want %q: a truncated pair is neither proven equal nor proven different (first-diff: %+v)",
			res.Verdict, VerdictUndecidable, res.FirstDiff)
	}
	if res.FirstDiff != nil {
		t.Errorf("first-diff = %+v, want none: a ranking difference is not a divergence to chase", res.FirstDiff)
	}
	wantCompared := traceSearchReplayLimit - skew
	if res.ComparedUnits != wantCompared {
		t.Errorf("ComparedUnits = %d, want %d: the overlap is exactly what the comparator field-diffed",
			res.ComparedUnits, wantCompared)
	}
	lim := limitationByCode(t, res.Limitations, LimitSearchTruncated)
	if lim.Count != 2*skew {
		t.Errorf("%s count = %d, want %d: the residue is every trace exactly one backend returned",
			LimitSearchTruncated, lim.Count, 2*skew)
	}
	// The per-query detail must carry BOTH side counts and the limit, so an
	// operator can tell a truncated run from a genuinely small result.
	for _, want := range []string{
		fmt.Sprintf("The reference returned %d summaries and cerberus returned %d, at limit=%d.",
			traceSearchReplayLimit, traceSearchReplayLimit, traceSearchReplayLimit),
		"membership was not judged",
	} {
		if !strings.Contains(lim.Detail, want) {
			t.Errorf("%s detail = %q, want it to state %q", LimitSearchTruncated, lim.Detail, want)
		}
	}
	// One undecidable trace-search summary, plus maxDerivedTraceFetchesPerSearch
	// derived trace-by-id fetches — each drawn from the ID overlap, where both
	// sides render the SAME fixtureTrace(n) for a shared n, so every derived
	// fetch matches.
	s := rep.Summary
	if s.Undecidable != 1 || s.Match != maxDerivedTraceFetchesPerSearch || s.Diverge != 0 {
		t.Errorf("summary = %d undecidable / %d match / %d diverge, want one undecidable trace-search query "+
			"plus %d matching derived trace-by-id fetches", s.Undecidable, s.Match, s.Diverge, maxDerivedTraceFetchesPerSearch)
	}
	// Undecidable is an honest "I could not judge this", not a failure: the lane
	// still field-diffed 997 traces and found no difference.
	if rep.Failed() {
		t.Errorf("an undecidable membership over %d field-diffed traces must not block the run", wantCompared)
	}
	if len(rep.DeadFamilies()) != 0 {
		t.Errorf("DeadFamilies = %+v, want none: the family compared %d traces", rep.DeadFamilies(), wantCompared)
	}
}

// TestVerify_TruncatedTraceSearchStillDivergesOnAFieldBug pins the other half of
// the two-regime rule. Truncation declines to judge MEMBERSHIP and nothing else:
// a rootServiceName that differs on a trace both backends returned is a real
// defect, and a short-circuit to "undecidable" ahead of the field diff would let
// it ride out of the gate on any corpus whose searches hit the limit.
func TestVerify_TruncatedTraceSearchStillDivergesOnAFieldBug(t *testing.T) {
	const skew = 3
	// The overlapping window starts at trace 1+skew on the reference side; mutate
	// that same trace on cerberus's side so the bug sits inside the interior both
	// backends returned.
	bugID := hexTraceID(1 + skew)
	refBody, cerBody := truncatedPair(skew, func(tr *traceSpec) {
		if tr.id == bugID {
			tr.service = "checkout-v2"
		}
	})
	rep := runTraceSearch(t, refBody, cerBody)

	res := rep.Results[0]
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: a field bug on a trace BOTH backends returned is not excused by truncation", res.Verdict)
	}
	fd := res.FirstDiff
	if fd == nil {
		t.Fatal("a divergence must carry a first-diff")
		return
	}
	if fd.Kind != KindTraceSearch || fd.ReasonCode != ReasonTraceFieldDiffers {
		t.Errorf("first-diff kind/reason = %q/%q, want %q/%q", fd.Kind, fd.ReasonCode, KindTraceSearch, ReasonTraceFieldDiffers)
	}
	if fd.TraceID != bugID || fd.Field != "rootServiceName" {
		t.Errorf("first-diff = %+v, want it anchored at trace %s field rootServiceName", fd, bugID)
	}
	if fd.RefValue != "checkout" || fd.CerberusValue != "checkout-v2" {
		t.Errorf("first-diff values = %q / %q, want the two backends' own renderings", fd.RefValue, fd.CerberusValue)
	}
	if !rep.Failed() {
		t.Error("a trace-summary field difference must block the run")
	}
}

// TestVerify_TruncatedTraceSearchWithNoOverlapProvesNothing pins the boundary the
// undecidable verdict must never cross. Two full-page results that share no trace
// at all are still not a divergence — but they are not evidence of anything
// either, and a family whose only outcome is an undecidable that compared zero
// traces has to block, exactly as an all-match run that compared zero series does.
func TestVerify_TruncatedTraceSearchWithNoOverlapProvesNothing(t *testing.T) {
	refBody, cerBody := truncatedPair(traceSearchReplayLimit, nil)
	rep := runTraceSearch(t, refBody, cerBody)

	res := rep.Results[0]
	if res.Verdict != VerdictUndecidable {
		t.Fatalf("verdict = %q, want %q", res.Verdict, VerdictUndecidable)
	}
	if res.ComparedUnits != 0 {
		t.Fatalf("ComparedUnits = %d, want 0: the two results share no trace", res.ComparedUnits)
	}
	dead := rep.DeadFamilies()
	if len(dead) != 1 || dead[0].Kind != KindTraceSearch {
		t.Fatalf("DeadFamilies = %+v, want the tempo trace-search family named", dead)
	}
	if !rep.Failed() {
		t.Error("an undecidable verdict over zero compared traces must block: it proved nothing")
	}
}

// TestVerify_TraceReturnedByOneBackendOnlyDiverges pins the regime where set
// membership IS decidable: neither side hit the limit, so both answered
// completely, and a trace exactly one of them returned is a real difference.
func TestVerify_TraceReturnedByOneBackendOnlyDiverges(t *testing.T) {
	both := fixtureTrace(1)
	extra := fixtureTrace(2)
	cases := []struct {
		name          string
		ref, cerberus []traceSpec
		refValue      string
		cerValue      string
	}{
		{
			name: "reference only", ref: []traceSpec{both, extra}, cerberus: []traceSpec{both},
			refValue: `rootServiceName="checkout" rootTraceName="GET /cart" startTimeUnixNano=1780000000000000002`,
			cerValue: missingTraceValue,
		},
		{
			name: "cerberus only", ref: []traceSpec{both}, cerberus: []traceSpec{both, extra},
			refValue: missingTraceValue,
			cerValue: `rootServiceName="checkout" rootTraceName="GET /cart" startTimeUnixNano=1780000000000000002`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := runTraceSearch(t,
				traceSearchBody(completeMetrics, tc.ref...),
				traceSearchBody(cerberusMetrics, tc.cerberus...))
			res := rep.Results[0]
			if res.Verdict != VerdictDiverge {
				t.Fatalf("verdict = %q, want diverge: both results are complete, so a one-sided trace is a real difference", res.Verdict)
			}
			fd := res.FirstDiff
			if fd == nil {
				t.Fatal("a divergence must carry a first-diff")
				return
			}
			if fd.ReasonCode != ReasonTraceMissing || fd.TraceID != extra.id {
				t.Errorf("first-diff = %+v, want %q anchored at trace %s", fd, ReasonTraceMissing, extra.id)
			}
			if fd.RefValue != tc.refValue || fd.CerberusValue != tc.cerValue {
				t.Errorf("first-diff values = %q / %q, want %q / %q", fd.RefValue, fd.CerberusValue, tc.refValue, tc.cerValue)
			}
			// The one trace both backends returned was still compared, so the
			// divergence is reported over real evidence rather than over an absence.
			if res.ComparedUnits != 1 {
				t.Errorf("ComparedUnits = %d, want 1", res.ComparedUnits)
			}
			if !rep.Failed() {
				t.Error("a one-sided trace in a complete result must block")
			}
		})
	}
}

// TestVerify_SearchMetricsBlockNeverDiverges pins the field pair that must NOT be
// diffed. Cerberus's inspectedTraces is a ClickHouse row count and its
// inspectedBytes / totalBlocks are hard zeros; the reference's counters describe a
// sharded block search cerberus has no analogue for. Diffing them would make every
// tempo run red forever, which is a gate an operator learns to ignore.
func TestVerify_SearchMetricsBlockNeverDiverges(t *testing.T) {
	traces := []traceSpec{fixtureTrace(1)}
	rep := runTraceSearch(t,
		traceSearchBody(searchMetricsSpec{totalJobs: 12, completedJobs: 12, inspectedTraces: 4096}, traces...),
		traceSearchBody(searchMetricsSpec{}, traces...))

	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: the aggregate block is not a comparable dimension (first-diff: %+v)", res.Verdict, res.FirstDiff)
	}
	if res.ComparedUnits != 1 {
		t.Errorf("ComparedUnits = %d, want 1", res.ComparedUnits)
	}
}

// TestVerify_PartialReferenceSearchIsUndecidable pins the ONE counter inside that
// block the comparator does read. A reference that completed fewer jobs than it
// started is telling the gate its own answer is incomplete, so the traces it did
// not return are not evidence of absence and membership cannot be diffed against
// cerberus's complete answer.
func TestVerify_PartialReferenceSearchIsUndecidable(t *testing.T) {
	shared := []traceSpec{fixtureTrace(1), fixtureTrace(2)}
	partial := searchMetricsSpec{totalJobs: 7, completedJobs: 3, inspectedTraces: 11}
	rep := runTraceSearch(t,
		traceSearchBody(partial, shared...),
		traceSearchBody(cerberusMetrics, shared...))

	res := rep.Results[0]
	if res.Verdict != VerdictUndecidable {
		t.Fatalf("verdict = %q, want %q: a partial reference has no complete set to compare against", res.Verdict, VerdictUndecidable)
	}
	if res.ComparedUnits != len(shared) {
		t.Errorf("ComparedUnits = %d, want %d: the traces it DID return were still field-diffed", res.ComparedUnits, len(shared))
	}
	lim := limitationByCode(t, res.Limitations, LimitSearchRefPartial)
	if lim.Count != 1 {
		t.Errorf("%s count = %d, want 1: one search was answered partially", LimitSearchRefPartial, lim.Count)
	}
	if !strings.Contains(lim.Detail, "The reference completed 3 of 7 jobs.") {
		t.Errorf("%s detail = %q, want both job counts named", LimitSearchRefPartial, lim.Detail)
	}
	if rep.Failed() {
		t.Error("a partial reference is the reference's own limitation, not a cerberus defect: it must not block")
	}
}

// TestVerify_PartialReferenceStillDivergesOnAFieldBug pins that the partial-reference
// rule declines MEMBERSHIP only, exactly as truncation does.
func TestVerify_PartialReferenceStillDivergesOnAFieldBug(t *testing.T) {
	refTrace := fixtureTrace(1)
	cerTrace := fixtureTrace(1)
	cerTrace.durationMs = refTrace.durationMs + 1
	rep := runTraceSearch(t,
		traceSearchBody(searchMetricsSpec{totalJobs: 7, completedJobs: 3}, refTrace),
		traceSearchBody(cerberusMetrics, cerTrace))

	res := rep.Results[0]
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge", res.Verdict)
	}
	fd := res.FirstDiff
	if fd == nil {
		t.Fatal("a divergence must carry a first-diff")
		return
	}
	if fd.Field != "durationMs" || fd.RefValue != "41" || fd.CerberusValue != "42" {
		t.Errorf("first-diff = %+v, want durationMs 41 vs 42 compared exactly", fd)
	}
	if !rep.Failed() {
		t.Error("a durationMs difference is at least a whole millisecond apart and must block")
	}
}

// cappedTrace is one trace whose matched spanset was cut at the spans-per-set cap:
// it matched ten spans and returned two of them.
func cappedTrace(id int, spanIDs ...int) traceSpec {
	tr := fixtureTrace(id)
	tr.matched = 10
	tr.spans = nil
	for _, s := range spanIDs {
		tr.spans = append(tr.spans, spanSpec{
			id:            hexSpanID(s),
			startNano:     fmt.Sprintf("178000000000000%04d", s),
			durationNanos: "5000000",
		})
	}
	return tr
}

// TestVerify_CappedSpanSetIsJudgedOnCountsNotMembers pins the spanset rule. Which
// matched spans survive the cap is unspecified upstream, so two backends can
// legitimately keep different ones; the comparator judges the uncapped matched
// total and the kept-span count — both pure functions of query and data — and says
// in a counted limitation that it did not judge the members.
func TestVerify_CappedSpanSetIsJudgedOnCountsNotMembers(t *testing.T) {
	rep := runTraceSearch(t,
		traceSearchBody(completeMetrics, cappedTrace(1, 11, 12)),
		traceSearchBody(cerberusMetrics, cappedTrace(1, 21, 22)))

	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: which spans survive the cap is not a comparable dimension (first-diff: %+v)",
			res.Verdict, res.FirstDiff)
	}
	lim := limitationByCode(t, res.Limitations, LimitSpanSetCapped)
	if lim.Count != 1 {
		t.Errorf("%s count = %d, want 1 trace covered", LimitSpanSetCapped, lim.Count)
	}
	if res.ComparedUnits != 1 {
		t.Errorf("ComparedUnits = %d, want 1", res.ComparedUnits)
	}
}

// TestVerify_CappedSpanSetStillDivergesOnItsCounts pins that the cap declines the
// MEMBERS only. The uncapped matched total is what the TraceQL expression found in
// that trace, so a disagreement there is a genuine query-evaluation defect and must
// survive the cap rule intact.
func TestVerify_CappedSpanSetStillDivergesOnItsCounts(t *testing.T) {
	cer := cappedTrace(1, 21, 22)
	cer.matched = 11
	rep := runTraceSearch(t,
		traceSearchBody(completeMetrics, cappedTrace(1, 11, 12)),
		traceSearchBody(cerberusMetrics, cer))

	res := rep.Results[0]
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: the uncapped matched total is comparable whatever the cap kept", res.Verdict)
	}
	fd := res.FirstDiff
	if fd == nil {
		t.Fatal("a divergence must carry a first-diff")
		return
	}
	if fd.Field != "spanSets.matched" || fd.RefValue != "10" || fd.CerberusValue != "11" {
		t.Errorf("first-diff = %+v, want spanSets.matched 10 vs 11", fd)
	}
}

// TestVerify_MatchedSpanDifferencesDivergeOnAnUncappedSpanSet pins the regime where
// span membership IS decidable: an uncapped spanset returned every span the query
// matched, so its members are a complete answer and diff exactly.
func TestVerify_MatchedSpanDifferencesDivergeOnAnUncappedSpanSet(t *testing.T) {
	base := fixtureTrace(1)
	base.matched = 2
	base.spans = []spanSpec{
		{id: hexSpanID(11), startNano: "1780000000000000011", durationNanos: "5000000"},
		{id: hexSpanID(12), startNano: "1780000000000000012", durationNanos: "6000000"},
	}

	renamed := base
	renamed.spans = []spanSpec{base.spans[0], {
		id: hexSpanID(12), name: "db.query", startNano: "1780000000000000012", durationNanos: "6000000",
	}}
	swapped := base
	swapped.spans = []spanSpec{base.spans[0], {
		id: hexSpanID(13), startNano: "1780000000000000012", durationNanos: "6000000",
	}}

	cases := []struct {
		name      string
		cerberus  traceSpec
		wantCode  string
		wantSpan  string
		wantField string
	}{
		{name: "span field differs", cerberus: renamed, wantCode: ReasonSpanFieldDiffers, wantSpan: hexSpanID(12), wantField: "name"},
		{name: "span present on one side only", cerberus: swapped, wantCode: ReasonSpanMissing, wantSpan: hexSpanID(12)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := runTraceSearch(t,
				traceSearchBody(completeMetrics, base),
				traceSearchBody(cerberusMetrics, tc.cerberus))
			res := rep.Results[0]
			if res.Verdict != VerdictDiverge {
				t.Fatalf("verdict = %q, want diverge", res.Verdict)
			}
			fd := res.FirstDiff
			if fd == nil {
				t.Fatal("a divergence must carry a first-diff")
				return
			}
			if fd.ReasonCode != tc.wantCode || fd.SpanID != tc.wantSpan || fd.Field != tc.wantField {
				t.Errorf("first-diff = %+v, want %q at span %s field %q", fd, tc.wantCode, tc.wantSpan, tc.wantField)
			}
			if fd.TraceID != base.id {
				t.Errorf("first-diff trace = %q, want the trace the span sits in (%s)", fd.TraceID, base.id)
			}
			if !rep.Failed() {
				t.Error("a matched-span difference on a complete spanset must block")
			}
		})
	}
}

// TestVerify_DuplicateTraceSummaryIsAnError pins that a repeated trace ID inside one
// response is reported, not folded away. A search returns one summary per matching
// trace, so a duplicate is a genuine wire anomaly, and a first-wins collapse would
// silently discard whichever copy disagreed — the exact defect the comparison
// exists to find.
func TestVerify_DuplicateTraceSummaryIsAnError(t *testing.T) {
	dup := fixtureTrace(1)
	rep := runTraceSearch(t,
		traceSearchBody(completeMetrics, dup, dup),
		traceSearchBody(cerberusMetrics, dup))

	res := rep.Results[0]
	if res.Verdict != VerdictError {
		t.Fatalf("verdict = %q, want error", res.Verdict)
	}
	if !strings.Contains(res.Detail, dup.id) || !strings.Contains(res.Detail, "reference") {
		t.Errorf("detail = %q, want it to name the side and the duplicated trace", res.Detail)
	}
	if !rep.Failed() {
		t.Error("a response the comparator could not read must block")
	}
}

// TestTraceSearchDecoder_PreservesNanosecondPrecision pins the reason the two
// nanosecond fields never touch a float: today's epoch is above 2^53, so two
// summaries one nanosecond apart are the SAME float64 and a decoder that routed
// through one would call two different traces equal.
func TestTraceSearchDecoder_PreservesNanosecondPrecision(t *testing.T) {
	const (
		earlier = "1780000000000000001"
		later   = "1780000000000000002"
	)
	body := traceSearchBody(completeMetrics,
		traceSpec{id: hexTraceID(1), startNano: earlier, matched: 1, spans: []spanSpec{
			{id: hexSpanID(1), startNano: earlier, durationNanos: "9007199254740992"},
		}},
		traceSpec{id: hexTraceID(2), startNano: later, matched: 1, spans: []spanSpec{
			{id: hexSpanID(2), startNano: later, durationNanos: "9007199254740993"},
		}})

	got, err := decodeTraceSearch([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, ok := got.(traceSearchResult)
	if !ok {
		t.Fatalf("decoded %T, want traceSearchResult", got)
	}
	if len(res.Traces) != 2 {
		t.Fatalf("decoded %d traces, want 2", len(res.Traces))
	}
	if res.Traces[0].StartTimeUnixNano != 1_780_000_000_000_000_001 || res.Traces[1].StartTimeUnixNano != 1_780_000_000_000_000_002 {
		t.Errorf("start times = %d / %d, want the two decimal strings verbatim",
			res.Traces[0].StartTimeUnixNano, res.Traces[1].StartTimeUnixNano)
	}
	if res.Traces[0].Spans[0].DurationNanos != 9_007_199_254_740_992 || res.Traces[1].Spans[0].DurationNanos != 9_007_199_254_740_993 {
		t.Errorf("durations = %d / %d, want both above 2^53 intact",
			res.Traces[0].Spans[0].DurationNanos, res.Traces[1].Spans[0].DurationNanos)
	}
	// The reason this matters, asserted rather than described: routed through the
	// float slot these four distinct integers collapse into two.
	if float64(res.Traces[0].StartTimeUnixNano) != float64(res.Traces[1].StartTimeUnixNano) {
		t.Error("fixture no longer exercises the precision boundary: pick timestamps that DO collapse under float64")
	}
	if float64(res.Traces[0].Spans[0].DurationNanos) != float64(res.Traces[1].Spans[0].DurationNanos) {
		t.Error("fixture no longer exercises the precision boundary for durations")
	}
}

// TestTraceSearchDecoder_ReadsEveryIntegerEncoding pins that a 64-bit field arrives
// quoted, a 32-bit field bare, and an absent value as the empty string — all three
// of which the two backends actually emit. A decoder that accepted only one shape
// would record every response on one side as a broken backend.
func TestTraceSearchDecoder_ReadsEveryIntegerEncoding(t *testing.T) {
	const body = `{"traces":[{"traceID":"aabb","rootServiceName":"api",` +
		`"startTimeUnixNano":"1780000000000000001","durationMs":42,` +
		`"spanSets":[{"spans":[{"spanID":"0c","startTimeUnixNano":"1780000000000000001","durationNanos":""}],"matched":3}]}],` +
		`"metrics":{"totalJobs":7,"completedJobs":3}}`
	got, err := decodeTraceSearch([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := got.(traceSearchResult)
	tr := res.Traces[0]
	if tr.StartTimeUnixNano != 1_780_000_000_000_000_001 || tr.DurationMs != 42 {
		t.Errorf("quoted start / bare durationMs = %d / %d, want both read", tr.StartTimeUnixNano, tr.DurationMs)
	}
	if tr.Spans[0].DurationNanos != 0 {
		t.Errorf("empty-string durationNanos = %d, want 0", tr.Spans[0].DurationNanos)
	}
	if res.TotalJobs != 7 || res.CompletedJobs != 3 || !res.partial() {
		t.Errorf("job counters = %d/%d (partial=%v), want 3 of 7 read as partial",
			res.CompletedJobs, res.TotalJobs, res.partial())
	}
	// A short identifier is left-padded to its canonical width, so the same bytes
	// align across two backends that render them differently.
	if want := strings.Repeat("0", canonicalTraceIDHexLen-len("aabb")) + "aabb"; tr.TraceID != want {
		t.Errorf("traceID = %q, want the canonical 32-hex form %q", tr.TraceID, want)
	}
	if tr.Spans[0].SpanID != "000000000000000c" {
		t.Errorf("spanID = %q, want the canonical 16-hex form", tr.Spans[0].SpanID)
	}
}

// TestVerify_TraceIdentifiersAlignAcrossRenderings pins the canonicalisation where
// it matters: reference Tempo strips a trace ID's leading zeros and cerberus emits
// the fixed-width form, so keying on the raw strings would report every such trace
// as present on one side only — a divergence that does not exist.
func TestVerify_TraceIdentifiersAlignAcrossRenderings(t *testing.T) {
	cer := fixtureTrace(1)
	ref := cer
	ref.id = "1"
	ref.spans = []spanSpec{{
		id: "1", startNano: cer.spans[0].startNano, durationNanos: cer.spans[0].durationNanos,
	}}

	rep := runTraceSearch(t, traceSearchBody(completeMetrics, ref), traceSearchBody(cerberusMetrics, cer))
	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: the two renderings designate the same bytes (first-diff: %+v)", res.Verdict, res.FirstDiff)
	}
	if res.ComparedUnits != 1 {
		t.Errorf("ComparedUnits = %d, want 1: the trace must align, not vanish from both sides", res.ComparedUnits)
	}
}

// TestTraceSearchDecoder_DoesNotDoubleCountTheLegacySpanSet pins the one place the
// two spanset fields could inflate a count. Cerberus populates the deprecated
// single `spanSet` and the modern `spanSets` from the SAME set, so reading both
// would double every span count and every matched total.
func TestTraceSearchDecoder_DoesNotDoubleCountTheLegacySpanSet(t *testing.T) {
	tr := fixtureTrace(1)
	if !tr.legacySpanSet {
		t.Fatal("fixture must render both spanset fields to exercise the rule")
	}
	got, err := decodeTraceSearch([]byte(traceSearchBody(cerberusMetrics, tr)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := got.(traceSearchResult)
	if len(res.Traces[0].Spans) != 1 || res.Traces[0].Matched != 1 {
		t.Errorf("spans = %d, matched = %d, want one span matched once", len(res.Traces[0].Spans), res.Traces[0].Matched)
	}
	if res.Traces[0].Capped {
		t.Error("an uncapped set must not read as capped: matched equals the kept-span count")
	}
}

// TestTraceSearchDecoder_RejectsAnUnusableBody pins that a body the comparator
// cannot align on is a decode ERROR, never a partial comparison: a half-read
// response must not reach the comparator as if it were the backend's whole answer.
func TestTraceSearchDecoder_RejectsAnUnusableBody(t *testing.T) {
	cases := []struct {
		name, body, wantSubstr string
	}{
		{"not json", `{"traces":`, "decode trace-search response"},
		{"summary without a trace id", `{"traces":[{"rootServiceName":"api"}]}`, "carries no traceID"},
		{"unparseable integer", `{"traces":[{"traceID":"ab","durationMs":"twelve"}]}`, "parse trace-search integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeTraceSearch([]byte(tc.body)); err == nil {
				t.Fatalf("decoding %s must fail", tc.name)
			} else if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestTraceSearchDecoder_ToleratesUnknownFields pins that a reference body is never
// rejected for carrying data this lane does not diff — serviceStats, per-span
// attributes and the rest of the metrics block all ride along untouched.
func TestTraceSearchDecoder_ToleratesUnknownFields(t *testing.T) {
	const body = `{"traces":[{"traceID":"ab","serviceStats":{"api":{"spanCount":3}},` +
		`"spanSets":[{"spans":[{"spanID":"cd","attributes":[{"key":"k","value":{"stringValue":"v"}}]}],"matched":1}]}],` +
		`"metrics":{"inspectedBytes":"991","totalBlocks":4,"totalJobs":2,"completedJobs":2}}`
	got, err := decodeTraceSearch([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := got.(traceSearchResult)
	if len(res.Traces) != 1 || len(res.Traces[0].Spans) != 1 {
		t.Fatalf("decoded %+v, want the one trace and its one span", res.Traces)
	}
	if res.partial() {
		t.Error("2 of 2 jobs completed is a complete reference answer")
	}
}

// TestVerify_TraceSearchPayloadMismatchIsAnError pins that a comparator handed a
// payload it cannot read says so. The alternative — treating an unreadable payload
// as an empty result — would score a match having compared nothing.
func TestVerify_TraceSearchPayloadMismatchIsAnError(t *testing.T) {
	out := compareTraceSearch(searchQuery("{}"), logStreamResult{}, traceSearchResult{}, testParams())
	if out.Verdict != VerdictError {
		t.Fatalf("verdict = %q, want error", out.Verdict)
	}
	if out.Compared != 0 {
		t.Errorf("Compared = %d, want 0: nothing was judged", out.Compared)
	}
	if !strings.Contains(out.Detail, "cannot read") {
		t.Errorf("detail = %q, want it to say the payload was unreadable", out.Detail)
	}
}

// TestVerify_TraceSearchDivergenceRendersItsAnchor pins the human report. The
// matrix lane's first-diff line names a series and a float timestamp, neither of
// which exists here; a trace-search divergence has to name the trace, the span when
// there is one, and the field that differed.
func TestVerify_TraceSearchDivergenceRendersItsAnchor(t *testing.T) {
	ref := fixtureTrace(1)
	cer := fixtureTrace(1)
	cer.spans[0].name = "db.query"
	rep := runTraceSearch(t,
		traceSearchBody(completeMetrics, ref),
		traceSearchBody(cerberusMetrics, cer))

	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	want := fmt.Sprintf("first-diff: trace=%s span=%s field=name", ref.id, hexSpanID(1))
	if !strings.Contains(out.String(), want) {
		t.Errorf("report must anchor the divergence (%q), got:\n%s", want, out.String())
	}
	// The matrix lane's anchor would print series= and a float timestamp, which
	// point at nothing on this shape.
	if strings.Contains(out.String(), "first-diff: series=") {
		t.Errorf("a trace-search divergence must not borrow the matrix anchor, got:\n%s", out.String())
	}
}

// TestMergeLimitations_FoldsTheTraceSearchCodes pins that the trace-search codes
// roll up like every other one: summed by code, canonical detail, code-sorted.
func TestMergeLimitations_FoldsTheTraceSearchCodes(t *testing.T) {
	got := mergeLimitations(
		[]Limitation{limitation(LimitSpanSetCapped, 2), limitation(LimitSearchTruncated, 6)},
		[]Limitation{limitation(LimitSearchTruncated, 4), limitation(LimitSearchRefPartial, 1)},
	)
	want := []Limitation{
		limitation(LimitSearchRefPartial, 1),
		limitation(LimitSpanSetCapped, 2),
		limitation(LimitSearchTruncated, 10),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %+v, want %+v", got, want)
	}
}
