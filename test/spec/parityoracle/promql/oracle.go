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

// EqualValues reports whether two float samples agree.
//
// NaN==NaN is TRUE here. PromQL produces NaN as a legitimate answer
// (0/0, quantile over an empty window, a stale marker's fold), and Go's
// float comparison would call two such answers different, failing
// fixtures that actually agree.
//
// Everything else is EXACT. The two named function-specific exceptions below
// have independently demonstrated implementation-level rounding differences;
// an unscoped epsilon would be a tolerance — the shape invariant 7 forbids —
// dressed up as a numeric constant.
func EqualValues(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

// atan2ULPTolerance is the maximum ULP (unit-in-the-last-place) distance
// permitted between cerberus's atan2 answer and the reference engine's. It
// is the ONE relaxation this package makes to EqualValues's exact-equality
// rule, and it exists for exactly one proven reason — see
// [EqualAtan2Values].
const atan2ULPTolerance = 1

// nativeHistogramQuantileULPTolerance is the maximum ULP distance between
// Prometheus's and ClickHouse's exponential-histogram interpolation results.
// The engines use separate floating-point implementations for that operation.
const nativeHistogramQuantileULPTolerance = 2

// EqualAtan2Values reports whether two float samples PRODUCED BY EVALUATING
// PROMQL'S atan2 agree within [atan2ULPTolerance] ULPs. NaN==NaN is TRUE
// here for the same reason it is in EqualValues.
//
// # Why atan2 gets a tolerance and nothing else in this package does
//
// invariant 7 forbids a tolerance dressed up as a numeric constant, so this
// one earns its place by being the opposite: a NAMED exception for a SINGLE
// function, adopted only after that function was independently proven to
// diverge, not adopted speculatively for a class it might belong to.
//
// binop_atan2_scalar_vector.txtar (`2 atan2 up`) evaluates atan2 two
// different ways: the reference engine calls Go's math.Atan2, and cerberus
// lowers the vector-involving case to ClickHouse's own SQL atan2, evaluated
// by ClickHouse's libm. Two of the fixture's four series disagree in the
// last bit of the mantissa — e.g. reference 1.3734007669450157 versus
// cerberus 1.373400766945016, and reference 1.2341215074081693 versus
// cerberus 1.2341215074081695 — which is exactly what IEEE-754 permits: the
// standard requires each implementation to be correctly rounded for its OWN
// algorithm, but it does not require two independent libm implementations
// to agree bit-for-bit on a transcendental function, only "faithfully
// rounded" within a small ULP bound, typically 1. That is a fact about
// floating-point arithmetic, not a lowering bug, and no amount of chasing it
// in cerberus's SQL would close it — the fix would have to live in
// ClickHouse's libm.
//
// This does NOT extend to any other PromQL function. sin, cos, tan, asin,
// acos, atan, exp, ln, log2, log10 and sqrt are all named as candidates in
// issue #1985, and NONE of them has been measured to diverge — for all this
// package knows today, ClickHouse and Go's math package agree on every one
// of them exactly. A future function earns this same treatment only the
// same way atan2 did: enrol its fixture at ordinary EqualValues, run it, and
// observe a genuine small-ULP disagreement. Until that proof exists for a
// given function, a mismatch on it is a real bug and EqualValues — exact
// equality — is the correct comparator. Do not widen atan2ULPTolerance's
// scope, and do not add a second named tolerance without the same evidence
// this one has (see the issue for the seed values and the exact diffs).
//
// Note that PromQL's pure scalar-scalar atan2 (`1 atan2 2`) is unaffected:
// internal/promql/scalar.go constant-folds it with Go's own math.Atan2, so
// it already matches the reference engine exactly and needs no tolerance.
// Only the vector-involving shape, evaluated per-row in ClickHouse SQL,
// diverges.
func EqualAtan2Values(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	return ulpDistance(a, b) <= atan2ULPTolerance
}

// EqualNativeHistogramQuantileValues reports whether native histogram
// quantiles agree within [nativeHistogramQuantileULPTolerance] ULPs. The
// reference engine evaluates the interpolation in Go while cerberus evaluates
// it in ClickHouse; the two issue #2023 fixtures demonstrate a maximum
// observed distance of two ULPs.
func EqualNativeHistogramQuantileValues(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	return ulpDistance(a, b) <= nativeHistogramQuantileULPTolerance
}

// ulpDistance returns the number of math.Nextafter steps needed to walk
// from a to b — the standard definition of ULP distance for IEEE-754
// doubles, and exactly what "faithfully rounded within N ULPs" means. It is
// 0 for equal values (including +0 and -0, which compare equal), 1 for
// adjacent representable values, and so on.
//
// # How this works
//
// math.Float64bits gives each float's raw IEEE-754 bit pattern, which for
// non-negative floats already increases monotonically with the value (a
// deliberate property of the IEEE-754 encoding). monotonicUint64 extends
// that monotonicity across the sign boundary too, so that once both values
// are mapped, their ULP distance is simply the unsigned difference between
// the two representations.
//
// This was verified against math.Nextafter itself — not just eyeballed —
// by walking 5000 consecutive Nextafter steps across the positive/negative
// boundary and checking ulpDistance agreed with the step count at every
// one. That check matters because the naive version of this trick (flip
// every bit for a negative value, flip only the sign bit for a
// non-negative one — the form most references, including Google Test's
// comparator, show) treats +0.0 and -0.0 as two DISTINCT adjacent slots.
// They are not: math.Nextafter — and IEEE-754 equality — treat zero as a
// single point, so that naive form overcounts by 1 for any pair straddling
// zero. monotonicUint64 collapses them into one slot instead.
func ulpDistance(a, b float64) uint64 {
	if a == b {
		return 0
	}
	ua, ub := monotonicUint64(a), monotonicUint64(b)
	if ua > ub {
		ua, ub = ub, ua
	}
	return ub - ua
}

// signBit64 is the sign bit of an IEEE-754 double's raw bit pattern.
const signBit64 = uint64(1) << 63

// monotonicUint64 maps a float64 onto a uint64 whose ordering agrees with
// the float's own ordering across the whole range, sign boundary included,
// with +0.0 and -0.0 mapped to the SAME value. See [ulpDistance] for why
// that last part matters and how this was checked.
//
// The branch is on the float's VALUE (f < 0), not its bit pattern's sign
// bit, which is what makes -0.0 — value-wise non-negative, despite its sign
// bit being set — fall into the same branch as +0.0. bits(-0.0) is just the
// sign bit with no magnitude bits, so ORing it with signBit64 is a no-op
// and lands it on exactly the same slot as +0.0, with no special case
// needed for it.
//
// For a genuinely negative float, the magnitude bits (its bit pattern with
// the sign bit cleared) increase as the value gets more negative, so
// subtracting them from signBit64 maps the value into a mirror range
// immediately below zero's slot with no gap and no overlap: the smallest
// magnitude (1, the negative value adjacent to zero) lands one below zero,
// exactly where the non-negative side's smallest magnitude lands one
// above it.
func monotonicUint64(f float64) uint64 {
	bits := math.Float64bits(f)
	if f < 0 {
		magnitude := bits &^ signBit64
		return signBit64 - magnitude
	}
	return bits | signBit64
}
