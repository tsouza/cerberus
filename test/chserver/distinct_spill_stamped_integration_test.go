//go:build integration

package chserver

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/schema"
)

// stampedProbeBudget bounds the seed and every probe of
// TestDistinctSpill_StampedShapesAnswer.
const stampedProbeBudget = 15 * time.Minute

// stampedCapFactors are the caps, as multiples of a shape's unspilled peak,
// that TestDistinctSpill_StampedShapesAnswer requires the request to answer
// under: from harmCapFactors' first, just above the peak, to just under
// twice it.
var stampedCapFactors = []float64{1.11, 1.2, 1.4, 1.6, 1.9}

// stampedAttempt is what system.query_log records of one run of a
// statement: how it ended, the DISTINCT threshold it carried, and its peak.
type stampedAttempt struct {
	logType  string
	distinct string
	memory   uint64
}

// stampedShape is one Tempo tag-discovery request that keeps the DISTINCT
// spill threshold over String values.
type stampedShape struct {
	name   string
	path   string
	params url.Values
	// answers decodes the response body into the number of values it lists.
	answers func(t *testing.T, body string) int
}

// TestDistinctSpill_StampedShapesAnswer drives the two Tempo tag-discovery
// statements that keep the DISTINCT spill threshold over long String values,
// the auto-scope tag values of the materialized rpc.method span attribute
// and the event tag names, over 1M unique 256-byte values each
// (seedStampedProbe), through the production handler on ClickHouse 26.9
// with the engine's settings rules resolved from "auto,distinct_spill", on
// harmProbeThreads threads. For each it measures the statement's unspilled
// peak P, the largest of harmCalibrationRuns runs under harmCalibrationCap,
// and then requires, under every cap in stampedCapFactors x P, the request
// to answer every seeded value from one run of the statement, carrying the
// DISTINCT threshold at half the cap.
func TestDistinctSpill_StampedShapesAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), stampedProbeBudget)
	defer cancel()
	s := startServer(ctx, t, metadataSpillImage)
	rules := resolveDistinctSpillRules(t, s)
	traces := seedStampedProbe(ctx, t, s)

	for _, shape := range []stampedShape{
		{"auto-scope values", "/api/v2/search/tag/." + harmSpanKey + "/values", url.Values{}, tagValuesAnswers},
		{"event tag names", "/api/search/tags", url.Values{"scope": {"event"}}, tagNamesAnswers},
	} {
		t.Run(shape.name, func(t *testing.T) {
			// run sends the request under memCap and returns its status, body
			// and the one run of its statement query_log records.
			run := func(memCap int64, tag string) (int, string, stampedAttempt) {
				qid := fmt.Sprintf("stamped-%s-%d", tag, time.Now().UnixNano())
				client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: memCap})
				reqCtx := chclient.WithQuerySetting(chclient.WithQueryID(ctx, qid), settingMaxThreads, harmProbeThreads)
				params := url.Values{}
				maps.Copy(params, shape.params)
				code, body := runTempoProbeOn(reqCtx, t, client, rules, traces, shape.path, params)
				attempts := s.stampedAttempts(ctx, t, qid)
				if len(attempts) != 1 {
					t.Fatalf("under the %d-byte cap the request ran its statement %d times (%v); want once", memCap, len(attempts), attempts)
				}
				return code, body, attempts[0]
			}

			peak := int64(0)
			for range harmCalibrationRuns {
				code, body, a := run(harmCalibrationCap, "peak")
				if code != http.StatusOK {
					t.Fatalf("under the %d-byte calibration cap the probe answered HTTP %d: %.500s", harmCalibrationCap, code, body)
				}
				if got := shape.answers(t, body); got != harmSpans {
					t.Fatalf("under the calibration cap the probe returned %d values; want %d", got, harmSpans)
				}
				peak = max(peak, int64(a.memory))
			}

			for _, factor := range stampedCapFactors {
				memCap := int64(factor * float64(peak))
				code, body, a := run(memCap, "band")
				if code != http.StatusOK {
					t.Errorf("under the %d-byte cap (%.2f x the %d-byte unspilled peak) the probe answered HTTP %d: %.300s",
						memCap, factor, peak, code, body)
					continue
				}
				if got := shape.answers(t, body); got != harmSpans {
					t.Errorf("under the %d-byte cap the probe returned %d values; want %d", memCap, got, harmSpans)
				}
				if want := strconv.FormatInt(memCap/2, 10); a.logType != queryLogFinished[0] || a.distinct != want {
					t.Errorf("under the %d-byte cap the statement ran as %s with %s = %q; want %s with %s",
						memCap, a.logType, chopt.SettingMaxBytesBeforeExternalDistinct, a.distinct, queryLogFinished[0], want)
				}
				t.Logf("%.2f x the %d-byte unspilled peak (%d bytes): answered, threshold %s, peak %d bytes",
					factor, peak, memCap, a.distinct, a.memory)
			}
		})
	}
}

// stampedAttempts reads every run of statement qid from system.query_log,
// finished or failed, in the order they ran.
func (s *server) stampedAttempts(ctx context.Context, t *testing.T, qid string) []stampedAttempt {
	t.Helper()
	s.flushLogs(ctx, t)
	types := append(append([]string{}, queryLogFinished...), queryLogFailed...)
	rows, err := s.admin.Conn().Query(ctx,
		"SELECT toString(type), Settings[?], memory_usage FROM system.query_log "+
			"WHERE type IN ? AND query_id = ? ORDER BY event_time_microseconds",
		chopt.SettingMaxBytesBeforeExternalDistinct, types, qid)
	if err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	defer rows.Close()
	var out []stampedAttempt
	for rows.Next() {
		var a stampedAttempt
		if err := rows.Scan(&a.logType, &a.distinct, &a.memory); err != nil {
			t.Fatalf("scan query_log for %s: %v", qid, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return out
}

// seedStampedProbe applies cerberus's traces DDL with the default
// materialized attribute columns and writes harmSpans spans inside the probe
// window, each carrying its own harmValueBytes-long value as the
// materialized rpc.method span attribute and as the key of one span event's
// attributes. It returns the traces schema the handler reads them under.
func seedStampedProbe(ctx context.Context, t *testing.T, s *server) schema.Traces {
	t.Helper()
	traces := applyMaterializedTracesDDL(ctx, t, s)
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState,
     `+"`Events.Timestamp`, `Events.Name`, `Events.Attributes`"+`)
SELECT
    toDateTime64(%[1]d, 9) + toIntervalMillisecond(1 + number %% 3599999) AS Timestamp,
    leftPad(hex(number), 32, '0') AS TraceId,
    leftPad(hex(number), 16, '0') AS SpanId,
    '' AS ParentSpanId, 'op' AS SpanName, 'SPAN_KIND_SERVER' AS SpanKind,
    'api' AS ServiceName,
    map('%[2]s', 'ns') AS ResourceAttributes,
    map('%[3]s', %[4]s) AS SpanAttributes,
    1000 AS Duration, 'STATUS_CODE_UNSET' AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState,
    [Timestamp] AS `+"`Events.Timestamp`"+`,
    ['event'] AS `+"`Events.Name`"+`,
    [map(%[5]s, 'v')] AS `+"`Events.Attributes`"+`
FROM numbers(%[6]d)`,
		metadataSpillWindowStart.Unix(),
		harmResourceKey,
		harmSpanKey, harmUniqueValue("m"),
		harmUniqueValue("e"),
		harmSpans))
	s.exec(ctx, t, "OPTIMIZE TABLE otel_traces FINAL")
	return traces
}
