package chsql_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEmitVectorJoin_RoleOneStepAligned_NoRedundantMax pins cerberus issue
// #2818's actual resolution: roleOne's StepAligned arm ("one" side of
// group_left/group_right, or either side of an on()/ignoring()-reduced
// one-to-one match, in range mode) no longer computes max(TimeUnix) as a
// separate aggregate. TimeUnix is already a GROUP BY key in that arm (the
// StepAligned match key is (match-key, TimeUnix)), so every row within one
// group carries the identical TimeUnix value — max(TimeUnix) over that
// group was always just the group key restated. The fix reads the column
// directly instead, matching roleMany's identical StepAligned arm.
// argMax(Value, TimeUnix) is untouched: it is still choosing which Value
// goes with that TimeUnix (relevant under a genuine tie — see
// TestVectorJoinRoleOneStepAligned_TiedTimestamp_ChDB).
func TestEmitVectorJoin_RoleOneStepAligned_NoRedundantMax(t *testing.T) {
	t.Parallel()

	plan := &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_gauge"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpAdd,
		Card:             chplan.CardManyToOne, // left=roleMany, right=roleOne
		Include:          []string{"instance"},
		StepAligned:      true,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}

	sql := emitJoin(t, plan)

	if strings.Contains(sql, "max(`TimeUnix`)") {
		t.Errorf("roleOne StepAligned emit must NOT contain a redundant max(TimeUnix); got:\n%s", sql)
	}
	if !strings.Contains(sql, "argMax(`Value`, `TimeUnix`)") {
		t.Errorf("roleOne StepAligned emit must still contain argMax(Value, TimeUnix); got:\n%s", sql)
	}
	// The plain group-key read renders as `_join_TimeUnix` AS aliased
	// TimeUnix, the same shape roleMany's StepAligned arm already uses.
	if !strings.Contains(sql, "`TimeUnix` AS `_join_TimeUnix`") {
		t.Errorf("roleOne StepAligned emit must select the bare TimeUnix group-key column; got:\n%s", sql)
	}
}

// TestEmitVectorJoin_RoleOneStepAligned_ArgAndMaxFusionInert pins that
// ArgAndMaxFusion has nothing to act on in roleOne's StepAligned arm either
// way — the redundant max(TimeUnix) this PR deletes was never fusable
// (there is no second aggregate to fuse argMax with once max(TimeUnix) is
// gone), so the emitted SQL is byte-identical regardless of the flag.
func TestEmitVectorJoin_RoleOneStepAligned_ArgAndMaxFusionInert(t *testing.T) {
	t.Parallel()

	planWith := func(fused bool) *chplan.VectorJoin {
		return &chplan.VectorJoin{
			Left:             &chplan.Scan{Table: "otel_metrics_gauge"},
			Right:            &chplan.Scan{Table: "otel_metrics_sum"},
			Op:               chplan.OpAdd,
			Card:             chplan.CardManyToOne,
			Include:          []string{"instance"},
			StepAligned:      true,
			ArgAndMaxFusion:  fused,
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}

	fusedSQL := emitJoin(t, planWith(true))
	unfusedSQL := emitJoin(t, planWith(false))
	if fusedSQL != unfusedSQL {
		t.Errorf("roleOne StepAligned emit must be byte-identical regardless of ArgAndMaxFusion:\nfused:\n%s\nunfused:\n%s", fusedSQL, unfusedSQL)
	}
	if strings.Contains(fusedSQL, "argAndMax") {
		t.Errorf("roleOne StepAligned emit must never contain argAndMax; got:\n%s", fusedSQL)
	}
}
