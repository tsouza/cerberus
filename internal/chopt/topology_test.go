package chopt

import "testing"

// TestDefaultClusterTopology_SingleShard pins the zero-risk default every
// deployment gets unless CERBERUS_CH_DATA_SHARDS is set: DataShardCount == 1,
// the value that makes internal/solver's DataShardFanoutGate mechanism a
// structural no-op (cerberus issue #3081).
func TestDefaultClusterTopology_SingleShard(t *testing.T) {
	t.Parallel()
	got := DefaultClusterTopology()
	if got.DataShardCount != 1 {
		t.Fatalf("DefaultClusterTopology().DataShardCount = %d, want 1", got.DataShardCount)
	}
}

// TestClusterTopology_ExplicitOverride pins that ClusterTopology is a plain
// value type an operator-facing loader can freely override — no hidden
// validation or clamping lives on the type itself (internal/config owns
// fail-fast validation of the parsed CERBERUS_CH_DATA_SHARDS value).
func TestClusterTopology_ExplicitOverride(t *testing.T) {
	t.Parallel()
	got := ClusterTopology{DataShardCount: 4}
	if got.DataShardCount != 4 {
		t.Fatalf("DataShardCount = %d, want 4", got.DataShardCount)
	}
}
