package migrategate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// matrixLane builds one head row for a lane that judged only metric matrices —
// the artifact shape every verify fixture below describes. It mirrors what a real
// run writes: the head roll-up, the same counts partitioned into that head's
// metric-matrix family, and the shape-agnostic evidence counter alongside the
// series one. Building the family here rather than by hand is what keeps a
// fixture an artifact this build could actually have produced.
func matrixLane(head string, s migrateverify.Summary) migrateverify.HeadSummary {
	s.ComparedUnits = s.ComparedSeries
	h := migrateverify.HeadSummary{Head: head, Configured: true, Summary: s}
	if s.Total > 0 {
		h.Families = []migrateverify.FamilySummary{{
			Kind: migrateverify.KindMetricMatrix, Unit: migrateverify.UnitSeries,
			Total: s.Total, Match: s.Match, Diverge: s.Diverge, Undecidable: s.Undecidable,
			Unsupported: s.Unsupported, Error: s.Error, Compared: s.ComparedSeries,
		}}
	}
	return h
}

// verifyStage returns the verify stage result from a decision, failing the test
// if the gate produced none.
func verifyStage(t *testing.T, dec migrategate.Decision) migrategate.StageResult {
	t.Helper()
	for _, s := range dec.Stages {
		if s.Stage == migrategate.StageVerify {
			return s
		}
	}
	t.Fatalf("decision carries no verify stage: %+v", dec.Stages)
	return migrategate.StageResult{}
}

// evalWithVerify runs the gate over a verify report plus otherwise-clean required
// artifacts, so the assertions below isolate the verify stage's own rules.
func evalWithVerify(t *testing.T, rep migrateverify.Report) migrategate.Decision {
	t.Helper()
	in := migrategate.Inputs{
		Verify:    writeArtifact(t, "verify.json", rep.WriteJSON),
		Classify:  cleanClassify(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return dec
}

// TestEvalVerify_DeadLaneBlocks pins the per-lane "compared nothing" rule: a
// healthy prom lane must not mask a loki lane that replayed queries and compared
// none of them. The aggregate compared-count is deliberately non-zero here (40
// prom matches), so only the per-head split can catch this — which is the whole
// reason the report carries one.
func TestEvalVerify_DeadLaneBlocks(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 52, Match: 40, Unsupported: 12, ComparedSeries: 40, ComparedUnits: 40},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadProm, migrateverify.Summary{Total: 40, Match: 40, ComparedSeries: 40}),
			matrixLane(migrateverify.HeadLoki, migrateverify.Summary{Total: 12, Unsupported: 12}),
		},
	}
	dec := evalWithVerify(t, rep)
	if dec.Pass {
		t.Fatalf("a lane that compared nothing must fail the gate, got PASS: %+v", dec)
	}
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictFail || !st.Blocking {
		t.Errorf("verify stage = %q blocking=%v, want fail + blocking", st.Verdict, st.Blocking)
	}
	if !containsSubstr(st.Reasons, "head loki family metric-matrix compared nothing") {
		t.Errorf("the blocking reason must name the dead lane, got %+v", st.Reasons)
	}
	// The healthy lane must not be accused: naming prom here would send the
	// operator to fix a backend that answered every query.
	if containsSubstr(st.Reasons, "head prom family metric-matrix compared nothing") {
		t.Errorf("the healthy prom lane must not be reported dead, got %+v", st.Reasons)
	}
}

// TestEvalVerify_HealthyLanesDoNotBlock is the other half of the dead-lane rule:
// two lanes that each compared something pass, so the guard cannot be satisfied
// by simply blocking every multi-head report.
func TestEvalVerify_HealthyLanesDoNotBlock(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 5, Match: 5, ComparedSeries: 9, ComparedUnits: 9},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadProm, migrateverify.Summary{Total: 3, Match: 3, ComparedSeries: 5}),
			matrixLane(migrateverify.HeadLoki, migrateverify.Summary{Total: 2, Match: 2, ComparedSeries: 4}),
		},
	}
	dec := evalWithVerify(t, rep)
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictPass {
		t.Errorf("two healthy lanes must PASS, got %q reasons=%+v", st.Verdict, st.Reasons)
	}
}

