//go:build integration

package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// logFilterOrderImage is the supported floor (versions.yaml min_clickhouse):
// a server that runs a multi-step PREWHERE in the order of the WHERE
// conjuncts.
const logFilterOrderImage = "clickhouse/clickhouse-server:24.8.14.39-alpine"

// logFilterOrderQuery filters on the detected level first and on the line
// second, as the Loki compatibility corpus's log query does.
const logFilterOrderQuery = `{service_name="tempo", pod="pod-8"} | detected_level="error" |~ "(?i)failed"`

// The seeded stream: logFilterOrderInfoRows info lines spread over the
// window's first logFilterOrderInfoSpan, then logFilterOrderErrorRows error
// lines in its last minute, every logFilterOrderFailedEvery-th of them
// carrying "failed". Every body is logFilterOrderBodyBytes long, so the body
// column dwarfs every column the level filter reads.
const (
	logFilterOrderInfoRows    = 98_304 // twelve 8192-row granules
	logFilterOrderErrorRows   = 1_000
	logFilterOrderFailedEvery = 2
	logFilterOrderBodyBytes   = 1_024
	logFilterOrderInfoSpan    = 20 * time.Hour
)

// logFilterOrderLimit is the query's line limit, above the number of lines
// that match, so the answer is every one of them.
const logFilterOrderLimit = 1_000

var (
	logFilterOrderStart = time.Date(2026, time.May, 11, 0, 0, 0, 0, time.UTC)
	logFilterOrderEnd   = logFilterOrderStart.Add(24 * time.Hour)
)

// TestLogQueryLevelFilterSparesTheBody runs logFilterOrderQuery on the floor
// server and requires it to read fewer bytes than the table's body column
// holds. The level filter keeps only the error lines, which sit together at
// the end of the window, so a statement that runs the filters in the order
// the query writes them reads the body of those rows alone. One that runs the
// line filter first reads every body in the stream.
func TestLogQueryLevelFilterSparesTheBody(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, logFilterOrderImage)
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("apply the logs schema: %v", err)
	}
	seedLogFilterOrder(ctx, t, s)

	var bodyBytes uint64
	if err := s.admin.Conn().QueryRow(ctx, "SELECT sum(length(Body)) FROM otel_logs").Scan(&bodyBytes); err != nil {
		t.Fatalf("measure the body column: %v", err)
	}

	client := s.client(t, adminUser, adminPassword, chclient.Config{})
	mux := http.NewServeMux()
	loki.New(client, schema.DefaultOTelLogs(), nil).Mount(mux)
	path := "/loki/api/v1/query_range?" + url.Values{
		"query": {logFilterOrderQuery},
		"start": {strconv.FormatInt(logFilterOrderStart.UnixNano(), 10)},
		"end":   {strconv.FormatInt(logFilterOrderEnd.UnixNano(), 10)},
		"limit": {strconv.Itoa(logFilterOrderLimit)},
	}.Encode()
	qid := "log-filter-order-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(chclient.WithQueryID(ctx, qid))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	lines := 0
	for _, stream := range body.Data.Result {
		lines += len(stream.Values)
	}
	if want := logFilterOrderErrorRows / logFilterOrderFailedEvery; lines != want {
		t.Errorf("the query returned %d lines; want the %d error lines that say failed", lines, want)
	}

	s.flushLogs(ctx, t)
	var readBytes uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT read_bytes FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?", qid).Scan(&readBytes); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	t.Logf("%s: the query read %d bytes; the body column holds %d", s.version, readBytes, bodyBytes)
	if readBytes >= bodyBytes {
		t.Errorf("the query read %d bytes, no fewer than the %d the body column holds: "+
			"it read the body of rows the level filter drops", readBytes, bodyBytes)
	}
}

// seedLogFilterOrder seeds the stream logFilterOrderQuery selects.
func seedLogFilterOrder(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	const columns = `INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body, ResourceAttributes) `
	const resource = `map('service.name', 'tempo', 'service_name', 'tempo', 'pod', 'pod-8')`
	infoStep := logFilterOrderInfoSpan / logFilterOrderInfoRows
	s.exec(ctx, t, columns+
		`SELECT toDateTime64(?, 9) + toIntervalNanosecond(number * ?), 'tempo', 'INFO', 9, `+
		`leftPad(concat('handled request ', toString(number), ' '), ?, 'x'), `+resource+` FROM numbers(?)`,
		logFilterOrderStart, infoStep.Nanoseconds(), logFilterOrderBodyBytes, logFilterOrderInfoRows)
	errorAt := logFilterOrderEnd.Add(-time.Minute)
	s.exec(ctx, t, columns+
		`SELECT toDateTime64(?, 9) + toIntervalMillisecond(number), 'tempo', 'ERROR', 17, `+
		`leftPad(concat(if(number % ? = 0, 'request failed ', 'request retried '), toString(number), ' '), ?, 'x'), `+
		resource+` FROM numbers(?)`,
		errorAt, logFilterOrderFailedEvery, logFilterOrderBodyBytes, logFilterOrderErrorRows)
	s.exec(ctx, t, "OPTIMIZE TABLE otel_logs FINAL")
}
