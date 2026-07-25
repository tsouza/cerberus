package migrateverify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// streamSpec / entrySpec build a Loki streams body from a small spec. Timestamps
// are written as the decimal STRINGS both backends emit, so a decoder that
// shortcut through float64 could not pass these fixtures.
type streamSpec struct {
	labels  map[string]string
	entries []entrySpec
}

type entrySpec struct {
	tsNano string
	line   string
}

// logStreamBody renders the streams envelope both backends return for a log
// query. Cerberus's default (non-categorized) value is the same two-element
// [ts, line] array reference Loki emits, so one builder stands in for both sides.
func logStreamBody(streams ...streamSpec) string {
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"streams","result":[`)
	for i, st := range streams {
		if i > 0 {
			b.WriteString(",")
		}
		labels, err := json.Marshal(st.labels)
		if err != nil {
			panic(err)
		}
		b.WriteString(`{"stream":`)
		b.Write(labels)
		b.WriteString(`,"values":[`)
		for j, e := range st.entries {
			if j > 0 {
				b.WriteString(",")
			}
			line, err := json.Marshal(e.line)
			if err != nil {
				panic(err)
			}
			b.WriteString(`["` + e.tsNano + `",`)
			b.Write(line)
			b.WriteString("]")
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]},"stats":{"summary":{"execTime":0.01}}}`)
	return b.String()
}

// logStreamServer answers the Loki log-query endpoint with a fixed body and
// records the request it was asked, so a test can assert both the comparison and
// the wire encoding that produced it.
type logStreamServer struct {
	url    string
	params url.Values
	header http.Header
}

func newLogStreamServer(t *testing.T, body string, status int) *logStreamServer {
	t.Helper()
	rec := &logStreamServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != lokiQueryRangePath {
			t.Errorf("log-stream probe hit path %q, want %q", r.URL.Path, lokiQueryRangePath)
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

// logStreamLanes wires one loki lane whose matrix and non-matrix contracts are
// served by the SAME backend pair, exactly as the CLI builds it.
func logStreamLanes(ref, cerberus *logStreamServer) map[string]Lane {
	mk := func(u string) *HTTPBackend {
		return NewHTTPBackend(u, WithDialect(LokiDialect()), WithKindDialects(LokiLogStreamDialect()))
	}
	r, c := mk(ref.url), mk(cerberus.url)
	return map[string]Lane{HeadLoki: {Ref: r, Cerberus: c, RefKind: r, CerberusKind: c}}
}

// logStreamQuery is one corpus entry routed to the log-stream comparator.
func logStreamQuery(expr string) Query {
	return Query{Expr: expr, Source: "panel:logs", Head: HeadLoki, Lang: "logql", Kind: KindLogStream}
}

// runLogStream replays one log-stream query against two fixed bodies and returns
// the whole report, so a test can assert the verdict AND the evidence the report
// attributes to the log-stream family.
func runLogStream(t *testing.T, refBody, cerBody string) Report {
	t.Helper()
	ref := newLogStreamServer(t, refBody, http.StatusOK)
	cer := newLogStreamServer(t, cerBody, http.StatusOK)
	return Verify(context.Background(),
		Corpus{Queries: []Query{logStreamQuery(`{job="api"}`)}},
		logStreamLanes(ref, cer), testParams())
}

// logFamily returns the loki head's log-stream family row, failing if the report
// carries none: a run that judged a log-stream query and reported no family for it
// would let the dead-family rule find nothing to check.
func logFamily(t *testing.T, rep Report) FamilySummary {
	t.Helper()
	for _, h := range rep.Heads {
		if h.Head != HeadLoki {
			continue
		}
		for _, f := range h.Families {
			if f.Kind == KindLogStream {
				return f
			}
		}
	}
	t.Fatalf("report carries no loki log-stream family: %+v", rep.Heads)
	return FamilySummary{}
}

// TestVerify_LogStreamAgreementCountsEveryEntry pins the positive case with its
// EVIDENCE, not just its verdict: two empty responses also score a match, so a
// test that asserted only "match" would pass over a comparator that compared
// nothing at all.
func TestVerify_LogStreamAgreementCountsEveryEntry(t *testing.T) {
	body := logStreamBody(
		streamSpec{
			labels: map[string]string{"job": "api"},
			entries: []entrySpec{
				{tsNano: "1780000000000000001", line: "boot"},
				{tsNano: "1780000000000000002", line: "serve"},
			},
		},
		streamSpec{
			labels:  map[string]string{"job": "db"},
			entries: []entrySpec{{tsNano: "1780000000000000003", line: "connect"}},
		},
	)
	rep := runLogStream(t, body, body)

	if len(rep.Results) != 1 {
		t.Fatalf("want one result, got %+v", rep.Results)
	}
	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (detail: %s, first-diff: %+v)", res.Verdict, res.Detail, res.FirstDiff)
	}
	if res.Kind != KindLogStream {
		t.Errorf("Kind = %q, want %q", res.Kind, KindLogStream)
	}
	const wantEntries = 3
	if res.ComparedUnits != wantEntries {
		t.Errorf("ComparedUnits = %d, want %d: every entry slot on both sides must be counted", res.ComparedUnits, wantEntries)
	}
	if res.ComparedSeries != 0 {
		t.Errorf("ComparedSeries = %d, want 0: the series counter is matrix-only and must not silently count log lines", res.ComparedSeries)
	}
	f := logFamily(t, rep)
	if f.Unit != UnitLogEntries || f.Compared != wantEntries || f.Match != 1 {
		t.Errorf("log-stream family = %+v, want %d entries compared in %q on one match", f, wantEntries, UnitLogEntries)
	}
	wantLimits := []Limitation{
		limitation(LimitLogEntryOrder, wantEntries),
		limitation(LimitLogStructuredMeta, wantEntries),
	}
	if !reflect.DeepEqual(res.Limitations, wantLimits) {
		t.Errorf("limitations = %+v, want exactly %+v: what was NOT judged must be stated with the count it covers",
			res.Limitations, wantLimits)
	}
	if rep.Failed() {
		t.Errorf("a log-stream lane that compared %d entries must pass", wantEntries)
	}
}

// TestVerify_EmptyLogStreamsAreNotProvenParity is the failure this whole evidence
// mechanism exists to prevent: two backends that returned nothing DO agree, and a
// verdict-only assertion would call that parity.
func TestVerify_EmptyLogStreamsAreNotProvenParity(t *testing.T) {
	empty := logStreamBody()
	rep := runLogStream(t, empty, empty)

	if rep.Results[0].Verdict != VerdictMatch {
		t.Fatalf("verdict = %q: two empty responses genuinely do agree", rep.Results[0].Verdict)
	}
	if rep.Results[0].ComparedUnits != 0 {
		t.Errorf("ComparedUnits = %d, want 0: nothing was diffed", rep.Results[0].ComparedUnits)
	}
	if len(rep.Results[0].Limitations) != 0 {
		t.Errorf("limitations = %+v, want none: a comparison that judged nothing has no dimension to except",
			rep.Results[0].Limitations)
	}
	dead := rep.DeadFamilies()
	if len(dead) != 1 || dead[0].Head != HeadLoki || dead[0].Kind != KindLogStream || dead[0].Unit != UnitLogEntries {
		t.Fatalf("DeadFamilies = %+v, want the loki log-stream family named in log entries", dead)
	}
	if !rep.Failed() {
		t.Error("a log-stream family that compared nothing proved nothing and must fail the run")
	}
	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(out.String(), "loki/log-stream compared nothing: 0 log entries diffed") {
		t.Errorf("the report must name the dead family in its own unit, got:\n%s", out.String())
	}
}

// TestVerify_StreamAndEntryOrderNeverDiverge pins that order is excluded from the
// DEFINITION of equality, and that the exclusion is stated rather than silent.
// Cerberus renders every stream ascending regardless of direction and builds the
// stream list from a Go map, so an order-sensitive comparator would report a
// difference that means nothing.
func TestVerify_StreamAndEntryOrderNeverDiverge(t *testing.T) {
	api := []entrySpec{
		{tsNano: "1780000000000000001", line: "boot"},
		{tsNano: "1780000000000000002", line: "serve"},
	}
	reversed := []entrySpec{api[1], api[0]}
	db := streamSpec{labels: map[string]string{"job": "db"}, entries: []entrySpec{{tsNano: "1780000000000000003", line: "connect"}}}

	refBody := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: api}, db)
	cerBody := logStreamBody(db, streamSpec{labels: map[string]string{"job": "api"}, entries: reversed})

	rep := runLogStream(t, refBody, cerBody)
	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: neither stream order nor entry order is a wire contract (first-diff: %+v)",
			res.Verdict, res.FirstDiff)
	}
	// Asserting the match alone would hide that order was never checked. The
	// limitation with its count is what makes the exclusion visible.
	var order *Limitation
	for i := range res.Limitations {
		if res.Limitations[i].Code == LimitLogEntryOrder {
			order = &res.Limitations[i]
		}
	}
	if order == nil {
		t.Fatalf("limitations = %+v, want %s stated: a comparison that ignores order must say so", res.Limitations, LimitLogEntryOrder)
	}
	if order.Count != res.ComparedUnits {
		t.Errorf("%s count = %d, want %d: the limitation must cover every entry it applies to",
			LimitLogEntryOrder, order.Count, res.ComparedUnits)
	}
	if !strings.Contains(order.Detail, "multiset") {
		t.Errorf("%s detail = %q, want it to state the definition of equality that replaced order", LimitLogEntryOrder, order.Detail)
	}
}

