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
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// logFilterOrderImage is the supported floor (versions.yaml min_clickhouse):
// a server that runs a multi-step PREWHERE in the order of the WHERE
// conjuncts.
const logFilterOrderImage = "clickhouse/clickhouse-server:24.8.14.39-alpine"

// logFilterReorderImage is a server that reorders the conditions it moves
// into PREWHERE (chopt.PrewhereReorderedSince).
const logFilterReorderImage = "clickhouse/clickhouse-server:26.8.36.3-alpine"

// logFilterOrderQuery filters on the detected level first and on the line
// second, as the Loki compatibility corpus's log query does.
const logFilterOrderQuery = `{service_name="tempo", pod="pod-8"} | detected_level="error" |~ "(?i)failed"`

// The seeded stream: logFilterOrderInfoRows info lines spread over the
// window's first logFilterOrderInfoSpan, then logFilterOrderErrorRows error
// lines in its last minute, every logFilterOrderFailedEvery-th of them
// carrying "failed", each with a status of 500 in its structured metadata.
// Every body is logFilterOrderBodyBytes long, so the body column dwarfs
// every column the level filter reads.
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
	s, mux, bodyBytes := startLogFilterOrder(ctx, t, logFilterOrderImage, true)
	var body struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	readBytes := queryLogFilterOrder(ctx, t, s, mux, url.Values{
		"query": {logFilterOrderQuery},
		"limit": {strconv.Itoa(logFilterOrderLimit)},
	}, &body)
	lines := 0
	for _, stream := range body.Data.Result {
		lines += len(stream.Values)
	}
	if want := logFilterOrderErrorRows / logFilterOrderFailedEvery; lines != want {
		t.Errorf("the query returned %d lines; want the %d error lines that say failed", lines, want)
	}
	requireBodySpared(t, s, readBytes, bodyBytes)
}

// logFilterOrderStep is the range aggregations' step and range: windows that
// do not overlap, so the counts add up to the matching lines.
const logFilterOrderStep = time.Hour

// logFilterOrderRangeQueries filter on the detected level, then on a label
// that reads LogAttributes whole, then on the line.
var logFilterOrderRangeQueries = []string{
	`sum(count_over_time({service_name="tempo", pod="pod-8"} | detected_level="error" | tenant!="tenant-7" |~ "(?i)failed" [1h]))`,
	`sum(count_over_time({service_name="tempo", pod="pod-8"} | detected_level="error" | json | status >= 400 |~ "(?i)failed" [1h]))`,
}

// TestRangeAggregationLevelFilterSparesTheBody runs logFilterOrderRangeQueries
// on the floor server, with the strategies its build resolves to, and
// requires each to read fewer bytes than the body column holds, as the log
// query does. The level filter has to run where the table is read, ahead of
// the line filter, or the scan reads every body in the stream.
func TestRangeAggregationLevelFilterSparesTheBody(t *testing.T) {
	ctx := context.Background()
	s, mux, bodyBytes := startLogFilterOrder(ctx, t, logFilterOrderImage, true)
	if !chopt.PrewhereKeepsWhereOrder(s.version) {
		t.Fatalf("%s reorders PREWHERE conditions; the test needs a build that keeps WHERE order", s.version)
	}
	for _, query := range logFilterOrderRangeQueries {
		t.Run(query, func(t *testing.T) {
			var body struct {
				Data struct {
					Result []struct {
						Values [][2]any `json:"values"`
					} `json:"result"`
				} `json:"data"`
			}
			readBytes := queryLogFilterOrder(ctx, t, s, mux, url.Values{
				"query": {query},
				"step":  {strconv.FormatFloat(logFilterOrderStep.Seconds(), 'f', -1, 64)},
			}, &body)
			total := 0
			for _, series := range body.Data.Result {
				for _, v := range series.Values {
					n, err := strconv.Atoi(v[1].(string))
					if err != nil {
						t.Fatalf("sample value %v: %v", v[1], err)
					}
					total += n
				}
			}
			if want := logFilterOrderErrorRows / logFilterOrderFailedEvery; total != want {
				t.Errorf("the query counted %d lines; want the %d error lines that say failed", total, want)
			}
			requireBodySpared(t, s, readBytes, bodyBytes)
		})
	}
}

