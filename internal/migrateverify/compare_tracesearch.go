package migrateverify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Canonical hex widths for the two OTel identifiers a trace search returns: 16
// bytes of trace ID and 8 bytes of span ID. Cerberus emits the fixed-width form
// while reference Tempo strips leading zeros on the wire, so the two backends
// designate the same bytes with different strings unless both are widened to the
// canonical form before they are keyed on.
const (
	canonicalTraceIDHexLen = 32
	canonicalSpanIDHexLen  = 16
)

// traceSearchResult is one backend's decoded /api/search response: the trace
// summaries it returned, plus the reference's own report of whether it finished.
//
// The `metrics` block is otherwise NOT carried, because it is not comparable:
// cerberus's inspectedTraces is a ClickHouse row count and its inspectedBytes /
// totalBlocks are hard zeros, while the reference's describe a sharded block
// search cerberus has no analogue for. Diffing it would make every tempo run red
// for ever. The two job counters ARE read, because they are the reference saying
// it did not finish — a claim about the completeness of its OWN answer, which the
// comparator must respect rather than diff.
type traceSearchResult struct {
	Traces        []traceSummary
	TotalJobs     int64
	CompletedJobs int64
}

// partial reports whether the reference answered with an incomplete search. A
// zero TotalJobs means the backend reported no job accounting at all (cerberus
// never does — it runs one ClickHouse query and is complete by construction, or
// it fails), which is not a partiality claim.
func (r traceSearchResult) partial() bool {
	return r.TotalJobs > 0 && r.CompletedJobs < r.TotalJobs
}

// traceSummary is one trace both backends can be asked about: its identity, the
// four summary fields, and the flattened matched spanset.
//
// Capped records that the spans list was cut at the spans-per-set cap (fewer
// spans kept than Matched says were found). Which spans survive that cap is
// unspecified upstream, so a capped spanset is comparable on its counts and not
// on its members — see LimitSpanSetCapped.
type traceSummary struct {
	TraceID           string
	RootServiceName   string
	RootTraceName     string
	StartTimeUnixNano int64
	DurationMs        int64
	Spans             []searchSpan
	Matched           int
	Capped            bool
}

// searchSpan is one span inside a trace's matched spanset.
//
// The two timestamps are int64 nanoseconds parsed from the wire's decimal string,
// never float64: a present-day nanosecond epoch is above 2^53, so a float64
// cannot hold one exactly and two genuinely different spans would compare equal
// after rounding.
type searchSpan struct {
	SpanID            string
	Name              string
	StartTimeUnixNano int64
	DurationNanos     int64
}

// tempoSearchResponse is the /api/search envelope. Both backends emit the same
// shape — cerberus's Tempo head mirrors tempopb.SearchResponse's JSON — so one
// decoder reads both. Fields no dimension of the parity rule depends on
// (serviceStats, per-span attributes, the rest of the metrics block) are simply
// not read, and an unknown field is tolerated: a reference body must never be
// rejected for carrying data this lane does not diff.
type tempoSearchResponse struct {
	Traces  []tempoTraceSummaryJSON `json:"traces"`
	Metrics struct {
		TotalJobs     traceFlexInt64 `json:"totalJobs"`
		CompletedJobs traceFlexInt64 `json:"completedJobs"`
	} `json:"metrics"`
}

type tempoTraceSummaryJSON struct {
	TraceID           string             `json:"traceID"`
	RootServiceName   string             `json:"rootServiceName"`
	RootTraceName     string             `json:"rootTraceName"`
	StartTimeUnixNano traceFlexInt64     `json:"startTimeUnixNano"`
	DurationMs        traceFlexInt64     `json:"durationMs"`
	SpanSet           *tempoSpanSetJSON  `json:"spanSet"`
	SpanSets          []tempoSpanSetJSON `json:"spanSets"`
}

type tempoSpanSetJSON struct {
	Spans   []tempoSpanJSON `json:"spans"`
	Matched traceFlexInt64  `json:"matched"`
}

type tempoSpanJSON struct {
	SpanID            string         `json:"spanID"`
	Name              string         `json:"name"`
	StartTimeUnixNano traceFlexInt64 `json:"startTimeUnixNano"`
	DurationNanos     traceFlexInt64 `json:"durationNanos"`
}

// traceFlexInt64 decodes an integer that may arrive in any of the three forms the
// two backends actually emit: a bare JSON number (proto3 JSON leaves 32-bit
// fields such as durationMs and the job counters unquoted), a quoted decimal
// string (it quotes every 64-bit field, so startTimeUnixNano and durationNanos
// arrive quoted), or the empty string a backend writes for a field it has no
// value for. A decoder that accepted only one of the three would record every
// response on one side as an error, which reads as a broken backend rather than
// as the wire-shape difference it is.
type traceFlexInt64 int64