// TestVerify_LogEntryMissingOnOneSideDiverges pins that an entry only one backend
// returned is a blocking divergence anchored at that exact entry, in both
// directions — an intersection-only comparator could never report either.
func TestVerify_LogEntryMissingOnOneSideDiverges(t *testing.T) {
	shared := entrySpec{tsNano: "1780000000000000001", line: "boot"}
	extra := entrySpec{tsNano: "1780000000000000002", line: "serve"}

	both := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{shared, extra}})
	onlyShared := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{shared}})

	cases := []struct {
		name             string
		refBody, cerBody string
		wantRef, wantCer string
		wantReason       string
	}{
		{
			name: "reference has an entry cerberus does not", refBody: both, cerBody: onlyShared,
			wantRef: extra.line, wantCer: missingEntryValue, wantReason: "entry present in reference only",
		},
		{
			name: "cerberus has an entry the reference does not", refBody: onlyShared, cerBody: both,
			wantRef: missingEntryValue, wantCer: extra.line, wantReason: "entry present in cerberus only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := runLogStream(t, tc.refBody, tc.cerBody)
			res := rep.Results[0]
			if res.Verdict != VerdictDiverge {
				t.Fatalf("verdict = %q, want diverge", res.Verdict)
			}
			fd := res.FirstDiff
			if fd == nil {
				t.Fatal("a divergence must carry a first-diff")
				return
			}
			if fd.Kind != KindLogStream || fd.ReasonCode != ReasonEntryMissing {
				t.Errorf("first-diff kind/code = %q/%q, want %q/%q", fd.Kind, fd.ReasonCode, KindLogStream, ReasonEntryMissing)
			}
			if fd.Stream != `{job="api"}` {
				t.Errorf("first-diff stream = %q, want the canonical label set of the stream that carried it", fd.Stream)
			}
			if fd.TimestampNano != extra.tsNano {
				t.Errorf("first-diff timestamp_nano = %q, want %q exactly", fd.TimestampNano, extra.tsNano)
			}
			if fd.Timestamp != 0 {
				t.Errorf("first-diff timestamp = %v, want 0: a nanosecond epoch must never ride in the float slot", fd.Timestamp)
			}
			if fd.RefValue != tc.wantRef || fd.CerberusValue != tc.wantCer {
				t.Errorf("first-diff values = ref %q / cerberus %q, want %q / %q", fd.RefValue, fd.CerberusValue, tc.wantRef, tc.wantCer)
			}
			if fd.Reason != tc.wantReason {
				t.Errorf("first-diff reason = %q, want %q", fd.Reason, tc.wantReason)
			}
			// The entry both sides returned plus the entry only one side did: the
			// comparator judged two slots, and says so.
			if res.ComparedUnits != 2 {
				t.Errorf("ComparedUnits = %d, want 2: a one-sided entry was still diffed", res.ComparedUnits)
			}
			if !rep.Failed() {
				t.Error("a log-stream divergence must fail the run")
			}
		})
	}
}

