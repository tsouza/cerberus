package migrateverify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// resultTypeStreams is the only Loki resultType a log-stream query returns. A
// 200 carrying anything else means the backend answered a different question
// than the one this lane asked, so there is nothing to compare — it is reported
// as a decode failure rather than folded into the comparison.
const resultTypeStreams = "streams"

// logStreamResult is one backend's decoded log-stream response: every entry in
// the response, flattened out of its stream, each carrying the stream label set
// it belongs to.
//
// The entries are held flat because the parity rule is defined on the entry
// multiset (see compareLogStreams): the stream grouping is preserved inside each
// entry's Stream key, and the response's stream ORDER — which neither backend
// makes a wire contract — is discarded by construction rather than compared and
// then ignored.
type logStreamResult struct {
	ResultType string
	Entries    []logEntry
}

// logEntry is one log line: the canonical label set of the stream that carries
// it, its nanosecond timestamp, and the line itself.
//
// TimestampNano is an int64, parsed from the wire's decimal string. It is
// deliberately NOT a float64: a present-day nanosecond epoch is above 2^53, so a
// float64 cannot hold one exactly and two genuinely different entries would
// compare equal after rounding.
type logEntry struct {
	Stream        string
	TimestampNano int64
	Line          string
}

// lokiStreamsResponse is the Loki log-query envelope. Both backends emit the
// same shape: cerberus's default (non-categorized) stream value is the
// two-element [ts, line] array reference Loki returns, so one decoder reads
// both. Fields neither side's parity depends on (stats, warnings,
// encodingFlags) are simply not read.
type lokiStreamsResponse struct {
	Data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string   `json:"stream"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// minStreamValueElements is the number of leading elements every stream value
// carries on both backends: the nanosecond timestamp string and the log line. A
// categorized third element is never requested (see LimitLogStructuredMeta) and
// is not read when a backend sends one anyway.
const minStreamValueElements = 2

// decodeLogStreams parses a 200 log-query body into the flat entry list the
// comparator works on. A body that is not valid JSON, does not carry a streams
// result, or carries a value the wire format cannot describe is a decode error:
// there is no partial log-stream comparison, so a half-read body must never
// reach the comparator as if it were the backend's whole answer.
func decodeLogStreams(body []byte) (any, error) {
	var raw lokiStreamsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode log-stream response: %w", err)
	}
	if raw.Data.ResultType != resultTypeStreams {
		return nil, fmt.Errorf("log-stream response carries resultType=%q, want %q", raw.Data.ResultType, resultTypeStreams)
	}
	out := logStreamResult{ResultType: raw.Data.ResultType}
	for _, st := range raw.Data.Result {
		key := canonicalLabels(st.Stream)
		for _, v := range st.Values {
			if len(v) < minStreamValueElements {
				return nil, fmt.Errorf("log-stream entry in %s has %d elements, want at least %d", key, len(v), minStreamValueElements)
			}
			var tsStr, line string
			if err := json.Unmarshal(v[0], &tsStr); err != nil {
				return nil, fmt.Errorf("decode log-stream timestamp in %s: %w", key, err)
			}
			if err := json.Unmarshal(v[1], &line); err != nil {
				return nil, fmt.Errorf("decode log-stream line in %s: %w", key, err)
			}
			ts, err := strconv.ParseInt(tsStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse log-stream timestamp %q in %s: %w", tsStr, key, err)
			}
			out.Entries = append(out.Entries, logEntry{Stream: key, TimestampNano: ts, Line: line})
		}
	}
	return out, nil
}

// compareLogStreams judges two log-stream responses.
//
// The parity rule, in the operator's words: two backends agree on a log-stream
// query when, for every (stream label set, nanosecond timestamp, log line)
// triple, both returned it the SAME NUMBER OF TIMES, over the part of the window
// both backends fully cover. Order is not compared, in any axis. Nothing is
// dropped: every entry either agrees, disagrees, or is named in a counted
// limitation.
//
// Order is excluded from the DEFINITION of equality rather than tolerated: it is
// provably non-informative on at least one side (cerberus renders every stream
// ascending regardless of direction and iterates a Go map for stream order), so
// comparing it would report a difference that means nothing. Every entry is
// still compared; only its position is not, and LimitLogEntryOrder says so with
// the count of entries that statement covers.
//
// Multiplicity IS compared. Collapsing duplicates would hide a real double-emit
// or double-ingest defect — upstream dedupes byte-identical entries across its
// merge iterators and cerberus does not — so a triple returned once by one
// backend and twice by the other is a blocking divergence with its own reason
// code, not a rounding difference.
func compareLogStreams(_ Query, refAny, cerAny any, _ Params) Outcome {
	ref, refOK := refAny.(logStreamResult)
	cer, cerOK := cerAny.(logStreamResult)
	if !refOK || !cerOK {
		return Outcome{
			Verdict: VerdictError,
			Detail:  "log-stream comparator received a payload it cannot read; the lane is wired to the wrong dialect",
		}
	}

	// Both sides truncate on the TIMESTAMP axis (each returns the newest
	// logStreamReplayLimit entries), so when either side is cut, everything
	// strictly newer than the deeper side's oldest entry is complete on BOTH
	// sides. That interior is a genuine, complete answer to a well-defined
	// sub-question and is compared for a real verdict; the residue below the
	// boundary is named and counted, never scored as agreement.
	truncated := len(ref.Entries) == logStreamReplayLimit || len(cer.Entries) == logStreamReplayLimit
	var watermark int64
	if truncated {
		watermark = truncationWatermark(ref.Entries, cer.Entries)
	}

	refBag, refInterior := entryMultiset(ref.Entries, truncated, watermark)
	cerBag, cerInterior := entryMultiset(cer.Entries, truncated, watermark)
	band := (len(ref.Entries) - refInterior) + (len(cer.Entries) - cerInterior)

	out := Outcome{Verdict: VerdictMatch, Compared: comparedEntrySlots(refBag, cerBag)}
	if out.Compared > 0 {
		out.Limitations = append(out.Limitations,
			limitation(LimitLogEntryOrder, out.Compared),
			limitation(LimitLogStructuredMeta, out.Compared))
	}
	if truncated && band > 0 {
		out.Limitations = append(out.Limitations, Limitation{
			Code:  LimitLogTruncationBand,
			Count: band,
			Detail: limitationDetails[LimitLogTruncationBand] +
				fmt.Sprintf(" The boundary was timestamp %d ns, at limit=%d.", watermark, logStreamReplayLimit),
		})
	}
	// Limitations are code-sorted for the same reason the roll-up merges them
	// sorted: the report's byte-determinism is a pinned contract.
	sort.Slice(out.Limitations, func(i, j int) bool { return out.Limitations[i].Code < out.Limitations[j].Code })

	if fd := firstEntryDiff(refBag, cerBag); fd != nil {
		out.Verdict, out.FirstDiff = VerdictDiverge, fd
	}
	return out
}

// truncationWatermark is the deepest boundary both backends provably reached:
// the LATER of the two oldest returned timestamps. Above it each side returned
// everything it had, so the interiors are comparable; at or below it the two
// results cover different depths.
//
// An empty side contributes no boundary. A side that returned nothing while the
// other returned a full page is a real divergence, and taking the full side's
// own oldest entry as the boundary is what surfaces it: every entry above that
// boundary is then reported missing on the empty side, instead of an
// "everything is below the watermark" answer that would compare nothing.
func truncationWatermark(ref, cerberus []logEntry) int64 {
	var mark int64
	set := false
	for _, side := range [][]logEntry{ref, cerberus} {
		if len(side) == 0 {
			continue
		}
		oldest := side[0].TimestampNano
		for _, e := range side[1:] {
			if e.TimestampNano < oldest {
				oldest = e.TimestampNano
			}
		}
		if !set || oldest > mark {
			mark, set = oldest, true
		}
	}
	return mark
}

// logTriple is the comparison unit: a stream label set, a nanosecond timestamp
// and a line. Two entries are the same entry exactly when all three agree.
type logTriple struct {
	stream string
	tsNano int64
	line   string
}

// entryMultiset counts each triple in the comparable interior and reports how
// many of the side's entries fell inside it. When the result was not truncated
// the whole response is the interior; when it was, the interior is everything
// strictly newer than the boundary — a tie AT the boundary is part of the band,
// because the two backends break a timestamp tie by different secondary keys.
func entryMultiset(entries []logEntry, truncated bool, watermark int64) (map[logTriple]int, int) {
	bag := make(map[logTriple]int, len(entries))
	inside := 0
	for _, e := range entries {
		if truncated && e.TimestampNano <= watermark {
			continue
		}
		bag[logTriple{stream: e.Stream, tsNano: e.TimestampNano, line: e.Line}]++
		inside++
	}
	return bag, inside
}

// comparedEntrySlots counts the entry slots the comparator actually judged: for
// each distinct triple, the larger of the two sides' multiplicities. It counts
// the union, not the sum, so an entry both backends returned counts once and the
// number reads as "entries diffed" rather than "entries received".
//
// It is the log lane's ComparedSeries: two empty interiors agree, so the verdict
// alone cannot tell agreement from absence and only this counter can.
func comparedEntrySlots(ref, cerberus map[logTriple]int) int {
	total := 0
	for t, n := range ref {
		if c := cerberus[t]; c > n {
			n = c
		}
		total += n
	}
	for t, n := range cerberus {
		if _, both := ref[t]; !both {
			total += n
		}
	}
	return total
}

// firstEntryDiff returns the first triple the two multisets disagree on, in
// (stream, timestamp, line) order so the reported diff is stable across runs, or
// nil when they agree exactly.
func firstEntryDiff(ref, cerberus map[logTriple]int) *FirstDiff {
	triples := make([]logTriple, 0, len(ref)+len(cerberus))
	for t := range ref {
		triples = append(triples, t)
	}
	for t := range cerberus {
		if _, both := ref[t]; !both {
			triples = append(triples, t)
		}
	}
	sort.Slice(triples, func(i, j int) bool {
		a, b := triples[i], triples[j]
		switch {
		case a.stream != b.stream:
			return a.stream < b.stream
		case a.tsNano != b.tsNano:
			return a.tsNano < b.tsNano
		default:
			return a.line < b.line
		}
	})
	for _, t := range triples {
		r, c := ref[t], cerberus[t]
		switch {
		case c == 0:
			return entryDiff(t, ReasonEntryMissing, "entry present in reference only", t.line, missingEntryValue)
		case r == 0:
			return entryDiff(t, ReasonEntryMissing, "entry present in cerberus only", missingEntryValue, t.line)
		case r != c:
			return entryDiff(t, ReasonEntryMultiplicity, "the same entry was returned a different number of times",
				strconv.Itoa(r), strconv.Itoa(c))
		}
	}
	return nil
}

// missingEntryValue is what a first-diff prints on the side that did not return
// the entry, mirroring the matrix lane's "<missing series>".
const missingEntryValue = "<missing entry>"

// entryDiff anchors a log-stream divergence at its exact entry. The timestamp
// rides in TimestampNano as a decimal STRING: a nanosecond epoch exceeds
// float64's exact-integer range, so the matrix lane's float slot cannot carry
// one without silently losing precision.
func entryDiff(t logTriple, code, reason, refValue, cerValue string) *FirstDiff {
	return &FirstDiff{
		Kind:          KindLogStream,
		Stream:        t.stream,
		TimestampNano: strconv.FormatInt(t.tsNano, 10),
		RefValue:      refValue,
		CerberusValue: cerValue,
		Reason:        reason,
		ReasonCode:    code,
	}
}