func (v *traceFlexInt64) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("decode trace-search integer: %w", err)
		}
		if s == "" {
			*v = 0
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("parse trace-search integer %q: %w", s, err)
		}
		*v = traceFlexInt64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("decode trace-search integer: %w", err)
	}
	*v = traceFlexInt64(n)
	return nil
}

// decodeTraceSearch parses a 200 /api/search body into the summaries the
// comparator works on. A body that is not valid JSON, or that carries a summary
// with no trace ID, is a decode error: two results can only be aligned on trace
// identity, so a summary without one cannot be compared at all and must not reach
// the comparator as if it had been.
func decodeTraceSearch(body []byte) (any, error) {
	var raw tempoSearchResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode trace-search response: %w", err)
	}
	out := traceSearchResult{
		TotalJobs:     int64(raw.Metrics.TotalJobs),
		CompletedJobs: int64(raw.Metrics.CompletedJobs),
	}
	for i, t := range raw.Traces {
		if t.TraceID == "" {
			return nil, fmt.Errorf("trace-search summary %d carries no traceID: two results can only be aligned on trace identity", i)
		}
		spans, matched := flattenSearchSpanSets(t)
		out.Traces = append(out.Traces, traceSummary{
			TraceID:           canonicalHexID(t.TraceID, canonicalTraceIDHexLen),
			RootServiceName:   t.RootServiceName,
			RootTraceName:     t.RootTraceName,
			StartTimeUnixNano: int64(t.StartTimeUnixNano),
			DurationMs:        int64(t.DurationMs),
			Spans:             spans,
			Matched:           matched,
			Capped:            len(spans) < matched,
		})
	}
	return out, nil
}

// flattenSearchSpanSets folds a summary's spansets into one (spans, matched)
// pair: spans concatenated, matched summed. A set whose `matched` is absent
// (proto3 JSON omits a zero) counts its own span list instead, which is the only
// reading that keeps an uncapped set uncapped.
//
// The legacy single `spanSet` field is read only when `spanSets` is absent: both
// backends populate both from the same set, so reading both would double every
// count.
func flattenSearchSpanSets(t tempoTraceSummaryJSON) ([]searchSpan, int) {
	sets := t.SpanSets
	if len(sets) == 0 && t.SpanSet != nil {
		sets = []tempoSpanSetJSON{*t.SpanSet}
	}
	var spans []searchSpan
	matched := 0
	for _, s := range sets {
		for _, sp := range s.Spans {
			spans = append(spans, searchSpan{
				SpanID:            canonicalHexID(sp.SpanID, canonicalSpanIDHexLen),
				Name:              sp.Name,
				StartTimeUnixNano: int64(sp.StartTimeUnixNano),
				DurationNanos:     int64(sp.DurationNanos),
			})
		}
		if s.Matched > 0 {
			matched += int(s.Matched)
			continue
		}
		matched += len(s.Spans)
	}
	return spans, matched
}

// canonicalHexID left-pads a hex identifier to its spec-canonical lowercase
// width, so the same bytes align across two backends that render them
// differently. An ID already at or beyond that width passes through lowercased,
// so a genuinely different identifier still diffs.
func canonicalHexID(id string, width int) string {
	id = strings.ToLower(id)
	if len(id) >= width {
		return id
	}
	return strings.Repeat("0", width-len(id)) + id
}

