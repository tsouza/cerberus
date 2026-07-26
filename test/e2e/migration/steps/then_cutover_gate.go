package steps

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateverify"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// verifyEvidenceName is the workspace filename the synthetic clean parity
// artifact is written to. Named distinctly from verifyGolden-style names so
// it reads unambiguously as evidence a Given step manufactured, not an
// artifact a `cerberus migrate verify` run produced.
const verifyEvidenceName = "verify-evidence.json"

// gateGoGolden is the committed decision the go-path scenario's gate run is
// compared against — distinct from gateGolden (then_gate.go's no-evidence
// golden), since the two runs feed the gate different inputs and cannot
// share one expected artifact.
const gateGoGolden = "gate-go.json"

// gateOKExit is the exit status `cerberus migrate gate` leaves with on a go
// decision — 0, the ordinary success status, asserted by name here rather
// than a bare literal so a reader comparing it against gateNoGoExit /
// gateGenericErrorExit sees all three exit contracts side by side.
const gateOKExit = 0

// cleanVerifyDataPoints is the compared-unit count the synthetic evidence
// reports for its one query. It only has to be non-zero — evalVerify's
// ComparedUnits==0 guard is what a real empty/all-unsupported run would trip
// — so one is the honest minimum that still proves the guard passes.
const cleanVerifyDataPoints = 1

