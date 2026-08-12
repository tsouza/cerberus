package steps

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrateverify"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// The recording-rule fixture this file drives, named here so a drift against
// tiers/tier2-ruler/grafana/alerting/rules.yaml's `model.expr` /
// `record.metric` fails loudly at compile-adjacent review rather than as a
// silent mismatch between what this file seeds and what the rule actually
// computes.
const (
	// nodeCPUMetricName is the recording rule's own source series.
	nodeCPUMetricName = "node_cpu_seconds_total"
	// nodeCPURecordedMetricName must match rules.yaml's `record.metric`.
	nodeCPURecordedMetricName = "node:node_cpu_utilisation:avg1m"
	// nodeCPUSourceExpr must match rules.yaml's `model.expr` byte-for-byte —
	// MIG-19's sample-by-sample check re-evaluates exactly this expression,
	// narrowed to one seed scope by tier2ScopedSourceExpr.
	nodeCPUSourceExpr = `1 - avg without (cpu) (rate(node_cpu_seconds_total{mode="idle"}[5m]))`
	// nodeCPUServiceName / nodeCPUMode / nodeCPUCPULabel are the seeded
	// series' resource/attribute set. A single `cpu` value is sufficient:
	// `avg without (cpu)` degenerates to the identity function over one
	// series, which is fine — the assertions verify write-back fidelity, not
	// the aggregation's own correctness (that is PromQL's own compatibility
	// suite's job).
	nodeCPUServiceName = "node-exporter"
	nodeCPUMode        = "idle"
	nodeCPUCPULabel    = "0"
	// nodeCPUSaturationThreshold mirrors rules.yaml's NodeCPUSaturation
	// evaluator (`gt`, params [0.9]). Nothing queries it — it is the bound
	// tier2_writeback_test.go holds the seed geometry against, so a future
	// re-tuning of the seed that would silently start tripping the alert
	// (and so start dropping notifications into the dead-end receiver that
	// MIG-09's own notification-delta assertion counts) fails in the unit
	// lane instead of as a confusing Tier-2 flake.
	nodeCPUSaturationThreshold = 0.9
)

// tier2RecordingRuleInterval must match rules.yaml's `interval: 10s` — the
// cadence the ruler evaluates at, and therefore the cadence at which write-back
// samples must land. MIG-19 asserts the landed samples reproduce exactly this
// cadence with no gap and no duplicate.
const tier2RecordingRuleInterval = 10 * time.Second

// Seed geometry.
//
// The idle-seconds-per-second the counter accrues is RAMPED across the window
// rather than held constant, and that is load-bearing rather than decorative.
// The previous fixture accrued exactly one idle-second per wall-clock second,
// so `1 - avg without (cpu) (rate(...))` evaluated to 0.0 at every instant —
// and a sample-by-sample comparison of 0.0 against 0.0 reads as perfect
// agreement whether or not the write-back path preserved anything at all. A
// ramp makes every landed sample a different number, so agreement has to be
// earned. tier2_writeback_test.go pins both properties (strictly increasing
// counter, strictly increasing rate) plus the bound below.
//
// The ramp's endpoints are chosen so the derived utilisation `1 - rate` stays
// strictly inside (0, nodeCPUSaturationThreshold): never zero (which would
// re-introduce the degenerate comparison) and never near NodeCPUSaturation's
// firing threshold (which would race MIG-18's own use of the dead-end
// receiver).
const (
	tier2SeedWindow = 10 * time.Minute
	// tier2SeedStep is deliberately coarser than tier2RecordingRuleInterval:
	// two consecutive ruler evaluations therefore cannot always see an
	// identical set of source samples, which is what makes consecutive landed
	// values genuinely distinct rather than distinct only in principle.
	tier2SeedStep          = 15 * time.Second
	tier2SeedIdleRateStart = 0.5
	tier2SeedIdleRateEnd   = 0.9
)

// Budgets. Each is a bounded deadline on a poll loop — never a sleep,
// never a retry-then-continue: every one of them fails the scenario when it
// expires.
const (
	tier2SeedBudget   = 30 * time.Second
	tier2QueryTimeout = 15 * time.Second
	// tier2WritebackPollBudget must cover the write-back leg's cold start
	// (a fresh recording-rule tick, relay-prom's own flush, the collector's
	// own flush) AND tier2WritebackMinSamples further evaluations after it.
	// It is deliberately wider than rulerLandingWait, which bounds only the
	// FIRST landing: this poll now counts real landed rows, where it used to
	// count query_range grid points that a single landed sample could fill by
	// staleness carry-forward alone.
	tier2WritebackPollBudget   = 3 * time.Minute
	tier2WritebackPollInterval = 3 * time.Second
	tier2SampleCompareBudget   = 60 * time.Second
	// tier2WritebackMinSamples requires more than one tick to have landed, so
	// MIG-19's sample-by-sample check has more than a single point to prove
	// itself against — one matching sample could be luck, several in a row are
	// not. It is also what makes the landed set span more than one
	// tier2SeedStep, so the "not all one value" assertion has a source-sample
	// boundary to have crossed.
	tier2WritebackMinSamples = 3
	// tier2IncumbentRecordWait bounds the poll for the INCUMBENT ruler's own
	// recorded samples. It covers the remote write becoming visible to that
	// ruler plus tier2WritebackMinSamples of its own evaluations, which is a
	// different and later clock from the shadow's write-back landing — see
	// thenIncumbentRecordedSeriesIsItsOwnEngineOutput.
	tier2IncumbentRecordWait = 2 * time.Minute
)

