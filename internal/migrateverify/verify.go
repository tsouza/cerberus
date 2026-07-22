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
	"sort"
	"strconv"
	"strings"
)

// DefaultTolerance is the absolute epsilon two sample values may differ by and
// still count as equal. It is deliberately tiny — parity means the same number,
// and this only absorbs float round-trips through JSON string encoding, not real
// numeric drift.
const DefaultTolerance = 1e-9

// resultTypeMatrix is the only Prometheus resultType a range query can return;
// anything else from cerberus means it could not serve the query as a range.
const resultTypeMatrix = "matrix"

// Verdict is the classification of a single query's parity check.
type Verdict string

const (
	// VerdictMatch: both backends returned matrix data that agrees within tolerance.
	VerdictMatch Verdict = "match"
	// VerdictDiverge: both backends returned matrix data, but the results differ.
	VerdictDiverge Verdict = "diverge"
	// VerdictUnsupported: cerberus returned a non-200 status or a non-matrix
	// result — it could not serve the query as a range at all.
	VerdictUnsupported Verdict = "unsupported"
	// VerdictError: the reference failed, or a transport/parse error prevented a
	// comparison. Distinct from unsupported: the fault is not cerberus rejecting
	// the query, so there is nothing to compare.
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

// valuesEqual reports whether two sample values agree within tol, treating two
// NaNs as equal (both backends declaring "no value here" is agreement, not a
// divergence).
func valuesEqual(a, b, tol float64) bool {
	aNaN, bNaN := math.IsNaN(a), math.IsNaN(b)
	if aNaN || bNaN {
		return aNaN && bNaN
	}
	return math.Abs(a-b) <= tol
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

// Corpus is the verify input: the PromQL queries to replay plus the non-PromQL
// entries carried through for honest accounting.
type Corpus struct {
	PromQL     []Query
	OutOfScope []OutOfScopeEntry
}

// QueryResult is the parity verdict for a single replayed query.
type QueryResult struct {
	Source    string     `json:"source"`
	Expr      string     `json:"expr"`
	Verdict   Verdict    `json:"verdict"`
	FirstDiff *FirstDiff `json:"first_diff,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

// Summary counts verdicts across the whole run.
type Summary struct {
	Total       int `json:"total"`
	Match       int `json:"match"`
	Diverge     int `json:"diverge"`
	Unsupported int `json:"unsupported"`
	Error       int `json:"error"`
	OutOfScope  int `json:"out_of_scope"`
}

// Report is the full parity result: per-query verdicts, the out-of-scope
// accounting, and the roll-up summary.
type Report struct {
	Summary    Summary           `json:"summary"`
	Results    []QueryResult     `json:"results"`
	OutOfScope []OutOfScopeEntry `json:"out_of_scope,omitempty"`
}

// Failed reports whether the gate should exit non-zero: any diverging or erroring
// query fails it. Unsupported queries are reported but do not fail the gate — an
// unsupported query is a coverage gap surfaced for the operator, not a wrong
// answer. Out-of-scope entries never affect the gate.
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
//   - transport/decode failure on either backend, or a reference that did not
//     return a 200 matrix → error (nothing to compare against);
//   - cerberus non-200 or non-matrix → unsupported;
//   - otherwise → the comparator's match/diverge verdict.
func Verify(ctx context.Context, corpus Corpus, ref, cerberus Backend, p Params) Report {
	rep := Report{OutOfScope: corpus.OutOfScope}
	rep.Summary.OutOfScope = len(corpus.OutOfScope)

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
	case cerRes.Status != 200 || cerRes.ResultType != resultTypeMatrix:
		out.Verdict = VerdictUnsupported
		out.Detail = fmt.Sprintf("cerberus returned status=%d resultType=%q", cerRes.Status, cerRes.ResultType)
	case refRes.Status != 200 || refRes.ResultType != resultTypeMatrix:
		out.Verdict = VerdictError
		out.Detail = fmt.Sprintf("reference returned status=%d resultType=%q (no baseline to compare)", refRes.Status, refRes.ResultType)
	default:
		verdict, fd := Compare(refRes.Series, cerRes.Series, p.Tolerance)
		out.Verdict, out.FirstDiff = verdict, fd
	}
	return out
}

// WriteText renders the report as a scannable, human-readable gate report: a
// header, one block per non-matching query (matches are summarised, not listed,
// so a diverging result is not buried), the out-of-scope accounting, and the
// roll-up counts. It ends with an explicit PASS / FAIL line.
func (r Report) WriteText(w io.Writer) error {
	bw := &errWriter{w: w}
	bw.printf("# cerberus migrate verify\n")
	bw.printf("#\n")
	bw.printf("# Parity gate: each corpus query replayed against the reference Prometheus\n")
	bw.printf("# and cerberus over one query_range window, results diffed series-by-series.\n")
	bw.printf("# A divergence is never allow-listed — the gate fails if any query diverges\n")
	bw.printf("# or errors.\n")
	bw.printf("#\n")
	bw.printf("# %d queries: %d match, %d diverge, %d unsupported, %d error (+%d out of scope)\n\n",
		r.Summary.Total, r.Summary.Match, r.Summary.Diverge, r.Summary.Unsupported, r.Summary.Error, r.Summary.OutOfScope)

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
		bw.printf("\n")
	}

	if len(r.OutOfScope) > 0 {
		bw.printf("== out of scope (%d) — not PromQL, no Prometheus baseline\n", len(r.OutOfScope))
		for _, e := range r.OutOfScope {
			bw.printf("   %s: lang=%s\n", e.Source, e.Lang)
		}
		bw.printf("\n")
	}

	if r.Failed() {
		bw.printf("FAIL: %d diverge, %d error\n", r.Summary.Diverge, r.Summary.Error)
	} else {
		bw.printf("PASS: %d match, %d unsupported (no divergence)\n", r.Summary.Match, r.Summary.Unsupported)
	}
	return bw.err
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