// registerCutoverGateSteps binds the MIG-24 steps that are NOT already covered
// by registerGateSteps (then_gate.go): establishing CLEAN parity evidence (the
// mirror image of givenNoParityEvidence) and asserting the gate's go path.
// Distinct from then_cutover.go's registerCutoverSteps, which binds MIG-22's
// flip-and-revert and MIG-23's ingest-start boundary against the live stack.
func (w *World) registerCutoverGateSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^clean query-parity evidence, because the reference and cerberus already agree$`,
		w.givenCleanParityEvidence)
	ctx.Step(`^the operator asks the cutover gate for a go or no-go decision using that evidence$`,
		w.whenGateWithVerify)
	ctx.Step(`^the gate returns a go decision$`, w.thenGateSaysGo)
	ctx.Step(`^the gate leaves with a clean exit status rather than its no-go or tool-error status$`,
		w.thenGateLeavesClean)
	ctx.Step(`^the gate reports no blocking stage$`, w.thenGateReportsNoBlockingStage)
}

// givenCleanParityEvidence manufactures a verify.json reporting exactly the
// shape a real `cerberus migrate verify` run leaves behind after a clean
// pass: one query, one compared series, zero divergence, zero error, no
// unconfigured or dead-family gap. It is built from the actual
// migrateverify.Report struct (not hand-typed JSON) precisely so this
// fixture can never drift from the schema evalVerify decodes — a field
// rename on either side breaks the compile, not a silent zero-fill.
func (w *World) givenCleanParityEvidence() error {
	if err := w.requireArchetypes("the parity evidence"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		rep := cleanVerifyReport(a)
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("archetype %s: encode the synthetic clean verify report: %w", a, err)
		}
		out, err := w.workPath(a, verifyEvidenceName)
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, data, workspaceFileMode); err != nil {
			return fmt.Errorf("archetype %s: write the synthetic clean verify report: %w", a, err)
		}
	}
	return nil
}

// workspaceFileMode is the permission a scenario-manufactured artifact gets
// inside the scenario workspace.
const workspaceFileMode = 0o644

// cleanVerifyReport builds the one-query, zero-divergence report
// givenCleanParityEvidence writes to disk.
func cleanVerifyReport(archetype string) migrateverify.Report {
	family := migrateverify.FamilySummary{
		Kind:     migrateverify.KindMetricMatrix,
		Unit:     migrateverify.UnitSeries,
		Total:    cleanVerifyDataPoints,
		Match:    cleanVerifyDataPoints,
		Compared: cleanVerifyDataPoints,
	}
	summary := migrateverify.Summary{
		Total:          cleanVerifyDataPoints,
		Match:          cleanVerifyDataPoints,
		ComparedSeries: cleanVerifyDataPoints,
		ComparedUnits:  cleanVerifyDataPoints,
	}
	return migrateverify.Report{
		SchemaVersion: migrateverify.ReportVersion,
		Summary:       summary,
		Heads: []migrateverify.HeadSummary{{
			Head:       migrateverify.HeadProm,
			Configured: true,
			Summary:    summary,
			Families:   []migrateverify.FamilySummary{family},
		}},
		Results: []migrateverify.QueryResult{{
			Head:           migrateverify.HeadProm,
			Kind:           migrateverify.KindMetricMatrix,
			Source:         fmt.Sprintf("synthetic:%s/clean-parity-evidence", archetype),
			Expr:           "up",
			Verdict:        migrateverify.VerdictMatch,
			ComparedSeries: cleanVerifyDataPoints,
			ComparedUnits:  cleanVerifyDataPoints,
		}},
	}
}

// whenGateWithVerify folds each archetype's offline artifacts PLUS the
// synthetic clean verify evidence into one decision — the mirror image of
// then_gate.go's whenGate, which deliberately omits --verify.
func (w *World) whenGateWithVerify() error {
	if err := w.requireArchetypes("the cutover gate"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		run, err := w.runGateWithVerify(a, gateGoGolden)
		if err != nil {
			return err
		}
		w.gate = run
	}
	return nil
}

// runGateWithVerify is runGate (then_gate.go) plus a --verify flag pointing
// at the archetype's synthetic clean evidence.
func (w *World) runGateWithVerify(archetype, outName string) (gateRun, error) {
	classify, err := w.workPath(archetype, classifyGolden)
	if err != nil {
		return gateRun{}, err
	}
	rulegraph, err := w.workPath(archetype, ruleGraphGolden)
	if err != nil {
		return gateRun{}, err
	}
	verify, err := w.workPath(archetype, verifyEvidenceName)
	if err != nil {
		return gateRun{}, err
	}
	for _, p := range []string{classify, rulegraph, verify} {
		if _, err := os.Stat(p); err != nil {
			return gateRun{}, fmt.Errorf("archetype %s: %s was never produced: %w", archetype, p, err)
		}
	}
	out, err := w.workPath(archetype, outName)
	if err != nil {
		return gateRun{}, err
	}
	res, err := w.run(nil,
		"migrate", "gate",
		"--classify", classify, "--rulegraph", rulegraph, "--verify", verify,
		"--json", "--out", out)
	if err != nil {
		return gateRun{}, err
	}
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

// thenGateSaysGo asserts the fold authorized cutover, in both the field a
// script reads and the word an operator reads.
func (w *World) thenGateSaysGo() error {
	dec, err := w.gateDecision()
	if err != nil {
		return err
	}
	if !dec.Pass {
		return fmt.Errorf("the gate refused the cutover despite clean parity evidence: %+v", dec)
	}
	if dec.Overall != migrategate.OverallPass {
		return fmt.Errorf("the gate reports %q rather than %q", dec.Overall, migrategate.OverallPass)
	}
	return nil
}

// thenGateLeavesClean asserts the process status is the ordinary success
// status — distinguishing "authorized" from both documented refusal
// statuses, the same way thenGateLeavesWithNoGoStatus distinguishes refusal
// from a crash.
func (w *World) thenGateLeavesClean() error {
	if !w.gate.ran {
		return fmt.Errorf("the cutover gate has not been run")
	}
	if w.gate.exitCode != gateOKExit {
		return fmt.Errorf("the gate left with status %d, not its clean status %d", w.gate.exitCode, gateOKExit)
	}
	return nil
}

// thenGateReportsNoBlockingStage asserts every stage the checklist carries
// came back non-blocking — the positive-path counterpart to
// thenGateExplainsTheBlockingStage, which pins the SINGLE blocker the
// no-evidence scenario expects.
func (w *World) thenGateReportsNoBlockingStage() error {
	dec, err := w.gateDecision()
	if err != nil {
		return err
	}
	for _, s := range dec.Stages {
		if s.Blocking {
			return fmt.Errorf("stage %q blocks despite clean parity evidence: %+v", s.Stage, s)
		}
	}
	return nil
}