// TestVerify_DuplicateLogEntryMultiplicityDiverges pins multiset semantics. Upstream
// dedupes byte-identical entries across its merge iterators and cerberus does not,
// so collapsing duplicates to a set would hide a genuine double-emit defect behind
// a green gate.
func TestVerify_DuplicateLogEntryMultiplicityDiverges(t *testing.T) {
	e := entrySpec{tsNano: "1780000000000000001", line: "boot"}
	once := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{e}})
	twice := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{e, e}})

	rep := runLogStream(t, once, twice)
	res := rep.Results[0]
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: the same line returned once and twice is not parity", res.Verdict)
	}
	fd := res.FirstDiff
	if fd == nil {
		t.Fatal("a divergence must carry a first-diff")
		return
	}
	if fd.ReasonCode != ReasonEntryMultiplicity {
		t.Errorf("first-diff code = %q, want %q", fd.ReasonCode, ReasonEntryMultiplicity)
	}
	if fd.RefValue != "1" || fd.CerberusValue != "2" {
		t.Errorf("first-diff values = ref %q / cerberus %q, want the two multiplicities 1 / 2", fd.RefValue, fd.CerberusValue)
	}
	if res.ComparedUnits != 2 {
		t.Errorf("ComparedUnits = %d, want 2: both slots the deeper side returned were judged", res.ComparedUnits)
	}
	if !rep.Failed() {
		t.Error("a multiplicity difference must block: it is a real double-emit, not a rounding difference")
	}
}

