// Package downsampletier backfills and verifies the operator opt-in
// downsampled long-range tier (cerberus issue #2751,
// schema.DownsampleTierTable, internal/schema/ddl). It is the query layer
// behind the `cerberus schema downsample-tier-backfill` and
// `cerberus schema downsample-tier-verify` CLI verbs (cmd/cerberus), kept as
// its own package — mirroring internal/deltaprefix's shape — so the SQL
// composition + result diffing is unit testable without a live ClickHouse
// connection (see Conn).
//
// # Why a backfill is needed at all
//
// Exactly the same reason as internal/deltaprefix's own doc: `CREATE
// MATERIALIZED VIEW` does not retroactively process rows already in the
// source table, only INSERTs from the moment it is created onward. Once
// internal/schema/ddl provisions the tier table + its MV, history that
// predates that moment needs this package's Backfill to run a one-time
// `INSERT INTO ... SELECT` covering it.
//
// # A fundamentally lower-stakes failure mode than DeltaPrefix's
//
// internal/deltaprefix's package doc spends most of its length on silent
// corruption hazards — a wrong `before` bound double-counts or
// under-counts a SimpleAggregateFunction(sum, ...) column, invisibly. This
// tier has NO equivalent hazard: it is read back only through
// irate()/idelta()/last_over_time() (chopt.FeatureDownsampleTier's own
// doc), each of which is a function of exactly the last one or two samples
// in its window. A bucket this package has not yet backfilled is simply
// ABSENT from the tier table — the query-time read
// (internal/chsql.emitRangeWindowDownsampleTier) treats an absent or
// under-populated bucket exactly like PromQL's own "insufficient samples in
// window" rule, dropping the series point rather than answering with a
// wrong number. So, unlike Verify in internal/deltaprefix, this package's
// Verify checks COMPLETENESS (does every bucket the base table has data for
// also have a tier row) rather than VALUE PARITY (does a computed total
// match) — there is no total to parity-check against; the tier does not
// aggregate a scalar the way DeltaPrefix's PartialSum column does.
//
// # Rebuild vs Backfill
//
// Persisting an EXPERIMENTAL aggregate function's state
// (AggregateFunction(timeSeriesLastTwoSamples, ...) inside an
// AggregatingMergeTree) carries a real risk internal/deltaprefix's
// SimpleAggregateFunction(sum, ...) column does not: a ClickHouse upgrade
// that changes the experimental function's on-disk state FORMAT could
// strand every already-written row unreadable (cerberus issue #2751's own
// "Risks" section). Backfill alone — an incremental INSERT ... SELECT
// bounded by --before, mirroring internal/deltaprefix's Backfill — cannot
// recover from that: the already-written, now-unreadable rows are still
// there, blocking a clean re-backfill of the same range. Rebuild exists for
// exactly this case: it TRUNCATEs the tier table (idempotent even after a
// state-format break, since it does not need to READ the old rows) and
// re-populates the FULL configured history in one pass, with no --before
// bound at all. An operator who suspects a stranded state (a query
// unexpectedly dropping every point that should route to the tier, or a
// ClickHouse changelog entry naming this function) runs Rebuild, not
// Backfill.
//
// # One-time backfill vs. the tier table's own steady-state TTL
//
// The SAME retention-boundary hazard internal/deltaprefix's package doc
// describes at length applies here identically: the tier table shares the
// base Sum table's TTL (internal/schema/ddl's renderDownsampleTierTable), so
// a Backfill or Rebuild invoked after the earliest historical day has
// already crossed its own BucketEnd + retention instant writes rows that
// are already past their own TTL the moment they land, and ClickHouse's
// routine background TTL cleanup reaps them almost immediately (cerberus
// issue #2652's finding, reproduced here rather than re-derived). Both
// Backfill and Rebuild take the same explicit retention duration
// internal/deltaprefix's own verbs do (schemaboot.MetricsRetention) and
// report any day already outside it on the returned Result, rather than
// mishandling it silently — see OutsideRetentionDays.
package downsampletier