// compareTraceSearch judges two trace-search responses.
//
// The parity rule, in the operator's words: when NEITHER backend hit the request
// limit, both returned complete answers and parity means exact trace-ID set
// equality plus exact field equality on every returned summary. When EITHER
// backend hit the limit, its result is a prefix of a ranking of unknown size, so
// the gate does not claim set parity — it reports how many IDs each side
// returned and how many were returned by only one of them, and it still
// field-diffs every trace both sides returned, so a genuine field bug is never
// masked by truncation.
//
// The two regimes exist because the interior of a truncated trace search is NOT
// provable. The log-stream lane can compare an interior because both sides
// truncate on the timestamp axis, so everything above the deeper boundary is
// complete on both. Reference Tempo makes no such promise: it searches blocks and
// stops at the limit, which is only approximately recency-ordered, so asserting
// set parity over a truncated result asserts something that was never
// established — manufacturing false divergences an operator would chase as
// phantom bugs, or, worse, false confidence.
//
// Result ORDER is never compared, in either regime. The comparator keys on trace
// identity, never on position, and the truncated limitation names order as the
// dimension that is not judgeable.
//
// Every field it does compare is compared EXACTLY. durationMs is an integer
// millisecond count on both sides, so any real difference is at least 1 ms; a
// relative epsilon over it would be exact equality wearing a tolerance's
// clothing, and --tolerance stays what it is: an absolute epsilon over sample
// values, of which a trace summary has none.
func compareTraceSearch(q Query, refAny, cerAny any, _ Params) Outcome {
	ref, refOK := refAny.(traceSearchResult)
	cer, cerOK := cerAny.(traceSearchResult)
	if !refOK || !cerOK {
		return Outcome{
			Verdict: VerdictError,
			Detail:  "trace-search comparator received a payload it cannot read; the lane is wired to the wrong dialect",
		}
	}

	refByID, err := indexTraceSummaries(ref.Traces, "reference")
	if err != nil {
		return Outcome{Verdict: VerdictError, Detail: err.Error()}
	}
	cerByID, err := indexTraceSummaries(cer.Traces, "cerberus")
	if err != nil {
		return Outcome{Verdict: VerdictError, Detail: err.Error()}
	}

	both := sortedCommonTraceIDs(refByID, cerByID)
	onlyRef := sortedExclusiveTraceIDs(refByID, cerByID)
	onlyCer := sortedExclusiveTraceIDs(cerByID, refByID)

	// Compared counts the trace IDs actually field-diffed — the intersection, not
	// the union. In the truncated regime the symmetric difference is precisely the
	// residue the comparator DECLINES to judge, so counting it as evidence would
	// inflate the number with the units nobody compared, and a family whose only
	// evidence is a disagreement about membership it cannot rule on would look
	// proven. Two empty responses therefore leave this at 0, the family dead, and
	// the gate blocking — the ComparedSeries rule applied to a new shape.
	//
	// Derived is set unconditionally, before any of the early returns below, so a
	// diverging or truncated search still spawns its trace-by-id probes: the
	// deeper structural check is independent evidence, not contingent on the
	// summary-level verdict.
	out := Outcome{Verdict: VerdictMatch, Compared: len(both), Derived: deriveTraceByIDProbes(q, both)}

	truncated := len(ref.Traces) == traceSearchReplayLimit || len(cer.Traces) == traceSearchReplayLimit
	capped := 0
	for _, id := range both {
		if refByID[id].Capped || cerByID[id].Capped {
			capped++
		}
	}
	if capped > 0 {
		out.Limitations = append(out.Limitations, limitation(LimitSpanSetCapped, capped))
	}
	if truncated {
		out.Limitations = append(out.Limitations, Limitation{
			Code:  LimitSearchTruncated,
			Count: len(onlyRef) + len(onlyCer),
			Detail: limitationDetails[LimitSearchTruncated] +
				fmt.Sprintf(" The reference returned %d summaries and cerberus returned %d, at limit=%d.",
					len(ref.Traces), len(cer.Traces), traceSearchReplayLimit),
		})
	}
	if ref.partial() {
		out.Limitations = append(out.Limitations, Limitation{
			Code:  LimitSearchRefPartial,
			Count: 1,
			Detail: limitationDetails[LimitSearchRefPartial] +
				fmt.Sprintf(" The reference completed %d of %d jobs.", ref.CompletedJobs, ref.TotalJobs),
		})
	}
	// Code-sorted for the same reason the roll-up merges them sorted: the report's
	// byte-determinism is a pinned contract.
	sort.Slice(out.Limitations, func(i, j int) bool { return out.Limitations[i].Code < out.Limitations[j].Code })

	// Field diffs are judged FIRST and unconditionally. A truncation short-circuit
	// ahead of them would let a real rootServiceName or duration bug ride out of the
	// gate as "undecidable" on any corpus whose searches hit the limit.
	for _, id := range both {
		if fd := diffTraceSummary(refByID[id], cerByID[id]); fd != nil {
			out.Verdict, out.FirstDiff = VerdictDiverge, fd
			return out
		}
	}

	switch {
	case truncated || ref.partial():
		// Membership is not judgeable here, so no membership verdict is claimed —
		// in either direction. The named limitation carries the counts.
		out.Verdict = VerdictUndecidable
	case len(onlyRef) > 0:
		out.Verdict = VerdictDiverge
		out.FirstDiff = missingTraceDiff(refByID[onlyRef[0]], describeTrace(refByID[onlyRef[0]]), missingTraceValue,
			"trace present in reference only")
	case len(onlyCer) > 0:
		out.Verdict = VerdictDiverge
		out.FirstDiff = missingTraceDiff(cerByID[onlyCer[0]], missingTraceValue, describeTrace(cerByID[onlyCer[0]]),
			"trace present in cerberus only")
	}
	return out
}