// landedSample is ONE raw row the write-back leg wrote into the ClickHouse
// landing zone: the timestamp the ruler evaluated at and the value it
// recorded, read straight out of the table rather than through cerberus's
// read path.
//
// Reading the raw rows — rather than iterating a `query_range` grid, which is
// what this file used to do — is the difference between comparing landed
// SAMPLES and comparing GRID POINTS. cerberus carries a landed value forward
// across a 5m staleness window, so a `query_range` at the ruler's own 10s step
// repeats a single landed sample across up to thirty consecutive grid
// instants; a scenario iterating that grid could report thirty "compared"
// points while one landed sample did all the work.
type landedSample struct {
	at    time.Time
	value float64
}

// tier2WritebackState is the MIG-13/MIG-19 scenario's accumulated state: the
// window the source series was seeded over, the raw rows the write-back leg
// landed in ClickHouse, and the same recorded series as cerberus's own read
// path answers for it.
type tier2WritebackState struct {
	seedStart, seedEnd time.Time
	landed             []landedSample
	readBack           []lib.PromSeries
}

// registerTier2WritebackSteps binds the MIG-13 (write-back half) / MIG-19
// (write-back timing half) steps: seeding the recording rule's own input
// series with live samples (nothing in the Tier-2 ingest path does this on
// its own — the Tier-1 fixture carries no node_cpu_seconds_total, and the
// rule's relativeTimeRange is wall-clock-relative), waiting for the ruler's
// write-back to land, and asserting the landed series both exists and
// reproduces its source query sample-by-sample.
func (w *World) registerTier2WritebackSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the recording rule's source series is seeded with live samples$`, w.givenTier2SourceSeriesSeeded)
	ctx.Step(`^the operator waits for the ruler's write-back to land$`, w.whenTier2WritebackLands)
	ctx.Step(`^the recorded series is selectable through cerberus$`, w.thenRecordedSeriesSelectable)
	ctx.Step(`^the recorded series carries at least one sample$`, w.thenTier2RecordedSeriesHasSamples)
	ctx.Step(`^no recorded metric name is silently missing from the landing zone$`, w.thenTier2NoMetricNameMissingFromLandingZone)
	ctx.Step(
		`^the landed sample count matches the ruler's evaluation cadence across the window they span$`,
		w.thenTier2LandedCadenceIsComplete,
	)
	ctx.Step(`^the landed samples do not all carry the same value$`, w.thenTier2LandedSamplesVary)
	ctx.Step(
		`^every landed sample matches a live re-evaluation of the recording rule's source query within the exact-parity epsilon$`,
		w.thenTier2LandedSamplesMatchLiveReEvaluation,
	)
	ctx.Step(
		`^the incumbent ruler recorded the same rule over its own copy of the source data$`,
		w.thenIncumbentRecordedSeriesIsItsOwnEngineOutput,
	)
	ctx.Step(
		`^every landed sample matches what the incumbent ruler's engine computes at the same instant$`,
		w.thenLandedSamplesMatchIncumbentEngine,
	)
}

// === MIG-19's incumbent-versus-shadow recorded-series diff ===
//
// thenTier2LandedSamplesMatchLiveReEvaluation above proves the write-back
// path: what the shadow ruler recorded is what cerberus says its source
// expression evaluates to. What it cannot prove is PARITY, because cerberus is
// on both sides of it — a cerberus evaluation bug moves the recorded value and
// the re-evaluation identically and cancels out.
//
// The two steps below close that. The oracle is the INCUMBENT's engine:
// reference Prometheus, evaluating the same expression over its own copy of
// the same samples, which the harness remote-wrote from the one fixture that
// produced the ClickHouse rows.
//
//   - thenLandedSamplesMatchIncumbentEngine compares the SHADOW's landed
//     recorded samples against that oracle at the exact instants they were
//     recorded at. cerberus stands on exactly one side, so a cerberus
//     evaluation bug now has nothing to cancel against.
//   - thenIncumbentRecordedSeriesIsItsOwnEngineOutput compares the
//     INCUMBENT's own recorded series against the same oracle on the
//     incumbent's own grid. Together the two mean: both rulers' recorded
//     output is the same function of the same data, each checked on the grid
//     it actually evaluates on.
//
// Why the incumbent's recorded series is not compared to the shadow's
// point-for-point: the two rulers never record at the same instants and
// cannot be made to. Prometheus offsets a group by hash(group, file) %
// interval and Grafana ticks epoch-aligned, so their grids share a length and
// not a phase, and the source series is a RAMP — its value genuinely differs
// between two instants a few seconds apart. Comparing across grids would
// therefore diff two correct answers to two different questions, and the only
// way to make it pass would be a tolerance wide enough to swallow the ramp,
// which would swallow real divergence with it. Routing both through one oracle
// at each ruler's own instants keeps every comparison exact.

// tier2IncumbentRecordedSelector is the recorded series as the INCUMBENT
// stores it, narrowed to this scenario's seed scope exactly as the shadow-side
// selector is.
func tier2IncumbentRecordedSelector(scope string) string {
	return tier2ScopedRecordedSelector(scope)
}

