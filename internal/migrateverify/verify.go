// Package migrateverify is cerberus's cutover parity gate. It replays a
// harvested corpus against the reference backend for each head — Prometheus,
// Loki, Tempo — AND against cerberus over one query_range window and diffs the
// results, so an operator can prove, before flipping a datasource, that cerberus
// returns the same numbers their current stack does for the queries they actually
// run.
//
// Every corpus entry is routed by the SHAPE of the result it returns, and each
// shape has its own definition of equality and its own comparator. PromQL, LogQL
// metric queries and TraceQL metrics queries all return matrix-shaped results, so
// one comparator judges all three; a LogQL log stream returns log lines and is
// judged on the entry multiset; a TraceQL search returns trace summaries and is
// judged on trace identity plus exact field equality. A shape with no definition
// of equality is reported out of scope with the specific reason it was not
// judged, never dropped and never guessed at.
//
// The flow is read-only against every backend: for each comparison unit it
// issues an identical request to that head's reference and to cerberus, decodes
// both responses into that shape's payload, and diffs them. Every replayed unit
// lands in exactly one verdict — match, diverge, undecidable, unsupported, or
// error — and a divergence is never allow-listed: the gate exits non-zero if
// anything diverges or errors, if a head with replayable queries had no backend
// pair configured to judge them, if nothing was replayable at all, or if any
// (head, shape) family replayed units and compared none of them.
//
// Honesty is the whole point: a comparator only claims a match where both
// backends returned data that agrees. A series or an entry present in one backend
// but not the other is itself a divergence (reported with its first differing
// point), not a silent omission. A dimension that CANNOT be compared — one whose
// ordering neither backend fixes, or a band below a truncation boundary — is
// named as a counted limitation rather than quietly excluded, and it never scores
// as agreement. And a match is only evidence if something was compared — two
// empty results agree, so the report counts what each family actually diffed and
// refuses to call a family that diffed nothing proven.
package migrateverify

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// DefaultTolerance is the absolute epsilon two sample values may differ by and
// still count as equal. It is deliberately tiny — parity means the same number,
// and this only absorbs float round-trips through JSON string encoding, not real
// numeric drift. At large magnitudes a fixed absolute epsilon cannot express
// last-ULP equality, so valuesEqual also applies relativeTolerance (see there).
const DefaultTolerance = 1e-9

// relativeTolerance is the fractional epsilon combined with the absolute
// tolerance so a match survives at large magnitudes. A float64 counter near 1e9
// carries an intrinsic ULP of ~2e-7 — far above DefaultTolerance — so two
// backends that agree to the last representable digit still differ by more than
// an absolute 1e-9. valuesEqual accepts a diff within max(absTol, relTol·|max|),
// so "the same number" holds across the whole float range, not just near zero.
const relativeTolerance = 1e-9

// maxVerifyTolerance caps the absolute --tolerance BuildParams accepts. The gate
// proves the SAME number on both backends; the absolute tolerance exists only to
// absorb float round-trips (large-magnitude equality is handled by
// relativeTolerance). A value at or above 1.0 is already looser than any
// round-trip slack and reads as a fat-finger (e.g. tolerance=1000) that would
// silently bless real divergences, so it is rejected rather than recorded.
const maxVerifyTolerance = 1.0

// resultTypeMatrix is the only Prometheus resultType a range query can return;
// anything else from cerberus means it could not serve the query as a range.
const resultTypeMatrix = "matrix"

// HTTP status-class boundaries verify treats differently. A cerberus 4xx is an
// honest "I can't serve this query" (unsupported, non-blocking); a 5xx — or any
// other non-200, non-4xx status — is a half-broken backend (e.g. its ClickHouse
// is down, 503 on every query) that MUST fail the gate, consistent with a
// connection refusal already failing it. Reporting a half-broken backend as a
// non-blocking WARN would let "VERIFICATION PASSED" ship over a dead backend.
const (
	minClientErrorStatus = 400
	minServerErrorStatus = 500
)

// isClientReject reports whether status is a 4xx — the only non-200 class verify
// treats as a non-blocking "unsupported" query rejection.
func isClientReject(status int) bool {
	return status >= minClientErrorStatus && status < minServerErrorStatus
}

// Verdict is the classification of a single query's parity check.
type Verdict string

const (
	// VerdictMatch: both backends returned matrix data that agrees within tolerance.
	VerdictMatch Verdict = "match"
	// VerdictDiverge: both backends returned matrix data, but the results differ.
	VerdictDiverge Verdict = "diverge"
	// VerdictUnsupported: cerberus ANSWERED but could not serve the query as a
	// range — a 4xx rejection, or a 200 whose body is not a matrix. This is a
	// non-blocking coverage gap, NOT a half-broken backend (a 5xx is VerdictError).
	VerdictUnsupported Verdict = "unsupported"
	// VerdictError: the reference failed, a transport/parse error prevented a
	// comparison, or cerberus returned a 5xx / other non-200, non-4xx status (a
	// half-broken backend, e.g. its ClickHouse is down). Distinct from
	// unsupported: there is either nothing to compare, or the backend is broken
	// rather than honestly rejecting the query — both must fail the gate.
	VerdictError Verdict = "error"
	// VerdictUndecidable: both backends answered successfully and the comparator
	// diffed everything it could without finding a difference, but a named,
	// counted structural limitation prevented a full parity claim. It is
	// NON-BLOCKING on its own (like unsupported, it is an honest "I could not
	// judge this"), and it can never green-light a family that compared nothing:
	// the evidence counter and DeadFamilies do that independently.
	VerdictUndecidable Verdict = "undecidable"
)

// First-diff reason codes. The report's PROSE explains a divergence to a human;
// the code is what other code switches on, so a new comparator's wording can
// never silently fall through a substring match written for the matrix lane.
const (
	// ReasonSeriesMissing: a series one backend returned and the other did not.
	ReasonSeriesMissing = "series-missing"
	// ReasonNoSampleAtStep: a step one backend has a sample at and the other does not.
	ReasonNoSampleAtStep = "no-sample-at-step"
	// ReasonValueDiffers: both backends have a sample at this step, with different values.
	ReasonValueDiffers = "value-differs"
	// ReasonEntryMissing: a log entry one backend returned and the other did not.
	ReasonEntryMissing = "entry-missing"
	// ReasonEntryMultiplicity: both backends returned this log entry, a different
	// number of times.
	ReasonEntryMultiplicity = "entry-multiplicity"
	// ReasonTraceMissing: a trace summary one backend returned and the other did not.
	ReasonTraceMissing = "trace-missing"
	// ReasonTraceFieldDiffers: both backends returned this trace, disagreeing on one
	// of its summary fields.
	ReasonTraceFieldDiffers = "trace-field-differs"
	// ReasonSpanMissing: a matched span one backend returned and the other did not.
	ReasonSpanMissing = "span-missing"
	// ReasonSpanFieldDiffers: both backends returned this span, disagreeing on one of
	// its fields.
	ReasonSpanFieldDiffers = "span-field-differs"
	// ReasonTagMissing: a tag/label NAME one backend returned and the other did not.
	ReasonTagMissing = "tag-missing"
	// ReasonTagValueMissing: a tag/label VALUE one backend returned and the other
	// did not.
	ReasonTagValueMissing = "tag-value-missing"
	// ReasonTagTypeDiffers: both backends returned this tag value (V2 discovery),
	// disagreeing on its type.
	ReasonTagTypeDiffers = "tag-type-differs"
)

// Sample is one point of a range result: a Unix-seconds timestamp and its value.
type Sample struct {
	T float64
	V float64
}

