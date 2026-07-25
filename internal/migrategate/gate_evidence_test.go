package migrategate_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// TestEvalVerify_AllEmptyMatricesLaneBlocks pins the dead-lane rule against the
// shape a verdict-count guard cannot see: every query on the lane MATCHED, because
// both backends returned an empty matrix over the window (a quiet window, a tenant
// with no traces, a metrics-generator not yet enabled). Match+Diverge+Error is 3,
// so the old predicate read the lane as alive while the comparator had diffed
// nothing at all. Keying on the evidence counter is what makes it block.
func TestEvalVerify_AllEmptyMatricesLaneBlocks(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 8, Match: 8, ComparedSeries: 5, ComparedUnits: 5},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadProm, migrateverify.Summary{Total: 5, Match: 5, ComparedSeries: 5}),
			matrixLane(migrateverify.HeadTempo, migrateverify.Summary{Total: 3, Match: 3}),
		},
	}
	dec := evalWithVerify(t, rep)
	if dec.Pass {
		t.Fatalf("a lane whose every match compared zero series must block the cutover, got PASS: %+v", dec)
	}
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictFail || !st.Blocking {
		t.Errorf("verify stage = %q blocking=%v, want fail + blocking", st.Verdict, st.Blocking)
	}
	if !containsSubstr(st.Reasons, "head tempo family metric-matrix compared nothing") {
		t.Errorf("the blocking reason must name the lane that proved nothing, got %+v", st.Reasons)
	}
	if containsSubstr(st.Reasons, "head prom family metric-matrix compared nothing") {
		t.Errorf("the healthy prom lane must not be reported dead, got %+v", st.Reasons)
	}
}

// TestEvalVerify_EveryLaneEmptyBlocksOnTheAggregate pins the whole-run case: no
// lane compared anything, so the aggregate guard fires first and says so in one
// line rather than repeating a per-lane reason for every head.
func TestEvalVerify_EveryLaneEmptyBlocksOnTheAggregate(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 4, Match: 4},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadLoki, migrateverify.Summary{Total: 4, Match: 4}),
		},
	}
	dec := evalWithVerify(t, rep)
	if dec.Pass {
		t.Fatalf("a run whose comparator diffed nothing must block, got PASS: %+v", dec)
	}
	if !containsSubstr(verifyStage(t, dec).Reasons, "nothing actually compared: 0 comparison units were diffed") {
		t.Errorf("the reason must say nothing was diffed, got %+v", verifyStage(t, dec).Reasons)
	}
}

// TestEvalVerify_ConfiguredIdleLaneIsACaveatNotSilence pins that a lane the
// operator supplied backends for, whose corpus entries all routed out of scope, is
// named in the decision. Those entries are honestly out of scope, so it does not
// block — but the artifact must be able to say "loki configured, judged nothing",
// or a green gate is the entire evidence behind flipping that datasource.
func TestEvalVerify_ConfiguredIdleLaneIsACaveatNotSilence(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 3, Match: 3, ComparedSeries: 3, OutOfScope: 12, ComparedUnits: 3},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadLoki, migrateverify.Summary{OutOfScope: 12}),
			matrixLane(migrateverify.HeadProm, migrateverify.Summary{Total: 3, Match: 3, ComparedSeries: 3}),
		},
	}
	dec := evalWithVerify(t, rep)
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictWarn {
		t.Errorf("an idle-but-configured lane WARNs (its entries are honestly out of scope), got %q reasons=%+v", st.Verdict, st.Reasons)
	}
	if !containsSubstr(st.Reasons, "head loki was configured but had no replayable query") {
		t.Errorf("the caveat must name the configured lane that was not judged, got %+v", st.Reasons)
	}
	if containsSubstr(st.Reasons, "head prom was configured but had no replayable query") {
		t.Errorf("the prom lane replayed 3 queries and must not be reported idle, got %+v", st.Reasons)
	}
}
