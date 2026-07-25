package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// gateGolden is the committed decision each archetype's gate run is compared
// against by the tier's whole-run fold.
const gateGolden = "gate.json"

// gateNoGoExit is the exit status `cerberus migrate gate` leaves with when a
// blocking stage says no-go. It mirrors the CLI's own gateExitFail and is
// distinct from 1, which is what any other tool error exits with — an operator
// script that treats "the gate refused" and "the gate crashed" alike would tear
// down a cluster on a typo.
const gateNoGoExit = 3

// gateGenericErrorExit is the status the CLI leaves with on any other failure.
// The no-go status must never collapse onto it.
const gateGenericErrorExit = 1

// verifyFlag is the input whose absence blocks a cutover for want of parity
// evidence; the gate's reason for the blocking stage must name it so the
// operator knows what to produce.
const verifyFlag = "--verify"

// registerGateSteps binds the MIG-26 steps.
func (w *World) registerGateSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the classification and the rule graph produced offline for each tagged archetype$`,
		w.givenClassifyAndRuleGraph)
	ctx.Step(`^no query-parity evidence, because proving parity needs a live reference backend$`,
		w.givenNoParityEvidence)
	ctx.Step(`^the operator asks the cutover gate for a go or no-go decision$`, w.whenGate)
	ctx.Step(`^the gate returns a no-go decision$`, w.thenGateSaysNoGo)
	ctx.Step(`^the gate leaves with its documented no-go status rather than a tool-error status$`,
		w.thenGateLeavesWithNoGoStatus)
	ctx.Step(`^the gate names query parity among the artifacts it is missing$`, w.thenGateNamesMissingParity)
	ctx.Step(`^the gate still records a verdict for every stage in its checklist$`, w.thenGateRecordsEveryStage)
	ctx.Step(`^the gate states, for the blocking stage, why it cannot prove that stage safe$`,
		w.thenGateExplainsTheBlockingStage)
}

// givenClassifyAndRuleGraph produces the two required artifacts the gate can be
// given offline: the classification ledger and the rule graph.
func (w *World) givenClassifyAndRuleGraph() error {
	if err := w.givenHarvestedCorpus(); err != nil {
		return err
	}
	if err := w.whenClassify(); err != nil {
		return err
	}
	return w.whenBuildRuleGraph()
}

// givenNoParityEvidence asserts the scenario really is running without a parity
// artifact: proving parity replays queries against a reference backend, which
// no Tier-0 scenario has. It is a PRECONDITION the run is checked against, not
// a scenario opting out of an assertion — the whole point of MIG-26 is that the
// gate must refuse precisely because this evidence is absent.
func (w *World) givenNoParityEvidence() error {
	if err := w.requireArchetypes("the parity precondition"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		matches, err := filepath.Glob(filepath.Join(w.work, a, "verify*.json"))
		if err != nil {
			return fmt.Errorf("archetype %s: scan the workspace for parity evidence: %w", a, err)
		}
		if len(matches) > 0 {
			return fmt.Errorf("archetype %s: the workspace already holds parity evidence %v; the scenario asserts the gate's behaviour WITHOUT it",
				a, matches)
		}
	}
	return nil
}

// whenGate folds each archetype's offline artifacts into one decision, keeping
// the exit code as data.
func (w *World) whenGate() error {
	if err := w.requireArchetypes("the cutover gate"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		run, err := w.runGate(a, gateGolden)
		if err != nil {
			return err
		}
		w.gate = run
	}
	return nil
}

// runGate runs one `migrate gate` over an archetype's classification and rule
// graph, deliberately WITHOUT a --verify artifact, and decodes the decision.
func (w *World) runGate(archetype, outName string) (gateRun, error) {
	classify := filepath.Join(w.work, archetype, classifyGolden)
	rulegraph := filepath.Join(w.work, archetype, ruleGraphGolden)
	for _, p := range []string{classify, rulegraph} {
		if _, err := os.Stat(p); err != nil {
			return gateRun{}, fmt.Errorf("archetype %s: %s was never produced: %w", archetype, p, err)
		}
	}
	out, err := w.workPath(archetype, outName)
	if err != nil {
		return gateRun{}, err
	}
	res, err := w.run(nil,
		"migrate", "gate", "--classify", classify, "--rulegraph", rulegraph, "--json", "--out", out)
	if err != nil {
		return gateRun{}, err
	}
	// Assert against the environment this exact command actually ran under,
	// not a freshly re-derived one — see World.envUsed.
	if err := lib.RequireOffline(res.Env); err != nil {
		return gateRun{}, err
	}
	data, err := lib.ReadArtifact(out)
	if err != nil {
		return gateRun{}, err
	}
	var dec migrategate.Decision
	if err := lib.DecodeArtifact(data, &dec); err != nil {
		return gateRun{}, fmt.Errorf("archetype %s: %w", archetype, err)
	}
	return gateRun{ran: true, decision: dec, exitCode: res.ExitCode, raw: data, env: res.Env}, nil
}

// thenGateSaysNoGo asserts the fold refused, in both the field a script reads
// and the word an operator reads.
func (w *World) thenGateSaysNoGo() error {
	dec, err := w.gateDecision()
	if err != nil {
		return err
	}
	if dec.Pass {
		return fmt.Errorf("the gate passed the cutover with no parity evidence: %+v", dec)
	}
	if dec.Overall != migrategate.OverallFail {
		return fmt.Errorf("the gate reports %q rather than %q", dec.Overall, migrategate.OverallFail)
	}
	return nil
}

// thenGateLeavesWithNoGoStatus asserts the process status distinguishes a
// refusal from a crash. Asserting merely "non-zero" would be satisfied by the
// tool falling over, which is not a no-go decision at all.
func (w *World) thenGateLeavesWithNoGoStatus() error {
	if !w.gate.ran {
		return fmt.Errorf("the cutover gate has not been run")
	}
	if w.gate.exitCode == gateGenericErrorExit {
		return fmt.Errorf("the gate left with the generic tool-error status %d; a refusal must be distinguishable from a crash",
			gateGenericErrorExit)
	}
	if w.gate.exitCode != gateNoGoExit {
		return fmt.Errorf("the gate left with status %d, not its documented no-go status %d", w.gate.exitCode, gateNoGoExit)
	}
	return nil
}

// thenGateNamesMissingParity asserts the decision names the parity stage among
// the artifacts it did not receive.
func (w *World) thenGateNamesMissingParity() error {
	dec, err := w.gateDecision()
	if err != nil {
		return err
	}
	for _, m := range dec.Missing {
		if m == migrategate.StageVerify {
			return nil
		}
	}
	return fmt.Errorf("the gate lists %v as missing, without naming %q", dec.Missing, migrategate.StageVerify)
}

// thenGateRecordsEveryStage asserts the checklist is complete: every stage the
// gate folds carries a verdict. A decision that aborted after its first blocker
// would leave the operator without the rest of the picture.
func (w *World) thenGateRecordsEveryStage() error {
	dec, err := w.gateDecision()
	if err != nil {
		return err
	}
	want := map[string]bool{
		migrategate.StageVerify:    false,
		migrategate.StageClassify:  false,
		migrategate.StageInventory: false,
		migrategate.StageRuleGraph: false,
	}
	for _, s := range dec.Stages {
		seen, known := want[s.Stage]
		if !known {
			return fmt.Errorf("the gate reports an unknown stage %q", s.Stage)
		}
		if seen {
			return fmt.Errorf("the gate reports stage %q twice", s.Stage)
		}
		want[s.Stage] = true
		if strings.TrimSpace(s.Verdict) == "" {
			return fmt.Errorf("stage %q carries no verdict", s.Stage)
		}
	}
	var missing []string
	for stage, seen := range want {
		if !seen {
			missing = append(missing, stage)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the gate's checklist omits %v", missing)
	}
	if len(dec.Stages) != len(want) {
		return fmt.Errorf("the gate records %d stages, want %d", len(dec.Stages), len(want))
	}
	return nil
}

// thenGateExplainsTheBlockingStage asserts exactly one stage blocks, that it is
// the parity stage, and that its entry says WHY: required, not supplied, and
// naming the input the operator has to produce. Pinning the blocker to a single
// named stage is what makes this scenario sharp — the fixture is clean enough
// that parity evidence is the only thing missing, so a second blocker appearing
// is a real regression rather than noise.
func (w *World) thenGateExplainsTheBlockingStage() error {
	dec, err := w.gateDecision()
	if err != nil {
		return err
	}
	var blocking []migrategate.StageResult
	for _, s := range dec.Stages {
		if s.Blocking {
			blocking = append(blocking, s)
		}
	}
	if len(blocking) != 1 {
		names := make([]string, 0, len(blocking))
		for _, s := range blocking {
			names = append(names, s.Stage)
		}
		return fmt.Errorf("the gate blocks on %v; this corpus is clean apart from its missing parity evidence, so parity must be the only blocker", names)
	}
	s := blocking[0]
	if s.Stage != migrategate.StageVerify {
		return fmt.Errorf("the gate blocks on %q rather than %q", s.Stage, migrategate.StageVerify)
	}
	if !s.Required {
		return fmt.Errorf("the gate blocks on %q while reporting it is not required", s.Stage)
	}
	if s.Present {
		return fmt.Errorf("the gate reports %q as present while blocking for want of it", s.Stage)
	}
	if s.Verdict != migrategate.VerdictMissing {
		return fmt.Errorf("the gate records %q as %q rather than %q", s.Stage, s.Verdict, migrategate.VerdictMissing)
	}
	if len(s.Reasons) == 0 {
		return fmt.Errorf("the gate blocks on %q without stating a reason", s.Stage)
	}
	for _, r := range s.Reasons {
		if strings.Contains(r, verifyFlag) {
			return nil
		}
	}
	return fmt.Errorf("the gate's reasons for %q (%v) never name %s, so the operator is not told what to produce",
		s.Stage, s.Reasons, verifyFlag)
}

// gateDecision returns the decision the scenario's gate run produced, failing
// when no gate has been run.
func (w *World) gateDecision() (migrategate.Decision, error) {
	if !w.gate.ran {
		return migrategate.Decision{}, fmt.Errorf("the cutover gate has not been run")
	}
	return w.gate.decision, nil
}
