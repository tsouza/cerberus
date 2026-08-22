//go:build chdb

// chDB-backed regression pin for #2467: the dedicated-top-level-column
// overlay ([augmentAttributesForTopLevelExpr]) must make the synthesised
// `service_name` key win STRUCTURALLY, not merely by mapConcat ordering.
//
// ClickHouse's mapConcat does not collapse a duplicate key — it
// concatenates both sides' key/value arrays, leaving both entries in the
// resulting Map. Cerberus's series identity is the WHOLE projected
// Attributes map (the RangeWindow's GROUP BY, in this bare-selector
// shape's inner LWR Aggregate), not a subscript read, so pre-fix two
// datapoints that should fold into one series under Prometheus's real
// precedence (dedicated column wins) stayed distinct series, and the
// projected map itself carried two `service_name` entries.
//
// This test runs the REAL lowering + emitter over a chDB seed reaching
// the bug both ways the issue names — an Attributes key spelled exactly
// `service_name`, and a ResourceAttributes key (`service-name`) that
// sanitises to it — and asserts the axis the pre-fix tests missed
// entirely: the projected map has no duplicate key, not just that the
// subscript resolves to the right value.
package promql_test

import (
	"context"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// serviceNameOverlayDedupSeedDDL declares both metrics-fan-out arms (see
// [limitRatioSeed]'s doc comment for why both are required even though
// every row here lands in the sum table) plus ResourceAttributes, which
// the default schema's read path always references.
const serviceNameOverlayDedupSeedDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_sum (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	// Reachability #1: a datapoint Attributes key spelled exactly
	// `service_name`, colliding with the dedicated ServiceName column.
	// Two rows, same dedicated column, DIFFERENT (would-be-ignored)
	// Attributes value — Prometheus's real precedence folds both into
	// ONE series keyed on the dedicated column's value.
	"INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value) VALUES " +
	"('svc_overlay_dedup_total', map('service_name', 'ignored_a'), 'api', toDateTime64('2026-01-01 00:00:00', 9), 7.0), " +
	"('svc_overlay_dedup_total', map('service_name', 'ignored_b'), 'api', toDateTime64('2026-01-01 00:00:00', 9), 11.0);\n" +
	// Reachability #2: a ResourceAttributes key that SANITISES to
	// `service_name` (`service-name` — the literal-spelling exclusion in
	// resourceSourceMap only filters the dotted/underscored spellings).
	"INSERT INTO otel_metrics_sum (MetricName, Attributes, ResourceAttributes, ServiceName, TimeUnix, Value) VALUES " +
	"('svc_overlay_dedup_total', map(), map('service-name', 'from_ra'), 'api2', toDateTime64('2026-01-01 00:00:00', 9), 42.0);\n"

// TestServiceNameOverlay_ChDB_NoDuplicateKeyAndSeriesFold is the
// regression pin for #2467.
func TestServiceNameOverlay_ChDB_NoDuplicateKeyAndSeriesFold(t *testing.T) {
	fixture := newChDBFixture(t, serviceNameOverlayDedupSeedDDL)

	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	evalTS := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	expr, err := p.ParseExpr("svc_overlay_dedup_total")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	rows := fixture.queryOverEmitted(
		t,
		"length(mapKeys(`Attributes`)) AS key_count, "+
			"length(arrayDistinct(mapKeys(`Attributes`))) AS distinct_key_count, "+
			"`Attributes`['service_name'] AS service_name",
		sqlStr, args,
	)
	defer func() { _ = rows.Close() }()

	type row struct {
		keyCount, distinctKeyCount int
		serviceName                string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.keyCount, &r.distinctKeyCount, &r.serviceName); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// The whole-map series identity must have folded the two
	// service_name-colliding-Attributes-key rows into ONE series — the
	// #2467 series-splitting hazard.
	if len(got) != 2 {
		t.Fatalf("series count: got %d rows %#v, want 2 (the ignored_a/ignored_b pair must fold into one series; the ResourceAttributes-sanitised case is the other)", len(got), got)
	}

	for _, r := range got {
		// The core #2467 assertion: the projected map must have NO
		// duplicate key. Pre-fix, mapConcat(base, overlay) concatenated
		// both sides' entries, so key_count for the ignored_a/ignored_b
		// row would be 2 (two "service_name" entries) while
		// distinct_key_count stayed 1.
		if r.keyCount != r.distinctKeyCount {
			t.Errorf("row %#v: key_count=%d != distinct_key_count=%d — the projected Attributes map carries a duplicate key", r, r.keyCount, r.distinctKeyCount)
		}
		if r.keyCount != 1 {
			t.Errorf("row %#v: key_count=%d, want 1 (only the synthesised service_name key; the collapsed Attributes-map key is dropped, and the RA case's map() has nothing else to contribute)", r, r.keyCount)
		}
		// The dedicated column must win: 'api' (or 'api2'), never the
		// base-carried 'ignored_a' / 'ignored_b' / 'from_ra' value.
		if r.serviceName != "api" && r.serviceName != "api2" {
			t.Errorf("row %#v: service_name=%q, want the dedicated ServiceName column's value ('api' or 'api2'), not a base-carried value", r, r.serviceName)
		}
	}
}
