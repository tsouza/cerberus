package property

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Dataset is the random data shape every property iteration starts with.
//
// The DDL is a multi-statement script (CREATE TABLE + INSERTs) the chDB
// helpers will replay against an ephemeral session. The Metrics mirror is
// the same data in the in-memory shape the oracle reads — keeping both
// in sync is the generator's responsibility.
type Dataset struct {
	// DDL is the multi-statement seed: `CREATE OR REPLACE TABLE …;
	// INSERT … VALUES …;`. The runner splits on top-level semicolons
	// before exec'ing.
	DDL string
	// Metrics is the in-memory mirror of the dataset, in the shape the
	// oracle understands. The generator owns the invariant
	// `Metrics ⇔ DDL`.
	Metrics *MetricsModel
}

// MetricsModel is the in-memory metrics mirror. It's intentionally tiny
// — the generator and the oracle both consume it directly, so there's
// no point in mirroring the full OTel CH schema.
type MetricsModel struct {
	Series []SeriesData
}

// SeriesData is one time series in the dataset.
type SeriesData struct {
	MetricName string
	// Labels are user-defined dimensions (job, instance, …). The
	// __name__ label is implied by MetricName and never appears here.
	Labels map[string]string
	Points []Point
}

// Point is one (timestamp, value) sample in a SeriesData.
type Point struct {
	// TimestampMs is unix milliseconds, matching Prometheus's internal
	// convention so the oracle's storage layer can consume it directly.
	TimestampMs int64
	Value       float64
}