// TestVerify_LogStreamTruncationBandIsNotJudged pins the truncation rule on the
// only case where it bites: both sides are cut at the replay limit and disagree
// about which entries sit AT the boundary timestamp.
//
// Comparing the full bags would report those boundary entries as a divergence —
// a false positive from two backends breaking a tie differently. Dropping them
// without a count would be a silent exclusion. The rule compares the interior,
// which both sides provably returned in full, and states the residue with its
// exact size.
func TestVerify_LogStreamTruncationBandIsNotJudged(t *testing.T) {
	const boundaryTS = 1_780_000_000_000_000_000
	// Both sides return the same interior above the boundary, then fill the last
	// slot with a DIFFERENT entry sharing the boundary timestamp.
	interior := make([]entrySpec, 0, logStreamReplayLimit)
	for i := 1; i < logStreamReplayLimit; i++ {
		interior = append(interior, entrySpec{
			tsNano: strconv.Itoa(boundaryTS + i),
			line:   fmt.Sprintf("line-%d", i),
		})
	}
	refEntries := append(append([]entrySpec{}, interior...), entrySpec{tsNano: strconv.Itoa(boundaryTS), line: "tie-ref"})
	cerEntries := append(append([]entrySpec{}, interior...), entrySpec{tsNano: strconv.Itoa(boundaryTS), line: "tie-cerberus"})

	refBody := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: refEntries})
	cerBody := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: cerEntries})

	rep := runLogStream(t, refBody, cerBody)
	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: the two backends broke a boundary tie differently, which is not a divergence (first-diff: %+v)",
			res.Verdict, res.FirstDiff)
	}
	wantInterior := logStreamReplayLimit - 1
	if res.ComparedUnits != wantInterior {
		t.Errorf("ComparedUnits = %d, want %d: only the provably-complete interior was judged, not the whole response",
			res.ComparedUnits, wantInterior)
	}
	var band *Limitation
	for i := range res.Limitations {
		if res.Limitations[i].Code == LimitLogTruncationBand {
			band = &res.Limitations[i]
		}
	}
	if band == nil {
		t.Fatalf("limitations = %+v, want %s: the unjudged band must be named, not dropped", res.Limitations, LimitLogTruncationBand)
	}
	// One boundary entry on each side.
	if band.Count != 2 {
		t.Errorf("%s count = %d, want 2 (one boundary entry per side)", LimitLogTruncationBand, band.Count)
	}
	if !strings.Contains(band.Detail, strconv.Itoa(boundaryTS)) {
		t.Errorf("%s detail = %q, want it to name the boundary it applied", LimitLogTruncationBand, band.Detail)
	}
	if rep.Failed() {
		t.Error("a truncated comparison that judged its whole interior must pass")
	}
}

