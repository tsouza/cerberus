package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/cucumber/godog"
)

// recordedSeriesMetricSuffix marks the probe's synthetic metric name as a
// colon-named recorded series — the Prometheus recording-rule naming
// convention (`level:metric:operation`) — landed as a Gauge, the shape a
// ruler's remote-write of a computed instantaneous value takes through the
// OTel collector's prometheusremotewrite receiver.
const recordedSeriesMetricSuffix = "_recorded_probe"

// recordedSeriesValue is the value the simulated write-back leg landed —
// deliberately not an integer the fixture's own gauge/counter draws could
// coincide with, so a read-back that silently selected the wrong series
// would not accidentally match anyway.
const recordedSeriesValue = 987.5

// recordedSeriesProbe is one archetype's MIG-13 read-back probe: the
// synthetic recorded-series metric name a Given step wrote directly into
// the ClickHouse landing zone (standing in for a ruler's write-back, which
// Tier-1 has no live ruler to produce), and the value a When step read back
// through cerberus.
type recordedSeriesProbe struct {
	metric string
	at     time.Time
	want   float64
	got    float64
	gotOK  bool
}

// registerRecordedSeriesSteps binds the MIG-13 tier-1 (read-back) steps.
func (w *World) registerRecordedSeriesSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^a recorded series is written into the ClickHouse landing zone as if a ruler had produced it$`,
		w.givenRecordedSeriesWritten)
	ctx.Step(`^the operator reads the recorded series back through cerberus$`, w.whenReadRecordedSeriesBack)
	ctx.Step(`^cerberus returns the recorded series under its declared name with the exact value the landing zone holds$`,
		w.thenRecordedSeriesMatches)
}

const insertRecordedSeriesSQL = `INSERT INTO otel_metrics_gauge (
	ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit,
	Attributes, StartTimeUnix, TimeUnix, Value, Flags
)`

// insertRecordedSeriesProbe writes one gauge-shaped row under a colon-named
// metric, standing in for what a ruler's write-back leg (Tier-2, not yet
// built) would have landed.
func insertRecordedSeriesProbe(ctx context.Context, conn driver.Conn, metric string, at time.Time, value float64) error {
	batch, err := conn.PrepareBatch(ctx, insertRecordedSeriesSQL)
	if err != nil {
		return fmt.Errorf("prepare recorded-series probe batch: %w", err)
	}
	const noFlags = uint32(0)
	const description = "MIG-13 recorded-series read-back probe"
	const unit = "1"
	if err := batch.Append(
		map[string]string{"service.name": expHistProbeService},
		expHistProbeService, metric, description, unit,
		map[string]string{},
		at, at, value, noFlags,
	); err != nil {
		return fmt.Errorf("append recorded-series probe row: %w", err)
	}
	return batch.Send()
}

// givenRecordedSeriesWritten seeds one synthetic recorded-series row per
// tagged archetype directly into the CH landing zone.
func (w *World) givenRecordedSeriesWritten() error {
	if err := w.requireArchetypes("the recorded-series probe"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), livePollBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	at := time.Now().UTC().Truncate(time.Second)
	for _, a := range w.archetypes {
		metric := "svc:" + promSafeName(a) + recordedSeriesMetricSuffix
		if err := insertRecordedSeriesProbe(ctx, conn, metric, at, recordedSeriesValue); err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		w.recordedSeries[a] = recordedSeriesProbe{metric: metric, at: at, want: recordedSeriesValue}
	}
	return nil
}

// whenReadRecordedSeriesBack queries cerberus's normal PromQL read path for
// each archetype's seeded recorded-series name, polling briefly for
// single-node ClickHouse write visibility.
func (w *World) whenReadRecordedSeriesBack() error {
	if len(w.recordedSeries) == 0 {
		return fmt.Errorf("no recorded series has been written; the scenario must write one first")
	}
	if err := w.requireArchetypes("the recorded-series read-back"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		probe, ok := w.recordedSeries[a]
		if !ok {
			return fmt.Errorf("archetype %s has no seeded recorded series", a)
		}

		deadline := time.Now().Add(livePollBudget)
		var got float64
		var lastErr error
		for {
			ctx, cancel := context.WithTimeout(context.Background(), labelValuesFetchTimeout)
			got, lastErr = queryInstantScalar(ctx, w.live.CerberusURL, probe.metric, probe.at)
			cancel()
			if lastErr == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("archetype %s: cerberus never read back the recorded series %q: %w", a, probe.metric, lastErr)
			}
			time.Sleep(livePollInterval)
		}
		probe.got, probe.gotOK = got, true
		w.recordedSeries[a] = probe
	}
	return nil
}

// thenRecordedSeriesMatches asserts every archetype's read-back value
// exactly equals the value the landing zone holds — a plain gauge read is
// mode-1 exact parity, no epsilon.
func (w *World) thenRecordedSeriesMatches() error {
	if err := w.requireArchetypes("the recorded-series assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		probe, ok := w.recordedSeries[a]
		if !ok || !probe.gotOK {
			return fmt.Errorf("archetype %s: the recorded series has not been read back yet", a)
		}
		if probe.got != probe.want {
			return fmt.Errorf("archetype %s: cerberus read %q back as %v, want the landed %v",
				a, probe.metric, probe.got, probe.want)
		}
	}
	return nil
}
