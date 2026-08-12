package steps

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrateverify"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// This file holds MIG-18's incumbent-VERSUS-shadow diff: the half its PASS
// cell has always demanded and the substrate could not serve until the Tier-2
// stack grew a second ruler.
//
// The shape, end to end:
//
//  1. One in-memory fixture is written to BOTH backends — ClickHouse (which
//     the shadow ruler reaches through cerberus) and the incumbent ruler's own
//     TSDB (over remote write). Both sides are rendered from the same
//     seed.Fixture, so the two rulers cannot disagree because they read
//     different numbers; only because their engines answered differently.
//  2. Both rulers evaluate the SAME multi-window multi-burn-rate rule set —
//     tiers/tier2-ruler/grafana/alerting/rules.yaml's `migration.mwmbr` group
//     and tiers/tier2-ruler/incumbent/incumbent-rules.yml's, pinned equal by
//     tier2_parity_test.go — and dispatch through their OWN notifier into
//     their OWN dead-end receiver.
//  3. The two captured streams are diffed.
//
// The fixture drives two `slo` identities through those rules: one whose error
// budget is burning (both rulers must page it, on both burn tiers) and one
// whose budget is intact (neither may page it at all). That is what makes the
// diff two-sided. A single burning identity would only ever exercise
// DiffAlertStreams's false-NEGATIVE arm — the shadow failing to fire — and a
// shadow ruler that paged indiscriminately would sail through. The intact
// identity is the false-POSITIVE arm, and it is asserted by name as well as by
// count, because "cerberus made an alert fire that should not have" is the
// migration failure an operator feels first.
//
// What is asserted, and why each is separate:
//
//   - zero false positives and zero false negatives (DiffAlertStreams). This
//     is the verdict that cannot be blamed on scheduling.
//   - the firing edges landed within a derived bound of each other
//     (SkewBoundHolds). Quantization defines "the same evaluation" but throws
//     the magnitude away; this keeps it.
//   - the burn-rate delta holds zero across the FULL bake window
//     (BakeWindowHoldsZero), not at a spot instant — a burn-rate rule
//     evaluated continuously can disagree once and still page wrongly.

// The MWMBR fixture, mirrored from the two rule files rather than invented
// here. tier2_parity_test.go reads BOTH files and fails offline when any of
// these drifts on either side, so a one-sided edit cannot reach a live run and
// silently turn a real diff into a comparison of two different rules.
const (
	mwmbrRequestsMetric = "cerberus_migration_slo_requests_total"
	mwmbrErrorsMetric   = "cerberus_migration_slo_errors_total"
	mwmbrGroup          = "migration.mwmbr"
	mwmbrFolder         = "migration-lane"
	mwmbrServiceName    = "migration-lane"

	// mwmbrSLOKey is the dimension the rules aggregate by, so each SLO is its
	// own alert identity; mwmbrBurnRateKey is the label separating the two burn
	// tiers, so the fast page and the slow ticket are two identities rather
	// than one alert seen twice.
	mwmbrSLOKey      = "slo"
	mwmbrBurnRateKey = "burn_rate"

	// mwmbrBurningSLO's error budget is burning hard enough to trip BOTH burn
	// tiers; mwmbrIntactSLO serves no errors at all and must trip neither.
	mwmbrBurningSLO = "checkout"
	mwmbrIntactSLO  = "search"

	mwmbrFastAlertName = "MigrationSLOFastBurn"
	mwmbrSlowAlertName = "MigrationSLOSlowBurn"
	mwmbrFastSeverity  = "critical"
	mwmbrSlowSeverity  = "warning"
	mwmbrFastBurnLabel = "fast"
	mwmbrSlowBurnLabel = "slow"

	// mwmbrHoldDown must equal both rule sets' `for:`, and mwmbrEvalInterval
	// both groups' `interval:`. The latter is the shared evaluation cadence
	// QuantizeToEvalInterval floors edges to and the term the skew bound is
	// built from.
	mwmbrHoldDown     = 30 * time.Second
	mwmbrEvalInterval = 10 * time.Second
)

