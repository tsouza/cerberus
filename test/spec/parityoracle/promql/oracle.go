// Package promql evaluates a fixture's query on the REAL upstream
// Prometheus engine, so a TXTAR fixture's answer can be checked against
// something other than cerberus's own output.
//
// # The one rule this package exists to enforce
//
// It imports NOTHING from internal/{promql,logql,traceql,chplan,chsql,
// optimizer}. That is not a style preference — it is the entire point. An
// oracle that shares code with the system under test cannot disagree with
// it about the thing they share, so any such import silently converts this
// package from an oracle into a mirror. test/regression's parity contract
// test enforces the rule mechanically with `go list -deps -test`, because
// a rule this easy to break by accident and this invisible when broken
// cannot be left to review.
//
// The same reasoning rules out reusing compatibility/prometheus/cmd/seed's
// readFixtureSeries, which is otherwise a perfectly good CH-rows-to-series
// translator: it imports internal/promql and internal/schema. Being equal
// by construction is exactly right for the compat MIRROR (whose job is to
// put identical data in both backends) and disqualifying here.
//
// # Why the input is rows, not a seed
//
// Evaluate takes rows already read back out of the seeded chDB session
// rather than the fixture's `-- seed --` SQL. Two reasons. It keeps every
// chDB and engine-serialisation concern in package spec, where the session
// mutex lives. And it means the oracle sees the data as it ACTUALLY LANDED
// in ClickHouse — after DEFAULTs, after type coercion — rather than as the
// seed text claims it will land, which is the difference between checking
// cerberus against Prometheus and checking two readings of a SQL string.
//
// # Why not the promqltest load DSL
//
// promqltest's `load <step>` command lays samples on a FIXED step from a
// fixed start, and the corpus is not on a fixed step: binop_rate_plus_rate
// has a 29-second inter-sample gap, which is precisely the irregular
// cadence that surfaced the boundary-extrapolation bug this mechanism is
// meant to catch. Samples are therefore appended directly at their exact
// millisecond timestamps.
package promql

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/util/teststorage"
)

// lookbackDelta is Prometheus's own default staleness lookback, and the
// window cerberus's instant-selector lowering applies. The two must agree
// or every sparse-seed fixture diverges for a reason that has nothing to
// do with the query under test.
const lookbackDelta = 5 * time.Minute

// maxSamples bounds one evaluation. It is deliberately far above any
// fixture's working set — the corpus seeds a handful of samples per series
// — so that a fixture can never pass or fail because of the limit rather
// than because of the query.
const maxSamples = 1_000_000

// Series is one input time series: an identifying label set plus its
// samples, exactly as they were read back out of ClickHouse.
type Series struct {
	// Labels is the series' full label set INCLUDING __name__.
	Labels map[string]string

	// Points are the samples, which need not be evenly spaced.
	Points []Point
}

// Point is one sample at an exact millisecond timestamp.
type Point struct {
	// TMillis is the sample time in Unix milliseconds — Prometheus's
	// native resolution.
	TMillis int64

	// Value is the sample value.
	Value float64
}

// Result is one output sample from the reference engine, flattened so the
// caller can compare without depending on Prometheus's own types.
type Result struct {
	// Labels is the output series' label set.
	Labels map[string]string

	// TMillis is the output timestamp in Unix milliseconds.
	TMillis int64

	// Value is the output value.
	Value float64
}

// Query describes one evaluation to run.
type Query struct {
	// Expr is the PromQL expression, verbatim from the fixture.
	Expr string

	// Start is the instant for an instant query, or the range start.
	Start time.Time

	// End equals Start for an instant query.
	End time.Time

	// Step is zero for an instant query, and the range step otherwise.
	Step time.Duration
}

// IsRange reports whether this is a range query.
func (q Query) IsRange() bool { return q.Step > 0 }

// Evaluate runs q against the real Prometheus engine over series, and
// returns the resulting samples sorted deterministically.
//
// It returns an error rather than failing the test so the caller can
// attribute a failure precisely: a Prometheus PARSE error on a fixture's
// query usually means the fixture exercises a cerberus extension that
// upstream does not accept, which is a fact about the fixture and not a
// parity failure.
func Evaluate(tb testing.TB, series []Series, q Query) ([]Result, error) {
	tb.Helper()

	storage := teststorage.New(tb)
	tb.Cleanup(func() { _ = storage.Close() })

	if err := appendSeries(storage, series); err != nil {
		return nil, err
	}

	engine := promqltest.NewTestEngine(tb, false, lookbackDelta, maxSamples)
	ctx := context.Background()

	var query promql.Query
	var err error
	if q.IsRange() {
		query, err = engine.NewRangeQuery(ctx, storage, nil, q.Expr, q.Start, q.End, q.Step)
	} else {
		query, err = engine.NewInstantQuery(ctx, storage, nil, q.Expr, q.Start)
	}
	if err != nil {
		return nil, fmt.Errorf("reference engine rejected the query: %w", err)
	}
	defer query.Close()

	res := query.Exec(ctx)
	if res.Err != nil {
		return nil, fmt.Errorf("reference engine evaluation failed: %w", res.Err)
	}
	return flatten(res.Value)
}