// Series is one labelled time series from a matrix response.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// FirstDiff captures the first point at which two backends disagree for a query.
// Values are formatted strings (Prometheus renders values as strings) so NaN /
// +Inf survive both the human report and JSON encoding, where a float NaN would
// otherwise be unrepresentable.
//
// Kind names which anchor fields are meaningful; every kind-specific anchor is
// omitempty, so no kind ever borrows another kind's field to mean something else.
//
// A log-stream timestamp lives in TimestampNano as a DECIMAL STRING, not in
// Timestamp: a nanosecond epoch exceeds float64's exact-integer range, so the
// float slot cannot carry one without silently losing precision. A trace-search
// timestamp rides in RefValue / CerberusValue as a decimal string for the same
// reason.
type FirstDiff struct {
	Kind          string  `json:"kind"`
	Series        string  `json:"series"`
	Timestamp     float64 `json:"timestamp"`
	RefValue      string  `json:"ref_value"`
	CerberusValue string  `json:"cerberus_value"`
	Reason        string  `json:"reason"`
	ReasonCode    string  `json:"reason_code"`

	Stream        string `json:"stream,omitempty"`
	TimestampNano string `json:"timestamp_nano,omitempty"`
	TraceID       string `json:"trace_id,omitempty"`
	SpanID        string `json:"span_id,omitempty"`
	Field         string `json:"field,omitempty"`
	Scope         string `json:"scope,omitempty"`
	Tag           string `json:"tag,omitempty"`
}

