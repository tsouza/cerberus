// Package deltaprefix backfills and verifies the DELTA-temporality
// prefix-reconstruction aggregate table (cerberus issue #2389,
// schema.Metrics.DeltaPrefixTable, internal/schema/ddl). It is the query
// layer behind the `cerberus schema delta-prefix-backfill` and
// `cerberus schema delta-prefix-verify` CLI verbs (cmd/cerberus), kept
// as its own package so the SQL composition + result diffing is unit
// testable without a live ClickHouse connection (see Conn).
//
// # Why a backfill is needed at all
//
// `CREATE MATERIALIZED VIEW` in ClickHouse does not retroactively process
// rows already in the source table — the view only fires for INSERTs from
// the moment it is created onward. So once internal/schema/ddl provisions
// the DELTA-prefix table + its MV, history that predates that moment is
// simply absent from the aggregate table until this package's Backfill
// runs a one-time `INSERT INTO ... SELECT` covering it.
//
// # Why the cutoff bound matters
//
// Both Backfill and Verify take a `before` timestamp that MUST be the MV's
// own creation time (an operator reads it from
// `system.tables.metadata_modification_time`, or records it themselves at
// the moment `CREATE MATERIALIZED VIEW` ran — see docs/operations.md).
// Backfilling past that timestamp double-counts every row the live MV
// already captured; the resulting corruption is silent (a fabricated,
// too-large prefix level), not a query error. Both entrypoints round the
// bound down to `toStartOfDay` internally — the aggregate table's own
// bucket resolution — so a backfill (or a verify comparison) never split a
// calendar day between "backfilled" and "MV-captured": everything up to
// but excluding the cutover's calendar day is backfilled / compared;
// that partial last day is left for the live MV to capture as ordinary new
// inserts arrive.
//
// # Scope: completeness, not series-identity alignment
//
// Verify compares aggregate totals GROUPED BY MetricName ONLY — a coarse,
// deployment-tractable check that the DELTA-prefix table's per-metric total
// matches the base table's own DELTA-temporality total for the backfilled
// history. It does NOT validate that the aggregate table's raw-tuple rows
// join byte-identically against cerberus's read-time computed series key
// (the `mapSort(mapConcat(mapUpdate(...)))` shaping tower in
// internal/promql) — this package deliberately does not import that tower
// (internal/chsql must not depend on internal/promql; see CLAUDE.md's
// architecture decision tree), so it can only ever prove completeness, not
// identity alignment.
//
// The read-side emitter change that DOES consume the alignment property —
// internal/promql/lower.go's augmentDeltaPrefixAggregateAttributes (which
// reuses the SAME shaping tower for both the primary selector arm and the
// aggregate table's arm, rather than re-deriving a second join key) plus
// internal/chsql's deltaPrefixAggregateSource — has since shipped, gated
// behind the separate config.Config.DeltaPrefixReadEnabled /
// CERBERUS_DELTA_PREFIX_READ_ENABLED flag this package's `--before` cutover
// still has to precede: a clean Verify pass remains the required
// precondition before an operator sets that flag, exactly as before.
package deltaprefix
