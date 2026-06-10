//go:build integration

package chclient_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestQuery_E2E spins a real ClickHouse via testcontainers, ingests a few
// rows shaped like the OTel exporter's otel_metrics_gauge table, and
// confirms chclient.Query decodes them into Samples.
//
// Gated by the `integration` build tag (`go test -tags=integration ./...`)
// because it requires Docker; the E2E workflow runs it, regular CI doesn't.
func TestQuery_E2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:24.8-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})

	if err := client.Exec(ctx, `
		CREATE TABLE otel_metrics_gauge (
			MetricName String,
			Attributes Map(String, String),
			TimeUnix DateTime64(9),
			Value Float64
		) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix)
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := client.Exec(ctx, `
		INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
		('up', {'job': 'api'}, now64(9), 1.0),
		('up', {'job': 'db'}, now64(9), 0.0)
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	samples, err := client.Query(
		ctx,
		"SELECT MetricName, Attributes, TimeUnix, Value FROM otel_metrics_gauge WHERE MetricName = ? ORDER BY Value DESC",
		"up",
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	if samples[0].MetricName != "up" || samples[0].Value != 1.0 || samples[0].Labels["job"] != "api" {
		t.Errorf("samples[0]: got %+v", samples[0])
	}
	if samples[1].Value != 0.0 || samples[1].Labels["job"] != "db" {
		t.Errorf("samples[1]: got %+v", samples[1])
	}
}
