package chsql

import (
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// The downsample tier's write side — its materialized views
// (internal/schema/ddl) and its backfill (internal/downsampletier) — reads
// each raw sample through the two expressions below. Both writers render
// them from here, so a backfilled row and a live-MV row for the same raw
// sample land in the same bucket and fold the same state.

// DownsampleTierBucketEnd renders the tier's bucket-boundary expression over
// the source table's timestamp column col, for schema.DownsampleTierBucket
// buckets.
//
// It computes the CEILING bucket boundary — `bucket * ceil(t / bucket)` —
// NOT ClickHouse's native toStartOfInterval FLOOR convention
// (deltaprefix's own dayBucket). This is deliberate: PromQL's range-vector
// window is HALF-OPEN LEFT, CLOSED RIGHT — `(anchor-range, anchor]` — so a
// raw sample landing exactly ON a bucket boundary must join the bucket
// ENDING at that instant, not the one STARTING there. A plain
// toStartOfInterval(t, bucket) bucket is closed-left/open-right, which is
// off-by-one at BOTH edges against that PromQL window; the ceiling
// convention here reproduces PromQL's half-open-left/closed-right window
// exactly (verified empirically against a real ClickHouse boundary-exact
// sample — see internal/chsql's downsample-tier chDB test): every raw
// sample is guaranteed to land in exactly the ONE bucket row whose
// `(BucketStart, BucketEnd]` interval contains its timestamp, never split
// or duplicated across two rows. That unambiguous per-sample assignment is
// what makes the read side's bucket-to-anchor mapping exact — a query whose
// range equals the bucket (internal/chsql's emitRangeWindowDownsampleTier,
// cerberus issue #2751) looks up a SINGLE bucket row per grid anchor with
// no merge or re-filter at all; a query spanning N buckets (the same
// function, extended by issue #2857) merges exactly the N bucket rows whose
// BucketEnd falls in the anchor's own window, with no gap or overlap
// between them, and re-filters the merged result as a defensive check
// against malformed data rather than a correctness requirement of this
// boundary expression.
//
// Computed as `toStartOfInterval(t - toIntervalNanosecond(1), bucket) +
// bucket`: subtracting one nanosecond — the finest step of the DateTime64(9)
// the subtraction yields whether TimeUnix is DateTime64(9) or DateTime —
// before flooring turns ClickHouse's floor into the ceiling
// PromQL's window needs, EXCEPT for a sample exactly ON a bucket's OWN
// start (which correctly stays in that same, now-ending-here bucket rather
// than jumping to the one before it) — see the chDB test's boundary-exact
// case for the worked arithmetic.
func DownsampleTierBucketEnd(col string) Frag {
	bucketSeconds := int64(schema.DownsampleTierBucket / time.Second)
	epsilon := Sub(Col(col), Call("toIntervalNanosecond", InlineLit(int64(downsampleTierCeilingStepNanos))))
	floor := Call("toStartOfInterval", epsilon, Call("toIntervalSecond", InlineLit(bucketSeconds)))
	return Add(floor, Call("toIntervalSecond", InlineLit(bucketSeconds)))
}

// downsampleTierCeilingStepNanos is the one-nanosecond step
// DownsampleTierBucketEnd subtracts before flooring.
const downsampleTierCeilingStepNanos = 1

// DownsampleTierSampleTime is the sample timestamp the downsample-tier
// materialized views (internal/schema/ddl) and their backfill
// (internal/downsampletier) fold into the tier's timeSeriesLastTwoSamples
// state, read from the source table's timestamp column col. The state's
// timestamp type is fixed at DateTime64(9), an AggregateFunction state does
// not convert between argument types, and the source column is DateTime64(9)
// or DateTime depending on who created the table — so both read it through
// ToDateTime64Nanos, the identity on the former and an exact widening of the
// latter, and a backfilled row folds the same state a live-MV row does.
func DownsampleTierSampleTime(col string) Frag { return ToDateTime64Nanos(Col(col)) }

// DownsampleTierRecordedValue keeps a raw metric row only when it carries a
// recorded sample: `bitAnd(<flagsCol>, NoRecordedValue) = 0`. A row with the
// NoRecordedValue bit is a Prometheus stale marker whose Value is the
// exporter's empty placeholder 0; folded into the tier's
// timeSeriesLastTwoSamples state it would read as a real 0 — a counter reset
// for irate(), a spurious last_over_time() value. Both tier MVs and the
// backfill/rebuild/verify SELECTs apply it whenever the schema names the
// Flags column.
func DownsampleTierRecordedValue(flagsCol string) Frag {
	return Eq(Call("bitAnd", Col(flagsCol), InlineLit(int64(schema.NoRecordedValueFlag))), InlineLit(int64(0)))
}