// The error budget and burn factors the rules' thresholds are the arithmetic
// of: a 99.9% objective leaves a 0.001 budget, and the two standard
// multi-burn-rate factors turn it into the two thresholds both rule files
// spell out (14.4 * 0.001 and 6 * 0.001).
//
// They are named here rather than only in the YAML because the bake-window
// check reports a BURN RATE — the error ratio expressed in budgets-per-window
// — and dividing by the budget is what makes the number it prints the same
// quantity an SLO runbook talks about.
const (
	mwmbrErrorBudget    = 0.001
	mwmbrFastBurnFactor = 14.4
	mwmbrSlowBurnFactor = 6.0

	// The window pairs, longest first. Each alert requires its long AND its
	// short window over threshold at once; the long window is what stops a
	// spike paging, the short one what lets the alert clear promptly.
	mwmbrFastLongWindow  = 5 * time.Minute
	mwmbrFastShortWindow = time.Minute
	mwmbrSlowLongWindow  = 15 * time.Minute
	mwmbrSlowShortWindow = 3 * time.Minute
)

// Seed geometry.
//
// The window reaches into the FUTURE on purpose. Both rulers evaluate at
// wall-clock "now", and a fixture that stopped at the seeding instant would
// decay out of the rules' own rate windows within a minute — the alert would
// have to fire, be delivered through two notification policies, and be
// observed, all inside the sliver of time before its own input aged away. A
// window running past the observation period instead keeps the condition
// continuously true, so the scenario measures the rulers rather than a race
// against its own fixture.
const (
	mwmbrSeedHistory = 20 * time.Minute
	mwmbrSeedFuture  = 5 * time.Minute
	// mwmbrSeedStep is the sample cadence on both sides. It is the SAME
	// instants on both backends, because both are rendered from one fixture —
	// the property that makes a value comparison meaningful at all.
	mwmbrSeedStep = 15 * time.Second
	// mwmbrBurstStart is how far before the seeding instant the burning SLO
	// starts serving errors. It must be long enough that the LONGEST window
	// (mwmbrSlowLongWindow) already carries enough error mass to clear the
	// slow tier's threshold at the moment the rulers first look, so neither
	// ruler has to wait out a window before it can fire.
	mwmbrBurstStart = 2 * time.Minute
	// The rates the fixture serves, in events per second. The burning SLO's
	// error ratio inside the short window is mwmbrErrorRate/mwmbrRequestRate,
	// an order of magnitude above the fast tier's threshold, so arming is
	// unambiguous rather than borderline — tier2_parity_test.go pins that
	// margin against the thresholds so a future re-tuning that quietly stopped
	// tripping the rules fails in the unit lane.
	mwmbrRequestRate = 10.0
	mwmbrErrorRate   = 1.0
)

// The bake window: the span the burn-rate delta must hold zero across, and how
// finely it is sampled. Both are anchored in SEEDED history rather than to
// wall-clock "now", so the check evaluates the same instants on every run and
// a slow CI machine cannot change what it compared.
const (
	mwmbrBakeWindow = 2 * time.Minute
	mwmbrBakeStep   = 15 * time.Second
)

// Budgets. Each bounds a poll loop and fails its step when it expires.
const (
	// mwmbrFiringWait covers both rulers' next tick, the full hold-down, and
	// each notification policy's group_wait, plus a couple of missed ticks.
	mwmbrFiringWait = 3 * time.Minute
	mwmbrPoll       = 2 * time.Second
	mwmbrSeedBudget = 60 * time.Second
	mwmbrQueryWait  = 60 * time.Second
)

// tier2ParityState is the MIG-18 diff's accumulated state.
type tier2ParityState struct {
	// seedStart/seedEnd bound the fixture window; bakeEnd is the last instant
	// the bake-window sampler evaluates at.
	seedStart, seedEnd time.Time
	// seedVisibilitySpan is the wall-clock time it took to write BOTH
	// backends. It is the second term of the skew bound: the two rulers cannot
	// have seen the fixture at the same instant if the two writes did not land
	// at the same instant, and that difference is a property of this harness
	// rather than of either ruler. Measured, never assumed — an assumed
	// constant here would be a tolerance.
	seedVisibilitySpan time.Duration
	// incumbent and shadow are the firing edges each ruler's own receiver
	// captured, already projected onto ParityLabelKeys.
	incumbent, shadow []AlertEvent
}