// TestVerify_TruncationDoesNotHideAnInteriorDivergence is the other half of the
// truncation rule: the band is excused, the interior is not. A rule that declined
// the whole result once either side hit the limit would let a real bug ship.
func TestVerify_TruncationDoesNotHideAnInteriorDivergence(t *testing.T) {
	const boundaryTS = 1_780_000_000_000_000_000
	mk := func(swapAt int, line string) string {
		entries := make([]entrySpec, 0, logStreamReplayLimit)
		for i := 0; i < logStreamReplayLimit; i++ {
			e := entrySpec{tsNano: strconv.Itoa(boundaryTS + i), line: fmt.Sprintf("line-%d", i)}
			if i == swapAt {
				e.line = line
			}
			entries = append(entries, e)
		}
		return logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: entries})
	}
	// The differing entry sits at the NEWEST timestamp, far inside the interior.
	rep := runLogStream(t, mk(logStreamReplayLimit-1, "reference-line"), mk(logStreamReplayLimit-1, "cerberus-line"))
	res := rep.Results[0]
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: a truncated result must still be diffed above its boundary", res.Verdict)
	}
	if res.FirstDiff == nil || res.FirstDiff.ReasonCode != ReasonEntryMissing {
		t.Fatalf("first-diff = %+v, want an entry-missing anchor inside the interior", res.FirstDiff)
	}
	if !rep.Failed() {
		t.Error("an interior divergence must block even when both results were truncated")
	}
}

// TestVerify_LogStreamEmptyAgainstFullIsADivergence pins the asymmetric-truncation
// case: one backend returned a full page and the other returned nothing. Taking
// the full side's own oldest entry as the boundary is what surfaces that; a rule
// that treated an absent side as "no boundary" would compare nothing and score a
// match.
func TestVerify_LogStreamEmptyAgainstFullIsADivergence(t *testing.T) {
	const oldestTS = 1_780_000_000_000_000_000
	entries := make([]entrySpec, 0, logStreamReplayLimit)
	for i := 0; i < logStreamReplayLimit; i++ {
		entries = append(entries, entrySpec{tsNano: strconv.Itoa(oldestTS + i), line: fmt.Sprintf("line-%d", i)})
	}
	full := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: entries})

	rep := runLogStream(t, full, logStreamBody())
	res := rep.Results[0]
	if res.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: cerberus returned nothing where the reference returned a full page", res.Verdict)
	}
	if res.ComparedUnits == 0 {
		t.Error("ComparedUnits = 0: the reference's entries above its own boundary were still diffed and must be counted")
	}
	if !rep.Failed() {
		t.Error("an empty cerberus result against a full reference page must block")
	}
}

