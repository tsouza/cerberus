package schema

import "time"

// DownsampleTierTable / DownsampleTierBucketColumn / DownsampleTierSamplesColumn
// name the operator opt-in downsampled long-range tier (cerberus issue
// #2751): an AggregatingMergeTree table folding raw Sum-table samples into
// timeSeriesLastTwoSamples aggregate states per DownsampleTierBucket-aligned
// bucket, read back by irate() / idelta() / last_over_time() query_range
// shapes whose window matches the bucket exactly (see
// internal/promql/lower_strategy.go's DownsampleTier*Lowerer strategies and
// chopt.FeatureDownsampleTier).
//
// Unlike Metrics.DeltaPrefixTable this is NOT a per-Metrics, per-deployment
// overridable field: like schema.LabelCatalogTable / LabelCatalogKeyColumn
// (internal/schema/ddl provisions, internal/api/loki queries), the table has
// one fixed, cerberus-owned name that needs no per-deployment override —
// internal/schema/ddl (the DDL), internal/promql + internal/chsql (the read
// path), and internal/downsampletier (the backfill CLI) all reference these
// same literals. Provisioning is gated by config.Config.SchemaDownsampleTier
// (the resolved chopt.FeatureDownsampleTier boot verdict — see
// internal/schemaboot.DDLConfig): unlike DeltaPrefixTable, there is no
// separate "does this deployment declare the table" bit, because a missing
// or not-yet-backfilled bucket for THIS mechanism degrades safely — an
// absent tier row reads back as "fewer than 2 samples in window", which
// chsql's downsample-tier read path already treats as an absent series
// point (the same "insufficient samples" gap PromQL itself would render for
// missing raw data) rather than a silently wrong non-zero value. That is a
// direct consequence of restricting this tier to irate() / idelta() /
// last_over_time() only (see chopt.FeatureDownsampleTier's own doc for why
// rate()/increase()/delta() are hard-rejected) — those three functions are
// exact functions of "the last k samples in the window", so a missing or
// partial bucket loses SAMPLES, never introduces a wrong DELTA the way an
// incomplete SimpleAggregateFunction(sum, ...) table would (contrast
// Metrics.DeltaPrefixTable's own doc, where exactly this silent-corruption
// risk is why that table needs its own separate, later
// DeltaPrefixReadEnabled declaration).
const (
	DownsampleTierTable         = "otel_metrics_sum_downsample_tier"
	DownsampleTierBucketColumn  = "BucketEnd"
	DownsampleTierSamplesColumn = "LastTwoSamples"

	// DownsampleTierTemporalityColumn names the tier table's own
	// any(AggregationTemporality) column — a single OTel series carries ONE
	// temporality for its lifetime, so any() over each bucket's rows is
	// exact, not a lossy pick (the same property
	// chplan.RangeWindow.TemporalityColumn's own doc relies on for the raw
	// fan-out path). irate()'s read (internal/chsql's
	// emitRangeWindowDownsampleTier) branches on it via the SAME
	// chsql.CounterOrDeltaPairDelta primitive the fan-out's irateValueFrag
	// uses, so a DELTA-temporality counter routed through the tier gets the
	// SAME "raw current sample, no reset-correction" numerator the raw scan
	// would compute — see that primitive's own doc. idelta() and
	// last_over_time() ignore this column: idelta never branches on
	// temporality anywhere in this codebase (the fan-out's own
	// emitRangeWindowIDelta does not either), and last_over_time is
	// temporality-agnostic by definition.
	DownsampleTierTemporalityColumn = "Temporality"
)

// DownsampleTierBucket is the tier's single supported aggregation bucket —
// fixed, not operator-configurable (cerberus issue #2751's deliberate v1
// scope decision). It mirrors the retired otel_metrics_sum_5m rollup's own
// granularity (see the retired MetricsRollups' defaultOTelRollups doc),
// chosen because it suits PromQL query_range's common 5-minute step. A
// configurable bucket size would multiply the routing-eligibility surface
// (internal/promql/lower_strategy.go's resolution-aware eligibility check)
// for marginal v1 benefit; a deployment needing a different granularity can
// follow this package + internal/schema/ddl's DDL as a template for its own
// table.
const DownsampleTierBucket = 5 * time.Minute
