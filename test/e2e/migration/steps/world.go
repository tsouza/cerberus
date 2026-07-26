// Package steps is the godog step-definition library for the Layer-14
// migration scenarios. Every step reads the artifact a `cerberus migrate`
// command emitted as a typed value and asserts on its fields, so a change to an
// artifact's shape breaks the compile rather than silently weakening a
// scenario.
//
// The package carries no build tag: it is the assertion machinery, so it is
// compiled by `go build ./...` and linted like production code. Only the tier
// runners that exec a built binary are tagged.
package steps

import (
	"context"
	"fmt"
	"os"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateverify"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// workspacePattern names the per-scenario temporary directory every `--out`
// artifact is written into. It is removed after the scenario, so one scenario
// can never read an artifact another one left behind.
const workspacePattern = "cerberus-migration-tier0-"

// World is the state one scenario accumulates: the repository root every
// fixture path resolves against, the cerberus binary the When steps drive, the
// archetypes the scenario's tags selected, and the typed artifacts each
// command produced.
type World struct {
	root string
	bin  string

	// archetypes is the sorted, deduplicated list from the scenario's
	// `@archetype:` tags. The tags are load-bearing input: every "for each
	// tagged archetype" step loops over exactly this list.
	archetypes []string

	// scenariosRun counts the scenarios that reached their After hook. The
	// runner asserts it is non-zero, so a suite that matched no feature file
	// fails instead of reporting a green run over nothing.
	scenariosRun int

	// work is the per-scenario artifact workspace, created in the Before hook.
	work string

	schema    schemaRender
	retention signalRetention

	// Each map is keyed by archetype name. The raw bytes back the golden
	// comparisons; the decoded values back every assertion on a field.
	corpusRaw    map[string][]byte
	corpus       map[string]migrate.Corpus
	classifyRaw  map[string][]byte
	classify     map[string]migrate.Classification
	ruleGraphRaw map[string][]byte
	ruleGraph    map[string]migrate.RuleGraph
	explainRaw   map[string][]byte
	explain      map[string]explainReport
	lookback     map[string]migrate.Lookback

	// envUsed is the environment the most recent `cerberus migrate` child
	// process for that archetype actually ran under, echoed back from
	// lib.Result rather than re-derived. An offline-proof Then step asserts
	// lib.RequireOffline against THIS value, so a regression in how run()
	// wires the environment into exec.Command fails the scenario instead of
	// being invisible to a check that only re-validates its own construction.
	envUsed map[string][]string

	gate gateRun

	// live is the Tier-1 stack's endpoints, established by "the tier-1 stack
	// is live" — nil-valued (a zero LiveEndpoints) until that Given runs, so
	// a Tier-1 step used from a Tier-0 scenario fails with a clear "establish
	// it first" rather than dialing an empty URL.
	live     lib.LiveEndpoints
	liveSet  bool
	manifest map[string]seed.Manifest

	// verifyCorpusFile names which committed corpus a Tier-1 "When the
	// operator verifies" step replays — set by the Given that selects breadth
	// (MIG-16), a semantic-hotspot subset (MIG-17), the label-mapping subset
	// (MIG-11) or the histogram-fidelity subset (MIG-12), so the When step
	// never guesses which fixture a scenario meant.
	verifyCorpusFile string
	verifyReport     map[string]migrateverify.Report
	verifyRaw        map[string][]byte
	verifyExitCode   map[string]int

	// inventory is MIG-02's live-cardinality-inventory state: the decoded
	// report, the archetype declaration its expectations are checked
	// against, and the deliberately-unreachable probe source the negative
	// case stands up.
	inventory inventoryState

	// bridge is MIG-06's ingest-bridge state: the synthetic OTLP batch a
	// scenario pushed directly at the collector, keyed by the metric shape
	// it named so a Then can look each one back up by kind.
	bridge bridgeState

	// scrape is MIG-07's collector-scrape-parity state: the target both the
	// reference Prometheus and the collector's prometheusreceiver scrape.
	scrape scrapeState

	// fault is MIG-08's fault-injection state: the heavy query's measured
	// latencies and which compose service (if any) is currently paused, so
	// the scenario's own cleanup can always restore it even on failure.
	fault faultState

	// schemaLive is what the MIG-10 tier-1 "diff the rendered schema against
	// the live database" step produced.
	schemaLive schemaLiveDiff

	// expHist is the MIG-12 exponential-histogram probe: one synthetic row
	// seeded per tagged archetype, keyed by archetype, carrying the true
	// quantile the seeding step computed independently and (once the When
	// step has run) cerberus's own answer.
	expHist map[string]expHistProbe

	// recordedSeries is the MIG-13 read-back probe: one synthetic
	// recorded-series row seeded per tagged archetype, as if a ruler's
	// write-back had produced it, keyed by archetype.
	recordedSeries map[string]recordedSeriesProbe
}

// NewWorld binds a World to the repository root and the binary the scenarios
// exec.
func NewWorld(root, bin string) *World {
	return &World{root: root, bin: bin}
}

// ScenariosRun reports how many scenarios completed. The tier runner turns a
// zero count into a failure.
func (w *World) ScenariosRun() int { return w.scenariosRun }

// InitializeScenario registers the hooks and the step definitions with godog.
// The Before hook resets every per-scenario field, so one scenario can never
// assert against an artifact a previous one produced.
func (w *World) InitializeScenario(ctx *godog.ScenarioContext) {
	ctx.Before(func(c context.Context, sc *godog.Scenario) (context.Context, error) {
		w.archetypes = ArchetypesOf(sc.Tags)
		w.schema = schemaRender{}
		w.retention = signalRetention{}
		w.gate = gateRun{}
		w.corpusRaw, w.corpus = map[string][]byte{}, map[string]migrate.Corpus{}
		w.classifyRaw, w.classify = map[string][]byte{}, map[string]migrate.Classification{}
		w.ruleGraphRaw, w.ruleGraph = map[string][]byte{}, map[string]migrate.RuleGraph{}
		w.explainRaw, w.explain = map[string][]byte{}, map[string]explainReport{}
		w.lookback = map[string]migrate.Lookback{}
		w.envUsed = map[string][]string{}
		w.live, w.liveSet = lib.LiveEndpoints{}, false
		w.manifest = map[string]seed.Manifest{}
		w.verifyCorpusFile = ""
		w.verifyReport = map[string]migrateverify.Report{}
		w.verifyRaw = map[string][]byte{}
		w.verifyExitCode = map[string]int{}
		w.inventory = inventoryState{}
		w.bridge = bridgeState{}
		w.scrape = scrapeState{}
		w.fault = faultState{}
		w.schemaLive = schemaLiveDiff{}
		w.expHist = map[string]expHistProbe{}
		w.recordedSeries = map[string]recordedSeriesProbe{}

		if err := requireArchetypeFixtures(w.root, w.archetypes); err != nil {
			return c, err
		}
		dir, err := os.MkdirTemp("", workspacePattern)
		if err != nil {
			return c, fmt.Errorf("migration harness: create the scenario workspace: %w", err)
		}
		w.work = dir
		return c, nil
	})
	ctx.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.scenariosRun++
		// Restore any compose service MIG-08's fault-injection paused,
		// regardless of whether the scenario's own restoring Then step ever
		// ran — a failed assertion mid-fault must never leave the shared
		// Tier-1 stack degraded for the next scenario.
		faultErr := w.restorePausedService()
		if w.work == "" {
			return c, faultErr
		}
		dir := w.work
		w.work = ""
		if err := os.RemoveAll(dir); err != nil {
			if faultErr != nil {
				return c, fmt.Errorf("migration harness: remove the scenario workspace %s: %w (also: %w)", dir, err, faultErr)
			}
			return c, fmt.Errorf("migration harness: remove the scenario workspace %s: %w", dir, err)
		}
		return c, faultErr
	})

	w.registerSchemaSteps(ctx)
	w.registerFixtureSteps(ctx)
	w.registerHarvestSteps(ctx)
	w.registerClassifySteps(ctx)
	w.registerRuleGraphSteps(ctx)
	w.registerExplainSteps(ctx)
	w.registerLookbackSteps(ctx)
	w.registerGateSteps(ctx)
	w.registerVerifySteps(ctx)
	w.registerInventorySteps(ctx)
	w.registerIngestBridgeSteps(ctx)
	w.registerScrapeParitySteps(ctx)
	w.registerFaultInjectionSteps(ctx)
	w.registerSchemaLiveSteps(ctx)
	w.registerLabelMappingSteps(ctx)
	w.registerHistogramFidelitySteps(ctx)
	w.registerRecordedSeriesSteps(ctx)
}