// thenLandedSamplesMatchIncumbentEngine is MIG-19's incumbent-versus-shadow
// diff: every sample the shadow ruler's write-back landed in ClickHouse must
// equal what the incumbent's engine computes for the same expression at the
// same instant, under the same exact-parity epsilon (migrateverify.
// DefaultTolerance) — not a new tolerance, the one `cerberus migrate verify`
// already uses.
func (w *World) thenLandedSamplesMatchIncumbentEngine() error {
	if err := w.thenTier2RecordedSeriesHasSamples(); err != nil {
		return err
	}
	landed := w.tier2Writeback.landed
	if len(landed) == 0 {
		return fmt.Errorf("no landed sample was read out of the landing zone; the scenario's verdict would prove nothing")
	}
	expr, err := tier2ScopedSourceExpr(w.tier2SeedScope)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2SampleCompareBudget)
	defer cancel()

	for _, sample := range landed {
		want, err := tier2SingleValueAt(ctx, w.tier2.IncumbentURL, expr, sample.at)
		if err != nil {
			return fmt.Errorf(
				"the incumbent ruler could not answer the recording rule's source query at %s, so the shadow's "+
					"landed sample there has nothing independent to be checked against: %w",
				sample.at, err,
			)
		}
		if diff := math.Abs(sample.value - want); diff > migrateverify.DefaultTolerance {
			return fmt.Errorf(
				"the shadow ruler recorded %g at %s, but the incumbent's own engine computes %g for the same "+
					"expression over the same samples at that instant (diff %g exceeds the exact-parity tolerance "+
					"%g) — the two rulers do not agree, and cerberus is on exactly one side of this comparison",
				sample.value, sample.at, want, diff, migrateverify.DefaultTolerance,
			)
		}
	}
	return nil
}

// thenIncumbentRecordedSeriesIsItsOwnEngineOutput asserts the incumbent leg is
// a real RULER rather than a query endpoint the previous step happens to poll:
// it recorded the rule under its own name, on its own evaluation grid, and
// every sample it recorded is what its engine says the source expression
// evaluates to at that instant.
//
// Without this the incumbent's `record:` rule could be deleted entirely and
// MIG-19 would still pass — the diff above only ever asks the incumbent's
// engine ad-hoc questions. That would leave "the incumbent records the same
// rule set" as a claim about a config file nothing executes.
func (w *World) thenIncumbentRecordedSeriesIsItsOwnEngineOutput() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	if w.tier2Writeback.seedEnd.IsZero() {
		return fmt.Errorf("the recording rule's source series has not been seeded; the scenario must seed it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2SampleCompareBudget)
	defer cancel()

	// The window runs to NOW, not to the end of the seeded window. The
	// incumbent's ruler only starts producing this series once the fixture is
	// visible to it, so every sample it recorded lies AFTER the seed window
	// closes — a range query bounded by the seed window asks about an interval
	// in which the recorded series does not yet exist and comes back empty,
	// which reads as "the incumbent never recorded the rule" rather than as
	// "the question was asked about the wrong interval".
	// A bounded POLL, not a single read. The two rulers start recording at
	// different moments — the shadow's samples are already in hand because
	// whenTier2WritebackLands polled for them through the whole write-back
	// bridge, while the incumbent only begins recording once the remote write
	// makes the fixture visible to it. Reading once would assert against
	// however many ticks happened to have elapsed, which is a race rather than
	// a check. The budget owns the verdict: it fails when it expires.
	deadline := time.Now().Add(tier2IncumbentRecordWait)
	var recorded []landedSample
	for {
		var err error
		recorded, err = tier2IncumbentRecordedSamples(
			ctx, w.tier2.IncumbentURL, tier2IncumbentRecordedSelector(w.tier2SeedScope),
			w.tier2Writeback.seedStart, time.Now().UTC(),
		)
		if err != nil {
			return err
		}
		if len(recorded) >= tier2WritebackMinSamples {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"the incumbent ruler recorded only %d sample(s) of %q under seed scope %q within %s, want at "+
					"least %d — its own `record:` rule "+
					"(tiers/tier2-ruler/incumbent/incumbent-rules.yml) is not producing the series the shadow's "+
					"output is supposed to be diffed against",
				len(recorded), nodeCPURecordedMetricName, w.tier2SeedScope, tier2IncumbentRecordWait,
				tier2WritebackMinSamples,
			)
		}
		time.Sleep(tier2WritebackPollInterval)
	}
	expr, err := tier2ScopedSourceExpr(w.tier2SeedScope)
	if err != nil {
		return err
	}
	for _, sample := range recorded {
		want, err := tier2SingleValueAt(ctx, w.tier2.IncumbentURL, expr, sample.at)
		if err != nil {
			return fmt.Errorf("re-evaluate %q on the incumbent at %s: %w", expr, sample.at, err)
		}
		if diff := math.Abs(sample.value - want); diff > migrateverify.DefaultTolerance {
			return fmt.Errorf(
				"the incumbent ruler recorded %g at %s but its own engine computes %g for the rule's expression "+
					"there (diff %g exceeds %g) — the incumbent's recorded series is not its own rule's output, so "+
					"it cannot serve as the oracle the shadow is diffed against",
				sample.value, sample.at, want, diff, migrateverify.DefaultTolerance,
			)
		}
	}
	return nil
}

