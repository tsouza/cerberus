//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Conversion-setting probe (ClickHouse#119168). A conversion function captures
// the settings of the session that built it — precise_float_parsing among them
// — while the condition cache keyed the filter by the function's name and
// result type alone, so two sessions that differ only in that setting shared
// one cache entry. A LogQL numeric label filter lowers to
// toFloat64OrZero(<label>) <op> <literal>, a conversion of exactly that kind,
// and the logs table carries no skip index on the attribute values that would
// stop the WHERE writer from recording a verdict.
const (
	settingPreciseFloatParsing = "precise_float_parsing"
	// conversionProbeLiteral is a value whose fast and precise parses differ:
	// only the precise parse equals the literal the query compares against.
	conversionProbeLiteral = "1.1e-300"
	// conversionProbeOther is the value every other probe row carries; it
	// compares the same under either parse.
	conversionProbeOther = "2"
	conversionProbeLogs  = 1_200_000
	// conversionProbeLiteralLogs rows carry conversionProbeLiteral. They are
	// the oldest rows, so with the logs table's time-first sort key they fill
	// whole granules of their own.
	conversionProbeLiteralLogs = conversionProbeLogs / 2
	conversionProbeService     = "ccsvc"
	conversionProbeLabel       = "ccv"
)

// conversionProbeQueries are the two LogQL comparisons the probe runs. Which
// one a priming session with the opposite precise_float_parsing can poison
// depends on the server's default: under precise parsing only `=` matches the
// literal rows and the opposite session records them as matching nothing;
// under fast parsing only `!=` does.
var conversionProbeQueries = []string{
	`sum(count_over_time({service_name="` + conversionProbeService + `"} | ` + conversionProbeLabel + ` = ` + conversionProbeLiteral + ` [1h]))`,
	`sum(count_over_time({service_name="` + conversionProbeService + `"} | ` + conversionProbeLabel + ` != ` + conversionProbeLiteral + ` [1h]))`,
}

// conversionProbeLineFilterQuery reads the same logs with no String-to-float
// filter: the shape the conversion defect leaves the cache on for.
const conversionProbeLineFilterQuery = `sum(count_over_time({service_name="` + conversionProbeService + `"} |= "line" [1h]))`

// seedConversionProbe seeds the logs the conversion-setting probe reads.
func seedConversionProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_logs (Timestamp, ServiceName, Body, ResourceAttributes, LogAttributes, SeverityText)
SELECT
    toDateTime64(%d, 9) + toIntervalMillisecond(1 + number %% 3599999) AS Timestamp,
    '%s' AS ServiceName,
    'line' AS Body,
    map('service.name', '%s') AS ResourceAttributes,
    map('%s', if(number < %d, '%s', '%s')) AS LogAttributes,
    'info' AS SeverityText