// registerTier2ParitySteps binds MIG-18's incumbent-versus-shadow diff.
func (w *World) registerTier2ParitySteps(ctx *godog.ScenarioContext) {
	ctx.Step(
		`^the same error budget burn is seeded to both rulers' backends$`,
		w.givenMwmbrSeededBothSides,
	)
	ctx.Step(
		`^the operator waits for both rulers to report the burn alerts firing$`,
		w.whenMwmbrBothRulersFire,
	)
	ctx.Step(
		`^both rulers paged for the same alerts, with neither raising one the other did not$`,
		w.thenMwmbrStreamsAgree,
	)
	ctx.Step(
		`^neither ruler paged for the service whose error budget was intact$`,
		w.thenMwmbrIntactSLONeverPaged,
	)
	ctx.Step(
		`^the two rulers' firing edges landed within the skew two independent schedulers can differ by$`,
		w.thenMwmbrSkewBounded,
	)
	ctx.Step(
		`^the burn rate the two rulers evaluate holds equal across the whole bake window$`,
		w.thenMwmbrBakeWindowHoldsZero,
	)
}

// givenMwmbrSeededBothSides writes ONE fixture to both backends: ClickHouse,
// which the shadow ruler reads through cerberus, and the incumbent ruler's own
// TSDB over remote write.
//
// One fixture, two renderings — never two generators. seed.Fixture's own
// doc comment states the reason and it is the whole basis of this scenario: two
// independent sample paths cannot land identical timestamps, and a diff over
// inputs that differ at the sample level measures the seeder rather than the
// rulers.
func (w *World) givenMwmbrSeededBothSides() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mwmbrSeedBudget)
	defer cancel()

	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(-mwmbrSeedHistory), now.Add(mwmbrSeedFuture)
	fixture := mwmbrFixture(w.tier2SeedScope, start, end, now.Add(-mwmbrBurstStart))

	conn, err := w.tier2.DialCH(ctx)
	if err != nil {
		return fmt.Errorf("migration harness: dial clickhouse to seed the burn-rate fixture: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// The span is measured across BOTH writes rather than around each, because
	// what the skew bound needs is the outside edge of "when could each ruler
	// first have seen this fixture".
	writeFrom := time.Now().UTC()
	if err := seed.InsertFixture(ctx, conn, fixture); err != nil {
		return fmt.Errorf("migration harness: seed the burn-rate fixture into clickhouse: %w", err)
	}
	if err := seed.WriteSeries(ctx, w.tier2.IncumbentURL, fixture.PromSeries()); err != nil {
		return fmt.Errorf(
			"migration harness: remote-write the burn-rate fixture to the incumbent ruler at %s: %w",
			w.tier2.IncumbentURL, err,
		)
	}
	w.tier2Parity = tier2ParityState{
		seedStart:          start,
		seedEnd:            end,
		seedVisibilitySpan: time.Since(writeFrom),
	}
	return nil
}

// mwmbrFixture builds the two counters, for both SLO identities, over
// [start, end]: requests at a constant rate for both, and errors only for the
// burning SLO and only from burstFrom onward. The intact SLO's error counter
// exists and stays flat rather than being omitted — an ABSENT counter would
// make the rules' ratio a division by a missing series, so the intact identity
// would go unpaged for the wrong reason (no data) instead of the right one
// (a healthy error budget).
func mwmbrFixture(scope string, start, end, burstFrom time.Time) seed.Fixture {
	flat := func(rate float64) func(time.Time) float64 {
		return func(time.Time) float64 { return rate }
	}
	burst := func(t time.Time) float64 {
		if t.Before(burstFrom) {
			return 0
		}
		return mwmbrErrorRate
	}
	return seed.Fixture{Counter: []seed.MetricSeries{
		mwmbrCounter(mwmbrRequestsMetric, mwmbrBurningSLO, scope, start, end, flat(mwmbrRequestRate)),
		mwmbrCounter(mwmbrRequestsMetric, mwmbrIntactSLO, scope, start, end, flat(mwmbrRequestRate)),
		mwmbrCounter(mwmbrErrorsMetric, mwmbrBurningSLO, scope, start, end, burst),
		mwmbrCounter(mwmbrErrorsMetric, mwmbrIntactSLO, scope, start, end, flat(0)),
	}}
}

// mwmbrCounter accumulates one monotonic counter across [start, end], adding
// ratePerSec(t) events per second over each step.
func mwmbrCounter(name, slo, scope string, start, end time.Time, ratePerSec func(time.Time) float64) seed.MetricSeries {
	samples := make([]seed.Sample, 0, int(end.Sub(start)/mwmbrSeedStep)+1)
	total := 0.0
	for t := start; !t.After(end); t = t.Add(mwmbrSeedStep) {
		samples = append(samples, seed.Sample{Time: t, Value: total})
		total += ratePerSec(t) * mwmbrSeedStep.Seconds()
	}
	return seed.MetricSeries{
		MetricName:  name,
		ServiceName: mwmbrServiceName,
		Attributes:  map[string]string{mwmbrSLOKey: slo, tier2SeedScopeLabel: scope},
		Samples:     samples,
	}
}

// mwmbrAlertNames is the set of alert names this scenario compares — both burn
// tiers, so a shadow ruler that got one right and the other wrong fails.
func mwmbrAlertNames() []string { return []string{mwmbrFastAlertName, mwmbrSlowAlertName} }

// whenMwmbrBothRulersFire polls BOTH receivers until each has captured a
// firing edge for every burn tier, then projects both streams onto the
// identity the two rulers share.
//
// Both sides must arrive before the budget expires, and the failure names
// which side is short. That asymmetry matters: "the shadow never fired" and
// "the incumbent never fired" are a cerberus bug and a substrate bug
// respectively, and a single "no edges" message would conflate them.
func (w *World) whenMwmbrBothRulersFire() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	if w.tier2Parity.seedEnd.IsZero() {
		return fmt.Errorf("the burn-rate fixture has not been seeded; the scenario must seed it first")
	}
	ctx := context.Background()
	deadline := time.Now().Add(mwmbrFiringWait)
	var last error
	for {
		incumbent, incErr := fetchAlertEvents(ctx, w.tier2.IncumbentDeadEndURL, mwmbrAlertNames())
		shadow, shErr := fetchAlertEvents(ctx, w.tier2.DeadEndURL, mwmbrAlertNames())
		switch {
		case incErr != nil:
			last = fmt.Errorf("read the incumbent ruler's captured stream: %w", incErr)
		case shErr != nil:
			last = fmt.Errorf("read the shadow ruler's captured stream: %w", shErr)
		default:
			incFiring := filterBySeedScope(filterByStatus(incumbent, alertStatusFiring), w.tier2SeedScope)
			shFiring := filterBySeedScope(filterByStatus(shadow, alertStatusFiring), w.tier2SeedScope)
			incMissing := mwmbrMissingAlertNames(incFiring)
			shMissing := mwmbrMissingAlertNames(shFiring)
			if len(incMissing) == 0 && len(shMissing) == 0 {
				return w.captureMwmbrStreams(incFiring, shFiring)
			}
			last = fmt.Errorf(
				"the incumbent ruler has not yet paged %v and the shadow ruler has not yet paged %v",
				incMissing, shMissing,
			)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("both rulers did not report the burn alerts firing within %s: %w", mwmbrFiringWait, last)
		}
		time.Sleep(mwmbrPoll)
	}
}