// tier2IncumbentRecordedSamples reads the incumbent's RECORDED samples at the
// instants it actually recorded them.
//
// A plain range query cannot answer this. Prometheus carries a sample forward
// across its staleness window, so a grid at the ruler's own interval reports a
// value at every grid point whether or not an evaluation landed there — the
// same trap the shadow side avoids by reading raw ClickHouse rows instead of a
// query_range grid. `timestamp()` is the way out: it returns each grid point's
// value's OWN timestamp, so pairing the two range queries and de-duplicating
// by that timestamp recovers exactly the distinct samples the ruler wrote,
// and a carried-forward repeat collapses into the one sample it is a repeat of.
func tier2IncumbentRecordedSamples(
	ctx context.Context, baseURL, selector string, start, end time.Time,
) ([]landedSample, error) {
	values, err := lib.QueryRange(ctx, baseURL, selector, start, end, tier2RecordingRuleInterval)
	if err != nil {
		return nil, fmt.Errorf("read the incumbent's recorded series %q: %w", selector, err)
	}
	stamps, err := lib.QueryRange(ctx, baseURL, "timestamp("+selector+")", start, end, tier2RecordingRuleInterval)
	if err != nil {
		return nil, fmt.Errorf("read the incumbent's recorded sample instants for %q: %w", selector, err)
	}
	if len(values) != 1 || len(stamps) != 1 {
		return nil, fmt.Errorf(
			"the incumbent's recorded series %q resolved to %d series (and %d timestamp series), want exactly one "+
				"of each; the seed scope should make it unique",
			selector, len(values), len(stamps),
		)
	}
	if len(values[0].Samples) != len(stamps[0].Samples) {
		return nil, fmt.Errorf(
			"the incumbent answered %d value points and %d timestamp points for %q over one grid; the two range "+
				"queries must align point for point for the instants to be attributable",
			len(values[0].Samples), len(stamps[0].Samples), selector,
		)
	}

	out := make([]landedSample, 0, len(values[0].Samples))
	seen := make(map[int64]bool, len(values[0].Samples))
	for i, v := range values[0].Samples {
		// timestamp() yields the sample's instant as float SECONDS, and a
		// float64 cannot hold a present-day Unix second to nanosecond
		// precision — an instant recorded at .080 comes back as .079999923.
		// Re-evaluating a rate() there is then a query at a DIFFERENT instant
		// from the one the ruler evaluated at, and over a ramped source that
		// shows up as a small but real value difference (observed: 2.7e-06,
		// against a 1e-09 exact-parity epsilon).
		//
		// Rounding to the nearest millisecond is exact rather than a
		// concession: Prometheus stores every sample timestamp as an int64
		// count of milliseconds, so the ruler's evaluation instant IS a whole
		// millisecond and rounding recovers it precisely. Anything finer is
		// float noise from the wire encoding, never information.
		recordedSeconds := stamps[0].Samples[i].Value
		at := time.UnixMilli(int64(math.Round(recordedSeconds * millisPerSecond))).UTC()
		if seen[at.UnixNano()] {
			continue
		}
		seen[at.UnixNano()] = true
		out = append(out, landedSample{at: at, value: v.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out, nil
}

// millisPerSecond converts the float-seconds instant `timestamp()` reports
// into the integer milliseconds Prometheus actually stores a sample at.
const millisPerSecond = 1000

// tier2SingleValueAt evaluates expr at one instant against one backend,
// requiring exactly one series carrying exactly one sample. An empty answer is
// an error rather than a skipped comparison: skipping would silently shrink
// the diff to whatever both sides happened to answer.
func tier2SingleValueAt(ctx context.Context, baseURL, expr string, at time.Time) (float64, error) {
	got, err := lib.QueryInstantSeries(ctx, baseURL, expr, at)
	if err != nil {
		return 0, fmt.Errorf("evaluate %q at %s: %w", expr, at, err)
	}
	if len(got) != 1 {
		return 0, fmt.Errorf("evaluating %q at %s returned %d series, want exactly one", expr, at, len(got))
	}
	if len(got[0].Samples) != 1 {
		return 0, fmt.Errorf(
			"evaluating %q at %s returned a series with %d samples, want exactly one",
			expr, at, len(got[0].Samples),
		)
	}
	return got[0].Samples[0].Value, nil
}

// givenTier2SourceSeriesSeeded writes a fresh, cumulative-increasing
// node_cpu_seconds_total{mode="idle"} series directly into ClickHouse, ending
// at "now" — the recording rule's own query window is wall-clock-relative
// (relativeTimeRange in rules.yaml), so a fixed historical window (the kind
// every Tier-1 fixture uses) would age out of the rule's lookback before a
// scenario ever got to assert on it.
//
// The series is written under this scenario's own seed scope, so it is a
// DIFFERENT stored series from the one MIG-09's Given writes and from the one
// any earlier run wrote. See tier2_seed_scope.go for the counter-reset
// artifact that colliding identities produced.
func (w *World) givenTier2SourceSeriesSeeded() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	if w.tier2SeedScope == "" {
		return fmt.Errorf("migration harness: no seed scope was derived for this scenario; the Before hook must set one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2SeedBudget)
	defer cancel()

	conn, err := w.tier2.DialCH(ctx)
	if err != nil {
		return fmt.Errorf("migration harness: dial clickhouse to seed the recording rule's source series: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Truncated to a whole second, and that is load-bearing rather than tidy.
	// This fixture is written to BOTH backends, and the two encodings do not
	// carry the same precision: ClickHouse stores TimeUnix as DateTime64(9),
	// while Prometheus remote write puts a sample on the wire as
	// `UnixMilli()` (seed/promwrite.go) — so an untruncated `now` leaves the
	// two backends holding the SAME sample at instants up to a millisecond
	// apart. A rate() over a ramped counter then answers slightly differently
	// on each side, and MIG-19's incumbent diff reports it as a parity
	// divergence when it is really a seeding artifact. Observed on CI as a
	// 2.24e-06 disagreement against a 1e-09 epsilon, having passed locally
	// three times running: whether it bites depends on where a window boundary
	// happens to fall relative to a sample, which is exactly the kind of
	// latent flake that must be removed rather than retried.
	//
	// A whole second is exactly representable in both encodings, so the two
	// backends hold identical instants by construction. tier2_parity.go's
	// MWMBR fixture truncates for the same reason, and measures agreement at
	// 2.8e-17 as a result. tier2_writeback_test.go pins the property.
	end := tier2DualSeedAnchor(time.Now())
	start := end.Add(-tier2SeedWindow)
	fixture := seed.Fixture{
		Counter: []seed.MetricSeries{{
			MetricName:  nodeCPUMetricName,
			ServiceName: nodeCPUServiceName,
			Attributes: map[string]string{
				"mode":              nodeCPUMode,
				"cpu":               nodeCPUCPULabel,
				tier2SeedScopeLabel: w.tier2SeedScope,
			},
			Samples: tier2SourceSamples(start),
		}},
	}
	if err := seed.InsertFixture(ctx, conn, fixture); err != nil {
		return fmt.Errorf("migration harness: seed %s: %w", nodeCPUMetricName, err)
	}
	// The SAME fixture, rendered into the incumbent ruler's wire shape. This is
	// what puts cerberus on exactly one side of MIG-19's comparison: the
	// incumbent's recording rule computes the same expression over its own copy
	// of these samples, so its recorded output is an independent answer rather
	// than an echo. Rendered from the one in-memory fixture rather than
	// regenerated, so the two backends cannot hold different numbers at the
	// same instant.
	if err := seed.WriteSeries(ctx, w.tier2.IncumbentURL, fixture.PromSeries()); err != nil {
		return fmt.Errorf(
			"migration harness: remote-write %s to the incumbent ruler at %s: %w",
			nodeCPUMetricName, w.tier2.IncumbentURL, err,
		)
	}

	w.tier2Writeback.seedStart, w.tier2Writeback.seedEnd = start, end
	return nil
}

// tier2SourceSamples builds the ramped, monotonically increasing counter the
// recording rule computes over: one sample every tier2SeedStep across
// tier2SeedWindow, accruing idle-seconds at a rate that ramps linearly from
// tier2SeedIdleRateStart to tier2SeedIdleRateEnd.
func tier2SourceSamples(start time.Time) []seed.Sample {
	steps := int(tier2SeedWindow / tier2SeedStep)
	samples := make([]seed.Sample, 0, steps+1)
	total := 0.0
	for i := range steps + 1 {
		samples = append(samples, seed.Sample{Time: start.Add(time.Duration(i) * tier2SeedStep), Value: total})
		total += tier2SeedIdleRateAt(i, steps) * tier2SeedStep.Seconds()
	}
	return samples
}

// tier2SeedIdleRateAt is the idle-seconds-per-second accrued over the step
// starting at index i of a totalSteps-long ramp. Linear from
// tier2SeedIdleRateStart at i=0 to tier2SeedIdleRateEnd at i=totalSteps.
func tier2SeedIdleRateAt(i, totalSteps int) float64 {
	if totalSteps <= 0 {
		return tier2SeedIdleRateStart
	}
	span := tier2SeedIdleRateEnd - tier2SeedIdleRateStart
	return tier2SeedIdleRateStart + span*float64(i)/float64(totalSteps)
}

// tier2ScopeMatcher renders the PromQL label matcher that narrows a selector
// to one seed scope. cerberus surfaces a metric's ClickHouse `Attributes` keys
// as labels verbatim, so the key the seeder writes and the label the query
// matches on are the same string — hence one constant, not two.
func tier2ScopeMatcher(scope string) string {
	return fmt.Sprintf("%s=%q", tier2SeedScopeLabel, scope)
}

// tier2ScopedSourceExpr is nodeCPUSourceExpr narrowed to one seed scope.
//
// This is DERIVED from the rule's own expression rather than written out a
// second time, so the two can never drift: the only edit is one extra label
// matcher on the source selector. Narrowing is exact rather than approximate —
// `rate()` is per-series and `avg without (cpu)` groups by every label except
// `cpu`, so `seed_scope` is a grouping key and the group for one scope
// evaluates to precisely the same number whether or not the other scopes'
// series are in the input.
//
// It fails rather than silently returning the unnarrowed expression when the
// matcher it splices onto is gone, which is what a future edit to
// nodeCPUSourceExpr (tracking a change in rules.yaml) would look like.
func tier2ScopedSourceExpr(scope string) (string, error) {
	anchor := fmt.Sprintf("mode=%q", nodeCPUMode)
	scoped := strings.Replace(nodeCPUSourceExpr, anchor, anchor+","+tier2ScopeMatcher(scope), 1)
	if scoped == nodeCPUSourceExpr {
		return "", fmt.Errorf(
			"cannot narrow %q to seed scope %q: it no longer carries the %q matcher this narrowing splices onto; "+
				"tier2ScopedSourceExpr must be updated alongside nodeCPUSourceExpr",
			nodeCPUSourceExpr, scope, anchor,
		)
	}
	return scoped, nil
}

// tier2ScopedRecordedSelector is the recorded series narrowed to one seed
// scope. The recording rule's `avg without (cpu)` keeps `seed_scope` on its
// output, so each scope's seed produces its own recorded series and a scenario
// can read back exactly the samples ITS seed caused — never a leftover from
// another scenario or an earlier run against the same long-lived stack.
func tier2ScopedRecordedSelector(scope string) string {
	return fmt.Sprintf("%s{%s}", nodeCPURecordedMetricName, tier2ScopeMatcher(scope))
}

// whenTier2WritebackLands polls until at least tier2WritebackMinSamples raw
// rows of THIS scenario's recorded series have landed in ClickHouse, or the
// budget expires. Each pass also reads the same series back through cerberus,
// so the shared "selectable through cerberus" slot reports what cerberus
// actually answered at the moment the poll finished (or timed out).
func (w *World) whenTier2WritebackLands() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	if w.tier2Writeback.seedEnd.IsZero() {
		return fmt.Errorf("the recording rule's source series has not been seeded; the scenario must seed it first")
	}

	deadline := time.Now().Add(tier2WritebackPollBudget)
	var lastErr error
	for {
		landed, err := w.readTier2LandedSamples()
		switch {
		case err != nil:
			lastErr = err
		case len(landed) >= tier2WritebackMinSamples:
			w.tier2Writeback.landed = landed
			w.pollTier2ReadBack()
			return nil
		default:
			lastErr = fmt.Errorf(
				"only %d row(s) of %q under seed scope %q have landed so far, want at least %d",
				len(landed), nodeCPURecordedMetricName, w.tier2SeedScope, tier2WritebackMinSamples,
			)
		}
		w.pollTier2ReadBack()
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"the recorded series %q never landed enough samples under seed scope %q within %s "+
					"(if the landing zone holds the metric but carries no %s attribute, the write-back "+
					"bridge dropped the label the scenarios use to keep their seeds apart): %w",
				nodeCPURecordedMetricName, w.tier2SeedScope, tier2WritebackPollBudget, tier2SeedScopeLabel, lastErr,
			)
		}
		time.Sleep(tier2WritebackPollInterval)
	}
}

// readTier2LandedSamples reads the raw landed rows for THIS scenario's
// recorded series out of the ClickHouse landing zone, oldest first. The seed
// scope is the whole filter: the scope did not exist before this scenario
// seeded it, so every row carrying it is one this scenario's own write-back
// produced, and no time predicate is needed to say so.
func (w *World) readTier2LandedSamples() ([]landedSample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tier2QueryTimeout)
	defer cancel()

	conn, err := w.tier2.DialCH(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration harness: dial clickhouse to read the landed recorded samples: %w", err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, landedRecordedSamplesSQL, nodeCPURecordedMetricName, tier2SeedScopeLabel, w.tier2SeedScope)
	if err != nil {
		return nil, fmt.Errorf("migration harness: read landed rows for %q: %w", nodeCPURecordedMetricName, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]landedSample, 0)
	for rows.Next() {
		var s landedSample
		if err := rows.Scan(&s.at, &s.value); err != nil {
			return nil, fmt.Errorf("migration harness: scan a landed row for %q: %w", nodeCPURecordedMetricName, err)
		}
		s.at = s.at.UTC()
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migration harness: iterate landed rows for %q: %w", nodeCPURecordedMetricName, err)
	}
	return out, nil
}

// queryTier2RecordedSeries reads THIS scenario's recorded series back through
// cerberus over the seeded window.
func (w *World) queryTier2RecordedSeries() ([]lib.PromSeries, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tier2QueryTimeout)
	defer cancel()
	return lib.QueryRange(
		ctx, w.tier2.CerberusURL, tier2ScopedRecordedSelector(w.tier2SeedScope),
		w.tier2Writeback.seedStart, time.Now().UTC(), tier2RecordingRuleInterval,
	)
}

// countSamples sums samples across every series a query returned.
func countSamples(series []lib.PromSeries) int {
	n := 0
	for _, s := range series {
		n += len(s.Samples)
	}
	return n
}

// pollTier2ReadBack reads the recorded series back through cerberus and
// publishes what it saw into the World's shared read-back slot, so the single
// "selectable through cerberus" step can report a real cardinality whichever
// scenario ran the poll. A read error is published rather than returned: this
// runs inside the write-back poll loop, whose own deadline owns the verdict.
func (w *World) pollTier2ReadBack() {
	got, err := w.queryTier2RecordedSeries()
	if err != nil {
		w.tier2ReadBack = recordedReadBack{polled: true, detail: err.Error()}
		return
	}
	w.tier2Writeback.readBack = got
	w.tier2ReadBack = recordedReadBack{
		polled: true,
		series: len(got),
		detail: fmt.Sprintf(
			"range query for %q over the seeded window answered %d series carrying %d sample(s)",
			tier2ScopedRecordedSelector(w.tier2SeedScope), len(got), countSamples(got),
		),
	}
}

// thenRecordedSeriesSelectable is the ONE definition of "the recorded series
// is selectable through cerberus" — MIG-09's core assertion and MIG-13's
// alike: the recording rule's output round-trips ruler -> relay -> collector
// -> ClickHouse -> cerberus and comes back out under its declared name. Each
// scenario's own When populates the shared slot this reads (see
// recordedReadBack in world.go); registering it twice would be an
// ambiguous-step error under godog's Strict mode, which is exactly how it
// used to fail both scenarios at once.
func (w *World) thenRecordedSeriesSelectable() error {
	rb := w.tier2ReadBack
	if !rb.polled {
		return fmt.Errorf(
			"no step has read %q back through cerberus; the scenario must poll for the write-back first",
			nodeCPURecordedMetricName,
		)
	}
	if rb.series == 0 {
		return fmt.Errorf("%q is not selectable through cerberus: %s", nodeCPURecordedMetricName, rb.detail)
	}
	return nil
}

// thenTier2RecordedSeriesHasSamples asserts the selectable series actually
// carries data, not merely an empty result shape.
func (w *World) thenTier2RecordedSeriesHasSamples() error {
	if err := w.thenRecordedSeriesSelectable(); err != nil {
		return err
	}
	if len(w.tier2Writeback.readBack) == 0 {
		return fmt.Errorf(
			"no recorded series was read back over the seeded window; this assertion needs MIG-13's own write-back poll, not MIG-09's instant read",
		)
	}
	if n := countSamples(w.tier2Writeback.readBack); n == 0 {
		return fmt.Errorf("the recorded series %q carries zero samples", nodeCPURecordedMetricName)
	}
	return nil
}

// thenTier2NoMetricNameMissingFromLandingZone queries ClickHouse DIRECTLY —
// bypassing cerberus's own read path entirely — so a landing-zone bug (the
// write-back leg silently dropping the metric) is never masked by, or
// conflated with, a cerberus read-side bug. This is MIG-13's "no derived
// name silently disappears" half.
func (w *World) thenTier2NoMetricNameMissingFromLandingZone() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2QueryTimeout)
	defer cancel()

	conn, err := w.tier2.DialCH(ctx)
	if err != nil {
		return fmt.Errorf("migration harness: dial clickhouse to check the landing zone: %w", err)
	}
	defer func() { _ = conn.Close() }()

	row := conn.QueryRow(ctx, landingZoneCountSQL, nodeCPURecordedMetricName)
	var count uint64
	if err := row.Scan(&count); err != nil {
		return fmt.Errorf("migration harness: count landed rows for %q: %w", nodeCPURecordedMetricName, err)
	}
	if count == 0 {
		return fmt.Errorf(
			"metric %q is not present in the ClickHouse landing zone (otel_metrics_gauge) at all; "+
				"the write-back leg silently dropped it", nodeCPURecordedMetricName,
		)
	}
	return nil
}