// deriveTraceByIDProbes spawns one trace-by-id fetch per trace ID BOTH
// backends returned, capped at maxDerivedTraceFetchesPerSearch. An ID only one
// side returned is already reported as a divergence by THIS comparator;
// fetching it would 404 on the side that never had it, by construction, and
// double-count one bug as two. both is already sorted (see
// sortedCommonTraceIDs), so the same run always derives the same fetches.
func deriveTraceByIDProbes(q Query, both []string) []Query {
	if len(both) == 0 {
		return nil
	}
	n := len(both)
	if n > maxDerivedTraceFetchesPerSearch {
		n = maxDerivedTraceFetchesPerSearch
	}
	out := make([]Query, 0, n)
	for _, id := range both[:n] {
		out = append(out, Query{
			Expr:    fmt.Sprintf("GET /api/traces/%s", id),
			Source:  fmt.Sprintf("derived-from:%s", q.Source),
			Head:    HeadTempo,
			Lang:    q.Lang,
			Kind:    KindTraceByID,
			TraceID: id,
		})
	}
	return out
}

// indexTraceSummaries keys one backend's summaries by canonical trace ID.
//
// A repeated trace ID within one response is an ERROR, not a silent
// first-wins collapse: a search returns one summary per matching trace, so a
// duplicate is a genuine wire anomaly, and folding it away would discard whichever
// copy disagreed — the exact defect the comparison exists to find.
func indexTraceSummaries(traces []traceSummary, side string) (map[string]traceSummary, error) {
	out := make(map[string]traceSummary, len(traces))
	for _, t := range traces {
		if _, dup := out[t.TraceID]; dup {
			return nil, fmt.Errorf("%s returned trace %s twice in one search response: a duplicate summary cannot be folded away without discarding one of the two", side, t.TraceID)
		}
		out[t.TraceID] = t
	}
	return out, nil
}