// NamesPresent returns the distinct metric names in the dataset, sorted
// for determinism. The PromQL generator uses this so every generated
// query targets a metric the dataset actually carries.
func (m *MetricsModel) NamesPresent() []string {
	if m == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, s := range m.Series {
		seen[s.MetricName] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LabelsPresentFor returns the union of label sets for every series
// matching name. Used by the query generator to bound matcher choices.
func (m *MetricsModel) LabelsPresentFor(name string) map[string][]string {
	if m == nil {
		return nil
	}
	out := map[string]map[string]struct{}{}
	for _, s := range m.Series {
		if s.MetricName != name {
			continue
		}
		for k, v := range s.Labels {
			if _, ok := out[k]; !ok {
				out[k] = map[string]struct{}{}
			}
			out[k][v] = struct{}{}
		}
	}
	result := make(map[string][]string, len(out))
	for k, vs := range out {
		values := make([]string, 0, len(vs))
		for v := range vs {
			values = append(values, v)
		}
		sort.Strings(values)
		result[k] = values
	}
	return result
}

// Query is one randomly generated query. The framework keeps both the
// string (the form cerberus's HTTP entry point accepts) and the AST
// (the form the oracle and any debug logging consumes). The generator
// produces the AST first, then pretty-prints it via expr.String(); the
// two are guaranteed in lock-step by construction.
type Query struct {
	// String is the AST pretty-printed by parser.Expr.String(). Cerberus
	// re-parses it before lowering. The oracle's bridge re-parses it
	// inside Prometheus's engine as well.
	String string
	// EvalTs is the instant the query should be evaluated at, in unix
	// seconds. The generator chooses a timestamp from the dataset's
	// active window so the query has at least one matching sample to
	// see. Range queries are out of scope for PR 1 (instant only).
	EvalTs int64
}

// Outcome is the structured result of an oracle or cerberus invocation
// for one query. Empty Rows + nil Err means "no series matched" — a
// valid outcome both sides have to agree on.
type Outcome struct {
	// Rows is the result reshaped into shadow.VectorResult-friendly
	// form via the framework's CompareOutcomes helper. The generator
	// for PR 1 only produces instant queries, so each row is one
	// (label set, value) pair at EvalTs.
	Rows []OutcomeRow
	// Err is the error the side returned, if any. Both sides must
	// agree on err-vs-rows; a mismatch (e.g. oracle errs but cerberus
	// returns rows) is a real failure to report.
	Err error
}

// OutcomeRow is one labeled sample in an Outcome. Timestamp is unix
// milliseconds (matching Prom).
type OutcomeRow struct {
	Labels      map[string]string
	TimestampMs int64
	Value       float64
}

// DatasetGen produces a random Dataset. Implementations should use
// rapid's Draw primitives so shrinking can minimise the dataset on
// failure.
type DatasetGen func(t *rapid.T) Dataset

// QueryGen produces a random Query targeting d. The generator's
// accept-set must match the oracle's accept-set — generating a query
// the oracle can't evaluate is a generator bug, not a cerberus bug.
type QueryGen func(t *rapid.T, d Dataset) Query

// OracleFn evaluates q against d using the independent specification.
// In Phase 1 PR 1 this is bridged to promshim/local; PR 2 replaces it
// with a from-scratch evaluator.
type OracleFn func(d Dataset, q Query) Outcome

// CerberusFn runs the cerberus pipeline against the dataset (seeded
// into chDB on the caller side) and returns the same-shaped Outcome.
// Tests pass a closure that owns the chclienttest.Client + the mounted
// httptest.Server lifecycle.
type CerberusFn func(d Dataset, q Query) Outcome

// Config is a forward-looking knob bag. Today it carries no fields
// that the framework reads — rapid's per-test iteration count is
// controlled via the `-rapid.checks=N` CLI flag (default 100), so a
// developer chasing a flake or running an overnight sweep crank N up
// without touching the runner. The type stays exported so future
// fields (e.g., per-runner timeout, generator-specific knobs) can land
// without breaking the Run signature.
type Config struct{}

// Run is the top-level property runner. It walks the rapid.Check loop:
// each iteration draws a dataset and a query, evaluates both sides,
// and compares — on drift, calls t.Fatalf with enough context to
// reproduce. Shrinking is implicit (rapid will minimise the failing
// generators before this function returns).
//
// The caller is responsible for closing over chDB / handler lifetime
// inside `ch` — the runner has no chDB knowledge of its own. This
// keeps the package free of chdb tags except in chdb.go.
//
// Iteration count is controlled by `-rapid.checks=N` (default 100).
// CI inherits the default; local debug runs can pass
// `-rapid.checks=1000` for a deeper sweep.
func Run(
	t *testing.T,
	_ Config,
	dgen DatasetGen,
	qgen QueryGen,
	oracle OracleFn,
	ch CerberusFn,
) {
	t.Helper()

	rapid.Check(t, func(rt *rapid.T) {
		ds := dgen(rt)
		if len(ds.Metrics.Series) == 0 || len(ds.Metrics.NamesPresent()) == 0 {
			// Generator produced an empty dataset — skip this draw.
			// rapid treats Skipf as "this case isn't interesting,
			// don't count it against the budget".
			rt.Skipf("empty dataset")
		}
		q := qgen(rt, ds)
		if q.String == "" {
			rt.Skipf("empty query")
		}

		oracleOut := oracle(ds, q)
		cerberusOut := ch(ds, q)

		if diff := CompareOutcomes(oracleOut, cerberusOut); diff != "" {
			rt.Fatalf("property drift\n--- query ---\n%s\nevalTs=%d\n--- dataset ---\n%s\n--- diff ---\n%s",
				q.String, q.EvalTs, dumpDataset(ds), diff)
		}
	})
}

// CompareOutcomes returns "" when both sides agree and a multiline
// diff string otherwise. The shape mirrors what shadow.Compare emits
// but is local to this package so the property test can render a
// failure without dragging shadow's VectorResult shape into the test
// code.
//
// Comparison is multiset-aware: row order doesn't matter, but every
// row on one side must have a same-(labels, ts, value) row on the
// other. Numeric tolerance follows shadow's defaults
// (abs=1e-9, rel=1e-9) so floating-point noise from a different
// evaluation order doesn't flag.
func CompareOutcomes(want, got Outcome) string {
	if (want.Err == nil) != (got.Err == nil) {
		return fmt.Sprintf("error mismatch: want_err=%v got_err=%v", want.Err, got.Err)
	}
	if want.Err != nil && got.Err != nil {
		// Both errored — we don't try to match error messages
		// byte-for-byte; the cerberus errors get wrapped through
		// classify*, and the oracle errors come from Prometheus's
		// internals. Treat any same-side error as "both refused"
		// and pass.
		return ""
	}

	wantIdx := indexOutcomeRows(want.Rows)
	gotIdx := indexOutcomeRows(got.Rows)

	var diff strings.Builder
	for key, ws := range wantIdx {
		gs, ok := gotIdx[key]
		if !ok {
			fmt.Fprintf(&diff, "missing series in got: %s\n", key)
			continue
		}
		if len(ws) != len(gs) {
			fmt.Fprintf(&diff, "series %s: sample count want=%d got=%d\n",
				key, len(ws), len(gs))
			continue
		}
		// Each series's sample list was sorted by indexOutcomeRows.
		for i := range ws {
			if ws[i].TimestampMs != gs[i].TimestampMs {
				fmt.Fprintf(&diff, "series %s: ts[%d] want=%d got=%d\n",
					key, i, ws[i].TimestampMs, gs[i].TimestampMs)
				continue
			}
			if !valuesClose(ws[i].Value, gs[i].Value) {
				fmt.Fprintf(&diff, "series %s: value[%d] @ts=%d want=%g got=%g\n",
					key, i, ws[i].TimestampMs, ws[i].Value, gs[i].Value)
			}
		}
	}
	for key := range gotIdx {
		if _, ok := wantIdx[key]; !ok {
			fmt.Fprintf(&diff, "extra series in got: %s\n", key)
		}
	}

	return diff.String()
}

func indexOutcomeRows(rows []OutcomeRow) map[string][]OutcomeRow {
	out := map[string][]OutcomeRow{}
	for _, r := range rows {
		key := labelKey(r.Labels)
		out[key] = append(out[key], r)
	}
	for _, samples := range out {
		sort.Slice(samples, func(i, j int) bool {
			return samples[i].TimestampMs < samples[j].TimestampMs
		})
	}
	return out
}

// labelKey is the stable string-form of a label set. Lifted in spirit
// from shadow/differ.go's labelKey so the comparator emits the same
// "{job=\"api\",instance=\"a\"}" notation a Prom user would recognise.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
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
		b.WriteString("=\"")
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// valuesClose returns whether two float64 values match within the
// shadow harness's tolerances. The bridge oracle and cerberus take
// different paths through float arithmetic; a strict == would flake
// on small rounding noise.
func valuesClose(a, b float64) bool {
	const (
		absEpsilon = 1e-9
		relEpsilon = 1e-9
	)
	// IsNaN handling: PromQL gives NaN for division-by-zero and a few
	// other arithmetic shapes. Two NaNs are equal for our purposes.
	if a != a && b != b { // both NaN
		return true
	}
	delta := a - b
	if delta < 0 {
		delta = -delta
	}
	if delta <= absEpsilon {
		return true
	}
	scale := a
	if scale < 0 {
		scale = -scale
	}
	if b > scale || -b > scale {
		scale = b
		if scale < 0 {
			scale = -scale
		}
	}
	return delta <= relEpsilon*scale
}

// dumpDataset renders the dataset for a failure log. Compact enough
// for a single test failure to be greppable; verbose enough that the
// reader can reconstruct what the generator produced.
func dumpDataset(d Dataset) string {
	var b strings.Builder
	if d.Metrics == nil {
		return "(nil metrics)"
	}
	fmt.Fprintf(&b, "series=%d\n", len(d.Metrics.Series))
	for _, s := range d.Metrics.Series {
		fmt.Fprintf(&b, "  %s%s points=%d\n", s.MetricName, labelKey(s.Labels), len(s.Points))
	}
	return b.String()
}