// TestRangeAggregationOnAReorderingServerKeepsItsStatement runs
// logFilterOrderRangeQueries on a server that reorders PREWHERE conditions
// three ways: with the PrewhereInWhereOrder chopt.PrewhereKeepsWhereOrder
// gives its build, with it unset, and with it set, dropping the query
// condition cache ahead of each run so no run reads less for following
// another. The first run must send the unset run's statement and read its
// bytes — on such a server the plan keeps the arrangement it had before the
// order mattered. The set run must send a different statement with the same
// answer, so the arrangement it renders for a server that keeps WHERE order
// is answer-preserving here too.
func TestRangeAggregationOnAReorderingServerKeepsItsStatement(t *testing.T) {
	ctx := context.Background()
	s, resolved, _ := startLogFilterOrder(ctx, t, logFilterReorderImage, true)
	unset := logFilterOrderMux(t, s, false)
	set := logFilterOrderMuxWith(t, s, true)
	for _, query := range logFilterOrderRangeQueries {
		t.Run(query, func(t *testing.T) {
			params := func() url.Values {
				return url.Values{"query": {query}, "step": {strconv.FormatFloat(logFilterOrderStep.Seconds(), 'f', -1, 64)}}
			}
			var a, b, c json.RawMessage
			s.dropConditionCache(ctx, t)
			readResolved, sqlResolved := queryLogFilterOrderStatement(ctx, t, s, resolved, params(), &a)
			s.dropConditionCache(ctx, t)
			readUnset, sqlUnset := queryLogFilterOrderStatement(ctx, t, s, unset, params(), &b)
			s.dropConditionCache(ctx, t)
			readSet, sqlSet := queryLogFilterOrderStatement(ctx, t, s, set, params(), &c)
			t.Logf("%s: resolved %d bytes, unset %d, set %d", s.version, readResolved, readUnset, readSet)
			if sqlResolved != sqlUnset || readResolved != readUnset || string(a) != string(b) {
				t.Errorf("the strategies %s resolves to change the statement:\n%s\n%s", s.version, sqlResolved, sqlUnset)
			}
			if sqlSet == sqlUnset {
				t.Errorf("PrewhereInWhereOrder set renders the unset statement: %s", sqlSet)
			}
			if string(c) != string(b) {
				t.Errorf("PrewhereInWhereOrder set answers %s; unset, %s", c, b)
			}
		})
	}
}

// startLogFilterOrder starts image, seeds the stream and mounts the Loki API
// over it (logFilterOrderMux), with the strategies the server's build
// resolves to when resolved is set and with PrewhereInWhereOrder unset
// otherwise. It returns the server, the mux and the bytes the body column
// holds.
func startLogFilterOrder(ctx context.Context, t *testing.T, image string, resolved bool) (*server, *http.ServeMux, uint64) {
	t.Helper()
	s := startServer(ctx, t, image)
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("apply the logs schema: %v", err)
	}
	seedLogFilterOrder(ctx, t, s)
	var bodyBytes uint64
	if err := s.admin.Conn().QueryRow(ctx, "SELECT sum(length(Body)) FROM otel_logs").Scan(&bodyBytes); err != nil {
		t.Fatalf("measure the body column: %v", err)
	}
	return s, logFilterOrderMux(t, s, resolved), bodyBytes
}

// logFilterOrderMux mounts the Loki API over s. With resolved, the handler
// renders under the PrewhereInWhereOrder s's build resolves to
// (chopt.PrewhereKeepsWhereOrder); otherwise under it unset.
func logFilterOrderMux(t *testing.T, s *server, resolved bool) *http.ServeMux {
	t.Helper()
	return logFilterOrderMuxWith(t, s, resolved && chopt.PrewhereKeepsWhereOrder(s.version))
}

// logFilterOrderMuxWith mounts the Loki API over s, rendering under
// PrewhereInWhereOrder set to inOrder.
func logFilterOrderMuxWith(t *testing.T, s *server, inOrder bool) *http.ServeMux {
	t.Helper()
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	cell.SetPrewhereInWhereOrder(inOrder)
	h := loki.New(s.client(t, adminUser, adminPassword, chclient.Config{}), schema.DefaultOTelLogs(), nil)
	h.Strategies = cell
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux
}

// queryLogFilterOrder sends a query_range over the seeded window with params,
// decodes the answer into out, and returns the bytes the statement read.
func queryLogFilterOrder(ctx context.Context, t *testing.T, s *server, mux *http.ServeMux, params url.Values, out any) uint64 {
	t.Helper()
	readBytes, _ := queryLogFilterOrderStatement(ctx, t, s, mux, params, out)
	return readBytes
}

// queryLogFilterOrderStatement is queryLogFilterOrder that also returns the
// statement the server ran.
func queryLogFilterOrderStatement(ctx context.Context, t *testing.T, s *server, mux *http.ServeMux, params url.Values, out any) (uint64, string) {
	t.Helper()
	params.Set("start", strconv.FormatInt(logFilterOrderStart.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(logFilterOrderEnd.UnixNano(), 10))
	qid := "log-filter-order-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/query_range?"+params.Encode(), nil).
		WithContext(chclient.WithQueryID(ctx, qid))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	s.flushLogs(ctx, t)
	var readBytes uint64
	var statement string
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT read_bytes, query FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?", qid).Scan(&readBytes, &statement); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return readBytes, statement
}

// requireBodySpared fails t unless readBytes is below bodyBytes.
func requireBodySpared(t *testing.T, s *server, readBytes, bodyBytes uint64) {
	t.Helper()
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
	s.exec(ctx, t, `INSERT INTO otel_logs (Timestamp, ServiceName, SeverityText, SeverityNumber, Body, ResourceAttributes, LogAttributes) `+
		`SELECT toDateTime64(?, 9) + toIntervalMillisecond(number), 'tempo', 'ERROR', 17, `+
		`leftPad(concat(if(number % ? = 0, 'request failed ', 'request retried '), toString(number), ' '), ?, 'x'), `+
		resource+`, map('status', '500') FROM numbers(?)`,
		errorAt, logFilterOrderFailedEvery, logFilterOrderBodyBytes, logFilterOrderErrorRows)
	s.exec(ctx, t, "OPTIMIZE TABLE otel_logs FINAL")
}