// captureMwmbrStreams projects both streams onto the shared identity and
// stores them for the Then steps.
func (w *World) captureMwmbrStreams(incumbent, shadow []AlertEvent) error {
	projectedInc, err := ProjectAlertLabels(incumbent, ParityLabelKeys)
	if err != nil {
		return fmt.Errorf("the incumbent ruler's stream: %w", err)
	}
	projectedShadow, err := ProjectAlertLabels(shadow, ParityLabelKeys)
	if err != nil {
		return fmt.Errorf("the shadow ruler's stream: %w", err)
	}
	w.tier2Parity.incumbent, w.tier2Parity.shadow = projectedInc, projectedShadow
	return nil
}

// filterBySeedScope keeps only the edges belonging to THIS run's seed scope.
//
// One long-lived stack can hold several runs' fixtures at once — the scope
// exists precisely so they are different series — and both rulers see every
// scope alike, so an earlier run's still-firing alerts would appear on BOTH
// sides and diff clean. Narrowing anyway keeps each run's verdict about its own
// data: without it the intact-SLO check and the per-tier arrival check would
// read another run's edges, and a scenario could pass on a fixture it never
// seeded. Narrowing is by the run's own identity, never by anything about what
// an edge says, so it can hide no divergence.
func filterBySeedScope(events []AlertEvent, scope string) []AlertEvent {
	out := make([]AlertEvent, 0, len(events))
	for _, e := range events {
		if e.Labels[tier2SeedScopeLabel] == scope {
			out = append(out, e)
		}
	}
	return out
}

