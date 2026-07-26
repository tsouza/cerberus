package steps

import (
	"context"
	"errors"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// errTier2NotLive is every Tier-2 step's precondition failure when a
// scenario never ran "the tier-2 ruler stack is live" first.
var errTier2NotLive = errors.New("the tier-2 ruler stack has not been established; the scenario must establish it first")

// tier2LiveProbeBudget bounds "the tier-2 ruler stack is live"'s wait for
// every endpoint. The stack `just migration-tier2-up` brings up already
// waits `--wait` on container healthchecks, so this only guards a stale env
// var left over from a torn-down run.
const tier2LiveProbeBudget = 2 * time.Minute

// registerTier2LiveSteps binds the one precondition every Tier-2 scenario
// shares: the ruler stack (Grafana, the dead-end receiver, cerberus, and the
// MIG-13/MIG-19 write-back bridge) answers.
func (w *World) registerTier2LiveSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the tier-2 ruler stack is live$`, w.givenTier2Live)
}

// givenTier2Live reads the live Tier-2 endpoints from the environment `just
// migration-tier2-up` publishes, and proves every one of them answers before
// any later step drives a command against them.
func (w *World) givenTier2Live() error {
	e := lib.LoadTier2Endpoints()
	ctx, cancel := context.WithTimeout(context.Background(), tier2LiveProbeBudget)
	defer cancel()
	if err := e.RequireLive(ctx); err != nil {
		return err
	}
	w.tier2, w.tier2Set = e, true
	return nil
}

// requireTier2Live fails a later step with a precise, actionable message
// when the scenario never established the live stack.
func (w *World) requireTier2Live() error {
	if !w.tier2Set {
		return errTier2NotLive
	}
	return nil
}
