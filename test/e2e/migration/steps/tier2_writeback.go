package steps

import (
	"context"
	"fmt"
	"math"
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
	// MIG-19's sample-by-sample check re-evaluates exactly this expression.
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
)

// tier2RecordingRuleInterval must match rules.yaml's `interval: 10s` — the
// query-range step the write-back poll reads landed samples at.
const tier2RecordingRuleInterval = 10 * time.Second

// Seed geometry. tier2SeedWindow together with tier2SeedIncrementPerStep
// (one idle-second gained per one wall-clock second, i.e. rate() ≈ 1.0 —
// realistic "mostly idle" data) keeps the alert's own >0.9 saturation
// threshold nowhere near tripped, so a write-back scenario never
// accidentally races NodeCPUSaturation's 15m `for:` hold-down; that is
// MIG-18's concern, not MIG-13/MIG-19's.
const (
	tier2SeedWindow           = 10 * time.Minute
	tier2SeedStep             = 15 * time.Second
	tier2SeedIncrementPerStep = float64(tier2SeedStep / time.Second)
)

// Budgets. Each is a bounded deadline on a poll loop — never a sleep,
// never a retry-then-continue: every one of them fails the scenario when it
// expires.
const (
	tier2SeedBudget            = 30 * time.Second
	tier2QueryTimeout          = 15 * time.Second
	tier2WritebackPollBudget   = 120 * time.Second
	tier2WritebackPollInterval = 3 * time.Second
	tier2SampleCompareBudget   = 60 * time.Second
	// tier2WritebackMinSamples requires more than one tick to have landed,
	// so MIG-19's sample-by-sample check has more than a single point to
	// prove itself against — one matching sample could be luck, several in a
	// row are not.
	tier2WritebackMinSamples = 3
)

// tier2WritebackState is the MIG-13/MIG-19 scenario's accumulated state: the
// window the source series was seeded over, and the recorded series read
// back through cerberus.
type tier2WritebackState struct {
	seedStart, seedEnd time.Time
	landed             []lib.PromSeries
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
		`^every landed sample matches a live re-evaluation of the recording rule's source query within the exact-parity epsilon$`,
		w.thenTier2LandedSamplesMatchLiveReEvaluation,
	)
}

// givenTier2SourceSeriesSeeded writes a fresh, cumulative-increasing
// node_cpu_seconds_total{mode="idle"} series directly into ClickHouse, ending
// at "now" — the recording rule's own query window is wall-clock-relative
// (relativeTimeRange in rules.yaml), so a fixed historical window (the kind
// every Tier-1 fixture uses) would age out of the rule's lookback before a
// scenario ever got to assert on it.
func (w *World) givenTier2SourceSeriesSeeded() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2SeedBudget)
	defer cancel()

	conn, err := w.tier2.DialCH(ctx)
	if err != nil {
		return fmt.Errorf("migration harness: dial clickhouse to seed the recording rule's source series: %w", err)
	}
	defer func() { _ = conn.Close() }()

	end := time.Now().UTC()
	start := end.Add(-tier2SeedWindow)
	samples := make([]seed.Sample, 0, int(tier2SeedWindow/tier2SeedStep)+1)
	total := 0.0
	for t := start; !t.After(end); t = t.Add(tier2SeedStep) {
		total += tier2SeedIncrementPerStep
		samples = append(samples, seed.Sample{Time: t, Value: total})
	}

	fixture := seed.Fixture{
		Counter: []seed.MetricSeries{{
			MetricName:  nodeCPUMetricName,
			ServiceName: nodeCPUServiceName,
			Attributes:  map[string]string{"mode": nodeCPUMode, "cpu": nodeCPUCPULabel},
			Samples:     samples,
		}},
	}
	if err := seed.InsertFixture(ctx, conn, fixture); err != nil {
		return fmt.Errorf("migration harness: seed %s: %w", nodeCPUMetricName, err)
	}

	w.tier2Writeback.seedStart, w.tier2Writeback.seedEnd = start, end
	return nil
}