FROM numbers(%d)`,
		probeWindowStart.Unix(), conversionProbeService, conversionProbeService,
		conversionProbeLabel, conversionProbeLiteralLogs, conversionProbeLiteral, conversionProbeOther, conversionProbeLogs))
}

// conversionTruth is the answer of each conversion probe query with the cache
// off, and the SQL cerberus sent for it, as system.query_log recorded it.
type conversionTruth struct {
	counts []float64
	sql    []string
}

// conversionTruths runs every conversion probe query with the cache off.
func (p probeMux) conversionTruths(ctx context.Context, t *testing.T, s *server) conversionTruth {
	t.Helper()
	var truth conversionTruth
	for i, q := range conversionProbeQueries {
		qid := fmt.Sprintf("cc-conversion-truth-%d-%d", i, time.Now().UnixNano())
		truth.counts = append(truth.counts, p.logCount(chclient.WithQueryID(ctx, qid), t, q, 0))
		truth.sql = append(truth.sql, s.loggedQuery(ctx, t, qid))
	}
	if truth.counts[0]+truth.counts[1] != conversionProbeLogs {
		t.Fatalf("conversion probe counts %v with the cache off; want them to partition the %d seeded logs", truth.counts, conversionProbeLogs)
	}
	return truth
}

// primeOppositeParsing is another ClickHouse client — a dashboard replaying
// the SQL cerberus emitted, from a session whose precise_float_parsing is
// the opposite of the server default — reading through the cache.
func (s *server) primeOppositeParsing(ctx context.Context, t *testing.T, sql string) {
	t.Helper()
	opposite := 1
	if s.defaultSetting(ctx, t, settingPreciseFloatParsing) == "1" {
		opposite = 0
	}
	// The settings ride in the statement's own SETTINGS clause: the raw driver
	// connection does not read chclient's per-query context settings.
	stmt := fmt.Sprintf("%s SETTINGS %s = %d, %s = 1", sql, settingPreciseFloatParsing, opposite, settingQCC)
	rows, err := s.admin.Conn().Query(ctx, stmt)
	if err != nil {
		t.Fatalf("conversion priming with %s = %d: %v", settingPreciseFloatParsing, opposite, err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("conversion priming with %s = %d: %v", settingPreciseFloatParsing, opposite, err)
	}
	rows.Close()
}

// oppositeParsingCount runs cerberus's SQL for one probe query, unchanged so
// it shares cerberus's cache key, from a session with the opposite
// precise_float_parsing and use_query_condition_cache = qcc, and returns the
// sum of its Value column.
func (s *server) oppositeParsingCount(ctx context.Context, t *testing.T, sql string, qcc int) float64 {
	t.Helper()
	opposite := 1
	if s.defaultSetting(ctx, t, settingPreciseFloatParsing) == "1" {
		opposite = 0
	}
	stmt := fmt.Sprintf("%s SETTINGS %s = %d, %s = %d", sql, settingPreciseFloatParsing, opposite, settingQCC, qcc)
	rows, err := s.admin.Conn().Query(ctx, stmt)
	if err != nil {
		t.Fatalf("opposite-parsing read with %s = %d: %v", settingQCC, qcc, err)
	}
	defer rows.Close()
	valueAt := slices.Index(rows.Columns(), conversionProbeValueColumn)
	if valueAt < 0 {
		t.Fatalf("opposite-parsing read returned columns %v; want a %s column", rows.Columns(), conversionProbeValueColumn)
	}
	cells := make([]any, len(rows.ColumnTypes()))
	for i, ct := range rows.ColumnTypes() {
		cells[i] = reflect.New(ct.ScanType()).Interface()
	}
	var total float64
	for rows.Next() {
		if err := rows.Scan(cells...); err != nil {
			t.Fatalf("opposite-parsing read: scan: %v", err)
		}
		v, ok := reflect.ValueOf(cells[valueAt]).Elem().Interface().(float64)
		if !ok {
			t.Fatalf("opposite-parsing read: %s is %T; want float64", conversionProbeValueColumn, reflect.ValueOf(cells[valueAt]).Elem().Interface())
		}
		total += v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("opposite-parsing read: %v", err)
	}
	return total
}

// conversionProbeValueColumn is the sample-value column of the SQL cerberus
// emits for a LogQL metric query.
const conversionProbeValueColumn = "Value"

// assertConversionCacheOffIsolated shows, on the server itself, that
// use_query_condition_cache=0 set on one query switches the cache off for that
// query in both directions, which is what lets cerberus gate the conversion
// shape per query:
//
//   - read: after an opposite-parsing session primes the cache, cerberus's
//     own SQL with the setting 0 returns the cache-off answer;
//   - write: after cerberus's SQL runs with the setting 0, an opposite-parsing
//     session reading through the cache returns its own cache-off answer,
//     where the same SQL run with the setting 1 poisons that session on every
//     build the defect reaches (the control that makes the write observable).
func (s *server) assertConversionCacheOffIsolated(ctx context.Context, t *testing.T, raw probeMux, truth conversionTruth, defect bool) {
	t.Helper()
	for i, q := range conversionProbeQueries {
		s.dropConditionCache(ctx, t)
		s.primeOppositeParsing(ctx, t, truth.sql[i])
		if got := raw.logCount(ctx, t, q, 0); got != truth.counts[i] {
			t.Errorf("read isolation: %s with %s = 0 after an opposite-parsing priming = %v; want %v", q, settingQCC, got, truth.counts[i])
		}
	}
	controlPoisoned := false
	for i, q := range conversionProbeQueries {
		s.dropConditionCache(ctx, t)
		oppositeTruth := s.oppositeParsingCount(ctx, t, truth.sql[i], 0)

		s.dropConditionCache(ctx, t)
		raw.logCount(ctx, t, q, 0)
		if got := s.oppositeParsingCount(ctx, t, truth.sql[i], 1); got != oppositeTruth {
			t.Errorf("write isolation: opposite-parsing read through the cache after %s ran with %s = 0 = %v; want %v",
				q, settingQCC, got, oppositeTruth)
		}

		s.dropConditionCache(ctx, t)
		raw.logCount(ctx, t, q, 1)
		if s.oppositeParsingCount(ctx, t, truth.sql[i], 1) != oppositeTruth {
			controlPoisoned = true
		}
	}
	if controlPoisoned != defect {
		t.Errorf("write control: an opposite-parsing read after cerberus's SQL ran with %s = 1 poisoned = %v; want %v",
			settingQCC, controlPoisoned, defect)
	}
}

// conversionPoisoned reports whether, for either probe query, a priming read
// from a session with the opposite precise_float_parsing changes the answer a
// later read through the cache (qcc: 1, or the handler's own wiring when
// negative) returns.
func (p probeMux) conversionPoisoned(ctx context.Context, t *testing.T, s *server, truth conversionTruth, qcc int) bool {
	t.Helper()
	poisoned := false
	for i, q := range conversionProbeQueries {
		s.dropConditionCache(ctx, t)
		s.primeOppositeParsing(ctx, t, truth.sql[i])
		if got := p.logCount(ctx, t, q, qcc); got != truth.counts[i] {
			t.Logf("%s: %s after an opposite-parsing priming = %v; the cache-off answer is %v", s.image, q, got, truth.counts[i])
			poisoned = true
		}
	}
	return poisoned
}

// loggedQuery is the text of finished query qid in system.query_log.
func (s *server) loggedQuery(ctx context.Context, t *testing.T, qid string) string {
	t.Helper()
	s.flushLogs(ctx, t)
	var sql string
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT query FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?", qid).Scan(&sql); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return sql
}

// logCount runs a LogQL instant vector query at the probe window's end and
// returns its single sample, or 0 when it returns no series. ctx is the
// request's context.
func (p probeMux) logCount(ctx context.Context, t *testing.T, q string, qcc int) float64 {
	t.Helper()
	params := url.Values{}
	params.Set("query", q)
	params.Set("time", strconv.FormatInt(probeWindowEnd.Unix(), 10))
	var resp struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	serveJSON(cacheCtx(ctx, qcc), t, p.mux, "/loki/api/v1/query?"+params.Encode(), &resp)
	switch len(resp.Data.Result) {
	case 0:
		return 0
	case 1:
	default:
		t.Fatalf("%s returned %d series; want at most one", q, len(resp.Data.Result))
	}
	sample := resp.Data.Result[0].Value
	if len(sample) != promSampleFields {
		t.Fatalf("%s: value %v is not a [timestamp, value] pair", q, sample)
	}
	text, ok := sample[1].(string)
	if !ok {
		t.Fatalf("%s: sample value %v is not a string", q, sample[1])
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		t.Fatalf("%s: sample value %q: %v", q, text, err)
	}
	return v
}