// landingZoneCountSQL and landedRecordedSamplesSQL are direct, ad-hoc
// inspection queries against the test substrate's own ClickHouse — not
// cerberus-generated SQL, so the repo's typed-chsql-only rule (which governs
// cerberus's OWN query emission) does not apply here, the same posture
// test/e2e/migration/seed's insert statements already take.
const (
	landingZoneCountSQL = `SELECT count() FROM otel_metrics_gauge WHERE MetricName = ?`

	landedRecordedSamplesSQL = `SELECT TimeUnix, Value FROM otel_metrics_gauge
		WHERE MetricName = ? AND Attributes[?] = ?
		ORDER BY TimeUnix`
)

// thenTier2LandedCadenceIsComplete asserts the landed samples reproduce the
// ruler's own evaluation cadence across the window they span: exactly one
// sample per tier2RecordingRuleInterval, strictly increasing in time.
//
// This is the count oracle MIG-19 previously had none of. The comparison in
// thenTier2LandedSamplesMatchLiveReEvaluation says every landed value is
// right; it cannot say every value the ruler produced actually arrived. A
// dropped evaluation (the ruler erroring against cerberus, the relay losing a
// batch, the collector dropping an export) leaves a gap, and a retried write
// leaves a duplicate — both are write-back defects, and both are invisible to
// a value-only comparison.
func (w *World) thenTier2LandedCadenceIsComplete() error {
	landed := w.tier2Writeback.landed
	if len(landed) < tier2WritebackMinSamples {
		return fmt.Errorf(
			"only %d landed sample(s) are available for the cadence check, want at least %d; "+
				"the scenario must wait for the write-back first",
			len(landed), tier2WritebackMinSamples,
		)
	}
	for i := 1; i < len(landed); i++ {
		if !landed[i].at.After(landed[i-1].at) {
			return fmt.Errorf(
				"landed samples %d and %d are at %s and %s — the write-back leg recorded the same evaluation twice, "+
					"or wrote it out of order",
				i-1, i, landed[i-1].at, landed[i].at,
			)
		}
	}
	span := landed[len(landed)-1].at.Sub(landed[0].at)
	want := tier2ExpectedTickCount(span, tier2RecordingRuleInterval)
	if len(landed) != want {
		return fmt.Errorf(
			"%d sample(s) landed across the %s from %s to %s, but a ruler evaluating every %s produces %d there; "+
				"the write-back leg dropped or duplicated an evaluation",
			len(landed), span, landed[0].at, landed[len(landed)-1].at, tier2RecordingRuleInterval, want,
		)
	}
	return nil
}

