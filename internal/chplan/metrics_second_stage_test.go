package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMetricsSecondStageEqual exercises structural equality across the
// node's fields — Op, K, ThresholdOp, ThresholdValue, PartitionBy,
// ValueAlias, Input.
func TestMetricsSecondStageEqual(t *testing.T) {
	t.Parallel()

	base := &chplan.MetricsSecondStage{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Op:          chplan.SecondStageTopK,
		K:           5,
		PartitionBy: []string{"anchor_ts"},
		ValueAlias:  "Value",
	}
	same := &chplan.MetricsSecondStage{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Op:          chplan.SecondStageTopK,
		K:           5,
		PartitionBy: []string{"anchor_ts"},
		ValueAlias:  "Value",
	}
	if !base.Equal(same) {
		t.Fatalf("identical MetricsSecondStage trees should be Equal")
	}

	// Different Op.
	diffOp := *same
	diffOp.Op = chplan.SecondStageBottomK
	if base.Equal(&diffOp) {
		t.Errorf("different Op should not be Equal")
	}

	// Different K.
	diffK := *same
	diffK.K = 10
	if base.Equal(&diffK) {
		t.Errorf("different K should not be Equal")
	}

	// Different ValueAlias.
	diffAlias := *same
	diffAlias.ValueAlias = "other"
	if base.Equal(&diffAlias) {
		t.Errorf("different ValueAlias should not be Equal")
	}

	// Different PartitionBy length.
	diffPartLen := *same
	diffPartLen.PartitionBy = []string{"anchor_ts", "service"}
	if base.Equal(&diffPartLen) {
		t.Errorf("different PartitionBy length should not be Equal")
	}

	// Different PartitionBy content.
	diffPartContent := *same
	diffPartContent.PartitionBy = []string{"step"}
	if base.Equal(&diffPartContent) {
		t.Errorf("different PartitionBy content should not be Equal")
	}

	// Different Input.
	diffInput := *same
	diffInput.Input = &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpCountOverTime,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	if base.Equal(&diffInput) {
		t.Errorf("different Input should not be Equal")
	}

	// Threshold variant — different op + value.
	threshold := &chplan.MetricsSecondStage{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Op:             chplan.SecondStageThreshold,
		ThresholdOp:    chplan.OpGt,
		ThresholdValue: 10,
		ValueAlias:     "Value",
	}
	thresholdSame := *threshold
	if !threshold.Equal(&thresholdSame) {
		t.Errorf("identical Threshold trees should be Equal")
	}
	thresholdDiffOp := *threshold
	thresholdDiffOp.ThresholdOp = chplan.OpLt
	if threshold.Equal(&thresholdDiffOp) {
		t.Errorf("different ThresholdOp should not be Equal")
	}
	thresholdDiffVal := *threshold
	thresholdDiffVal.ThresholdValue = 20
	if threshold.Equal(&thresholdDiffVal) {
		t.Errorf("different ThresholdValue should not be Equal")
	}

	// Different node type.
	scan := &chplan.Scan{Table: "otel_traces"}
	if base.Equal(scan) {
		t.Errorf("MetricsSecondStage.Equal of *Scan should be false")
	}
}

// TestMetricsSecondStageChildren confirms Walk descends through Input.
func TestMetricsSecondStageChildren(t *testing.T) {
	t.Parallel()

	inner := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	ms := &chplan.MetricsSecondStage{
		Input:      inner,
		Op:         chplan.SecondStageTopK,
		K:          5,
		ValueAlias: "Value",
	}
	kids := ms.Children()
	if len(kids) != 1 {
		t.Fatalf("expected 1 child, got %d", len(kids))
	}
	if kids[0] != inner {
		t.Errorf("Children[0] should be the Input MetricsAggregate, got %T", kids[0])
	}

	var visited []string
	chplan.Walk(ms, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.MetricsSecondStage:
			visited = append(visited, "MetricsSecondStage")
		case *chplan.MetricsAggregate:
			visited = append(visited, "MetricsAggregate")
		case *chplan.Scan:
			visited = append(visited, "Scan")
		}
		return true
	})
	want := []string{"MetricsSecondStage", "MetricsAggregate", "Scan"}
	if len(visited) != len(want) {
		t.Fatalf("Walk visited %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Errorf("Walk[%d] = %s, want %s", i, visited[i], want[i])
		}
	}
}

// TestSecondStageOpString round-trips the enum to its TraceQL-source name.
func TestSecondStageOpString(t *testing.T) {
	t.Parallel()

	cases := map[chplan.SecondStageOp]string{
		chplan.SecondStageInvalid:   "invalid",
		chplan.SecondStageTopK:      "topk",
		chplan.SecondStageBottomK:   "bottomk",
		chplan.SecondStageThreshold: "threshold",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("SecondStageOp(%d).String() = %q, want %q", op, got, want)
		}
	}
}
