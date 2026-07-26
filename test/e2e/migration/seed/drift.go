package seed

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Fault injection for the substrate's negative control.
//
// Every positive assertion in the Tier-1 lane proves the two sides AGREE.
// None of them proves the lane is capable of noticing that they DISAGREE — a
// harness that has silently stopped comparing anything satisfies all of them.
// The negative control closes that hole: it perturbs one side by a margin no
// float tolerance could absorb and requires the comparison to report the
// difference.
//
// The perturbation targets the ClickHouse side rather than the reference for a
// mechanical reason: reference Prometheus rejects a second sample at an
// already-ingested timestamp, so a reference-side perturbation would have to
// depend on that backend's out-of-order-ingestion policy, which is a knob, not
// a guarantee. A ClickHouse INSERT always lands. Either direction proves the
// same thing — that a real difference is observable — and this one is
// deterministic.
//
// This is fault injection consumed by an always-executed assertion. It is
// never a path that suppresses a result: nothing calls it to make a comparison
// pass.

// DriftFactor scales the injected series. It is far above any float tolerance
// a comparison could carry, so a lane that fails to notice it is not comparing
// values at all.
const DriftFactor = 2.0

// driftMarkerLabel / driftMarkerValue tag every injected row with a label no
// archetype's data declaration can produce, which is what makes an injected
// row identifiable as injected.
//
// This was previously an out-of-range `http_status_code` value (503). That only
// held while ONE archetype was seeded: the value has to be absent from the
// DECLARED status-code list, and the two archetypes the lane now seeds together
// declare different lists — kube-prometheus-stack declares 503 legitimately.
// The pristine guard then counted 363 perfectly ordinary fixture rows as a
// previous run's injection and refused to run the parity lane at all. A
// dedicated marker cannot collide with fixture data whatever an archetype
// declares.
//
// The injected series still lands as a NEW member of its group rather than a
// second copy of an existing one — the extra label makes its attribute set
// distinct — so the group sum stays unambiguous, which is what the
// out-of-range status code was for.
const (
	driftMarkerLabel = "cerberus_migration_injected_drift"
	driftMarkerValue = "1"
)

// DriftRowCount reports how many injected drift rows ClickHouse currently
// holds. The negative control deliberately corrupts the ClickHouse side, so a
// non-zero count before a parity run means the substrate is carrying a
// previous run's injection and must be re-seeded — which is a precondition
// failure worth naming precisely, not a mysterious extra series discovered
// mid-comparison.
func DriftRowCount(ctx context.Context, conn driver.Conn) (uint64, error) {
	const countSQL = "SELECT count() FROM otel_metrics_gauge WHERE Attributes[?] = ?"
	var n uint64
	if err := conn.QueryRow(ctx, countSQL, driftMarkerLabel, driftMarkerValue).Scan(&n); err != nil {
		return 0, fmt.Errorf("count injected drift rows: %w", err)
	}
	return n, nil
}

// InjectGaugeDrift writes an extra gauge series into ClickHouse for one
// service, carrying DriftFactor times that service's group total at every
// sample instant. After it lands, `sum by (service_name)` over the gauge reads
// (1 + DriftFactor)x the reference's value for that service, and every other
// service is untouched.
//
// It returns the per-instant values it injected, so the caller can assert the
// exact post-injection reading rather than merely "something changed".
func InjectGaugeDrift(ctx context.Context, conn driver.Conn, f Fixture, service string) (map[int64]float64, error) {
	base := map[int64]float64{}
	var metricName string
	for _, s := range f.Gauge {
		if s.Attributes[serviceNameLabel] != service {
			continue
		}
		metricName = s.MetricName
		for _, sample := range s.Samples {
			base[sample.Time.UnixMilli()] += sample.Value
		}
	}
	if metricName == "" {
		return nil, fmt.Errorf("fixture holds no gauge series for service %q", service)
	}

	injected := make(map[int64]float64, len(base))
	batch, err := conn.PrepareBatch(ctx, insertGaugeSQL)
	if err != nil {
		return nil, fmt.Errorf("prepare drift batch: %w", err)
	}
	resource := resourceAttributes(service)
	driftAttributes := sortedAttrs(map[string]string{
		serviceNameLabel: service,
		driftMarkerLabel: driftMarkerValue,
	})
	for _, t := range f.Window.SampleTimes() {
		value := base[t.UnixMilli()] * DriftFactor
		injected[t.UnixMilli()] = value
		if err := batch.Append(
			resource, service, metricName, fixtureMetricDescription, fixtureMetricUnit,
			driftAttributes, t, t, value, noSpanFlags,
		); err != nil {
			return nil, fmt.Errorf("append drift sample: %w", err)
		}
	}
	if err := sendBatch(batch); err != nil {
		return nil, err
	}
	return injected, nil
}