// TestEvalVerify_UnsupportedOnlyLaneStillBlocksEvenWhenItIsTheOnlyLane pins that
// the per-lane rule is not a replacement for the aggregate one: a single lane
// that compared nothing must still block, with the aggregate reason.
func TestEvalVerify_UnsupportedOnlyLaneStillBlocks(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 4, Unsupported: 4},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadTempo, migrateverify.Summary{Total: 4, Unsupported: 4}),
		},
	}
	dec := evalWithVerify(t, rep)
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictFail || !st.Blocking {
		t.Errorf("an all-unsupported single-lane run must block, got %q blocking=%v", st.Verdict, st.Blocking)
	}
	if !containsSubstr(st.Reasons, "nothing actually compared") {
		t.Errorf("want the aggregate nothing-compared reason, got %+v", st.Reasons)
	}
}

// TestEvalVerify_UnconfiguredBlocks pins that a replayable query whose head lane
// had no backend pair FAILS the cutover gate rather than warning. Everything else
// in the report is clean and the aggregate compared-count is healthy, so a WARN
// here would ship a green go/no-go over queries the report itself calls unjudged.
func TestEvalVerify_UnconfiguredBlocks(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 3, Match: 3, Unconfigured: 12, ComparedSeries: 3, ComparedUnits: 3},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadProm, migrateverify.Summary{Total: 3, Match: 3, ComparedSeries: 3}),
			{Head: migrateverify.HeadLoki, Summary: migrateverify.Summary{Unconfigured: 12}},
		},
		Unconfigured: []migrateverify.UnconfiguredEntry{
			{Source: "panel:api-logs", Expr: `sum(rate({job="api"}[5m]))`, Head: migrateverify.HeadLoki, Lang: "logql", Reason: "no backend pair"},
		},
	}
	dec := evalWithVerify(t, rep)
	if dec.Pass {
		t.Fatalf("unconfigured lanes must fail the gate, got PASS: %+v", dec)
	}
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictFail || !st.Blocking {
		t.Errorf("verify stage = %q blocking=%v, want fail + blocking (never WARN)", st.Verdict, st.Blocking)
	}
	if !containsSubstr(st.Reasons, "12 replayable queries had no configured backend pair") {
		t.Errorf("the blocking reason must count the unjudged queries, got %+v", st.Reasons)
	}
	// The remedy must be actionable: configure the lane, or narrow the corpus.
	// There is deliberately no third option that makes the gate ignore them.
	if !containsSubstr(st.Reasons, "--ref-*") {
		t.Errorf("the reason must point at the missing backend flags, got %+v", st.Reasons)
	}
}

// TestEvalVerify_RejectsV1Artifact pins the schema bump: a version-1 verify.json
// carries no heads and no unconfigured bucket, so decoding it under this build
// would zero-fill both into "every lane configured, none dead" — a silent green.
// It must be rejected outright, naming both versions.
func TestEvalVerify_RejectsV1Artifact(t *testing.T) {
	verify := rawArtifact(t, "verify.json",
		`{"schema_version":1,"params":{"tolerance":1e-9},"summary":{"total":3,"match":3},"results":[]}`)
	in := migrategate.Inputs{Verify: verify, Classify: cleanClassify(t), RuleGraph: cleanRuleGraph(t)}
	_, err := migrategate.Evaluate(in, migrategate.Options{})
	if err == nil {
		t.Fatal("a version-1 verify artifact must hard error, not decode to a zero-filled PASS")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unsupported schema_version 1") {
		t.Errorf("the rejection must name the version found on disk, got: %v", err)
	}
	if !strings.Contains(msg, fmt.Sprintf("this gate understands %d", migrateverify.ReportVersion)) {
		t.Errorf("the rejection must name the version this build expects, got: %v", err)
	}
}

// TestEvalVerify_OutOfScopeCaveatNamesTheShapes pins the caveat wording: an
// out-of-scope entry is no longer "not PromQL" — the metric lanes of all three
// heads are judged, so the honest caveat names the shapes that have no metric
// baseline at all.
func TestEvalVerify_OutOfScopeCaveatNamesTheShapes(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 2, Match: 2, OutOfScope: 3, ComparedSeries: 2, ComparedUnits: 2},
		Heads: []migrateverify.HeadSummary{
			matrixLane(migrateverify.HeadProm, migrateverify.Summary{Total: 2, Match: 2, ComparedSeries: 2}),
		},
	}
	dec := evalWithVerify(t, rep)
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictWarn {
		t.Errorf("out-of-scope entries WARN (they are not a wrong answer), got %q", st.Verdict)
	}
	if !containsSubstr(st.Reasons, "trace-search / compare / unparseable / unknown-language") {
		t.Errorf("the caveat must name the shapes with no comparator, got %+v", st.Reasons)
	}
	if containsSubstr(st.Reasons, "not PromQL") {
		t.Errorf("the caveat must not claim non-PromQL is out of scope — those lanes are judged, got %+v", st.Reasons)
	}
}