// sortedCommonTraceIDs returns the trace IDs both backends returned, sorted, so
// the "first" difference the comparator reports is stable across runs.
func sortedCommonTraceIDs(ref, cerberus map[string]traceSummary) []string {
	out := make([]string, 0, len(ref))
	for id := range ref {
		if _, both := cerberus[id]; both {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// sortedExclusiveTraceIDs returns the trace IDs in have that lack has not, sorted.
func sortedExclusiveTraceIDs(have, lacks map[string]traceSummary) []string {
	var out []string
	for id := range have {
		if _, both := lacks[id]; !both {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// missingTraceValue is what a first-diff prints on the side that did not return
// the trace, mirroring the matrix lane's "<missing series>".
const missingTraceValue = "<missing trace>"

// traceSummaryField names one comparable field of a trace summary, paired with
// the two backends' rendering of it. Every field here is a pure function of the
// query and the data, so an inequality is a defect on one of the two backends and
// nothing else.
type traceSummaryField struct {
	name          string
	ref, cerberus string
}

// diffTraceSummary returns the first field two same-trace summaries disagree on,
// or nil when they agree on every comparable dimension.
//
// serviceStats and the spanset's `attributes` are absent from the list because
// cerberus does not model them at all; they are named in the docs' scope
// statement as known non-comparable field pairs rather than silently diffed into
// a permanent divergence.
func diffTraceSummary(ref, cerberus traceSummary) *FirstDiff {
	fields := []traceSummaryField{
		{"rootServiceName", ref.RootServiceName, cerberus.RootServiceName},
		{"rootTraceName", ref.RootTraceName, cerberus.RootTraceName},
		{"startTimeUnixNano", strconv.FormatInt(ref.StartTimeUnixNano, 10), strconv.FormatInt(cerberus.StartTimeUnixNano, 10)},
		{"durationMs", strconv.FormatInt(ref.DurationMs, 10), strconv.FormatInt(cerberus.DurationMs, 10)},
		// The uncapped total of spans the TraceQL expression matched in this trace: a
		// pure function of query and data, and therefore comparable even when the cap
		// truncated which of them were returned.
		{"spanSets.matched", strconv.Itoa(ref.Matched), strconv.Itoa(cerberus.Matched)},
		// The kept-span count. Both sides receive the same cap, so an inequality here
		// is a real difference in what the two returned, capped or not.
		{"spanSets.spans", strconv.Itoa(len(ref.Spans)), strconv.Itoa(len(cerberus.Spans))},
	}
	for _, f := range fields {
		if f.ref != f.cerberus {
			return traceFieldDiff(ref.TraceID, "", f.name, f.ref, f.cerberus,
				"trace summary field differs", ReasonTraceFieldDiffers)
		}
	}
	if ref.Capped || cerberus.Capped {
		// Which matched spans survive the cap is unspecified upstream, so the counts
		// above are the strongest honest assertion available. LimitSpanSetCapped
		// states that, with the number of traces it covers.
		return nil
	}
	return diffSearchSpans(ref.TraceID, ref.Spans, cerberus.Spans)
}

// diffSearchSpans compares two complete (uncapped) matched-span lists: the span-ID
// set exactly, then name, start and duration on every span both sides returned.
// Spans are walked in span-ID order so the reported difference is stable.
func diffSearchSpans(traceID string, ref, cerberus []searchSpan) *FirstDiff {
	refByID := indexSearchSpans(ref)
	cerByID := indexSearchSpans(cerberus)

	ids := make([]string, 0, len(refByID)+len(cerByID))
	for id := range refByID {
		ids = append(ids, id)
	}
	for id := range cerByID {
		if _, both := refByID[id]; !both {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	for _, id := range ids {
		r, rok := refByID[id]
		c, cok := cerByID[id]
		switch {
		case !cok:
			return traceFieldDiff(traceID, id, "", describeSpan(r), missingSpanValue,
				"matched span present in reference only", ReasonSpanMissing)
		case !rok:
			return traceFieldDiff(traceID, id, "", missingSpanValue, describeSpan(c),
				"matched span present in cerberus only", ReasonSpanMissing)
		}
		for _, f := range []traceSummaryField{
			{"name", r.Name, c.Name},
			{"startTimeUnixNano", strconv.FormatInt(r.StartTimeUnixNano, 10), strconv.FormatInt(c.StartTimeUnixNano, 10)},
			{"durationNanos", strconv.FormatInt(r.DurationNanos, 10), strconv.FormatInt(c.DurationNanos, 10)},
		} {
			if f.ref != f.cerberus {
				return traceFieldDiff(traceID, id, f.name, f.ref, f.cerberus,
					"matched span field differs", ReasonSpanFieldDiffers)
			}
		}
	}
	return nil
}

// missingSpanValue is what a first-diff prints on the side that did not return
// the span.
const missingSpanValue = "<missing span>"

// indexSearchSpans keys a matched-span list by canonical span ID. A backend
// repeating a span ID within one trace would collapse here, which the kept-span
// COUNT comparison in diffTraceSummary catches independently.
func indexSearchSpans(spans []searchSpan) map[string]searchSpan {
	out := make(map[string]searchSpan, len(spans))
	for _, s := range spans {
		out[s.SpanID] = s
	}
	return out
}

// describeTrace renders the present side's summary for a one-sided trace, so the
// diff line names the trace in terms an operator can search their own backend for
// rather than only by an opaque ID.
func describeTrace(t traceSummary) string {
	return fmt.Sprintf("rootServiceName=%q rootTraceName=%q startTimeUnixNano=%d",
		t.RootServiceName, t.RootTraceName, t.StartTimeUnixNano)
}

// describeSpan renders the present side's span for a one-sided matched span.
func describeSpan(s searchSpan) string {
	return fmt.Sprintf("name=%q startTimeUnixNano=%d durationNanos=%d",
		s.Name, s.StartTimeUnixNano, s.DurationNanos)
}

// traceFieldDiff anchors a trace-search divergence at the exact trace, span and
// field it happened on. Timestamps ride in the value strings as decimal integers
// for the same reason the log lane keeps its own out of the float slot: a
// nanosecond epoch exceeds float64's exact-integer range.
func traceFieldDiff(traceID, spanID, field, refValue, cerValue, reason, code string) *FirstDiff {
	return &FirstDiff{
		Kind:          KindTraceSearch,
		TraceID:       traceID,
		SpanID:        spanID,
		Field:         field,
		RefValue:      refValue,
		CerberusValue: cerValue,
		Reason:        reason,
		ReasonCode:    code,
	}
}

// missingTraceDiff anchors a divergence on a trace exactly one backend returned.
func missingTraceDiff(t traceSummary, refValue, cerValue, reason string) *FirstDiff {
	return traceFieldDiff(t.TraceID, "", "", refValue, cerValue, reason, ReasonTraceMissing)
}