// appendSeries writes every sample into the reference storage at its exact
// timestamp.
//
// # Why the samples are flattened and globally time-sorted first
//
// Prometheus's TSDB head rejects a sample older than
// `head.MaxTime() - chunkRange/2` with ErrOutOfBounds. That bound moves
// forward as the head fills, so appending SERIES BY SERIES makes success
// depend on the order the series happen to arrive in: a fixture seeding
// one series near the epoch and another in the query's own year appends
// fine in one order and fails in the other, for a reason that has nothing
// to do with the query under test.
//
// Sorting every (series, sample) pair by timestamp before appending makes
// each sample land at or after the current head max, so the bound can
// never be crossed. This is not a relaxation of the storage's contract —
// no sample is dropped, moved or coerced, and the resulting head is
// identical to the one a lucky ordering would have produced. It only
// removes an ordering dependency the fixture never chose.
func appendSeries(storage *teststorage.TestStorage, series []Series) error {
	type sample struct {
		labels labels.Labels
		point  Point
	}

	var samples []sample
	for _, s := range series {
		lbls := labels.FromMap(s.Labels)
		for _, p := range s.Points {
			samples = append(samples, sample{labels: lbls, point: p})
		}
	}
	sort.SliceStable(samples, func(i, j int) bool {
		return samples[i].point.TMillis < samples[j].point.TMillis
	})

	app := storage.Appender(context.Background())
	for _, s := range samples {
		if _, err := app.Append(0, s.labels, s.point.TMillis, s.point.Value); err != nil {
			return fmt.Errorf("append %s at %d: %w", s.labels.String(), s.point.TMillis, err)
		}
	}
	if err := app.Commit(); err != nil {
		return fmt.Errorf("commit reference samples: %w", err)
	}
	return nil
}

// flatten converts Prometheus's result value into the transport-free
// Result shape, sorted by (labels, timestamp) so comparison never depends
// on the engine's internal ordering.
//
// Histogram-valued samples are rejected rather than silently coerced to
// their float field: a fixture whose reference answer is a native
// histogram cannot be compared against cerberus's float-only result path,
// and quietly comparing the wrong field would manufacture a green.
func flatten(v parser.Value) ([]Result, error) {
	var out []Result

	switch val := v.(type) {
	case promql.Matrix:
		for _, s := range val {
			if len(s.Histograms) > 0 {
				return nil, histogramValuedErr(s.Metric)
			}
			for _, p := range s.Floats {
				out = append(out, Result{Labels: s.Metric.Map(), TMillis: p.T, Value: p.F})
			}
		}
	case promql.Vector:
		for _, s := range val {
			if s.H != nil {
				return nil, histogramValuedErr(s.Metric)
			}
			out = append(out, Result{Labels: s.Metric.Map(), TMillis: s.T, Value: s.F})
		}
	case promql.Scalar:
		out = append(out, Result{Labels: map[string]string{}, TMillis: val.T, Value: val.V})
	default:
		return nil, fmt.Errorf("reference engine returned unsupported value type %T", v)
	}

	sortResults(out)
	return out, nil
}

func histogramValuedErr(m labels.Labels) error {
	return fmt.Errorf(
		"reference answer for %s is histogram-valued; cerberus's result path is float-only, "+
			"so this fixture cannot be parity-checked until native-histogram encoding lands",
		m.String(),
	)
}

// sortResults orders by label set then timestamp. Deterministic ordering
// is what lets the caller compare element-wise without re-deriving series
// identity.
func sortResults(rs []Result) {
	sort.SliceStable(rs, func(i, j int) bool {
		li, lj := labels.FromMap(rs[i].Labels).String(), labels.FromMap(rs[j].Labels).String()
		if li != lj {
			return li < lj
		}
		return rs[i].TMillis < rs[j].TMillis
	})
}

// EqualValues reports whether two float samples agree.
//
// NaN==NaN is TRUE here. PromQL produces NaN as a legitimate answer
// (0/0, quantile over an empty window, a stale marker's fold), and Go's
// float comparison would call two such answers different, failing
// fixtures that actually agree.
//
// Everything else is EXACT. This is deliberate and is the reason no
// epsilon appears anywhere in this package: cerberus and Prometheus
// evaluate the same IEEE-754 doubles, so a real disagreement is a real
// bug, and an epsilon would be a tolerance — the shape invariant 7
// forbids — dressed up as a numeric constant.
func EqualValues(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}