// whenTier2WritebackLands polls cerberus for the recorded series over the
// seeded window until at least tier2WritebackMinSamples samples have landed,
// or the budget expires.
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
		got, err := w.queryTier2RecordedSeries()
		switch {
		case err != nil:
			lastErr = err
			w.tier2ReadBack = recordedReadBack{polled: true, detail: err.Error()}
		case countSamples(got) >= tier2WritebackMinSamples:
			w.tier2Writeback.landed = got
			w.publishRecordedReadBack(got)
			return nil
		default:
			w.publishRecordedReadBack(got)
			lastErr = fmt.Errorf("only %d sample(s) landed so far, want at least %d", countSamples(got), tier2WritebackMinSamples)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"the recorded series %q never landed enough samples within %s: %w",
				nodeCPURecordedMetricName, tier2WritebackPollBudget, lastErr,
			)
		}
		time.Sleep(tier2WritebackPollInterval)
	}
}

// queryTier2RecordedSeries reads the recorded series back through cerberus
// over the seeded window.
func (w *World) queryTier2RecordedSeries() ([]lib.PromSeries, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tier2QueryTimeout)
	defer cancel()
	return lib.QueryRange(
		ctx, w.tier2.CerberusURL, nodeCPURecordedMetricName,
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

// publishRecordedReadBack records what a write-back poll last saw in the
// World's shared read-back slot, so the single "selectable through cerberus"
// step can report a real cardinality whichever scenario ran the poll.
func (w *World) publishRecordedReadBack(got []lib.PromSeries) {
	w.tier2ReadBack = recordedReadBack{
		polled: true,
		series: len(got),
		detail: fmt.Sprintf(
			"range query for %q over the seeded window answered %d series carrying %d sample(s)",
			nodeCPURecordedMetricName, len(got), countSamples(got),
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
	if len(w.tier2Writeback.landed) == 0 {
		return fmt.Errorf(
			"no recorded series was read back over the seeded window; this assertion needs MIG-13's own write-back poll, not MIG-09's instant read",
		)
	}
	if n := countSamples(w.tier2Writeback.landed); n == 0 {
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

// landingZoneCountSQL is a direct, ad-hoc inspection query against the test
// substrate's own ClickHouse — not cerberus-generated SQL, so the repo's
// typed-chsql-only rule (which governs cerberus's OWN query emission) does
// not apply here, the same posture test/e2e/migration/seed's insert
// statements already take.
const landingZoneCountSQL = `SELECT count() FROM otel_metrics_gauge WHERE MetricName = ?`

// thenTier2LandedSamplesMatchLiveReEvaluation is MIG-19: every landed sample
// must reproduce a live re-evaluation, through cerberus, of the exact
// expression the recording rule computed it from — at the exact instant it
// was recorded — under the exact-parity epsilon (migrateverify.
// DefaultTolerance, the same constant `cerberus migrate verify` uses; this
// is not a new tolerance).
func (w *World) thenTier2LandedSamplesMatchLiveReEvaluation() error {
	if err := w.thenTier2RecordedSeriesHasSamples(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2SampleCompareBudget)
	defer cancel()

	checked := 0
	for _, series := range w.tier2Writeback.landed {
		for _, sample := range series.Samples {
			got, err := lib.QueryInstantSeries(ctx, w.tier2.CerberusURL, nodeCPUSourceExpr, sample.Time)
			if err != nil {
				return fmt.Errorf("re-evaluate %q at %s: %w", nodeCPUSourceExpr, sample.Time, err)
			}
			if len(got) != 1 {
				return fmt.Errorf(
					"re-evaluating %q at %s returned %d series, want exactly 1",
					nodeCPUSourceExpr, sample.Time, len(got),
				)
			}
			if len(got[0].Samples) != 1 {
				return fmt.Errorf("re-evaluating %q at %s returned a series with %d samples, want exactly 1",
					nodeCPUSourceExpr, sample.Time, len(got[0].Samples))
			}
			want := got[0].Samples[0].Value
			if diff := math.Abs(sample.Value - want); diff > migrateverify.DefaultTolerance {
				return fmt.Errorf(
					"landed sample at %s = %g, live re-evaluation of its source query = %g "+
						"(diff %g exceeds the exact-parity tolerance %g)",
					sample.Time, sample.Value, want, diff, migrateverify.DefaultTolerance,
				)
			}
			checked++
		}
	}
	if checked == 0 {
		return fmt.Errorf("compared zero landed samples against a live re-evaluation; the scenario's verdict proves nothing")
	}
	return nil
}
