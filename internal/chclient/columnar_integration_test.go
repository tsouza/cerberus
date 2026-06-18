//go:build integration

package chclient_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// asTooMany unwraps err onto a *TooManySamplesError, reporting whether it
// matched.
func asTooMany(err error, target **chclient.TooManySamplesError) bool {
	return errors.As(err, target)
}

// columnar_integration_test.go — the parity gate for the production columnar
// `query_range` matrix decode (CERBERUS_COLUMNAR_MATRIX_DECODE). It spins one
// real ClickHouse, ingests a representative matrix (several series, each with
// many samples), then drains the SAME query through both the default row path
// and the flag-on columnar path and asserts the decoded Samples are
// byte-identical — including the over-budget case, which both paths must reject
// with the IDENTICAL *TooManySamplesError.
//
// Gated by the `integration` build tag (Docker required); the columnar path
// dials ch-go against a live server, so it cannot run without one.

const columnarMatrixDDL = `
	CREATE TABLE otel_metrics_gauge (
		MetricName String,
		Attributes Map(String, String),
		TimeUnix DateTime64(9),
		Value Float64
	) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix)
`

const columnarMatrixSQL = `
	SELECT MetricName, Attributes, TimeUnix, Value
	FROM otel_metrics_gauge
	WHERE MetricName = ?
	ORDER BY Attributes, TimeUnix
`

func TestColumnarMatrixParity_E2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	addr := startColumnarCH(ctx, t)
	seedMatrixFixture(ctx, t, addr)

	// Drain the SAME matrix query through both paths.
	rowSamples := drainMatrix(ctx, t, newColumnarClient(t, addr, false, 0))
	colSamples := drainMatrix(ctx, t, newColumnarClient(t, addr, true, 0))

	assertSamplesEqual(t, rowSamples, colSamples)

	// Sanity: the fixture really exercised multiple series and many rows, so
	// the parity above is meaningful (a 1-row result would prove nothing).
	if len(rowSamples) < 100 {
		t.Fatalf("fixture too small: got %d samples, want >= 100", len(rowSamples))
	}

	// Budget parity: with a max-samples cap below the result size, BOTH paths
	// must reject with the same *TooManySamplesError reporting the same limit.
	const budget = 10
	rowErr := drainMatrixErr(ctx, t, newColumnarClient(t, addr, false, budget))
	colErr := drainMatrixErr(ctx, t, newColumnarClient(t, addr, true, budget))
	assertBudgetErrEqual(t, rowErr, colErr, budget)
}

// startColumnarCH boots a ClickHouse container and returns its host:port.
func startColumnarCH(ctx context.Context, t *testing.T) string {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return host + ":" + port.Port()
}

