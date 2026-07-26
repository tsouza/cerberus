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
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// tier2LiveState is one Tier-2 live scenario's accumulated state: the
// endpoints it is bound to, whether the live precondition ran, the most
// recently polled rule groups, and the dead-end receiver's notification
// count as observed before the scenario's own trigger — so a Then step
// asserts against a delta, never an absolute count another scenario (or a
// previous run against the same long-lived stack) might have already moved.
type tier2LiveState struct {
	endpoints      lib.Tier2LiveEndpoints
	live           bool
	groups         []rulerRuleGroup
	recordedSeries recordedSeriesPoll
	notifBase      int
	notifSeen      bool
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

	tier2 tier2LiveState
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
		w.tier2 = tier2LiveState{}

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
		if w.work == "" {
			return c, nil
		}
		dir := w.work
		w.work = ""
		if err := os.RemoveAll(dir); err != nil {
			return c, fmt.Errorf("migration harness: remove the scenario workspace %s: %w", dir, err)
		}
		return c, nil
	})

	w.registerSchemaSteps(ctx)
	w.registerFixtureSteps(ctx)
	w.registerHarvestSteps(ctx)
	w.registerClassifySteps(ctx)
	w.registerRuleGraphSteps(ctx)
	w.registerExplainSteps(ctx)
	w.registerLookbackSteps(ctx)
	w.registerGateSteps(ctx)
	w.registerCutoverSteps(ctx)
	w.registerRulerSteps(ctx)
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
