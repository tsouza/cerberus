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

	"github.com/prometheus/prometheus/model/histogram"
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
//
// A sample is EITHER float-valued or histogram-valued, never both, which
// is Prometheus's own data model: its Matrix carries Floats and Histograms
// in two separate slices, and its Vector's H field is nil exactly when the
// sample is a float.
type Point struct {
	// TMillis is the sample time in Unix milliseconds — Prometheus's
	// native resolution.
	TMillis int64

	// Value is the sample value, and is meaningful only when Histogram
	// is nil.
	Value float64

	// Histogram is the sample's native-histogram value, or nil for an
	// ordinary float sample.
	Histogram *Histogram
}

// Result is one output sample from the reference engine, flattened so the
// caller can compare without depending on Prometheus's own types.
type Result struct {
	// Labels is the output series' label set.
	Labels map[string]string

	// TMillis is the output timestamp in Unix milliseconds.
	TMillis int64

	// Value is the output value, and is meaningful only when Histogram
	// is nil.
	Value float64

	// Histogram is the output sample's native-histogram value, or nil
	// when the reference engine answered with a float.
	Histogram *Histogram
}

// Histogram is one native (exponential) histogram sample, in a DENSE,
// span-free shape: a bucket array plus the index its first entry sits at.
//
// # Why this shape rather than Prometheus's own
//
// Prometheus encodes a native histogram's buckets as SPANS — (gap, length)
// pairs that skip runs of empty buckets — while OTel-CH stores one
// contiguous array per sign with a single starting offset. Those are two
// encodings of the same function from bucket index to count, and this type
// is the second one because it is what BOTH sides of the comparison
// already speak: the seeded rows arrive in it, and cerberus's own
// histogram-shaped projection emits exactly these columns
// (`HistogramScale`, `HistogramPositiveOffset`,
// `HistogramPositiveBucketCounts`, …). Keeping the comparison in the dense
// shape means only ONE translation exists — Prometheus's spans in and out
// of this type — instead of one per side.
//
// It also keeps this package's exported surface transport-free, the same
// reason [Result] carries a label MAP rather than labels.Labels.
//
// # Bucket indices are OTel's, not Prometheus's
//
// Offset and the array index address a bucket the way the OpenTelemetry
// exponential-histogram specification does: entry j of PositiveBuckets
// counts observations in (base**(Offset+j), base**(Offset+j+1)], where
// base is 2**(2**-Scale). Prometheus numbers the same bucket ONE HIGHER,
// because its index i denotes (base**(i-1), base**i]. That off-by-one is
// the whole of the index translation, and it is applied in exactly one
// place each way — see [Histogram.toFloatHistogram] and
// [histogramFromFloat].
type Histogram struct {
	// Count is the total observation count and Sum their sum.
	Count, Sum float64

	// Scale is OTel's scale, which is numerically Prometheus's schema:
	// both mean a bucket base of 2**(2**-Scale).
	Scale int32

	// ZeroThreshold is the half-width of the zero bucket and ZeroCount
	// the observations that fell into it.
	//
	// ZeroThreshold is NOT an oracled axis for any fixture, and cannot be:
	// upstream OTel-CH persists no zero-threshold column, so a histogram
	// reconstructed from ClickHouse rows must INVENT one, and the only
	// honest value to invent is the 0 cerberus's own emitter assumes.
	// Both sides therefore carry 0 here by construction. It is compared
	// anyway rather than excluded, because comparing it costs nothing and
	// a future emitter that started projecting a different constant
	// should turn this check red rather than pass unnoticed — but a green
	// on this field is evidence of nothing. Where that invented threshold
	// reaches the ANSWER — quantile interpolation inside the zero band —
	// the fixture declares `scope: except-zero-bucket` instead, so the
	// vacuity is recorded in the fixture rather than hidden here.
	ZeroThreshold, ZeroCount float64

	// PositiveOffset is the OTel bucket index of PositiveBuckets[0], and
	// NegativeOffset the same for NegativeBuckets, whose buckets cover
	// the mirrored negative range.
	PositiveOffset  int32
	PositiveBuckets []float64
	NegativeOffset  int32
	NegativeBuckets []float64
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
		var err error
		if h := s.point.Histogram; h != nil {
			var fh *histogram.FloatHistogram
			if fh, err = h.toFloatHistogram(); err == nil {
				_, err = app.AppendHistogram(0, s.labels, s.point.TMillis, nil, fh)
			}
		} else {
			_, err = app.Append(0, s.labels, s.point.TMillis, s.point.Value)
		}
		if err != nil {
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
// A histogram-valued sample is carried in Result.Histogram rather than
// coerced into the float field. That distinction is load-bearing on the
// comparator's side: cerberus's own histogram-shaped projection reports a
// PLACEHOLDER in its Value column, so a reference histogram silently
// flattened to a float would be compared against a number that means
// nothing, which is the manufactured green this layer exists to prevent.
// The two shapes stay distinguishable all the way to the assertion.
func flatten(v parser.Value) ([]Result, error) {
	var out []Result

	switch val := v.(type) {
	case promql.Matrix:
		for _, s := range val {
			for _, p := range s.Floats {
				out = append(out, Result{Labels: s.Metric.Map(), TMillis: p.T, Value: p.F})
			}
			for _, p := range s.Histograms {
				out = append(out, Result{
					Labels:    s.Metric.Map(),
					TMillis:   p.T,
					Histogram: histogramFromFloat(p.H),
				})
			}
		}
	case promql.Vector:
		for _, s := range val {
			r := Result{Labels: s.Metric.Map(), TMillis: s.T}
			if s.H != nil {
				r.Histogram = histogramFromFloat(s.H)
			} else {
				r.Value = s.F
			}
			out = append(out, r)
		}
	case promql.Scalar:
		out = append(out, Result{Labels: map[string]string{}, TMillis: val.T, Value: val.V})
	default:
		return nil, fmt.Errorf("reference engine returned unsupported value type %T", v)
	}

	sortResults(out)
	return out, nil
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

// float64UnitRoundoff is IEEE-754 binary64's unit roundoff u = 2^-53: the
// largest relative error a SINGLE correctly-rounded arithmetic operation may
// introduce. It is the atom [summationReorderRelativeTolerance] is built
// from, not a tunable.
const float64UnitRoundoff = 1.0 / (1 << 53)

// maxReorderedSamplesPerOutputValue is the sample budget
// [summationReorderRelativeTolerance] is derived for: the number of input
// samples a single output value may be accumulated from before the tolerance
// stops covering the reordering error.
//
// 4096 is chosen from the query shapes cerberus answers, not from any
// fixture: it exceeds an hour of one-second-resolution samples (3600) folded
// into one range-window value, which is already far past any resolution a
// Prometheus deployment scrapes at. It is a budget, so naming it larger than
// the corpus needs is the safe direction — the cost is a wider tolerance, and
// the section below measures how much width that actually buys (three orders
// of magnitude above the noise, ten below the smallest real divergence this
// lane has ever produced).
const maxReorderedSamplesPerOutputValue = 4096

// summationReorderRelativeTolerance is the maximum RELATIVE difference
// [EqualValues] accepts between the reference engine's answer and cerberus's.
//
// # Where the number comes from
//
// Summing n floats in floating point has the standard backward-error bound
//
//	|fl(Σxᵢ) − Σxᵢ| ≤ γ_{n−1} · Σ|xᵢ|,   γ_k = k·u / (1 − k·u),   u = 2^-53
//
// so two DIFFERENT summation orders of the same n terms — which is exactly
// what cerberus and Prometheus do, neither being obliged to match the other —
// differ from each other by at most twice that, 2·γ_{n−1}·Σ|xᵢ|. For the
// non-negative terms a counter's per-window deltas produce, Σ|xᵢ| is the
// answer itself, so the bound is a RELATIVE one of 2(n−1)·u, and this
// constant is literally that expression evaluated at
// [maxReorderedSamplesPerOutputValue]: 2 · 4095 · 2^-53 ≈ 9.09e-13.
//
// The bound is therefore derived from the arithmetic and a stated sample
// budget. It is not fitted to any failing fixture — the observed divergences
// it was adopted for sit three orders of magnitude BELOW it.
//
// # What it admits, and what it still rejects
//
// Adopted for issue #2909, where the native increase() grid path and
// Prometheus fold the same window's samples in different orders:
// native_increase_range_step answered 21.000000000000004 and
// 42.00000000000001 against the reference's 21 and 42, and
// native_increase_vector_agg_temporality_union answered 210.00000000000003
// and 245.00000000000003 against 210 and 245. Every one of those four pairs
// is exactly ONE ULP apart — a relative distance of ~1.2e-16 to ~1.7e-16,
// about 3.7 orders of magnitude inside this tolerance.
//
// Real divergence on the same lane is not close. Issue #2905's
// duplicate-timestamp fixtures answered 2.75 against a reference 2.6666666666666665
// (3.03e-2 relative) and a constant +3 absolute on values of 8 to 22 (1.2e-1
// to 2.7e-1 relative) — between ten and eleven orders of magnitude ABOVE this
// tolerance, and rejected by it. TestEqualValuesRejectsRealDivergence pins
// that, so the tolerance can never be widened into a rubber stamp without a
// test going red.
//
// # Why relative, and why there is no absolute floor
//
// Relative is the only scale the reordering bound is stated in, and it is the
// only scale available: the comparator sees two answers, not the n terms or
// the Σ|xᵢ| they were accumulated from.
//
// A relative-only comparison does NOT go slack near zero — it TIGHTENS,
// degenerating to exact equality at 0, because the tolerance is scaled by the
// larger operand's magnitude. So it is strictly a relaxation of the exact
// comparator it replaces: every pair the old comparator accepted, this one
// accepts, and near zero the two agree exactly. That is why no absolute floor
// is added. A floor would be the one change here that could START accepting a
// divergence exact equality rejected, at magnitudes where nothing has ever
// been measured to diverge, and it would have to be a bare number — the
// comparator has no data magnitude to scale it against, so no honest
// derivation exists for one. Cancellation (a sum of signed terms whose Σ|xᵢ|
// dwarfs its result) is the shape that would motivate one; if it ever surfaces
// it will surface as a concrete red pair, which is the evidence a floor would
// need and does not have today.
//
// # Why this is uniform rather than per-function
//
// Every float cerberus reports was computed by ClickHouse and every float the
// reference reports was computed by Go, so the reordering and rounding this
// covers is a property of the COMPARISON, not of any one function. Earlier
// revisions carried named per-function ULP tolerances for atan2 (#1985), pow
// (#2598), native exponential-histogram interpolation (#2024, #2023) and
// histogram_quantile over a rate()'d classic bucket ladder — measured at one
// to five ULPs, i.e. at most ~1.1e-15 relative, all of them strictly inside
// this bound. They were folded into it: keeping them would have left a query
// that happens to mention atan2 held to a TIGHTER bound than the same
// arithmetic without it. Their measured pairs survive as test cases on this
// comparator, so none of that evidence is lost.
//
// This is one number applied to every comparison, with no per-fixture opt-in
// and no exemption set — the distinction invariant 7 draws. A fixture cannot
// reach it, widen it, or be excused by it.
const summationReorderRelativeTolerance = 2 * (maxReorderedSamplesPerOutputValue - 1) * float64UnitRoundoff

// EqualValues reports whether two float samples agree, within
// [summationReorderRelativeTolerance].
//
// NaN==NaN is TRUE here. PromQL produces NaN as a legitimate answer
// (0/0, quantile over an empty window, a stale marker's fold), and Go's
// float comparison would call two such answers different, failing
// fixtures that actually agree. A NaN against a real number is still
// unequal, and so is an infinity against anything it is not bit-identical
// to — an engine that answered +Inf where the other answered a finite
// number disagrees about the answer, at any tolerance.
func EqualValues(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	// Identity first: it is the whole answer for ±Inf and for ±0, and it
	// keeps a-b below from evaluating Inf-Inf.
	if a == b {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return false
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b) <= summationReorderRelativeTolerance*scale
}
