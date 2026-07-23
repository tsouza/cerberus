// Package migrateverify is cerberus's cutover parity gate. It replays a
// harvested PromQL corpus against a reference Prometheus AND cerberus over one
// query_range window and diffs the results, so an operator can prove — before
// flipping a datasource — that cerberus returns the same numbers Prometheus
// does for the queries they actually run.
//
// The flow is read-only against both backends: for each query it issues an
// identical GET /api/v1/query_range to the reference and to cerberus, parses the
// standard Prometheus matrix response, matches series by their canonical label
// set, step-aligns the samples, and compares values within a tolerance (with
// NaN==NaN treated as equal). Every query lands in exactly one verdict — match,
// diverge, unsupported, or error — and a divergence is never allow-listed: the
// gate exits non-zero if any query diverges or errors.
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

// Query is one PromQL expression from the corpus to replay.
type Query struct {
	Expr   string `json:"expr"`
	Source string `json:"source"`
}

// OutOfScopeEntry records a corpus entry that is not PromQL (e.g. a LogQL
// dashboard panel). A Prometheus parity gate cannot judge it, so it is reported
// and counted here rather than dropped — its parity belongs to a different head's
// gate, and pretending otherwise would be a silent omission.
type OutOfScopeEntry struct {
	Source string `json:"source"`
	Lang   string `json:"lang"`
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

// Corpus is the verify input: the PromQL queries to replay, the non-PromQL
// entries carried through for honest accounting, and the harvest-time skips the
// corpus recorded (queries that never became replayable at all).
type Corpus struct {
	PromQL         []Query
	OutOfScope     []OutOfScopeEntry
	HarvestSkipped []HarvestSkippedEntry
}

// QueryResult is the parity verdict for a single replayed query. On a divergence
// it also carries Attribution: a list of CANDIDATE causes (never a detection —
// verify cannot introspect either backend) to steer triage.
type QueryResult struct {
	Source      string                 `json:"source"`
	Expr        string                 `json:"expr"`
	Verdict     Verdict                `json:"verdict"`
	FirstDiff   *FirstDiff             `json:"first_diff,omitempty"`
	Detail      string                 `json:"detail,omitempty"`
	Attribution []AttributionCandidate `json:"attribution,omitempty"`
}

// Summary counts verdicts across the whole run.
type Summary struct {
	Total          int `json:"total"`
	Match          int `json:"match"`
	Diverge        int `json:"diverge"`
	Unsupported    int `json:"unsupported"`
	Error          int `json:"error"`
	OutOfScope     int `json:"out_of_scope"`
	HarvestSkipped int `json:"harvest_skipped"`
}

// ReportParams records the comparison parameters the gate and humans need to
// judge how strict the parity check actually was — chiefly the tolerance, since
// a loosened tolerance silently weakens every "match" verdict. Recording it in
// the gate-consumed Report (not only the --report diagnostic) means a verify.json
// produced with a fat-fingered tolerance can no longer be blessed blind.
type ReportParams struct {
	Tolerance float64 `json:"tolerance"`
}

// Report is the full parity result: the resolved comparison params, per-query
// verdicts, the out-of-scope accounting, the harvest-time skips, and the roll-up
// summary.
type Report struct {
	Params         ReportParams          `json:"params"`
	Summary        Summary               `json:"summary"`
	Results        []QueryResult         `json:"results"`
	OutOfScope     []OutOfScopeEntry     `json:"out_of_scope,omitempty"`
	HarvestSkipped []HarvestSkippedEntry `json:"harvest_skipped,omitempty"`
}

// Failed reports whether the gate should exit non-zero: any diverging or erroring
// query fails it. Unsupported queries are reported but do not fail the gate — an
// unsupported query is a coverage gap surfaced for the operator, not a wrong
// answer. Out-of-scope and harvest-skipped entries never affect the gate: they
// carry no replayable query, so there is nothing for the comparator to judge.
func (r Report) Failed() bool {
	return r.Summary.Diverge > 0 || r.Summary.Error > 0
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

// Verify replays every PromQL query in the corpus against both backends and
// assembles the parity report. Queries are processed in corpus order for
// deterministic output. Each query is issued to the reference and to cerberus
// with identical parameters; the verdict is derived as:
//
//   - transport/decode failure on either backend, a reference that did not
//     return a 200 matrix, or a cerberus 5xx / other non-200-non-4xx status (a
//     half-broken backend) → error (nothing to compare, or the backend is broken);
//   - cerberus 4xx, or a 200 non-matrix body → unsupported (answered, but could
//     not serve the query as a range);
//   - otherwise → the comparator's match/diverge verdict.
func Verify(ctx context.Context, corpus Corpus, ref, cerberus Backend, p Params) Report {
	rep := Report{
		Params:         ReportParams{Tolerance: p.Tolerance},
		OutOfScope:     corpus.OutOfScope,
		HarvestSkipped: corpus.HarvestSkipped,
	}
	rep.Summary.OutOfScope = len(corpus.OutOfScope)
	rep.Summary.HarvestSkipped = len(corpus.HarvestSkipped)

	for _, q := range corpus.PromQL {
		res := verifyOne(ctx, q, ref, cerberus, p)
		rep.Results = append(rep.Results, res)
		rep.Summary.Total++
		switch res.Verdict {
		case VerdictMatch:
			rep.Summary.Match++
		case VerdictDiverge:
			rep.Summary.Diverge++
		case VerdictUnsupported:
			rep.Summary.Unsupported++
		case VerdictError:
			rep.Summary.Error++
		}
	}
	return rep
}

// verifyOne runs the parity check for a single query.
func verifyOne(ctx context.Context, q Query, ref, cerberus Backend, p Params) QueryResult {
	out := QueryResult{Source: q.Source, Expr: q.Expr}

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
		if verdict == VerdictDiverge {
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

// WriteText renders the human report with no CLI-derived bug-report guidance. It
// is the entrypoint for callers (and tests) that only have the Report in hand.
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
		bw.printf("VERIFICATION FAILED — %d diverged, %d errored, %d matched (of %d)\n\n",
			r.Summary.Diverge, r.Summary.Error, r.Summary.Match, r.Summary.Total)
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
	bw.printf("# Parity gate: each corpus query replayed against the reference Prometheus\n")
	bw.printf("# and cerberus over one query_range window, results diffed series-by-series.\n")
	bw.printf("# A divergence is never allow-listed — the gate fails if any query diverges\n")
	bw.printf("# or errors.\n")
	bw.printf("#\n")
	bw.printf("# Note: %s\n", ExperimentalNote)
	bw.printf("#\n")
	// Surface the tolerance the matches were judged at: a loosened tolerance
	// silently weakens every "match", so the operator must see how strict the
	// comparison actually was.
	bw.printf("# Match tolerance: %s (absolute; relative granularity also applied at large magnitudes)\n", formatValue(r.Params.Tolerance))
	bw.printf("#\n")
	bw.printf("# %d queries: %d match, %d diverge, %d unsupported, %d error (+%d out of scope, +%d harvest-skipped)\n\n",
		r.Summary.Total, r.Summary.Match, r.Summary.Diverge, r.Summary.Unsupported, r.Summary.Error, r.Summary.OutOfScope, r.Summary.HarvestSkipped)

	for _, res := range r.Results {
		if res.Verdict == VerdictMatch {
			continue
		}
		bw.printf("== [%s] %s\n", res.Verdict, res.Source)
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

	if len(r.OutOfScope) > 0 {
		bw.printf("== out of scope (%d) — not PromQL, no Prometheus baseline\n", len(r.OutOfScope))
		for _, e := range r.OutOfScope {
			bw.printf("   %s: lang=%s\n", e.Source, e.Lang)
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
		bw.printf("FAIL: %d diverge, %d error\n", r.Summary.Diverge, r.Summary.Error)
		r.writeBugReport(bw, g)
	} else {
		bw.printf("PASS: %d match, %d unsupported (no divergence)\n", r.Summary.Match, r.Summary.Unsupported)
	}
	return bw.err
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
