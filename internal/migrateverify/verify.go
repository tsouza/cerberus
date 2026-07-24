// Package migrateverify is cerberus's cutover parity gate. It replays a
// harvested corpus against the reference backend for each head — Prometheus,
// Loki, Tempo — AND against cerberus over one query_range window and diffs the
// results, so an operator can prove, before flipping a datasource, that cerberus
// returns the same numbers their current stack does for the queries they actually
// run.
//
// The gate covers the METRIC lane of each head: PromQL, LogQL metric queries, and
// TraceQL metrics queries all return matrix-shaped results, so one comparator
// judges all three. A query whose shape is not a metric matrix — a LogQL log
// stream, a TraceQL trace search, a compare() — has no matrix baseline to diff
// and is reported out of scope with the specific reason it was not judged, never
// dropped and never guessed at.
//
// The flow is read-only against every backend: for each query it issues an
// identical range request to that head's reference and to cerberus, decodes the
// response into the shared matrix shape, matches series by their canonical label
// set, step-aligns the samples, and compares values within a tolerance (with
// NaN==NaN treated as equal). Every replayed query lands in exactly one verdict —
// match, diverge, unsupported, or error — and a divergence is never allow-listed:
// the gate exits non-zero if any query diverges or errors, or if a head with
// replayable queries had no backend pair configured to judge them.
//
// Honesty is the whole point: the comparator only claims a match where both
// backends returned data that agrees. A series present in one backend but not
// the other is itself a divergence (reported with its first differing point),
// not a silent omission.
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
type FirstDiff struct {
	Series        string  `json:"series"`
	Timestamp     float64 `json:"timestamp"`
	RefValue      string  `json:"ref_value"`
	CerberusValue string  `json:"cerberus_value"`
	Reason        string  `json:"reason"`
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
	fd := &FirstDiff{Series: key, Reason: reason}
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
			return &FirstDiff{Series: key, Timestamp: ts, RefValue: formatValue(rv), CerberusValue: "<no sample>", Reason: "cerberus has no sample at this step"}
		case !rok:
			return &FirstDiff{Series: key, Timestamp: ts, RefValue: "<no sample>", CerberusValue: formatValue(cv), Reason: "reference has no sample at this step"}
		case !valuesEqual(rv, cv, tol):
			return &FirstDiff{Series: key, Timestamp: ts, RefValue: formatValue(rv), CerberusValue: formatValue(cv), Reason: "value differs beyond tolerance"}
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

// Query is one corpus expression to replay, tagged with the head lane that owns
// it and the language it came from.
type Query struct {
	Expr   string `json:"expr"`
	Source string `json:"source"`
	Head   string `json:"head"`
	Lang   string `json:"lang"`
}

// OutOfScopeEntry records a corpus entry no metric-lane parity check can judge —
// a LogQL log-stream query, a TraceQL trace search, a compare(), an expression
// the parser rejected, or a language this build has no lane for. Kind names the
// query SHAPE and Reason states, in the operator's words, exactly why the gate
// did not judge it. Reported and counted here, never dropped: pretending a query
// was covered when it was not is the failure this accounting exists to prevent.
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
type QueryResult struct {
	Head        string                 `json:"head"`
	Source      string                 `json:"source"`
	Expr        string                 `json:"expr"`
	Verdict     Verdict                `json:"verdict"`
	FirstDiff   *FirstDiff             `json:"first_diff,omitempty"`
	Detail      string                 `json:"detail,omitempty"`
	Attribution []AttributionCandidate `json:"attribution,omitempty"`
}

// Summary counts verdicts. Total counts REPLAYED queries only: Unconfigured
// entries were never issued to any backend, so folding them into Total would
// inflate the denominator of a claim the gate did not make.
type Summary struct {
	Total          int `json:"total"`
	Match          int `json:"match"`
	Diverge        int `json:"diverge"`
	Unsupported    int `json:"unsupported"`
	Error          int `json:"error"`
	Unconfigured   int `json:"unconfigured"`
	OutOfScope     int `json:"out_of_scope"`
	HarvestSkipped int `json:"harvest_skipped"`
}

// HeadSummary is one lane's counts, carried alongside the roll-up so a healthy
// head can never mask a dead one: an aggregate "40 matched" reads green even when
// a second lane compared nothing at all.
type HeadSummary struct {
	Head    string  `json:"head"`
	Summary Summary `json:"summary"`
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
// Version 2 carries the per-head lane split (Heads) and the Unconfigured bucket.
// A version-1 artifact decoded by this build would zero-fill both into a bogus
// "0 non-Prometheus queries, all lanes configured" — precisely the silent
// zero-fill the version check exists to stop — so the bump is enforcing, not
// cosmetic.
const ReportVersion = 2

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

// Failed reports whether the gate should exit non-zero: any diverging or erroring
// query, or any replayable query whose head lane was never configured — the gate
// cannot claim parity for a query it never ran, and reporting that as a
// non-blocking caveat would let "VERIFICATION PASSED" ship on zero evidence for a
// whole head. Unsupported, out-of-scope and harvest-skipped entries are reported
// but do not fail: each is a surfaced coverage gap, not a wrong answer.
func (r Report) Failed() bool {
	return r.Summary.Diverge > 0 || r.Summary.Error > 0 || r.Summary.Unconfigured > 0
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
type Lane struct {
	Ref      Backend
	Cerberus Backend
}

// Verify replays every corpus query against the lane its head names and assembles
// one report covering all heads. Queries are processed in corpus order for
// deterministic output. Each query is issued to that head's reference and to
// cerberus with identical parameters; the verdict is derived as:
//
//   - transport/decode failure on either backend, a reference that did not
//     return a 200 matrix, or a cerberus 5xx / other non-200-non-4xx status (a
//     half-broken backend) → error (nothing to compare, or the backend is broken);
//   - cerberus 4xx, or a 200 non-matrix body → unsupported (answered, but could
//     not serve the query as a range);
//   - otherwise → the comparator's match/diverge verdict.
//
// A query whose head has no configured lane is recorded as UNCONFIGURED: never
// replayed against another head's backend (which would compare two unrelated
// APIs), never silently dropped, and blocking — see Report.Failed.
func Verify(ctx context.Context, corpus Corpus, lanes map[string]Lane, p Params) Report {
	rep := Report{
		SchemaVersion:  ReportVersion,
		Params:         ReportParams{Tolerance: p.Tolerance},
		OutOfScope:     corpus.OutOfScope,
		HarvestSkipped: corpus.HarvestSkipped,
	}
	rep.Summary.OutOfScope = len(corpus.OutOfScope)
	rep.Summary.HarvestSkipped = len(corpus.HarvestSkipped)

	byHead := map[string]*Summary{}
	headSummary := func(head string) *Summary {
		s, ok := byHead[head]
		if !ok {
			s = &Summary{}
			byHead[head] = s
		}
		return s
	}

	for _, q := range corpus.Queries {
		hs := headSummary(q.Head)
		lane, ok := lanes[q.Head]
		if !ok {
			rep.Unconfigured = append(rep.Unconfigured, UnconfiguredEntry{
				Source: q.Source, Expr: q.Expr, Head: q.Head, Lang: q.Lang,
				Reason: fmt.Sprintf(
					"head=%s has replayable %s queries but no reference/cerberus backend pair was configured for it; the gate did not judge them",
					q.Head, q.Lang,
				),
			})
			rep.Summary.Unconfigured++
			hs.Unconfigured++
			continue
		}
		res := verifyOne(ctx, q, lane.Ref, lane.Cerberus, p)
		rep.Results = append(rep.Results, res)
		rep.Summary.Total++
		hs.Total++
		switch res.Verdict {
		case VerdictMatch:
			rep.Summary.Match++
			hs.Match++
		case VerdictDiverge:
			rep.Summary.Diverge++
			hs.Diverge++
		case VerdictUnsupported:
			rep.Summary.Unsupported++
			hs.Unsupported++
		case VerdictError:
			rep.Summary.Error++
			hs.Error++
		}
	}
	rep.Heads = sortedHeadSummaries(byHead)
	return rep
}

// sortedHeadSummaries flattens the per-head counters into a head-token-sorted
// slice so the JSON report is byte-deterministic across runs (Go map iteration
// order is not).
func sortedHeadSummaries(byHead map[string]*Summary) []HeadSummary {
	heads := make([]string, 0, len(byHead))
	for h := range byHead {
		heads = append(heads, h)
	}
	sort.Strings(heads)
	out := make([]HeadSummary, 0, len(heads))
	for _, h := range heads {
		out = append(out, HeadSummary{Head: h, Summary: *byHead[h]})
	}
	return out
}

// verifyOne runs the parity check for a single query on its head's lane.
func verifyOne(ctx context.Context, q Query, ref, cerberus Backend, p Params) QueryResult {
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
		// with no visible cause.
		bw.printf("VERIFICATION FAILED — %d diverged, %d errored, %d unjudged (unconfigured lane), %d matched (of %d replayed)\n\n",
			r.Summary.Diverge, r.Summary.Error, r.Summary.Unconfigured, r.Summary.Match, r.Summary.Total)
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
	bw.printf("# Parity gate: each corpus query replayed against its head's reference backend\n")
	bw.printf("# and cerberus over one query_range window, results diffed series-by-series.\n")
	bw.printf("# A divergence is never allow-listed — the gate fails if any query diverges\n")
	bw.printf("# or errors, or if a head's replayable queries had no backend pair to judge them.\n")
	bw.printf("#\n")
	bw.printf("# Note: %s\n", ExperimentalNote)
	bw.printf("#\n")
	// Surface the tolerance the matches were judged at: a loosened tolerance
	// silently weakens every "match", so the operator must see how strict the
	// comparison actually was.
	bw.printf("# Match tolerance: %s (absolute; relative granularity also applied at large magnitudes)\n", formatValue(r.Params.Tolerance))
	bw.printf("#\n")
	bw.printf("# %d queries: %d match, %d diverge, %d unsupported, %d error (+%d unconfigured, +%d out of scope, +%d harvest-skipped)\n\n",
		r.Summary.Total, r.Summary.Match, r.Summary.Diverge, r.Summary.Unsupported, r.Summary.Error,
		r.Summary.Unconfigured, r.Summary.OutOfScope, r.Summary.HarvestSkipped)

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
			fd := res.FirstDiff
			bw.printf("   first-diff: series=%s ts=%s ref=%s cerberus=%s (%s)\n",
				fd.Series, formatValue(fd.Timestamp), fd.RefValue, fd.CerberusValue, fd.Reason)
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

	if len(r.OutOfScope) > 0 {
		bw.printf("== out of scope (%d) — no metric-lane parity is definable for these queries\n", len(r.OutOfScope))
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
		r.writeBugReport(bw, g)
	} else {
		bw.printf("PASS: %d match, %d unsupported (no divergence)\n", r.Summary.Match, r.Summary.Unsupported)
	}
	return bw.err
}

// writeHeadTable prints the per-lane roll-up in head-token order. It exists so a
// healthy head cannot mask a dead one: the aggregate line above can read
// "40 match" while a second lane compared nothing, and only the per-head split
// makes that visible.
func (r Report) writeHeadTable(bw *errWriter) {
	if len(r.Heads) == 0 {
		return
	}
	bw.printf("# per-head lanes:\n")
	for _, h := range r.Heads {
		s := h.Summary
		bw.printf("#   %-6s %d replayed: %d match, %d diverge, %d unsupported, %d error, %d unconfigured\n",
			h.Head, s.Total, s.Match, s.Diverge, s.Unsupported, s.Error, s.Unconfigured)
	}
	bw.printf("\n")
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