// newColumnarClient builds a Client against addr with the columnar matrix flag
// set as requested and an optional per-query max-samples budget (0 = unlimited).
func newColumnarClient(t *testing.T, addr string, columnar bool, maxSamples int64) *chclient.Client {
	t.Helper()
	client, err := chclient.New(chclient.Config{
		Addr:                 addr,
		Database:             "otel",
		Username:             "cerberus",
		Password:             "cerberus",
		ColumnarMatrixDecode: columnar,
		MaxQuerySamples:      maxSamples,
	})
	if err != nil {
		t.Fatalf("connect (columnar=%v): %v", columnar, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// seedMatrixFixture ingests a representative matrix: several series (distinct
// label sets) each carrying many samples, plus one empty-label series so the
// empty-Map decode is covered on both paths.
func seedMatrixFixture(ctx context.Context, t *testing.T, addr string) {
	t.Helper()
	client := newColumnarClient(t, addr, false, 0)
	if err := client.Exec(ctx, columnarMatrixDDL); err != nil {
		t.Fatalf("create table: %v", err)
	}

	const (
		series    = 8
		perSeries = 40
	)
	base := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	var values [][]any
	for s := 0; s < series; s++ {
		attrs := map[string]string{
			"job":      "api",
			"env":      "prod",
			"instance": fmt.Sprintf("host-%d", s),
		}
		for p := 0; p < perSeries; p++ {
			ts := base.Add(time.Duration(s*perSeries+p) * time.Second)
			values = append(values, []any{"http_requests_total", attrs, ts, float64(p)})
		}
	}
	// One empty-label series so the empty-Map decode parity is exercised.
	for p := 0; p < perSeries; p++ {
		ts := base.Add(time.Duration(1000+p) * time.Second)
		values = append(values, []any{"http_requests_total", map[string]string{}, ts, float64(p)})
	}

	if err := batchInsert(ctx, client, values); err != nil {
		t.Fatalf("insert fixture: %v", err)
	}
}

// batchInsert ingests the fixture rows via the driver's batch API.
func batchInsert(ctx context.Context, client *chclient.Client, values [][]any) error {
	batch, err := client.Conn().PrepareBatch(ctx,
		"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value)")
	if err != nil {
		return err
	}
	for _, row := range values {
		if err := batch.Append(row...); err != nil {
			return err
		}
	}
	return batch.Send()
}

// drainMatrix runs the matrix query and returns every decoded Sample.
func drainMatrix(ctx context.Context, t *testing.T, client *chclient.Client) []chclient.Sample {
	t.Helper()
	cur, err := client.QueryCursor(ctx, columnarMatrixSQL, "http_requests_total")
	if err != nil {
		t.Fatalf("QueryCursor: %v", err)
	}
	defer func() { _ = cur.Close() }()

	var out []chclient.Sample
	for cur.Next() {
		out = append(out, cur.Sample())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("cursor.Err: %v", err)
	}
	return out
}

// drainMatrixErr runs the matrix query and returns the iteration error (the
// budget-exceeded path returns it via cursor.Err after Next reports false).
func drainMatrixErr(ctx context.Context, t *testing.T, client *chclient.Client) error {
	t.Helper()
	cur, err := client.QueryCursor(ctx, columnarMatrixSQL, "http_requests_total")
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
		_ = cur.Sample()
	}
	return cur.Err()
}

// assertSamplesEqual fails unless the two Sample slices are byte-identical in
// order, value, and label content. The interned-map identity is per-cursor, so
// parity is on CONTENT (the wire shape), not pointer equality.
func assertSamplesEqual(t *testing.T, row, col []chclient.Sample) {
	t.Helper()
	if len(row) != len(col) {
		t.Fatalf("length mismatch: row=%d col=%d", len(row), len(col))
	}
	for i := range row {
		r, c := row[i], col[i]
		if r.MetricName != c.MetricName {
			t.Fatalf("sample %d MetricName: row=%q col=%q", i, r.MetricName, c.MetricName)
		}
		if !r.Timestamp.Equal(c.Timestamp) {
			t.Fatalf("sample %d Timestamp: row=%v col=%v", i, r.Timestamp, c.Timestamp)
		}
		if r.Value != c.Value {
			t.Fatalf("sample %d Value: row=%v col=%v", i, r.Value, c.Value)
		}
		if r.SeriesID != c.SeriesID {
			t.Fatalf("sample %d SeriesID: row=%d col=%d", i, r.SeriesID, c.SeriesID)
		}
		if len(r.Labels) != len(c.Labels) {
			t.Fatalf("sample %d label count: row=%d col=%d", i, len(r.Labels), len(c.Labels))
		}
		for k, v := range r.Labels {
			if c.Labels[k] != v {
				t.Fatalf("sample %d label %q: row=%q col=%q", i, k, v, c.Labels[k])
			}
		}
	}
}

// assertBudgetErrEqual fails unless both paths rejected with a
// *TooManySamplesError reporting the same limit.
func assertBudgetErrEqual(t *testing.T, row, col error, limit int64) {
	t.Helper()
	if row == nil || col == nil {
		t.Fatalf("expected budget rejection on both paths: row=%v col=%v", row, col)
	}
	var rowTM, colTM *chclient.TooManySamplesError
	if !asTooMany(row, &rowTM) {
		t.Fatalf("row path error is not *TooManySamplesError: %v", row)
	}
	if !asTooMany(col, &colTM) {
		t.Fatalf("columnar path error is not *TooManySamplesError: %v", col)
	}
	if rowTM.Limit != limit || colTM.Limit != limit {
		t.Fatalf("limit mismatch: row=%d col=%d want=%d", rowTM.Limit, colTM.Limit, limit)
	}
	if row.Error() != col.Error() {
		t.Fatalf("error string mismatch: row=%q col=%q", row.Error(), col.Error())
	}
}
