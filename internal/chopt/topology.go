package chopt

// ClusterTopology is the boot-resolved, single source of truth for how many
// independent ClickHouse DATA shards sit behind cerberus's logical dataset
// (cerberus issue #3081, part of epic #3074's multi-shard support). It is
// read once at startup (CERBERUS_CH_DATA_SHARDS, internal/config) and
// threaded, unchanged, into every consumer that needs the count — today,
// internal/solver's admission control: cmd/cerberus's buildSolver copies
// ClusterTopology.DataShardCount into solver.Config.DataShardCount once, at
// solver construction, keeping internal/solver's own import surface at
// chplan + chclient + the standard library (its own package doc) rather
// than adding this package as a fifth dependency purely to read one int.
//
// DISAMBIGUATION — this is the THIRD, unrelated sense of "shard" in this
// codebase (epic #3074's own terminology table):
//   - internal/solver already uses bare `Shard`-prefixed identifiers
//     (EstimateMinRowsPerAdditionalShard, runShard, perShardMemoryBytes,
//     minAnchorsForPerRungShard) for its OWN query-time-range/anchor-grid
//     splitting — nothing to do with ClickHouse cluster topology.
//   - internal/config / internal/schema/ddl use DatabaseReplicatedShard and
//     the `{shard}`/`{replica}` Keeper macros for replication-coordinate
//     labeling — a different physical concept that happens to share
//     ClickHouse's own `{shard}` macro slot with sense 3 below, by
//     ClickHouse's own design (an intentional convergence, not a naming
//     collision).
//   - ClusterTopology.DataShardCount (sense 3, this type) counts a
//     ClickHouse cluster's own physical DATA partitions: how many-way a
//     `Distributed` table fans a single query out across. Every new
//     identifier for THIS sense uses the DataShard-prefixed compound form
//     (DataShardCount, DataShardFanoutGate, DataShardFanoutCap,
//     DisableSplitOnMultiDataShard) to keep it apart from the other two.
type ClusterTopology struct {
	// DataShardCount is the number of ClickHouse data shards the connected
	// cluster's `Distributed` tables fan a query out across. 1 (the
	// default) means a single logical dataset — unreplicated, or replicated
	// N ways onto identical copies — exactly matching every deployment that
	// predates this field. A value > 1 means the dataset is PARTITIONED
	// across that many shards, so a single cerberus-admitted connection
	// fans out to N ClickHouse-side statements once it reaches a
	// `Distributed` table, not just one.
	DataShardCount int
}

// defaultDataShardCount is ClusterTopology's zero-risk default: a single
// logical dataset, matching every deployment that predates this field. Named
// so DefaultClusterTopology's "1" is never a bare literal (invariant 13).
const defaultDataShardCount = 1

// DefaultClusterTopology returns the single-data-shard default — safe to
// wire in-process without any live cluster inspection, and the value every
// deployment gets unless CERBERUS_CH_DATA_SHARDS is set.
func DefaultClusterTopology() ClusterTopology {
	return ClusterTopology{DataShardCount: defaultDataShardCount}
}