// TestLogStreamDecoder_PreservesNanosecondPrecision pins the reason the log lane
// carries its own timestamp slot. A present-day nanosecond epoch is above 2^53, so
// two entries one nanosecond apart are the SAME float64; a decoder that routed
// through one would silently merge them and a real divergence would vanish.
func TestLogStreamDecoder_PreservesNanosecondPrecision(t *testing.T) {
	const first = "1780000000000000001"
	const second = "1780000000000000002"
	body := logStreamBody(streamSpec{
		labels:  map[string]string{"job": "api"},
		entries: []entrySpec{{tsNano: first, line: "a"}, {tsNano: second, line: "b"}},
	})
	got, err := decodeLogStreams([]byte(body))
	if err != nil {
		t.Fatalf("decodeLogStreams: %v", err)
	}
	res, ok := got.(logStreamResult)
	if !ok {
		t.Fatalf("decodeLogStreams returned %T, want logStreamResult", got)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("entries = %+v, want 2", res.Entries)
	}
	wantFirst, wantSecond := int64(1_780_000_000_000_000_001), int64(1_780_000_000_000_000_002)
	if res.Entries[0].TimestampNano != wantFirst || res.Entries[1].TimestampNano != wantSecond {
		t.Fatalf("timestamps = %d / %d, want %d / %d exactly",
			res.Entries[0].TimestampNano, res.Entries[1].TimestampNano, wantFirst, wantSecond)
	}
	// The rounding this guards against, spelled out: both values are the same
	// float64, so a float-based decoder cannot tell these two entries apart.
	if float64(wantFirst) != float64(wantSecond) {
		t.Fatal("fixture no longer exercises the float64 collision it was chosen for")
	}

	// And the precision survives all the way into the reported anchor.
	single := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{{tsNano: first, line: "a"}}})
	other := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{{tsNano: second, line: "a"}}})
	rep := runLogStream(t, single, other)
	if rep.Results[0].Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: two entries one nanosecond apart are different entries", rep.Results[0].Verdict)
	}
	if rep.Results[0].FirstDiff.TimestampNano != first {
		t.Errorf("first-diff timestamp_nano = %q, want %q", rep.Results[0].FirstDiff.TimestampNano, first)
	}
}

// TestLogStreamDecoder_RejectsANonStreamsBody pins that a 200 of the wrong shape
// is a decode failure rather than an empty comparison: a matrix body read as "no
// entries" would agree with anything and score a match on zero evidence.
func TestLogStreamDecoder_RejectsANonStreamsBody(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{
			name:    "matrix result",
			body:    `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
			wantErr: `resultType="matrix"`,
		},
		{
			name:    "not json",
			body:    `{`,
			wantErr: "decode log-stream response",
		},
		{
			name:    "value is not a decimal timestamp",
			body:    `{"data":{"resultType":"streams","result":[{"stream":{"job":"a"},"values":[["not-a-ts","x"]]}]}}`,
			wantErr: "parse log-stream timestamp",
		},
		{
			name:    "value has too few elements",
			body:    `{"data":{"resultType":"streams","result":[{"stream":{"job":"a"},"values":[["1780000000000000001"]]}]}}`,
			wantErr: "want at least 2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeLogStreams([]byte(tc.body))
			if err == nil {
				t.Fatalf("decodeLogStreams(%s) succeeded, want an error", tc.body)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestVerify_LogStreamCerberusRejectionIsUnsupportedNotError pins that the
// non-matrix transport applies the SAME status rule as the matrix path: a 4xx is
// an honest "I can't serve this" and a 5xx is a broken backend.
func TestVerify_LogStreamCerberusRejectionIsUnsupportedNotError(t *testing.T) {
	body := logStreamBody(streamSpec{labels: map[string]string{"job": "api"}, entries: []entrySpec{{tsNano: "1780000000000000001", line: "a"}}})
	cases := []struct {
		name        string
		status      int
		wantVerdict Verdict
	}{
		{name: "cerberus 4xx is unsupported", status: http.StatusBadRequest, wantVerdict: VerdictUnsupported},
		{name: "cerberus 5xx is a broken backend", status: http.StatusServiceUnavailable, wantVerdict: VerdictError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := newLogStreamServer(t, body, http.StatusOK)
			cer := newLogStreamServer(t, `{"status":"error"}`, tc.status)
			rep := Verify(context.Background(),
				Corpus{Queries: []Query{logStreamQuery(`{job="api"}`)}},
				logStreamLanes(ref, cer), testParams())
			res := rep.Results[0]
			if res.Verdict != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q (detail: %s)", res.Verdict, tc.wantVerdict, res.Detail)
			}
			if !strings.Contains(res.Detail, strconv.Itoa(tc.status)) {
				t.Errorf("detail = %q, want it to name the status", res.Detail)
			}
			// Either way the family compared nothing, so the run still blocks.
			if !rep.Failed() {
				t.Error("a lane whose every log-stream probe was rejected compared nothing and must block")
			}
		})
	}
}