// mwmbrMissingAlertNames lists the burn tiers no edge has arrived for yet.
func mwmbrMissingAlertNames(events []AlertEvent) []string {
	seen := make(map[string]bool, len(events))
	for _, e := range events {
		seen[e.RuleName] = true
	}
	var missing []string
	for _, name := range mwmbrAlertNames() {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// thenMwmbrStreamsAgree is the diff itself: every alert one ruler raised, the
// other raised too.
//
// False positives and false negatives must both be zero. Timing skew is
// reported but does not fail here — it is a MEASUREMENT of the sub-interval
// phase difference between two independent schedulers, which section 5 of
// docs/migration-testing.md names as not a cerberus artifact, and its
// magnitude is asserted separately and more strictly by thenMwmbrSkewBounded.
// Folding it in here would make this step fail on a scheduler phase rather
// than on a parity defect.
func (w *World) thenMwmbrStreamsAgree() error {
	if err := w.requireMwmbrStreams(); err != nil {
		return err
	}
	diff := DiffAlertStreams(w.tier2Parity.incumbent, w.tier2Parity.shadow, mwmbrEvalInterval)
	if diff.FalseNegative > 0 || diff.FalsePositive > 0 {
		return fmt.Errorf(
			"the two rulers disagreed: %d alert edge(s) the incumbent raised never reached the shadow ruler's "+
				"receiver (false negatives — a repointed pager would go silent on a real incident), and %d the "+
				"shadow raised the incumbent never did (false positives — a repointed pager would wake somebody "+
				"for nothing). Matched: %d, quantized-timing skew: %d. Incumbent stream: %s. Shadow stream: %s",
			diff.FalseNegative, diff.FalsePositive, diff.Matched, diff.TimingSkew,
			describeAlertStream(w.tier2Parity.incumbent), describeAlertStream(w.tier2Parity.shadow),
		)
	}
	if diff.Matched+diff.TimingSkew == 0 {
		return fmt.Errorf(
			"the diff compared no alert edge at all; two empty streams agree by construction, so this verdict " +
				"would mean nothing",
		)
	}
	return nil
}

// thenMwmbrIntactSLONeverPaged asserts by NAME what the false-positive count
// asserts by number: the SLO whose error budget never burned was paged by
// neither ruler.
//
// It is a separate step because it is the half a one-sided fixture cannot
// prove, and the half whose failure has a specific diagnosis: a shadow ruler
// that paged the intact SLO means cerberus answered a burn-rate query with a
// number the incumbent's engine does not agree is there. Reading that out of a
// bare "false positives: 1" would take a reader back to the fixture to work
// out which identity was supposed to stay quiet.
func (w *World) thenMwmbrIntactSLONeverPaged() error {
	if err := w.requireMwmbrStreams(); err != nil {
		return err
	}
	for _, side := range []struct {
		name   string
		events []AlertEvent
	}{
		{"incumbent", w.tier2Parity.incumbent},
		{"shadow", w.tier2Parity.shadow},
	} {
		for _, e := range side.events {
			if e.Labels[mwmbrSLOKey] == mwmbrIntactSLO {
				return fmt.Errorf(
					"the %s ruler paged %q for %s=%q, whose error budget the fixture never burns — its error "+
						"counter is seeded flat, so no correct evaluation of either burn tier can put it over "+
						"threshold; the edge landed at %s carrying %v",
					side.name, e.RuleName, mwmbrSLOKey, mwmbrIntactSLO, e.At, e.Labels,
				)
			}
		}
	}
	return nil
}

// thenMwmbrSkewBounded asserts the two rulers' firing edges landed close
// enough together to be the same decision made twice.
//
// The bound is DERIVED, not chosen: one evaluation interval, because two
// schedulers ticking at the same cadence on independent phases can pick
// evaluation instants up to one interval apart and neither is late; plus the
// measured span of this harness's own two writes, because a ruler cannot react
// to a fixture before the write that made it visible returned. Every term is a
// property of the substrate rather than a slack allowance, which is what keeps
// this an assertion instead of a tolerance.
func (w *World) thenMwmbrSkewBounded() error {
	if err := w.requireMwmbrStreams(); err != nil {
		return err
	}
	bound := mwmbrEvalInterval + w.tier2Parity.seedVisibilitySpan
	if err := SkewBoundHolds(w.tier2Parity.incumbent, w.tier2Parity.shadow, bound); err != nil {
		return fmt.Errorf(
			"%w (bound = one %s evaluation interval plus the %s this harness took to write both backends)",
			err, mwmbrEvalInterval, w.tier2Parity.seedVisibilitySpan,
		)
	}
	return nil
}

// thenMwmbrBakeWindowHoldsZero asserts the quantity the rules actually gate on
// — the error-budget burn rate — is the same number on both sides at EVERY
// instant across the bake window.
//
// This is the assertion the notification diff cannot make. Two rulers can
// agree on "fired" while disagreeing about the value that made them fire: a
// shadow ruler computing a burn rate twice the incumbent's still pages, and
// the stream diff comes back clean. Sampling the value itself across the whole
// window is what closes that, and it is sampled across the WINDOW rather than
// at one instant because a burn-rate rule evaluated continuously can disagree
// at a single evaluation and still page incorrectly.
//
// Both sides are asked the same expression at the same instants — cerberus
// over ClickHouse, the incumbent's own engine over its own TSDB — so this is a
// genuine two-engine comparison with cerberus on exactly one side.
func (w *World) thenMwmbrBakeWindowHoldsZero() error {
	if w.tier2Parity.seedEnd.IsZero() {
		return fmt.Errorf("the burn-rate fixture has not been seeded; the scenario must seed it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), mwmbrQueryWait)
	defer cancel()

	// Anchored to the END of seeded history rather than to "now": every instant
	// sampled is one both backends hold data for on every run, however long CI
	// took to get here.
	bakeEnd := w.tier2Parity.seedEnd.Add(-mwmbrSeedFuture)
	expr := mwmbrBurnRateExpr(w.tier2SeedScope, mwmbrFastLongWindow)

	samples := make([]MWMBRSample, 0, int(mwmbrBakeWindow/mwmbrBakeStep)+1)
	for at := bakeEnd.Add(-mwmbrBakeWindow); !at.After(bakeEnd); at = at.Add(mwmbrBakeStep) {
		shadowValue, err := mwmbrBurnRateAt(ctx, w.tier2.CerberusURL, expr, at)
		if err != nil {
			return fmt.Errorf("the shadow ruler's backend: %w", err)
		}
		incumbentValue, err := mwmbrBurnRateAt(ctx, w.tier2.IncumbentURL, expr, at)
		if err != nil {
			return fmt.Errorf("the incumbent ruler's backend: %w", err)
		}
		samples = append(samples, MWMBRSample{At: at, Delta: incumbentValue - shadowValue})
	}
	if err := BakeWindowHoldsZero(samples, migrateverify.DefaultTolerance); err != nil {
		return fmt.Errorf(
			"%w — the two rulers page off the same burn rate, so a delta here means cerberus and the incumbent's "+
				"engine answered %q differently over identical data",
			err, expr,
		)
	}
	return nil
}

// mwmbrBurnRateExpr renders the burning SLO's error-budget burn rate over one
// window: the error ratio divided by the budget, so the number is in
// budgets-per-window — the same quantity the rules' thresholds are expressed
// in and an SLO runbook talks about.
func mwmbrBurnRateExpr(scope string, window time.Duration) string {
	selector := func(metric string) string {
		return fmt.Sprintf("%s{%s=%q,%s}", metric, mwmbrSLOKey, mwmbrBurningSLO, tier2ScopeMatcher(scope))
	}
	return fmt.Sprintf(
		`(sum(rate(%s[%s])) / sum(rate(%s[%s]))) / %v`,
		selector(mwmbrErrorsMetric), promDuration(window),
		selector(mwmbrRequestsMetric), promDuration(window),
		mwmbrErrorBudget,
	)
}

// promDuration renders a duration in PromQL's own vocabulary. The windows are
// whole minutes on both rule files, so this stays exact rather than rounding
// something a rule would then not match.
func promDuration(d time.Duration) string {
	return strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m0s")
}

// mwmbrBurnRateAt evaluates expr at one instant against one backend, requiring
// exactly one series carrying exactly one sample. An empty or multi-series
// answer is an error rather than a skipped instant: silently dropping an
// instant would shrink the bake window to whatever both sides happened to
// answer, which is the degenerate mode BakeWindowHoldsZero's own empty-input
// check exists to refuse.
func mwmbrBurnRateAt(ctx context.Context, baseURL, expr string, at time.Time) (float64, error) {
	series, err := lib.QueryInstantSeries(ctx, baseURL, expr, at)
	if err != nil {
		return 0, fmt.Errorf("evaluate %q at %s: %w", expr, at, err)
	}
	if len(series) != 1 || len(series[0].Samples) != 1 {
		return 0, fmt.Errorf(
			"evaluating %q at %s returned %d series, want exactly one carrying one sample",
			expr, at, len(series),
		)
	}
	return series[0].Samples[0].Value, nil
}

// requireMwmbrStreams fails when the When step never captured both streams, so
// a Then that would otherwise pass over two empty slices reports the real
// problem.
func (w *World) requireMwmbrStreams() error {
	if len(w.tier2Parity.incumbent) == 0 || len(w.tier2Parity.shadow) == 0 {
		return fmt.Errorf(
			"the two rulers' notification streams were not both captured (incumbent: %d edge(s), shadow: %d); "+
				"the scenario must wait for both rulers to fire first",
			len(w.tier2Parity.incumbent), len(w.tier2Parity.shadow),
		)
	}
	return nil
}

// describeAlertStream renders a stream as a stable, sorted, readable list, so a
// failure names exactly what each ruler emitted rather than making a reader
// re-run to find out.
func describeAlertStream(events []AlertEvent) string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s@%s", alertEventKey(e), e.At.Format(time.RFC3339)))
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "(empty)"
	}
	return strings.Join(out, ", ")
}

// fetchAlertEvents reads every notification one dead-end receiver captured and
// returns the edges belonging to the named alerts. Each ruler has its own
// receiver, so this narrows by alert name only — never by which ruler sent it,
// which the payload does not carry and which is precisely why the two rulers
// need two receivers.
func fetchAlertEvents(ctx context.Context, deadEndURL string, names []string) ([]AlertEvent, error) {
	target := strings.TrimRight(deadEndURL, "/") + "/notifications/list"
	var resp deadEndListResponse
	if err := fetchJSONGet(ctx, target, &resp); err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}
	out := make([]AlertEvent, 0, len(resp.Notifications))
	for _, n := range resp.Notifications {
		events, err := ParseGrafanaWebhookEvents(n.Body)
		if err != nil {
			return nil, fmt.Errorf("notification received at %s: %w", n.ReceivedAt, err)
		}
		for _, e := range events {
			if wanted[e.RuleName] {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// mwmbrThreshold is a burn tier's alerting threshold: the factor times the
// budget, the arithmetic both rule files spell out. Used only by
// tier2_parity_test.go, which holds the seeded error ratio clear of it.
func mwmbrThreshold(burnFactor float64) float64 { return burnFactor * mwmbrErrorBudget }

// mwmbrSeededErrorRatio is the burning SLO's error ratio once its burst is
// fully inside a window — the number the thresholds are compared against.
func mwmbrSeededErrorRatio() float64 { return mwmbrErrorRate / mwmbrRequestRate }

// mwmbrRatioClearsThreshold reports whether the seeded ratio trips a tier by a
// margin, rather than sitting on its boundary where a rounding difference
// would decide whether the scenario has anything to observe.
func mwmbrRatioClearsThreshold(burnFactor float64) bool {
	return mwmbrSeededErrorRatio() > mwmbrThreshold(burnFactor)*mwmbrThresholdMargin
}

// mwmbrThresholdMargin is how far clear of a threshold the seeded ratio must
// sit. Not slack in an assertion — the assertions are exact — but a bound on
// the FIXTURE, so a future re-tuning that left the burn barely tripping (and
// so left the rules firing or not on a coin toss) fails offline.
const mwmbrThresholdMargin = 2.0
