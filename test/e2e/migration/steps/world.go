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
	"errors"
	"fmt"
	"os"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateverify"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// tier2RulerState is the MIG-09 ruler scenario's accumulated state beyond
// the live endpoints/liveness flag (tracked separately as World.tier2 /
// tier2Set, shared with the write-back and alerting scenarios): the most
// recently polled rule groups, and the dead-end receiver's notification
// count as observed before the scenario's own trigger — so a Then step
// asserts against a delta, never an absolute count another scenario (or a
// previous run against the same long-lived stack) might have already moved.
type tier2RulerState struct {
	groups    []rulerRuleGroup
	notifBase int
	notifSeen bool
}

// recordedReadBack is the ONE slot every "did the recording rule's output
// come back out of cerberus?" poll publishes into, so the single
// `the recorded series is selectable through cerberus` step definition
// (registered once, in tier2_writeback.go) serves both scenarios that assert
// it. MIG-09 reaches it through an instant query for the recorded name;
// MIG-13's Tier-2 half reaches it through a range query over the window it
// seeded. Both prove the same thing — the write-back landed and reads back —
// so they share one step rather than racing two definitions of one step text
// past godog's Strict ambiguity check.
//
// `polled` distinguishes "a poll ran and saw nothing" (a real failure, with
// `detail` naming what cerberus answered) from "no poll ran at all" (a
// scenario-construction bug, reported as one).
type recordedReadBack struct {
	polled bool
	series int
	detail string
}

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

	// tier2 is the live Tier-2 ruler stack's endpoints, set by "the tier-2
	// ruler stack is live" / "the shadow-ruler stack is live" (one shared
	// precondition, two Gherkin phrasings — see tier2_live.go). tier2Set
	// guards every later tier-2 step so a scenario that skips establishing
	// the stack fails there, naming the missing precondition, rather than
	// deep inside an HTTP call with a nil/zero-value endpoint set.
	tier2    lib.Tier2Endpoints
	tier2Set bool

	// tier2Writeback carries the MIG-13/MIG-19 write-back scenario's state:
	// the seeded input window and the recorded series read back through
	// cerberus. See steps/tier2_writeback.go.
	tier2Writeback tier2WritebackState

	// tier2SeedScope is the identity THIS scenario's Tier-2 source-series seed
	// writes under, and the identity every later step of the same scenario
	// selects on. It is derived per scenario in the Before hook rather than
	// per step, because the seeding Given and the asserting Then must agree on
	// it. See steps/tier2_seed_scope.go for why two Tier-2 scenarios must
	// never share one stored series identity.
	tier2SeedScope string

	// tier2Alerting carries the MIG-18 scenario's captured notification
	// streams. See steps/tier2_alerting.go.
	tier2Alerting tier2AlertingState

	// tier2Ruler carries the MIG-09 ruler scenario's state (rule groups
	// polled, notification delta). See steps/then_ruler.go.
	tier2Ruler tier2RulerState

	// tier2ReadBack is the shared recorded-series read-back slot MIG-09 and
	// MIG-13's Tier-2 half both publish into — see recordedReadBack.
	tier2ReadBack recordedReadBack

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

	// correlation is MIG-21's probe state: the log record and trace the
	// Given step establishes from the rebuilt live fixture, and what the
	// When step's hop through cerberus's own Loki/Tempo read paths resolved.
	correlation correlationProbe

	// tenant is MIG-15's probe state: the foreign-tenant data planted
	// directly in ClickHouse, outside cerberus's configured database, and
	// what cerberus's own query surface did and did not return for it.
	tenant tenantProbe

	// downsample is MIG-20's probe state: the raw and simulated-downsampled
	// series fetched from the live stack and the tolerant comparator's
	// verdict on them.
	downsample downsampleProbe

	// cutover is MIG-22's per-archetype flip/revert probe state, and boundary
	// is MIG-23's per-archetype ingest-start state. Both drive live HTTP/CH
	// probes directly rather than a `cerberus migrate` artifact, mirroring
	// MIG-20's "dedicated comparator, not verify" posture for a shape none of
	// the eight CLI subcommands cover.
	cutover  map[string]cutoverProbe
	boundary map[string]boundaryProbe

	// decommission is MIG-25's per-archetype residual-read-traffic audit
	// state, and retentionGate is MIG-26's tier-1 live-retention-vs-mandate
	// comparison state.
	decommission  map[string]decommissionRun
	retentionGate map[string]retentionGateRun
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
		w.tier2Set = false
		w.tier2SeedScope = tier2SeedScopeFor(sc.Name)
		w.tier2Writeback = tier2WritebackState{}
		w.tier2Alerting = tier2AlertingState{}
		w.tier2Ruler = tier2RulerState{}
		w.tier2ReadBack = recordedReadBack{}
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
		w.correlation = correlationProbe{}
		w.tenant = tenantProbe{}
		w.downsample = downsampleProbe{}
		w.cutover = map[string]cutoverProbe{}
		w.boundary = map[string]boundaryProbe{}
		w.decommission = map[string]decommissionRun{}
		w.retentionGate = map[string]retentionGateRun{}

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
		// Drop the foreign-tenant database MIG-15 planted. It lives on the
		// SHARED live ClickHouse, so leaving it behind leaks one scenario's
		// plant into every later run against that stack — including the next
		// run's own isolation assertion, which would then be judging a row
		// nobody in that run planted. Runs unconditionally, so a scenario that
		// failed mid-plant still cleans up after itself.
		if err := w.dropForeignTenantDatabase(); err != nil {
			faultErr = errors.Join(faultErr, err)
		}
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
	w.registerTier2LiveSteps(ctx)
	w.registerTier2WritebackSteps(ctx)
	w.registerTier2AlertingSteps(ctx)
	w.registerCutoverGateSteps(ctx)
	w.registerRulerSteps(ctx)
	w.registerVerifySteps(ctx)
	w.registerInventorySteps(ctx)
	w.registerIngestBridgeSteps(ctx)
	w.registerScrapeParitySteps(ctx)
	w.registerFaultInjectionSteps(ctx)
	w.registerSchemaLiveSteps(ctx)
	w.registerLabelMappingSteps(ctx)
	w.registerHistogramFidelitySteps(ctx)
	w.registerRecordedSeriesSteps(ctx)
	w.registerLiveRetentionSteps(ctx)
	w.registerTenantIsolationSteps(ctx)
	w.registerDownsampleSteps(ctx)
	w.registerCorrelationSteps(ctx)
	w.registerCutoverSteps(ctx)
	w.registerDecommissionSteps(ctx)
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

// runLiveConfigFile is runLive driven the way docs/migration.md tells an
// operator to drive it: the settings go into a cerberus.yaml in dir, the
// command line carries only the per-run output choices, and the child runs
// from dir so it discovers that file. Configuring a migration by exporting two
// dozen variables is the path almost nobody takes, so it is not the path these
// scenarios prove works.
//
// Nothing separate asserts the file was read: every setting it carries is
// mandatory, so a command that failed to find it exits non-zero naming the
// value it is missing, and RequireSettingsFromFile rejects a command line that
// smuggled one past the file.
func (w *World) runLiveConfigFile(dir string, settings []lib.Setting, args ...string) (lib.Result, error) {
	if w.bin == "" {
		return lib.Result{}, fmt.Errorf("migration harness: no cerberus binary bound to the scenario world")
	}
	if err := lib.WriteConfigFile(dir, settings); err != nil {
		return lib.Result{}, err
	}
	if err := lib.RequireSettingsFromFile(w.bin, args); err != nil {
		return lib.Result{}, err
	}
	return lib.Run(lib.RunSpec{
		Bin:  w.bin,
		Args: args,
		Dir:  dir,
		Env:  lib.LiveEnv(),
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
