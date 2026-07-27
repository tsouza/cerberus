package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrateverify"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// The four committed Tier-1 verify corpora, all hand-authored against the
// three-signal archetype's seed/fixture.json data declaration — the only
// archetype any `@tier1` scenario replays a `cerberus migrate verify` corpus
// against (MIG-02/06/07/08 drive the kube-prometheus-stack archetype the
// stack also seeds, but through `inventory` / the ingest-bridge / scrape
// parity / fault injection, never through `verify`). verifyCorpusFull is the
// breadth pass MIG-16
// drives; verifyCorpusHotspots is the semantic-hotspot subset (rate/increase
// over the seeded counter, absence over a genuinely never-seeded series
// selector, histogram_quantile over the seeded classic histogram) MIG-17
// drives; verifyCorpusLabels is the resource-attribute/grouping subset
// MIG-11 drives; verifyCorpusHistogram is the temporality/bucket-layout
// subset MIG-12 drives. All four live under the archetype's seed/ directory
// because they are tightly coupled to that declaration's metric and label
// names, not to its rules/dashboards fixtures (which exist for the unrelated
// Tier-0 harvest scenarios and use different, unseeded metric names).
const (
	verifyCorpusFull      = "verify-corpus.json"
	verifyCorpusHotspots  = "verify-hotspots.json"
	verifyCorpusLabels    = "verify-labels.json"
	verifyCorpusHistogram = "verify-histogram.json"
)

// verifyReportName is the workspace filename each archetype's `migrate
// verify --json --out` artifact is written to.
const verifyReportName = "verify.json"

// liveProbeBudget bounds how long "the dual-backend stack is live" waits for every
// endpoint. The stack `just migration-tier1-up` brings up already waits
// `--wait` on container healthchecks, so this only guards a stale env var
// left over from a torn-down run.
const liveProbeBudget = 2 * time.Minute