// tier2ExpectedTickCount is how many evaluations a ruler running at `interval`
// produces across a window whose first and last evaluation are `span` apart —
// the fencepost count, not the number of intervals.
//
// Rounding, not truncation: the scheduler's own jitter moves the measured span
// by a fraction of an interval in EITHER direction (a tick emitted late
// lengthens the span before it, and shortens the one after it), so truncation
// would turn a few milliseconds of lateness into a phantom missing tick. A
// genuinely dropped or duplicated evaluation moves the span by a WHOLE
// interval, which rounding still discriminates.
func tier2ExpectedTickCount(span, interval time.Duration) int {
	if interval <= 0 || span < 0 {
		return 0
	}
	return int(math.Round(float64(span)/float64(interval))) + 1
}

// thenTier2LandedSamplesVary asserts the landed values are not all one number.
//
// Without it the sample-by-sample comparison has a silent degenerate mode: a
// source series accruing exactly one idle-second per second makes
// `1 - rate(...)` evaluate to 0.0 everywhere, so "landed == re-evaluated"
// holds as 0.0 == 0.0 at every point and proves nothing about the write-back
// path. The ramped seed (see the seed-geometry block above) is what makes the
// values vary; this step is what stops a future edit from quietly flattening
// them again.
func (w *World) thenTier2LandedSamplesVary() error {
	landed := w.tier2Writeback.landed
	if len(landed) < tier2WritebackMinSamples {
		return fmt.Errorf(
			"only %d landed sample(s) are available for the variation check, want at least %d; "+
				"the scenario must wait for the write-back first",
			len(landed), tier2WritebackMinSamples,
		)
	}
	if !tier2ValuesVary(landed) {
		return fmt.Errorf(
			"all %d landed sample(s) of %q carry the value %g; a sample-by-sample comparison against a "+
				"constant series cannot distinguish agreement from a write-back path that preserved nothing",
			len(landed), nodeCPURecordedMetricName, landed[0].value,
		)
	}
	return nil
}

