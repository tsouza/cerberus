package tempo

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// errQuerier fails every Query / QueryStrings call with err — used to
// force attachMetricsExemplars down its ClickHouse-failure branch
// without wiring a full HTTP round trip.
type errQuerier struct{ err error }

func (q *errQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, q.err
}

func (q *errQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	return nil, q.err
}

// installManualReaderForExemplarTest swaps the process-global OTel
// MeterProvider with one backed by a ManualReader, so a test can pull
// a deterministic metrics snapshot after driving attachMetricsExemplars
// directly. Mirrors internal/telemetry/metrics_test.go's helper of the
// same name — duplicated here (test-only scaffolding, not a package
// API) rather than exported from internal/telemetry.
func installManualReaderForExemplarTest(t *testing.T) *metric.ManualReader {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })
	return reader
}

// collectExemplarFailures drains the manual reader and returns the
// cerberus_tempo_exemplar_failures_total sum's data points.
func collectExemplarFailures(t *testing.T, reader *metric.ManualReader) []metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if !strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == "cerberus_tempo_exemplar_failures_total" {
				return m.Data.(metricdata.Sum[int64]).DataPoints
			}
		}
	}
	t.Fatalf("cerberus_tempo_exemplar_failures_total not found; scopes=%v", rm.ScopeMetrics)
	return nil
}

// attrString reads a string-valued attribute off a data point, failing
// the test if it is absent.
func attrString(t *testing.T, dp metricdata.DataPoint[int64], key attribute.Key) string {
	t.Helper()
	v, ok := dp.Attributes.Value(key)
	if !ok {
		t.Fatalf("attribute %q missing; have %v", key, dp.Attributes)
	}
	return v.AsString()
}

// TestAttachMetricsExemplars_EmitFailure_RecordsCounter pins the
// chsql.EmitMetricsExemplars failure branch of attachMetricsExemplars
// (#1460): a nil RangeWindow trips EmitMetricsExemplars's own
// nil-guard, and the handler must record the failure on
// ExemplarFailuresTotal with stage=StageEmit rather than only logging
// it — before this fix, a systematic exemplar-emit outage was
// wire-indistinguishable from "no exemplars in this window".
func TestAttachMetricsExemplars_EmitFailure_RecordsCounter(t *testing.T) {
	// Not t.Parallel(): installManualReaderForExemplarTest swaps the
	// process-global OTel MeterProvider (telemetry.Install), which two
	// tests doing so concurrently would race — mirrors
	// internal/telemetry/metrics_test.go's convention.
	reader := installManualReaderForExemplarTest(t)

	h := &Handler{Logger: slog.Default()}
	series := []MetricsSeries{{}}
	metrics := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}

	h.attachMetricsExemplars(context.Background(), series, nil, metrics)

	dps := collectExemplarFailures(t, reader)
	if len(dps) != 1 {
		t.Fatalf("exemplar_failures DPs: got %d want 1: %+v", len(dps), dps)
	}
	if got := attrString(t, dps[0], telemetry.AttrStage); got != telemetry.StageEmit {
		t.Errorf("stage: got %q want %q", got, telemetry.StageEmit)
	}
}

// TestAttachMetricsExemplars_QueryFailure_RecordsCounter pins the
// ClickHouse-query failure branch (#1460): a valid, emittable exemplars
// query whose Client.Query call fails must record
// ExemplarFailuresTotal with stage=StageExecute — distinct from the
// emit-failure branch's StageEmit, so a dashboard can tell "we
// couldn't build the exemplars SQL" apart from "ClickHouse rejected
// it" without reading logs.
func TestAttachMetricsExemplars_QueryFailure_RecordsCounter(t *testing.T) {
	// Not t.Parallel() — see TestAttachMetricsExemplars_EmitFailure_RecordsCounter.
	reader := installManualReaderForExemplarTest(t)

	h := &Handler{
		Client: &errQuerier{err: errors.New("ch unavailable")},
		Schema: schema.DefaultOTelTraces(),
		Logger: slog.Default(),
	}
	series := []MetricsSeries{{}}
	metrics := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           metrics,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}

	h.attachMetricsExemplars(context.Background(), series, rw, metrics)

	dps := collectExemplarFailures(t, reader)
	if len(dps) != 1 {
		t.Fatalf("exemplar_failures DPs: got %d want 1: %+v", len(dps), dps)
	}
	if got := attrString(t, dps[0], telemetry.AttrStage); got != telemetry.StageExecute {
		t.Errorf("stage: got %q want %q", got, telemetry.StageExecute)
	}
}