// registerVerifySteps binds the MIG-16 / MIG-17 Tier-1 dual-backend steps:
// establishing the live stack, selecting a committed verify corpus, driving
// `cerberus migrate verify` against it, and asserting the honesty-contract
// shape of the returned report (docs/migration-testing.md section 5) rather
// than merely its exit code.
func (w *World) registerVerifySteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the dual-backend stack is live$`, w.givenTier1Live)
	ctx.Step(`^the committed differential-parity corpus for each tagged archetype$`, w.givenParityCorpus)
	ctx.Step(`^the committed semantic-hotspot corpus for each tagged archetype$`, w.givenHotspotCorpus)
	ctx.Step(`^the committed label-mapping corpus for each tagged archetype$`, w.givenLabelMappingCorpus)
	ctx.Step(`^the committed histogram-fidelity corpus for each tagged archetype$`, w.givenHistogramFidelityCorpus)
	ctx.Step(`^the operator verifies the corpus against the incumbent$`, w.whenVerifyCorpus)
	ctx.Step(`^the verify report replayed more than zero queries$`, w.thenReplayedSomething)
	ctx.Step(`^every configured head returned at least one comparison unit$`, w.thenEveryConfiguredHeadCompared)
	ctx.Step(`^the diverge count is exactly zero$`, w.thenDivergeIsZero)
	ctx.Step(`^no family compared zero evidence$`, w.thenNoDeadFamily)
	ctx.Step(`^the verify command's exit status agrees with the report's own verdict$`, w.thenExitAgreesWithVerdict)
	ctx.Step(`^every hotspot query is individually evidenced, not only the aggregate$`,
		w.thenEveryQueryIndividuallyEvidenced)
}

// givenTier1Live reads the live Tier-1 endpoints from the environment
// `just migration-tier1-up` / `just migration-tier1-seed` publish, and proves
// every one of them answers before any later step drives a command against
// them — a Tier-1 scenario against a half-torn-down stack must fail here,
// naming the unreachable endpoint, rather than deep inside `migrate verify`
// with a bare connection-refused.
func (w *World) givenTier1Live() error {
	le, err := lib.LoadLiveEndpoints()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveProbeBudget)
	defer cancel()
	if err := le.RequireLive(ctx); err != nil {
		return err
	}
	w.live, w.liveSet = le, true
	return nil
}

// givenParityCorpus selects the breadth corpus MIG-16 replays.
func (w *World) givenParityCorpus() error {
	if err := w.requireArchetypes("the differential-parity corpus"); err != nil {
		return err
	}
	w.verifyCorpusFile = verifyCorpusFull
	return w.requireVerifyCorpusFixtures()
}

// givenHotspotCorpus selects the semantic-hotspot subset MIG-17 replays.
func (w *World) givenHotspotCorpus() error {
	if err := w.requireArchetypes("the semantic-hotspot corpus"); err != nil {
		return err
	}
	w.verifyCorpusFile = verifyCorpusHotspots
	return w.requireVerifyCorpusFixtures()
}

// givenLabelMappingCorpus selects the resource-attribute/grouping subset
// MIG-11 replays.
func (w *World) givenLabelMappingCorpus() error {
	if err := w.requireArchetypes("the label-mapping corpus"); err != nil {
		return err
	}
	w.verifyCorpusFile = verifyCorpusLabels
	return w.requireVerifyCorpusFixtures()
}

// givenHistogramFidelityCorpus selects the temporality/bucket-layout subset
// MIG-12 replays.
func (w *World) givenHistogramFidelityCorpus() error {
	if err := w.requireArchetypes("the histogram-fidelity corpus"); err != nil {
		return err
	}
	w.verifyCorpusFile = verifyCorpusHistogram
	return w.requireVerifyCorpusFixtures()
}

// requireVerifyCorpusFixtures asserts every tagged archetype actually ships
// the selected committed corpus. A missing fixture would otherwise surface
// as a confusing "read corpus: no such file" out of the CLI child process
// rather than a harness-level "this archetype has no such fixture".
func (w *World) requireVerifyCorpusFixtures() error {
	for _, a := range w.archetypes {
		p := w.verifyCorpusPath(a)
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("archetype %s ships no committed verify corpus at %s: %w", a, p, err)
		}
	}
	return nil
}

// verifyCorpusPath resolves the selected corpus fixture for one archetype.
func (w *World) verifyCorpusPath(archetype string) string {
	return harnessPath(w.root, archetypeDir, archetype, "seed", w.verifyCorpusFile)
}

// whenVerifyCorpus drives `cerberus migrate verify` for every tagged
// archetype, over the manifest's exact [VerifyStart, VerifyEnd] window — the
// seeder's published fixture window, never a live `[-1h, now]`, because the
// fixture stopped moving the moment `migration-tier1-seed` returned and a
// window that keeps sliding cannot be compared. Corpus, backends and window
// reach the command through a cerberus.yaml rather than through flags: that is
// the shape docs/migration.md hands an operator, so it is the shape this
// scenario proves. It captures the exit code as data (never treats a non-zero
// parity-gate exit as a harness error) and decodes the `--json` artifact into a
// typed Report every Then step reads.
func (w *World) whenVerifyCorpus() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; the scenario must establish it first")
	}
	if w.verifyCorpusFile == "" {
		return fmt.Errorf("no verify corpus has been selected; the scenario must establish one first")
	}
	if err := w.requireArchetypes("the verify replay"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		manifest, err := w.live.LoadManifest(a)
		if err != nil {
			return err
		}
		w.manifest[a] = manifest
		dir, err := w.workDir(a)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, verifyReportName)
		res, err := w.runLiveConfigFile(dir, []lib.Setting{
			{Key: "CERBERUS_VERIFY_CORPUS", Value: w.verifyCorpusPath(a)},
			{Key: "CERBERUS_VERIFY_REF", Value: w.live.PromURL},
			{Key: "CERBERUS_VERIFY_CERBERUS", Value: w.live.CerberusURL},
			{Key: "CERBERUS_VERIFY_START", Value: manifest.VerifyStart.UTC().Format(time.RFC3339Nano)},
			{Key: "CERBERUS_VERIFY_END", Value: manifest.VerifyEnd.UTC().Format(time.RFC3339Nano)},
			{Key: "CERBERUS_VERIFY_STEP", Value: manifest.Step},
		}, "migrate", "verify", "--json", "--out", out)
		if err != nil {
			return err
		}
		w.verifyExitCode[a] = res.ExitCode
		data, err := lib.ReadArtifact(out)
		if err != nil {
			return fmt.Errorf("archetype %s: `cerberus migrate verify` exited %d without writing a report (stderr: %s): %w",
				a, res.ExitCode, strings.TrimSpace(string(res.Stderr)), err)
		}
		var rep migrateverify.Report
		if err := lib.DecodeArtifact(data, &rep); err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		w.verifyRaw[a], w.verifyReport[a] = data, rep
	}
	return nil
}

// eachVerifyReport runs fn over every tagged archetype's decoded report,
// failing when the When step never produced one.
func (w *World) eachVerifyReport(fn func(archetype string, rep migrateverify.Report) error) error {
	if err := w.requireArchetypes("the verify assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		rep, ok := w.verifyReport[a]
		if !ok {
			return fmt.Errorf("archetype %s has no verify report; the scenario must run verify first", a)
		}
		if err := fn(a, rep); err != nil {
			return err
		}
	}
	return nil
}

// thenReplayedSomething asserts the run is not JudgedNothing: a corpus whose
// every entry routed out of scope, or an empty corpus, leaves Total at zero,
// and two empty matrices agreeing proves nothing — see
// docs/migration-testing.md section 5, "a zero diverge count is necessary,
// not sufficient".
func (w *World) thenReplayedSomething() error {
	return w.eachVerifyReport(func(a string, rep migrateverify.Report) error {
		if rep.Summary.Total == 0 {
			return fmt.Errorf("archetype %s: the verify report replayed no query at all", a)
		}
		return nil
	})
}

// thenEveryConfiguredHeadCompared asserts every head lane the operator
// actually configured (--ref/--cerberus supplied) compared at least one real
// unit, and that at least one head was configured in the first place. A
// configured head that compared zero units scored a match by proving
// nothing — see Report.DeadFamilies.
func (w *World) thenEveryConfiguredHeadCompared() error {
	return w.eachVerifyReport(func(a string, rep migrateverify.Report) error {
		var configured int
		for _, h := range rep.Heads {
			if !h.Configured {
				continue
			}
			configured++
			if h.Summary.ComparedUnits == 0 {
				return fmt.Errorf("archetype %s: head %q is configured but compared zero units", a, h.Head)
			}
		}
		if configured == 0 {
			return fmt.Errorf("archetype %s: the verify report configured no backend lane at all", a)
		}
		return nil
	})
}

// thenDivergeIsZero is the exact-parity assertion (comparison mode one in
// docs/migration-testing.md section 5): a real divergence is a cerberus bug
// until fixed, never tolerated in place.
func (w *World) thenDivergeIsZero() error {
	return w.eachVerifyReport(func(a string, rep migrateverify.Report) error {
		if rep.Summary.Diverge != 0 {
			return fmt.Errorf("archetype %s: %d quer(y/ies) diverged: %s", a, rep.Summary.Diverge, firstDivergence(rep))
		}
		return nil
	})
}

// thenNoDeadFamily asserts no (head, result-kind) family replayed comparison
// units without comparing any of them — a family made entirely of
// out-of-scope or unconfigured entries still must not silently vouch for
// parity it never checked.
func (w *World) thenNoDeadFamily() error {
	return w.eachVerifyReport(func(a string, rep migrateverify.Report) error {
		if dead := rep.DeadFamilies(); len(dead) > 0 {
			return fmt.Errorf("archetype %s: %d famil(y/ies) replayed without comparing any evidence: %+v",
				a, len(dead), dead)
		}
		return nil
	})
}

// thenExitAgreesWithVerdict asserts the CLI's own process exit status agrees
// with the report it just wrote — catching a plumbing bug where the report
// and the exit code could disagree, rather than trusting either alone.
func (w *World) thenExitAgreesWithVerdict() error {
	return w.eachVerifyReport(func(a string, rep migrateverify.Report) error {
		code, ok := w.verifyExitCode[a]
		if !ok {
			return fmt.Errorf("archetype %s: no recorded verify exit status", a)
		}
		failed := rep.Failed()
		switch {
		case !failed && code != 0:
			return fmt.Errorf("archetype %s: the report shows no failure but the command exited %d", a, code)
		case failed && code == 0:
			return fmt.Errorf("archetype %s: the report shows a failure but the command exited zero", a)
		}
		return nil
	})
}

// thenEveryQueryIndividuallyEvidenced asserts every per-query result in the
// hotspot run carries its own query and its own non-zero comparison-unit
// count, so a bug hiding in one hotspot query can never be masked by the
// aggregate summary matching — the "per-query max/median divergence
// reported" half of MIG-17's PASS assertion.
func (w *World) thenEveryQueryIndividuallyEvidenced() error {
	return w.eachVerifyReport(func(a string, rep migrateverify.Report) error {
		if len(rep.Results) == 0 {
			return fmt.Errorf("archetype %s: the report carries no per-query results at all", a)
		}
		for _, r := range rep.Results {
			if r.Expr == "" {
				return fmt.Errorf("archetype %s: a result names no query", a)
			}
			if r.ComparedUnits == 0 {
				return fmt.Errorf("archetype %s: query %q (%s) compared zero units, so its verdict proves nothing",
					a, r.Expr, r.Source)
			}
		}
		return nil
	})
}

// firstDivergence renders the first diverging result's detail, so a failing
// scenario names the actual query and reason rather than only a count.
func firstDivergence(rep migrateverify.Report) string {
	for _, r := range rep.Results {
		if r.Verdict != migrateverify.VerdictDiverge {
			continue
		}
		if r.FirstDiff != nil {
			return fmt.Sprintf("%s (%s): %s", r.Source, r.Expr, r.FirstDiff.Reason)
		}
		return fmt.Sprintf("%s (%s): %s", r.Source, r.Expr, r.Detail)
	}
	return "no diverging result recorded"
}