// tier2ValuesVary reports whether any two landed samples differ. Exact
// inequality, not an epsilon: the ramped seed moves the recorded value by
// orders of magnitude more than migrateverify.DefaultTolerance between
// consecutive evaluations, so anything that comes out equal here is flat, not
// merely close.
func tier2ValuesVary(landed []landedSample) bool {
	for _, s := range landed {
		if s.value != landed[0].value {
			return true
		}
	}
	return false
}

// thenTier2LandedSamplesMatchLiveReEvaluation is MIG-19: every landed sample
// must reproduce a live re-evaluation, through cerberus, of the exact
// expression the recording rule computed it from — at the exact instant it
// was recorded — under the exact-parity epsilon (migrateverify.
// DefaultTolerance, the same constant `cerberus migrate verify` uses; this
// is not a new tolerance).
//
// What this proves, stated exactly: the value the ruler recorded, transported
// through relay-prom -> otel-collector-writeback -> ClickHouse, is the value
// cerberus computes for that expression at that instant. It covers rule
// translation, write-back transport fidelity and evaluation-timestamp
// alignment. It is NOT by itself a parity check: cerberus is on both sides of
// THIS comparison, so a cerberus evaluation bug would move the recorded value
// and the re-evaluation identically and cancel out. That is what
// thenLandedSamplesMatchIncumbentEngine adds, by re-asking the same question
// of the incumbent's engine; the two steps are complements, and this one is
// the leg that isolates the write-back TRANSPORT rather than the evaluation.
// See MIG-19.feature's scope note and section 6 of docs/migration-testing.md.
func (w *World) thenTier2LandedSamplesMatchLiveReEvaluation() error {
	if err := w.thenTier2RecordedSeriesHasSamples(); err != nil {
		return err
	}
	landed := w.tier2Writeback.landed
	if len(landed) == 0 {
		return fmt.Errorf("no landed sample was read out of the landing zone; the scenario's verdict would prove nothing")
	}
	expr, err := tier2ScopedSourceExpr(w.tier2SeedScope)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2SampleCompareBudget)
	defer cancel()

	for _, sample := range landed {
		got, err := lib.QueryInstantSeries(ctx, w.tier2.CerberusURL, expr, sample.at)
		if err != nil {
			return fmt.Errorf("re-evaluate %q at %s: %w", expr, sample.at, err)
		}
		if len(got) != 1 {
			return fmt.Errorf(
				"re-evaluating %q at %s returned %d series, want exactly 1",
				expr, sample.at, len(got),
			)
		}
		if len(got[0].Samples) != 1 {
			return fmt.Errorf("re-evaluating %q at %s returned a series with %d samples, want exactly 1",
				expr, sample.at, len(got[0].Samples))
		}
		want := got[0].Samples[0].Value
		if diff := math.Abs(sample.value - want); diff > migrateverify.DefaultTolerance {
			return fmt.Errorf(
				"landed sample at %s = %g, live re-evaluation of its source query = %g "+
					"(diff %g exceeds the exact-parity tolerance %g)",
				sample.at, sample.value, want, diff, migrateverify.DefaultTolerance,
			)
		}
	}
	return nil
}