// canonicalLabels renders a label set as a stable, order-independent key so the
// same series from two backends matches regardless of map iteration order.
func canonicalLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(labels[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// formatValue renders a sample value the way Prometheus does, so NaN / Inf are
// human- and JSON-safe.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// valuesEqual reports whether two sample values agree, treating two NaNs as
// equal (both backends declaring "no value here" is agreement, not a divergence)
// and two like-signed infinities as equal by exact, sign-aware comparison.
//
// Infinities are handled BEFORE the abs-diff test: math.Abs(+Inf - +Inf) is NaN,
// and NaN <= tol is false, so a 1/0-style query returning byte-identical +Inf on
// both backends would otherwise be reported divergent. Exact equality gets it
// right in every direction: +Inf==+Inf and -Inf==-Inf match, while +Inf vs -Inf
// and +Inf vs a finite value diverge.
//
// For finite values the match limit combines the absolute tol with a relative
// term so equality holds at large magnitudes where a fixed epsilon cannot
// express last-ULP agreement (see relativeTolerance).
func valuesEqual(a, b, tol float64) bool {
	aNaN, bNaN := math.IsNaN(a), math.IsNaN(b)
	if aNaN || bNaN {
		return aNaN && bNaN
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	limit := tol
	if rel := relativeTolerance * math.Max(math.Abs(a), math.Abs(b)); rel > limit {
		limit = rel
	}
	return math.Abs(a-b) <= limit
}

// Compare matches ref and cerberus series by canonical label set and returns the
// verdict plus, on divergence, the first differing point. It is deterministic:
// series are visited in sorted canonical-label order and, within a series,
// samples in sorted timestamp order, so the "first" diff is stable across runs.
// A series present in only one backend is a divergence, not a skip.
func Compare(ref, cerberus []Series, tol float64) (Verdict, *FirstDiff) {
	refByKey := indexSeries(ref)
	cerByKey := indexSeries(cerberus)

	keys := make([]string, 0, len(refByKey)+len(cerByKey))
	seen := map[string]struct{}{}
	for k := range refByKey {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	for k := range cerByKey {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		r, rok := refByKey[k]
		c, cok := cerByKey[k]
		switch {
		case !cok:
			return VerdictDiverge, missingSeriesDiff(k, r, "present in reference only")
		case !rok:
			return VerdictDiverge, missingSeriesDiff(k, c, "present in cerberus only")
		default:
			if fd := compareSeries(k, r, c, tol); fd != nil {
				return VerdictDiverge, fd
			}
		}
	}
	return VerdictMatch, nil
}

// comparedSeriesCount counts the DISTINCT canonical label sets Compare walks
// across both backends — the number of series the query actually put in front of
// the comparator. It counts the union, not the sum, so a series both backends
// returned counts once and the number reads as "series diffed" rather than
// "responses received".
func comparedSeriesCount(ref, cerberus []Series) int {
	seen := make(map[string]struct{}, len(ref)+len(cerberus))
	for _, s := range ref {
		seen[canonicalLabels(s.Labels)] = struct{}{}
	}
	for _, s := range cerberus {
		seen[canonicalLabels(s.Labels)] = struct{}{}
	}
	return len(seen)
}

// indexSeries keys series by canonical label set. If a backend repeats a label
// set (it should not), the last one wins — a benign, deterministic choice.
func indexSeries(series []Series) map[string]Series {
	out := make(map[string]Series, len(series))
	for _, s := range series {
		out[canonicalLabels(s.Labels)] = s
	}
	return out
}

// missingSeriesDiff builds a FirstDiff for a series that exists in only one
// backend, anchored at the present side's first sample.
func missingSeriesDiff(key string, present Series, reason string) *FirstDiff {
	fd := &FirstDiff{Kind: KindMetricMatrix, Series: key, Reason: reason, ReasonCode: ReasonSeriesMissing}
	presentVal := "<none>"
	if len(present.Samples) > 0 {
		fd.Timestamp = present.Samples[0].T
		presentVal = formatValue(present.Samples[0].V)
	}
	if strings.Contains(reason, "reference only") {
		fd.RefValue, fd.CerberusValue = presentVal, "<missing series>"
	} else {
		fd.RefValue, fd.CerberusValue = "<missing series>", presentVal
	}
	return fd
}

// compareSeries step-aligns two matched series by timestamp and returns the
// first differing point, or nil if they agree everywhere within tol. A timestamp
// present in only one side is a divergence (coverage gap) at that point.
func compareSeries(key string, ref, cerberus Series, tol float64) *FirstDiff {
	refAt := samplesByTS(ref.Samples)
	cerAt := samplesByTS(cerberus.Samples)

	timestamps := make([]float64, 0, len(refAt)+len(cerAt))
	seen := map[float64]struct{}{}
	for _, s := range ref.Samples {
		if _, ok := seen[s.T]; !ok {
			seen[s.T] = struct{}{}
			timestamps = append(timestamps, s.T)
		}
	}
	for _, s := range cerberus.Samples {
		if _, ok := seen[s.T]; !ok {
			seen[s.T] = struct{}{}
			timestamps = append(timestamps, s.T)
		}
	}
	sort.Float64s(timestamps)

	for _, ts := range timestamps {
		rv, rok := refAt[ts]
		cv, cok := cerAt[ts]
		switch {
		case !cok:
			return &FirstDiff{Kind: KindMetricMatrix, Series: key, Timestamp: ts, RefValue: formatValue(rv), CerberusValue: "<no sample>", Reason: "cerberus has no sample at this step", ReasonCode: ReasonNoSampleAtStep}
		case !rok:
			return &FirstDiff{Kind: KindMetricMatrix, Series: key, Timestamp: ts, RefValue: "<no sample>", CerberusValue: formatValue(cv), Reason: "reference has no sample at this step", ReasonCode: ReasonNoSampleAtStep}
		case !valuesEqual(rv, cv, tol):
			return &FirstDiff{Kind: KindMetricMatrix, Series: key, Timestamp: ts, RefValue: formatValue(rv), CerberusValue: formatValue(cv), Reason: "value differs beyond tolerance", ReasonCode: ReasonValueDiffers}
		}
	}
	return nil
}

// samplesByTS indexes samples by timestamp for O(1) step alignment.
func samplesByTS(samples []Sample) map[float64]float64 {
	out := make(map[float64]float64, len(samples))
	for _, s := range samples {
		out[s.T] = s.V
	}
	return out
}

// Query is one comparison unit to replay: a corpus expression, or a probe
// DERIVED from one (a trace-by-id fetch, a tag/tag-value enumeration), tagged
// with the head lane that owns it, the language it came from, and the result
// KIND that selects the comparator which judges it.
//
// TraceID / TagName / Surface are the probe arguments a derived query's kind
// reads — a trace-by-id fetch reads TraceID, a tag-discovery probe reads
// TagName (empty for an unfiltered enumeration) and Surface (which endpoint:
// "labels" / "label-values" for loki, "tags-v1" / "tags-v2" / "tag-values-v1" /
// "tag-values-v2" for tempo). A corpus-sourced query never sets them; no kind
// ever reads a field it did not ask for.
type Query struct {
	Expr    string `json:"expr"`
	Source  string `json:"source"`
	Head    string `json:"head"`
	Lang    string `json:"lang"`
	Kind    string `json:"kind"`
	TraceID string `json:"trace_id,omitempty"`
	TagName string `json:"tag_name,omitempty"`
	Surface string `json:"surface,omitempty"`
}

// OutOfScopeEntry records a corpus entry no comparator can judge — a TraceQL
// compare(), an expression the parser rejected, or a language this build has no
// lane for. Kind names the query SHAPE, in the same vocabulary
// a replayed query's Kind uses, and Reason states, in the operator's words,
// exactly why the gate did not judge it. Reported and counted here, never
// dropped: pretending a query was covered when it was not is the failure this
// accounting exists to prevent.
type OutOfScopeEntry struct {
	Source string `json:"source"`
	Expr   string `json:"expr"`
	Head   string `json:"head,omitempty"`
	Lang   string `json:"lang"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// UnconfiguredEntry records a replayable query whose head lane had no backend
// pair. This is a property of the INVOCATION, not of the query — which is why it
// is kept separate from OutOfScope (a property of the QUERY) and why it BLOCKS:
// the gate cannot claim parity for a query it never ran.
type UnconfiguredEntry struct {
	Source string `json:"source"`
	Expr   string `json:"expr"`
	Head   string `json:"head"`
	Lang   string `json:"lang"`
	Reason string `json:"reason"`
}

// HarvestSkippedEntry records a corpus entry that `migrate harvest` could not
// turn into a replayable query (an unreadable file, a YAML parse failure, a rule
// with no expression). It carries no PromQL to replay, so verify cannot check its
// parity — but it is reported and counted here rather than dropped, because the
// operator needs to know these queries never entered the gate at all.
type HarvestSkippedEntry struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Corpus is the verify input: every replayable query in corpus order tagged with
// its lane, the entries no metric lane can judge, and the harvest-time skips the
// corpus recorded (queries that never became replayable at all).
type Corpus struct {
	Queries        []Query
	OutOfScope     []OutOfScopeEntry
	HarvestSkipped []HarvestSkippedEntry
}

// QueryResult is the parity verdict for a single replayed query, tagged with the
// head lane it ran on. On a divergence it also carries Attribution: a list of
// CANDIDATE causes (never a detection — verify cannot introspect either backend)
// to steer triage.
//
// Kind names the result shape, and therefore which comparator judged this query.
//
// ComparedUnits records how many comparison units that comparator actually
// diffed, counted in its own unit (series for a matrix, log entries for a
// stream) — 0 when both backends returned nothing, which scores match but proves
// nothing. ComparedSeries is the matrix lane's counter and stays matrix-only, so
// a report that talks about "series" is always literally talking about series.
//
// Limitations names the dimensions this comparison could NOT judge, each with the
// count of units the statement covers.
type QueryResult struct {
	Head           string                 `json:"head"`
	Kind           string                 `json:"kind"`
	Source         string                 `json:"source"`
	Expr           string                 `json:"expr"`
	Verdict        Verdict                `json:"verdict"`
	ComparedSeries int                    `json:"compared_series"`
	ComparedUnits  int                    `json:"compared_units"`
	FirstDiff      *FirstDiff             `json:"first_diff,omitempty"`
	Detail         string                 `json:"detail,omitempty"`
	Limitations    []Limitation           `json:"limitations,omitempty"`
	Attribution    []AttributionCandidate `json:"attribution,omitempty"`
}

// Summary counts verdicts. Total counts REPLAYED queries only: Unconfigured
// entries were never issued to any backend, so folding them into Total would
// inflate the denominator of a claim the gate did not make.
//
// ComparedUnits counts the comparison units the comparators actually diffed,
// across every shape. It is the only counter that is EVIDENCE rather than
// bookkeeping: two empty results score VerdictMatch, so Match alone cannot
// distinguish "both backends agreed on real data" from "neither backend returned
// anything over this window". A family whose Compared is 0 proved nothing,
// however many matches it recorded — see Report.DeadFamilies.
//
// ComparedSeries keeps its matrix-only meaning: it counts series and nothing
// else, so a report sentence that says "series" is never quietly counting log
// lines. Limitations names, across the whole run, the dimensions no comparator
// could judge.
type Summary struct {
	Total          int          `json:"total"`
	Match          int          `json:"match"`
	Diverge        int          `json:"diverge"`
	Undecidable    int          `json:"undecidable"`
	Unsupported    int          `json:"unsupported"`
	Error          int          `json:"error"`
	Unconfigured   int          `json:"unconfigured"`
	ComparedSeries int          `json:"compared_series"`
	ComparedUnits  int          `json:"compared_units"`
	OutOfScope     int          `json:"out_of_scope"`
	HarvestSkipped int          `json:"harvest_skipped"`
	Limitations    []Limitation `json:"limitations,omitempty"`
}

// FamilySummary is one (head, result-kind) partition's counts and evidence. Unit
// names what Compared counts, so a row can never read "412 series" about a lane
// that diffed log lines.
//
// Families are carried as a KIND-SORTED SLICE, not a map: the report's
// byte-determinism is a pinned contract and a map would break it.
type FamilySummary struct {
	Kind        string       `json:"kind"`
	Unit        string       `json:"unit"`
	Total       int          `json:"total"`
	Match       int          `json:"match"`
	Diverge     int          `json:"diverge"`
	Undecidable int          `json:"undecidable"`
	Unsupported int          `json:"unsupported"`
	Error       int          `json:"error"`
	Compared    int          `json:"compared"`
	Limitations []Limitation `json:"limitations,omitempty"`
}

// HeadSummary is one lane's counts, carried alongside the roll-up so a healthy
// head can never mask a dead one: an aggregate "40 matched" reads green even when
// a second lane compared nothing at all.
//
// Families splits that lane one level further, for the same reason: a head now
// runs more than one shape, and 412 compared log entries must not vouch for a
// metric family that compared zero series.
//
// Configured records whether the operator supplied that head's backend pair, and
// is what makes a lane that replayed NOTHING legible. Without it a configured lane
// whose corpus entries all routed out of scope is indistinguishable from a head
// the operator never asked for — both would be absent from the table, and the
// operator would flip that datasource having read a green report that never
// mentions it.
type HeadSummary struct {
	Head       string          `json:"head"`
	Configured bool            `json:"configured"`
	Summary    Summary         `json:"summary"`
	Families   []FamilySummary `json:"families,omitempty"`
}

// ReportParams records the comparison parameters the gate and humans need to
// judge how strict the parity check actually was — chiefly the tolerance, since
// a loosened tolerance silently weakens every "match" verdict. Recording it in
// the gate-consumed Report (not only the --report diagnostic) means a verify.json
// produced with a fat-fingered tolerance can no longer be blessed blind.
type ReportParams struct {
	Tolerance float64 `json:"tolerance"`
}

// ReportVersion is the schema version stamped into the gate-consumed `--json`
// report (the plain Report, distinct from the richer `--report` VerifyReport
// diagnostic). WriteJSON stamps it and the cutover gate refuses a report whose
// version it does not understand, so a schema-drifted or wrong-type artifact
// blocks rather than zero-filling to a silent PASS. Bump it on any breaking
// change to the on-disk Report shape.
//
// Version 3 partitions each head by result kind (Heads[].Families), records the
// shape-agnostic ComparedUnits evidence counter alongside the matrix-only
// ComparedSeries, adds the undecidable verdict, and carries the counted
// Limitations each comparison could not judge. An older artifact decoded by this
// build would zero-fill all of them into a bogus "no families, 0 units compared,
// nothing undecidable, no limitations" — precisely the silent zero-fill the
// version check exists to stop — so the bump is enforcing, not cosmetic.
const ReportVersion = 3

// Report is the full parity result: the schema version, the resolved comparison
// params, the roll-up summary, the per-head lane summaries, per-query verdicts,
// the queries whose lane had no backends, the out-of-scope accounting, and the
// harvest-time skips.
type Report struct {
	SchemaVersion  int                   `json:"schema_version"`
	Params         ReportParams          `json:"params"`
	Summary        Summary               `json:"summary"`
	Heads          []HeadSummary         `json:"heads"`
	Results        []QueryResult         `json:"results"`
	Unconfigured   []UnconfiguredEntry   `json:"unconfigured,omitempty"`
	OutOfScope     []OutOfScopeEntry     `json:"out_of_scope,omitempty"`
	HarvestSkipped []HarvestSkippedEntry `json:"harvest_skipped,omitempty"`
}

// Failed reports whether the gate should exit non-zero. It blocks on a wrong
// answer AND on an absent answer, because "VERIFICATION PASSED" must never ship on
// zero evidence:
//
//   - any diverging or erroring query;
//   - any replayable query whose head lane was never configured — the gate cannot
//     claim parity for a query it never ran;
//   - a run that replayed nothing at all (see JudgedNothing) — an all-out-of-scope
//     corpus would otherwise print "all 0 queries matched" and exit 0;
//   - any (head, shape) family that replayed comparison units but compared none
//     of them (see DeadFamilies) — a family whose backend answered nothing
//     comparable, or answered two empty results, has proved nothing.
//
// The last two mirror the cutover gate's blocking rules exactly, so `verify`'s own
// exit code and banner can never disagree with `migrate gate`'s verdict on the
// same report.
//
// Undecidable, unsupported, out-of-scope and harvest-skipped entries are reported
// but do not fail on their own: each is a surfaced coverage gap, honestly named,
// not a wrong answer. (A family made up ENTIRELY of them still blocks, via
// DeadFamilies, because it compared nothing.)
func (r Report) Failed() bool {
	return r.Summary.Diverge > 0 || r.Summary.Error > 0 || r.Summary.Unconfigured > 0 ||
		r.JudgedNothing() || len(r.DeadFamilies()) > 0
}

// JudgedNothing reports whether the run replayed no query at all. A corpus whose
// every entry routed out of scope (a Loki-heavy dashboard set is all log-stream
// selectors), or an empty corpus, leaves Total at 0 — and a gate that judged
// nothing has no business printing PASSED, whatever a CI job reading its exit code
// concludes.
func (r Report) JudgedNothing() bool { return r.Summary.Total == 0 }

// FamilyRef names one (head, result-kind) partition for the blocking rules and
// the reasons rendered from them.
type FamilyRef struct {
	Head     string
	Kind     string
	Unit     string
	Total    int
	Compared int
}

// DeadFamilies returns every (head, family) partition that replayed comparison
// units but compared none of them.
//
// It is keyed on the evidence counter rather than on verdict counts because both
// ways of comparing nothing must block: a family whose cerberus side 4xx'd every
// query (all-unsupported, no verdict reaches the comparator) and a family where
// both backends returned nothing over the window (all-match, comparator walked
// zero keys) are equally devoid of evidence.
//
// It is per FAMILY, not per head, because a head runs more than one shape: the
// same masking argument that split the aggregate into per-head rows applies one
// level down, so 412 compared log entries must not vouch for a metric family that
// compared zero series. A head that replayed units without reporting any family
// partition has no evidence to attribute either, and is reported the same way —
// otherwise a malformed report would slip past the rule that every replay must
// show what it compared.
func (r Report) DeadFamilies() []FamilyRef {
	var out []FamilyRef
	for _, h := range r.Heads {
		if h.Summary.Total == 0 {
			continue
		}
		if len(h.Families) == 0 {
			out = append(out, FamilyRef{
				Head: h.Head, Unit: UnitComparisons,
				Total: h.Summary.Total, Compared: h.Summary.ComparedUnits,
			})
			continue
		}
		for _, f := range h.Families {
			if f.Total > 0 && f.Compared == 0 {
				out = append(out, FamilyRef{
					Head: h.Head, Kind: f.Kind, Unit: f.Unit,
					Total: f.Total, Compared: f.Compared,
				})
			}
		}
	}
	return out
}

// FamilyLabel renders a family reference for an operator: the head plus the shape
// it names, or the head alone when the report attributed no shape to it.
func (f FamilyRef) FamilyLabel() string {
	if f.Kind == "" {
		return f.Head
	}
	return f.Head + "/" + f.Kind
}

// IdleLanes returns every head lane the operator CONFIGURED that had no replayable
// query to run — every corpus entry for that head routed out of scope. It does not
// block (the entries are honestly out of scope, not failures), but it is surfaced
// so a configured lane can never be silently absent from the report the operator
// reads before flipping that datasource.
func (r Report) IdleLanes() []HeadSummary {
	var out []HeadSummary
	for _, h := range r.Heads {
		if h.Configured && h.Summary.Total == 0 {
			out = append(out, h)
		}
	}
	return out
}

// IdleDerivedFamilies returns every CONFIGURED head that ran the trace-search
// family (so it is CAPABLE of deriving a trace-by-id probe) but whose
// derived-only trace-by-id family never ran at all.
//
// trace-by-id is never a top-level corpus entry — no query language names a
// trace ID — so it exists in a report ONLY when trace-search derived it. A
// family that never derived anything leaves no row in Families at all (see
// reportRun.family), so "never ran" cannot be read off Total==0 the way a
// corpus-sourced family's deadness can; it has to be read off the family's
// ABSENCE alongside its capable sibling's presence.
//
// It does not block: an all-out-of-scope corpus already fails via
// JudgedNothing/DeadFamilies, and a head that legitimately never derived a
// probe (no trace-search query returned a trace both backends have) proved
// nothing wrong — it simply had nothing to derive from. But it is surfaced,
// not silent: without it, a derived family that never ran once is
// indistinguishable from one this build forgot to wire up at all, and an
// operator reading a report that never mentions trace-by-id has no way to
// tell "verified" from "never attempted".
func (r Report) IdleDerivedFamilies() []FamilyRef {
	var out []FamilyRef
	for _, h := range r.Heads {
		if !h.Configured {
			continue
		}
		var canDerive, derived bool
		for _, f := range h.Families {
			switch {
			case f.Kind == KindTraceSearch:
				canDerive = true
			case f.Kind == KindTraceByID && f.Total > 0:
				derived = true
			}
		}
		if canDerive && !derived {
			out = append(out, FamilyRef{Head: h.Head, Kind: KindTraceByID, Unit: UnitSpans})
		}
	}
	return out
}

// Backend issues a range query against one backend and returns the parsed
// result. Transport / decode failures are returned as err; an HTTP non-200 or a
// non-matrix body is carried in RangeResult (Status / ResultType) so the caller
// can classify unsupported-vs-error itself.
type Backend interface {
	QueryRange(ctx context.Context, expr string, p Params) (RangeResult, error)
}

// RangeResult is a parsed range response from one backend.
type RangeResult struct {
	Status     int
	ResultType string
	Series     []Series
}

// Lane is one head's reference/cerberus backend pair. Both sides speak that
// head's dialect, so the same query is replayed identically against the
// operator's current stack and against cerberus.
//
// RefKind / CerberusKind are the SAME connection as Ref / Cerberus — one
// *HTTPBackend implements both interfaces — and are separate fields only so the
// matrix Backend contract stays one method wide. A lane whose kind backends are
// absent records any non-matrix query as UNCONFIGURED, which blocks: the gate
// cannot claim parity for a shape it had no transport to fetch.
type Lane struct {
	Ref          Backend
	Cerberus     Backend
	RefKind      KindBackend
	CerberusKind KindBackend
}

// Verify replays every corpus query against the lane its head names and assembles
// one report covering all heads. Queries are processed in corpus order for
// deterministic output. Each query is issued to that head's reference and to
// cerberus with identical parameters; the verdict is derived as:
//
//   - transport/decode failure on either backend, a reference that did not
//     return a usable baseline, or a cerberus 5xx / other non-200-non-4xx status
//     (a half-broken backend) → error (nothing to compare, or the backend is
//     broken);
//   - cerberus 4xx, or a 200 body of the wrong result shape → unsupported
//     (answered, but could not serve the query in the shape this lane asked for);
//   - otherwise → the shape's comparator verdict.
//
// A query whose head has no configured lane is recorded as UNCONFIGURED: never
// replayed against another head's backend (which would compare two unrelated
// APIs), never silently dropped, and blocking — see Report.Failed.
func Verify(ctx context.Context, corpus Corpus, lanes map[string]Lane, p Params) Report {
	run := &reportRun{
		rep: Report{
			SchemaVersion:  ReportVersion,
			Params:         ReportParams{Tolerance: p.Tolerance},
			OutOfScope:     corpus.OutOfScope,
			HarvestSkipped: corpus.HarvestSkipped,
		},
		byHead: map[string]*headAccum{},
	}
	run.rep.Summary.OutOfScope = len(corpus.OutOfScope)
	run.rep.Summary.HarvestSkipped = len(corpus.HarvestSkipped)

	// Seed a row for every CONFIGURED lane before any query is routed. A lane whose
	// corpus entries all routed out of scope replays nothing, so the query loop
	// would never create its row and the per-head table — whose whole purpose is
	// that a healthy head cannot mask a quiet one — would silently omit the lane
	// the operator explicitly supplied backends for.
	for head := range lanes {
		run.head(head)
	}
	// Out-of-scope entries are attributed to the head that declined them, so a lane
	// showing 0 replayed also shows WHY: "tempo 0 replayed … 12 out of scope" is a
	// legible answer, "tempo absent" is not. An entry whose language this build has
	// no lane for carries no head and stays in the roll-up only.
	for _, e := range corpus.OutOfScope {
		if e.Head == "" {
			continue
		}
		run.head(e.Head).summary.OutOfScope++
	}

	// The corpus batch first, then whatever it derived. Both go through the SAME
	// loop body, so lane seeding, the unconfigured bucket, the verdict switch and
	// the per-head/per-family rows exist in exactly one place. Derived units never
	// derive further units: the depth is fixed at one.
	derived := run.runBatch(ctx, corpus.Queries, lanes, p)
	run.runBatch(ctx, derived, lanes, p)

	run.rep.Heads = sortedHeadSummaries(run.byHead, lanes)
	return run.rep
}

// headAccum accumulates one head's roll-up plus its per-family partitions while a
// run is in flight. Families are held in a map for O(1) accumulation and flattened
// into a kind-sorted slice at the end, where determinism is what matters.
type headAccum struct {
	summary  Summary
	families map[string]*FamilySummary
}

// reportRun is the in-flight report plus its per-head accumulators, so one batch
// loop can serve both the corpus batch and the batch it derives.
type reportRun struct {
	rep    Report
	byHead map[string]*headAccum
}

// head returns the accumulator for a head, creating it on first use.
func (rr *reportRun) head(head string) *headAccum {
	h, ok := rr.byHead[head]
	if !ok {
		h = &headAccum{families: map[string]*FamilySummary{}}
		rr.byHead[head] = h
	}
	return h
}

// family returns a head's accumulator for one result kind, creating it on first
// use and stamping the unit that kind counts in.
func (rr *reportRun) family(head, kind string) *FamilySummary {
	h := rr.head(head)
	f, ok := h.families[kind]
	if !ok {
		f = &FamilySummary{Kind: kind, Unit: kindUnit(kind)}
		h.families[kind] = f
	}
	return f
}

// runBatch replays one batch of comparison units through the shared accounting
// and returns the units that batch derived.
func (rr *reportRun) runBatch(ctx context.Context, qs []Query, lanes map[string]Lane, p Params) []Query {
	var derived []Query
	for _, q := range qs {
		lane, ok := lanes[q.Head]
		if !ok {
			rr.recordUnconfigured(q, fmt.Sprintf(
				"head=%s has replayable %s queries but no reference/cerberus backend pair was configured for it; the gate did not judge them",
				q.Head, q.Lang,
			))
			continue
		}
		if !isMatrixKind(q.Kind) && (lane.RefKind == nil || lane.CerberusKind == nil) {
			rr.recordUnconfigured(q, fmt.Sprintf(
				"head=%s has replayable %s queries of shape %s but its backend pair speaks no wire contract for that shape; the gate did not judge them",
				q.Head, q.Lang, q.Kind,
			))
			continue
		}
		res, next := verifyOne(ctx, q, lane, p)
		derived = append(derived, next...)
		rr.record(res)
	}
	return derived
}

// recordUnconfigured books a comparison unit that was never issued to anything.
func (rr *reportRun) recordUnconfigured(q Query, reason string) {
	rr.rep.Unconfigured = append(rr.rep.Unconfigured, UnconfiguredEntry{
		Source: q.Source, Expr: q.Expr, Head: q.Head, Lang: q.Lang, Reason: reason,
	})
	rr.rep.Summary.Unconfigured++
	rr.head(q.Head).summary.Unconfigured++
}

// record folds one replayed unit's outcome into the roll-up, its head, and its
// (head, shape) family. Every counter a blocking rule reads is updated here and
// nowhere else, so no shape can be counted into one view and out of another.
func (rr *reportRun) record(res QueryResult) {
	h := rr.head(res.Head)
	f := rr.family(res.Head, res.Kind)

	rr.rep.Results = append(rr.rep.Results, res)
	rr.rep.Summary.Total++
	h.summary.Total++
	f.Total++

	rr.rep.Summary.ComparedSeries += res.ComparedSeries
	h.summary.ComparedSeries += res.ComparedSeries
	rr.rep.Summary.ComparedUnits += res.ComparedUnits
	h.summary.ComparedUnits += res.ComparedUnits
	f.Compared += res.ComparedUnits

	rr.rep.Summary.Limitations = mergeLimitations(rr.rep.Summary.Limitations, res.Limitations)
	h.summary.Limitations = mergeLimitations(h.summary.Limitations, res.Limitations)
	f.Limitations = mergeLimitations(f.Limitations, res.Limitations)

	switch res.Verdict {
	case VerdictMatch:
		rr.rep.Summary.Match++
		h.summary.Match++
		f.Match++
	case VerdictDiverge:
		rr.rep.Summary.Diverge++
		h.summary.Diverge++
		f.Diverge++
	case VerdictUndecidable:
		rr.rep.Summary.Undecidable++
		h.summary.Undecidable++
		f.Undecidable++
	case VerdictUnsupported:
		rr.rep.Summary.Unsupported++
		h.summary.Unsupported++
		f.Unsupported++
	case VerdictError:
		rr.rep.Summary.Error++
		h.summary.Error++
		f.Error++
	}
}

// sortedHeadSummaries flattens the per-head counters into a head-token-sorted
// slice so the JSON report is byte-deterministic across runs (Go map iteration
// order is not). Each row is tagged with whether that head had a configured
// backend pair, which is what lets a consumer tell "lane supplied, judged nothing"
// apart from "lane never supplied", and carries its families in kind order.
func sortedHeadSummaries(byHead map[string]*headAccum, lanes map[string]Lane) []HeadSummary {
	heads := make([]string, 0, len(byHead))
	for h := range byHead {
		heads = append(heads, h)
	}
	sort.Strings(heads)
	out := make([]HeadSummary, 0, len(heads))
	for _, h := range heads {
		_, configured := lanes[h]
		out = append(out, HeadSummary{
			Head:       h,
			Configured: configured,
			Summary:    byHead[h].summary,
			Families:   sortedFamilies(byHead[h].families),
		})
	}
	return out
}

// sortedFamilies flattens one head's family accumulators into a kind-sorted slice.
func sortedFamilies(byKind map[string]*FamilySummary) []FamilySummary {
	if len(byKind) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	out := make([]FamilySummary, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, *byKind[k])
	}
	return out
}

// isMatrixKind reports whether a comparison unit is judged by the matrix
// comparator. The empty kind is included so a Query assembled without one — a
// hand-built corpus, a caller predating the kind vocabulary — is judged as a
// metric matrix rather than falling through to "no comparator".
func isMatrixKind(kind string) bool {
	return kind == "" || kind == KindMetricMatrix
}

// verifyOne dispatches one comparison unit to the comparator its result shape
// selects, and returns the units that comparison derived.
func verifyOne(ctx context.Context, q Query, lane Lane, p Params) (QueryResult, []Query) {
	if isMatrixKind(q.Kind) {
		res := verifyMatrix(ctx, q, lane.Ref, lane.Cerberus, p)
		res.Kind, res.ComparedUnits = KindMetricMatrix, res.ComparedSeries
		return res, nil
	}
	return verifyKind(ctx, q, lane, p)
}

// verifyMatrix runs the parity check for a single matrix-shaped query on its
// head's lane.
func verifyMatrix(ctx context.Context, q Query, ref, cerberus Backend, p Params) QueryResult {
	out := QueryResult{Head: q.Head, Source: q.Source, Expr: q.Expr}

	refRes, refErr := ref.QueryRange(ctx, q.Expr, p)
	cerRes, cerErr := cerberus.QueryRange(ctx, q.Expr, p)

	switch {
	case refErr != nil:
		out.Verdict, out.Detail = VerdictError, fmt.Sprintf("reference request failed: %v", refErr)
	case cerErr != nil:
		out.Verdict, out.Detail = VerdictError, fmt.Sprintf("cerberus request failed: %v", cerErr)
	case cerRes.Status != http.StatusOK && !isClientReject(cerRes.Status):
		// A 5xx (or any other non-200, non-4xx) means cerberus is half-broken —
		// e.g. its ClickHouse is down and it 503s every query. That is a BLOCKING
		// failure, not a query it honestly could not serve; classing it as
		// unsupported would let "VERIFICATION PASSED" ship over a dead backend.
		out.Verdict = VerdictError
		out.Detail = fmt.Sprintf("cerberus returned status=%d (backend error, not a query rejection)", cerRes.Status)
	case cerRes.Status != http.StatusOK || cerRes.ResultType != resultTypeMatrix:
		// A 4xx rejection or a 200 non-matrix body: cerberus answered but could
		// not serve the query as a range. Non-blocking coverage gap.
		out.Verdict = VerdictUnsupported
		out.Detail = fmt.Sprintf("cerberus returned status=%d resultType=%q", cerRes.Status, cerRes.ResultType)
	case refRes.Status != http.StatusOK || refRes.ResultType != resultTypeMatrix:
		out.Verdict = VerdictError
		out.Detail = fmt.Sprintf("reference returned status=%d resultType=%q (no baseline to compare)", refRes.Status, refRes.ResultType)
	default:
		verdict, fd := Compare(refRes.Series, cerRes.Series, p.Tolerance)
		out.Verdict, out.FirstDiff = verdict, fd
		// Recorded on EVERY comparator outcome, match included: a match over two
		// empty matrices diffed nothing, and only this counter says so.
		out.ComparedSeries = comparedSeriesCount(refRes.Series, cerRes.Series)
		// Attribution is PromQL-shaped: its hotspot matcher keys on bare
		// rate / increase / histogram_quantile tokens, which also occur verbatim
		// in LogQL and TraceQL, and its regression note describes a PromQL-only
		// experimental CH path. Firing it on another head would print a
		// confidently wrong candidate cause, so it is gated to the prom lane.
		if verdict == VerdictDiverge && q.Head == HeadProm {
			out.Attribution = attributeDivergence(out.Expr, fd)
		}
	}
	return out
}

// TextGuidance carries the CLI context the internal report cannot know on its
// own — the exact, copy-pasteable command that regenerates this diagnostic — so
// the failing text report can tell an operator precisely how to file a bug.
type TextGuidance struct {
	ReproCommand string
}

// WriteText renders the human report with no CLI-derived bug-report guidance.
//
// It is output-identical to WriteTextGuided with an empty TextGuidance, but it is
// deliberately KEPT (not dead): it is the Report-only entrypoint used by callers
// and tests that hold a Report without the CLI's repro-command context — the
// verify unit tests and the guided/unguided writeText parity checks drive it
// directly. The production CLI always calls WriteTextGuided so a failing run ends
// with a copy-pasteable reproduction; this variant keeps the guidance-free path
// exercised so the two never silently diverge.
func (r Report) WriteText(w io.Writer) error {
	return r.writeText(w, nil)
}

// WriteTextGuided renders the human report and, on failure, a bug-report section
// built from the CLI context in g (the repro command). The CLI uses this so a
// failing run ends with a copy-pasteable reproduction.
func (r Report) WriteTextGuided(w io.Writer, g TextGuidance) error {
	return r.writeText(w, &g)
}

// writeText renders the report as a scannable, human-readable gate report. It
// LEADS with an unmistakable PASSED / FAILED verdict banner, then a header (with
// the one-time experimental-feature note), one block per non-matching query (with
// its candidate-cause attribution), the out-of-scope accounting, the roll-up
// counts, and — on failure — a "Report this to cerberus" bug-report section.
func (r Report) writeText(w io.Writer, g *TextGuidance) error {
	bw := &errWriter{w: w}

	// R1: lead with a prominent, unmistakable verdict line.
	if r.Failed() {
		// Unconfigured is named in the banner: a run that failed ONLY because a
		// head lane was never configured must not read as "0 diverged, 0 errored"
		// with no visible cause. The no-evidence reasons follow it for the same
		// reason — a run that failed because a lane compared nothing must say so on
		// the load-bearing line, not only in the table below it.
		bw.printf("VERIFICATION FAILED — %d diverged, %d errored, %d unjudged (unconfigured lane), %d matched (of %d replayed)\n",
			r.Summary.Diverge, r.Summary.Error, r.Summary.Unconfigured, r.Summary.Match, r.Summary.Total)
		for _, reason := range r.noEvidenceReasons() {
			bw.printf("                     %s\n", reason)
		}
		bw.printf("\n")
	} else if r.Summary.Unsupported > 0 {
		// Unsupported queries pass the gate but are NOT matches; the banner must
		// not equate Total with matched or it overstates what agreed.
		bw.printf("VERIFICATION PASSED — %d matched, %d unsupported, 0 diverged (of %d)\n\n",
			r.Summary.Match, r.Summary.Unsupported, r.Summary.Total)
	} else {
		bw.printf("VERIFICATION PASSED — all %d queries matched\n\n", r.Summary.Total)
	}

	bw.printf("# cerberus migrate verify\n")
	bw.printf("#\n")
	if r.hasNonMatrixFamily() {
		// A run that judged more than one result shape describes itself in the
		// vocabulary that covers all of them: "series" is the matrix lane's unit,
		// and a header that says it while a log-stream lane ran would name the
		// wrong evidence.
		bw.printf("# Parity gate: each corpus query replayed against its head's reference backend\n")
		bw.printf("# and cerberus over one window, results diffed by the comparator its result\n")
		bw.printf("# shape selects. A divergence is never allow-listed — the gate fails if any\n")
		bw.printf("# query diverges or errors, if a head's replayable queries had no backend pair\n")
		bw.printf("# to judge them, if nothing was replayable at all, or if any family compared\n")
		bw.printf("# nothing.\n")
	} else {
		bw.printf("# Parity gate: each corpus query replayed against its head's reference backend\n")
		bw.printf("# and cerberus over one query_range window, results diffed series-by-series.\n")
		bw.printf("# A divergence is never allow-listed — the gate fails if any query diverges\n")
		bw.printf("# or errors, if a head's replayable queries had no backend pair to judge them,\n")
		bw.printf("# if nothing was replayable at all, or if any lane diffed zero series.\n")
	}
	bw.printf("#\n")
	bw.printf("# Note: %s\n", ExperimentalNote)
	bw.printf("#\n")
	// Surface the tolerance the matches were judged at: a loosened tolerance
	// silently weakens every "match", so the operator must see how strict the
	// comparison actually was.
	bw.printf("# Match tolerance: %s (absolute; relative granularity also applied at large magnitudes)\n", formatValue(r.Params.Tolerance))
	bw.printf("#\n")
	bw.printf("# %d queries: %d match, %d diverge, %d unsupported, %d error (+%d unconfigured, +%d out of scope, +%d harvest-skipped)\n",
		r.Summary.Total, r.Summary.Match, r.Summary.Diverge, r.Summary.Unsupported, r.Summary.Error,
		r.Summary.Unconfigured, r.Summary.OutOfScope, r.Summary.HarvestSkipped)
	if r.Summary.Undecidable > 0 {
		// Undecidable is NOT folded into the match count: both backends answered
		// and nothing disagreed, but a named limitation stopped short of a parity
		// claim, and a reader who sees only "match" would take the stronger claim.
		bw.printf("# %d undecidable — a named, counted limitation prevented a parity claim; see \"not judged\" below\n",
			r.Summary.Undecidable)
	}
	bw.printf("\n")

	r.writeHeadTable(bw)

	for _, res := range r.Results {
		if res.Verdict == VerdictMatch {
			continue
		}
		bw.printf("== [%s] %s %s\n", res.Verdict, res.Head, res.Source)
		bw.printf("   expr:   %s\n", res.Expr)
		if res.Detail != "" {
			bw.printf("   detail: %s\n", res.Detail)
		}
		if res.FirstDiff != nil {
			writeFirstDiff(bw, res.FirstDiff)
		}
		for _, a := range res.Attribution {
			bw.printf("   candidate-cause [%s]: %s\n", a.Category, a.Note)
		}
		bw.printf("\n")
	}

	// Unconfigured prints BEFORE out-of-scope because it blocks: an operator
	// scanning a failing report must reach the lane they forgot to configure
	// before the non-blocking accounting.
	if len(r.Unconfigured) > 0 {
		bw.printf("== unconfigured (%d) — replayable queries whose head lane had no backend pair; the gate did not judge them\n", len(r.Unconfigured))
		for _, e := range r.Unconfigured {
			bw.printf("   %s  %s\n", e.Head, e.Source)
			bw.printf("        %s\n", e.Reason)
		}
		bw.printf("\n")
	}

	// What the run could not judge, with the count each statement covers. It sits
	// with the other accounting rather than in a footnote: a limitation is the one
	// thing a reader cannot infer from the verdict counts, because a dimension
	// nobody compared leaves no trace in them.
	if r.Summary.Undecidable > 0 || len(r.Summary.Limitations) > 0 {
		bw.printf("== not judged — dimensions no comparator could judge, and how much each covers\n")
		if r.Summary.Undecidable > 0 {
			bw.printf("   %d quer%s reached no parity claim at all\n",
				r.Summary.Undecidable, pluralQueries(r.Summary.Undecidable))
		}
		for _, l := range r.Summary.Limitations {
			bw.printf("   [%s] %d: %s\n", l.Code, l.Count, l.Detail)
		}
		bw.printf("\n")
	}

	if len(r.OutOfScope) > 0 {
		bw.printf("== out of scope (%d) — no parity is definable for these queries\n", len(r.OutOfScope))
		for _, e := range r.OutOfScope {
			bw.printf("   %s %s %s: %s\n", outOfScopeLane(e), e.Kind, e.Source, e.Reason)
		}
		bw.printf("\n")
	}

	if len(r.HarvestSkipped) > 0 {
		bw.printf("== harvest-skipped (%d) — never became a replayable query, not checked\n", len(r.HarvestSkipped))
		for _, e := range r.HarvestSkipped {
			bw.printf("   %s: %s\n", e.Source, e.Reason)
		}
		bw.printf("\n")
	}

	if r.Failed() {
		bw.printf("FAIL: %d diverge, %d error, %d unconfigured\n", r.Summary.Diverge, r.Summary.Error, r.Summary.Unconfigured)
		for _, reason := range r.noEvidenceReasons() {
			bw.printf("FAIL: %s\n", reason)
		}
		r.writeBugReport(bw, g)
	} else {
		if r.hasNonMatrixFamily() {
			bw.printf("PASS: %d match, %d undecidable, %d unsupported, %d comparison units compared (no divergence)\n",
				r.Summary.Match, r.Summary.Undecidable, r.Summary.Unsupported, r.Summary.ComparedUnits)
		} else {
			bw.printf("PASS: %d match, %d unsupported, %d series compared (no divergence)\n",
				r.Summary.Match, r.Summary.Unsupported, r.Summary.ComparedSeries)
		}
		for _, h := range r.IdleLanes() {
			bw.printf("NOTE: head %s was configured but had no replayable query (%d out of scope); "+
				"this lane was not judged\n", h.Head, h.Summary.OutOfScope)
		}
		for _, f := range r.IdleDerivedFamilies() {
			bw.printf("NOTE: head %s ran trace-search but never derived a %s probe (no trace-search result "+
				"gave it a trace both backends have); that shape was not judged this run\n", f.Head, f.Kind)
		}
	}
	return bw.err
}

// noEvidenceReasons renders the blocking causes that are an ABSENCE rather than a
// wrong answer: a run that replayed nothing, and each lane that replayed queries
// but diffed no series. They are spelled out because the counts on the banner
// ("0 diverged, 0 errored") describe them as zeros, and a reader has no way to
// tell a clean run from a run that judged nothing without being told.
func (r Report) noEvidenceReasons() []string {
	var out []string
	if r.JudgedNothing() {
		out = append(out, fmt.Sprintf(
			"the gate judged nothing: 0 corpus queries were replayable on any lane (+%d out of scope, +%d harvest-skipped)",
			r.Summary.OutOfScope, r.Summary.HarvestSkipped,
		))
	}
	for _, f := range r.DeadFamilies() {
		out = append(out, fmt.Sprintf(
			"%s compared nothing: 0 %s diffed across %d replayed probes; that shape is unproven on that datasource",
			f.FamilyLabel(), f.Unit, f.Total,
		))
	}
	return out
}

// pluralQueries renders the "query"/"queries" suffix for a count.
func pluralQueries(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// hasNonMatrixFamily reports whether any head judged a shape other than the
// metric matrix. It gates every sentence whose vocabulary would otherwise be
// wrong — "series compared" on a run that diffed log lines — so a matrix-only
// run reads exactly as it always has.
func (r Report) hasNonMatrixFamily() bool {
	for _, h := range r.Heads {
		for _, f := range h.Families {
			if f.Kind != KindMetricMatrix {
				return true
			}
		}
	}
	return false
}

// writeFirstDiff renders a divergence anchored in the vocabulary of the shape it
// came from. The matrix anchor is a series and a step; a log-stream anchor is a
// stream and a nanosecond timestamp, which is printed from its decimal string —
// routing it through formatValue's float rendering would print a present-day
// nanosecond epoch as "1.78e+18" and lose the entry it points at. A trace-search
// anchor is a trace, optionally a span within it, and the field that differed.
func writeFirstDiff(bw *errWriter, fd *FirstDiff) {
	switch fd.Kind {
	case KindLogStream:
		bw.printf("   first-diff: stream=%s ts=%sns ref=%s cerberus=%s (%s)\n",
			fd.Stream, fd.TimestampNano, fd.RefValue, fd.CerberusValue, fd.Reason)
	case KindTraceSearch, KindTraceByID:
		bw.printf("   first-diff: %s ref=%s cerberus=%s (%s)\n",
			traceDiffAnchor(fd), fd.RefValue, fd.CerberusValue, fd.Reason)
	case KindTagDiscovery:
		bw.printf("   first-diff: %s ref=%s cerberus=%s (%s)\n",
			tagDiffAnchor(fd), fd.RefValue, fd.CerberusValue, fd.Reason)
	default:
		bw.printf("   first-diff: series=%s ts=%s ref=%s cerberus=%s (%s)\n",
			fd.Series, formatValue(fd.Timestamp), fd.RefValue, fd.CerberusValue, fd.Reason)
	}
}

// traceDiffAnchor names the exact place a trace-search divergence sits. The span
// and field clauses appear only when the diff actually has one, so a whole-trace
// difference is never printed with an empty "span=" or "field=" that reads as a
// missing value rather than an inapplicable one.
func traceDiffAnchor(fd *FirstDiff) string {
	anchor := "trace=" + fd.TraceID
	if fd.SpanID != "" {
		anchor += " span=" + fd.SpanID
	}
	if fd.Field != "" {
		anchor += " field=" + fd.Field
	}
	return anchor
}

// tagDiffAnchor names the exact place a tag-discovery divergence sits: the
// scope (tempo v2 only; empty elsewhere) and the tag name or value it concerns.
func tagDiffAnchor(fd *FirstDiff) string {
	anchor := "tag=" + fd.Tag
	if fd.Scope != "" {
		anchor = "scope=" + fd.Scope + " " + anchor
	}
	return anchor
}

// writeHeadTable prints the per-lane roll-up in head-token order. It exists so a
// healthy head cannot mask a dead one: the aggregate line above can read
// "40 match" while a second lane compared nothing, and only the per-head split
// makes that visible.
//
// Each row states whether the operator configured that lane and how much it
// actually diffed, because those two numbers — not the match count — are what says
// whether the lane proved anything. A configured lane with 0 replayed queries and a
// head that only ever appeared in the out-of-scope list both get a row.
//
// A head that judged ONE shape and reached a verdict on every replay states its
// evidence on the head line itself. A head that judged more than one shape, or
// that left anything undecidable, moves its evidence to one indented row per
// family — the head line alone would sum two shapes into one number, which is the
// masking the split exists to prevent.
func (r Report) writeHeadTable(bw *errWriter) {
	if len(r.Heads) == 0 {
		return
	}
	bw.printf("# per-head lanes:\n")
	for _, h := range r.Heads {
		s := h.Summary
		if len(h.Families) > 1 || s.Undecidable > 0 {
			bw.printf("#   %-6s %-14s %d replayed: %d match, %d diverge, %d unsupported, %d error, %d unconfigured; %d out of scope\n",
				h.Head, headLaneState(h), s.Total, s.Match, s.Diverge, s.Unsupported, s.Error, s.Unconfigured, s.OutOfScope)
			for _, f := range h.Families {
				bw.printf("#          %-14s %d replayed: %d match, %d diverge, %d undecidable, %d unsupported, %d error; %d %s compared\n",
					f.Kind, f.Total, f.Match, f.Diverge, f.Undecidable, f.Unsupported, f.Error, f.Compared, f.Unit)
			}
			continue
		}
		bw.printf("#   %-6s %-14s %d replayed: %d match, %d diverge, %d unsupported, %d error, %d unconfigured; %d %s compared, %d out of scope\n",
			h.Head, headLaneState(h), s.Total, s.Match, s.Diverge, s.Unsupported, s.Error, s.Unconfigured,
			s.ComparedUnits, headLaneUnit(h), s.OutOfScope)
	}
	bw.printf("\n")
}

// headLaneUnit names the unit a single-family head's evidence is counted in. A
// head that replayed nothing has no family and therefore no unit of its own; it
// reports zero, so the matrix lane's noun is used and the row claims nothing.
func headLaneUnit(h HeadSummary) string {
	if len(h.Families) == 1 {
		return h.Families[0].Unit
	}
	return UnitSeries
}

// headLaneState labels a lane row with what the operator supplied for it, so
// "0 replayed" is never ambiguous between a lane that was never configured and one
// that was configured and had nothing to run.
func headLaneState(h HeadSummary) string {
	if h.Configured {
		return "[configured]"
	}
	return "[no backends]"
}

// outOfScopeLane renders an out-of-scope entry's lane label. An unknown-language
// entry has no head, so it is labelled by language alone rather than printing an
// empty head slot.
func outOfScopeLane(e OutOfScopeEntry) string {
	if e.Head == "" {
		return "lang=" + e.Lang
	}
	return e.Head + "/" + e.Lang
}

// writeBugReport prints the "Report this to cerberus" section shown after a
// failing run: it frames a divergence as a possible cerberus bug, points at the
// issues tracker, prints the exact copy-pasteable command to regenerate the
// diagnostic (when the CLI supplied it), and asks the operator to attach the JSON.
func (r Report) writeBugReport(bw *errWriter, g *TextGuidance) {
	bw.printf("\n")
	bw.printf("== Report this to cerberus\n")
	bw.printf("   A divergence may indicate a cerberus bug. If the candidate causes above\n")
	bw.printf("   (especially experimental-CH-feature deviations) are ruled out, please\n")
	bw.printf("   open an issue so it can be fixed at the source:\n")
	bw.printf("     %s\n", IssuesURL)
	if g != nil && g.ReproCommand != "" {
		bw.printf("   Regenerate the full JSON diagnostic with this exact command:\n")
		bw.printf("     %s\n", g.ReproCommand)
		bw.printf("   Then attach the resulting verify-report.json to the issue.\n")
	} else {
		bw.printf("   Re-run with --report verify-report.json to capture the full JSON\n")
		bw.printf("   diagnostic, and attach it to the issue.\n")
	}
}

// errWriter collapses the repeated Fprintf error checks into a single
// short-circuiting sink: once a write fails, later printf calls are no-ops and
// the first error is returned.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