// harnessPath resolves a path inside the harness tree under a repository root.
func harnessPath(root string, elem ...string) string {
	return lib.HarnessPath(root, elem...)
}

// run executes the cerberus binary with the offline environment plus extraEnv,
// from the repository root.
func (w *World) run(extraEnv []string, args ...string) (lib.Result, error) {
	if w.bin == "" {
		return lib.Result{}, fmt.Errorf("migration harness: no cerberus binary bound to the scenario world")
	}
	return lib.Run(lib.RunSpec{
		Bin:  w.bin,
		Args: args,
		Dir:  w.root,
		Env:  lib.OfflineEnv(extraEnv...),
	})
}

// runLive executes the cerberus binary with the live environment (lib.LiveEnv
// — no blackholed network) plus extraEnv, from the repository root. It is the
// Tier-1 counterpart of run: every `cerberus migrate verify` invocation
// against the live stack goes through this, never through run, because run's
// OfflineEnv would blackhole exactly the HTTP routes a Tier-1 command needs.
func (w *World) runLive(extraEnv []string, args ...string) (lib.Result, error) {
	if w.bin == "" {
		return lib.Result{}, fmt.Errorf("migration harness: no cerberus binary bound to the scenario world")
	}
	return lib.Run(lib.RunSpec{
		Bin:  w.bin,
		Args: args,
		Dir:  w.root,
		Env:  lib.LiveEnv(extraEnv...),
	})
}

// gateRun is one `migrate gate` invocation: the decision it emitted and the
// exit code it left with. The exit code is asserted on directly — the gate's
// documented no-go status is part of its contract with the operator's cutover
// script, not an incidental detail.
type gateRun struct {
	ran      bool
	decision migrategate.Decision
	exitCode int
	raw      []byte
	env      []string
}
