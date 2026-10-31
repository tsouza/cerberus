//go:build integration

package chserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// distinctSpillBuilds are the builds the external-DISTINCT probe runs on, each
// with whether it carries max_bytes_before_external_distinct: the latest 26.8
// build the repository pins (the line has no backport of ClickHouse#116569),
// the first 26.9 release (the feature's floor) and the 26.9 build the other
// probes in this package pin.
var distinctSpillBuilds = []struct {
	image      string
	hasSetting bool
}{
	{"clickhouse/clickhouse-server:26.8.10.6-alpine", false},
	{"clickhouse/clickhouse-server:26.9.1.1629-alpine", true},
	{"clickhouse/clickhouse-server:26.9.25.15-alpine", true},
}

// Probe shape. Every trace is one root span of distinctSpillParentService
// with distinctSpillChildrenPerTrace children of distinctSpillChildService.
// The structural query's rooted closure walks every span of every trace, and
// its `SELECT DISTINCT TraceId, SpanId` holds one hash-set entry per span: at
// distinctSpillTraces traces that set outgrows distinctSpillQueryMemoryBytes,
// the per-query cap the client stamps as max_memory_usage, while the
// closure's own in-memory working table stays under it.
const (
	distinctSpillTraces           = 1_500_000
	distinctSpillChildrenPerTrace = 1
	distinctSpillQueryMemoryBytes = 1 << 30
	distinctSpillParentService    = "frontend"
	distinctSpillChildService     = "api"
	settingExternalDistinct       = "max_bytes_before_external_distinct"
	externalDistinctWritePart     = "ExternalDistinctWritePart"
	memoryLimitExceeded           = 241
	unknownSetting                = 115
	distinctSpillProbeBudget      = 15 * time.Minute
)

// distinctSpillTraceQL lowers to the structural rooted closure: a recursive
// CTE whose anchor and steps are SELECT DISTINCT TraceId, SpanId,
// ParentSpanId, read back through SELECT DISTINCT TraceId, SpanId.
const distinctSpillTraceQL = `{ resource.service.name = "` + distinctSpillParentService +
	`" } > { resource.service.name = "` + distinctSpillChildService + `" } | count_over_time()`

// distinctSpillWindow is the hour every seeded span lands in.
var (
	distinctSpillWindowStart = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	distinctSpillWindowEnd   = distinctSpillWindowStart.Add(time.Hour)
)

// TestDistinctSpill_StructuralTraceQLAcrossBuilds checks
// chopt.FeatureDistinctSpill against real servers. On every build it requires
// the feature's floor to agree with the server: the setting exists exactly
// where an explicit listing resolves the feature in, and stamping it below the
// floor fails with code 115, which is why the stamp is version-gated. On a
// build that carries the setting it drives a high-cardinality structural
// TraceQL query through the production Tempo handler under a capped
// max_memory_usage twice, with the engine's settings rules resolved from
// "auto,distinct_spill":
//
//   - with SettingsRules.DistinctSpill switched off, the query aborts with
//     MEMORY_LIMIT_EXCEEDED (code 241) — the other spill thresholds, which
//     the same rules still stamp, do not reach a DISTINCT;
//   - with it on, the query completes with every child span counted, ran with
//     max_bytes_before_external_distinct = cap/2, and spilled its DISTINCT
//     (system.query_log records ExternalDistinctWritePart).
func TestDistinctSpill_StructuralTraceQLAcrossBuilds(t *testing.T) {
	for _, build := range distinctSpillBuilds {
		t.Run(build.image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), distinctSpillProbeBudget)
			defer cancel()
			s := startServer(ctx, t, build.image)

			set, _, err := chopt.Resolve(chopt.Config{
				Optimizations: chopt.SelectionAuto + "," + chopt.FeatureDistinctSpill,
				Mode:          chopt.Permissive,
			}, s.version)
			if err != nil {
				t.Fatalf("resolve against %s: %v", s.version, err)
			}
			if got := set.Has(chopt.FeatureDistinctSpill); got != build.hasSetting {
				t.Fatalf("listed distinct_spill resolves in on %s = %v; the server carries %s = %v",
					s.version, got, settingExternalDistinct, build.hasSetting)
			}
			if got := s.hasSetting(ctx, t, settingExternalDistinct); got != build.hasSetting {
				t.Fatalf("%s carries %s = %v; want %v", s.version, settingExternalDistinct, got, build.hasSetting)
			}
			if !build.hasSetting {
				err := s.admin.Conn().Exec(ctx, "SELECT 1 SETTINGS "+settingExternalDistinct+" = 1")
				if code := exceptionCode(err); code != unknownSetting {
					t.Fatalf("stamping %s on %s: error %v (code %d); want code %d, the unknown-setting refusal the floor avoids",
						settingExternalDistinct, s.version, err, code, unknownSetting)
				}
				return
			}

			seedDistinctSpillProbe(ctx, t, s)
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
			if !rules.DistinctSpill || !rules.JoinSpill {
				t.Fatalf("rules resolved on %s carry DistinctSpill = %v, JoinSpill = %v; want both", s.version, rules.DistinctSpill, rules.JoinSpill)
			}
			if rules.ResultCache {
				t.Fatal("the resolved rules enable result_cache; both runs must be computed, not served from cache")
			}
			client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: distinctSpillQueryMemoryBytes})

			control := rules
			control.DistinctSpill = false
			qid := fmt.Sprintf("distinct-spill-off-%d", time.Now().UnixNano())
			code, body := runDistinctSpillProbe(chclient.WithQueryID(ctx, qid), t, client, control)
			if code == http.StatusOK {
				t.Fatalf("without the stamp the probe answered 200 (%s); it no longer outgrows the %d-byte cap, so the stamped run below proves nothing",
					body, distinctSpillQueryMemoryBytes)
			}
			if got, msg := s.finishedException(ctx, t, qid); got != memoryLimitExceeded {
				t.Fatalf("without the stamp the probe failed with HTTP %d and server code %d (%s); want code %d", code, got, msg, memoryLimitExceeded)
			} else {
				t.Logf("without the stamp: HTTP %d, server code %d: %s", code, got, msg)
			}

			qid = fmt.Sprintf("distinct-spill-on-%d", time.Now().UnixNano())
			code, body = runDistinctSpillProbe(chclient.WithQueryID(ctx, qid), t, client, rules)
			if code != http.StatusOK {
				t.Fatalf("with the stamp the probe answered HTTP %d: %s", code, body)
			}
			var resp tempo.MetricsQueryInstantResponse
			decodeJSON(t, body, &resp)
			var counted float64
			for _, series := range resp.Series {
				counted += series.Value
			}
			if want := float64(distinctSpillTraces * distinctSpillChildrenPerTrace); counted != want {
				t.Errorf("with the stamp the probe counted %v child spans; want %v", counted, want)
			}
			s.assertEffectiveSetting(ctx, t, qid, settingExternalDistinct, strconv.FormatInt(distinctSpillQueryMemoryBytes/2, 10))
			if parts := s.profileEvent(ctx, t, qid, externalDistinctWritePart); parts == 0 {
				t.Errorf("with the stamp the probe completed without spilling its DISTINCT (%s = 0)", externalDistinctWritePart)
			} else {
				t.Logf("with the stamp: %v child spans counted, %s = %d, peak memory %d of %d bytes",
					counted, externalDistinctWritePart, parts, s.peakMemory(ctx, t, qid), distinctSpillQueryMemoryBytes)
			}
		})
	}
}

// seedDistinctSpillProbe applies cerberus's traces DDL and writes
// distinctSpillTraces traces: span 0 of trace n is the root, of
// distinctSpillParentService, and spans 1..distinctSpillChildrenPerTrace are
// its children, of distinctSpillChildService.
func seedDistinctSpillProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	spansPerTrace := distinctSpillChildrenPerTrace + 1
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    toDateTime64(%[1]d, 9) + toIntervalMillisecond(1 + number %% 3599999) AS Timestamp,
    leftPad(hex(intDiv(number, %[2]d)), 32, '0') AS TraceId,
    leftPad(hex(number), 16, '0') AS SpanId,
    if(number %% %[2]d = 0, '', leftPad(hex(number - number %% %[2]d), 16, '0')) AS ParentSpanId,
    'op' AS SpanName,
    'Server' AS SpanKind,
    if(number %% %[2]d = 0, '%[3]s', '%[4]s') AS ServiceName,
    map('service.name', if(number %% %[2]d = 0, '%[3]s', '%[4]s')) AS ResourceAttributes,
    map() AS SpanAttributes,
    1000 AS Duration,
    'Unset' AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM numbers(%[5]d)`,
		distinctSpillWindowStart.Unix(), spansPerTrace, distinctSpillParentService, distinctSpillChildService,
		distinctSpillTraces*spansPerTrace))
	s.exec(ctx, t, "OPTIMIZE TABLE otel_traces FINAL")
}

// runDistinctSpillProbe issues distinctSpillTraceQL as an instant metrics
// query over the seeded window through a Tempo handler on client whose engine
// runs under rules, and returns the response status and body.
func runDistinctSpillProbe(ctx context.Context, t *testing.T, client *chclient.Client, rules engine.SettingsRules) (int, string) {
	t.Helper()
	th := tempo.New(client, schema.DefaultOTelTraces(), "chserver-distinct-spill", nil)
	th.Engine.SetSettings(rules)
	mux := http.NewServeMux()
	th.Mount(mux)
	params := url.Values{}
	params.Set("q", distinctSpillTraceQL)
	params.Set("start", strconv.FormatInt(distinctSpillWindowStart.Unix(), 10))
	params.Set("end", strconv.FormatInt(distinctSpillWindowEnd.Unix(), 10))
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/query?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// hasSetting reports whether the server knows the setting name.
func (s *server) hasSetting(ctx context.Context, t *testing.T, name string) bool {
	t.Helper()
	var n uint64
	if err := s.admin.Conn().QueryRow(ctx, "SELECT count() FROM system.settings WHERE name = ?", name).Scan(&n); err != nil {
		t.Fatalf("read system.settings for %s: %v", name, err)
	}
	return n > 0
}

// finishedException is the server exception code and message query qid ended
// with (0 and "" when it finished cleanly).
func (s *server) finishedException(ctx context.Context, t *testing.T, qid string) (int32, string) {
	t.Helper()
	s.flushLogs(ctx, t)
	var (
		code int32
		msg  string
		rows uint64
	)
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(exception_code), max(exception), count() FROM system.query_log WHERE type != 'QueryStart' AND query_id = ?",
		qid).Scan(&code, &msg, &rows); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if rows == 0 {
		t.Fatalf("query_log has no finished entry for %s", qid)
	}
	return code, msg
}

// profileEvent sums one ProfileEvents counter over query qid's finished entries.
func (s *server) profileEvent(ctx context.Context, t *testing.T, qid, event string) uint64 {
	t.Helper()
	s.flushLogs(ctx, t)
	var n uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT sum(ProfileEvents[?]) FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?",
		event, qid).Scan(&n); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return n
}

// peakMemory is query qid's peak memory_usage.
func (s *server) peakMemory(ctx context.Context, t *testing.T, qid string) uint64 {
	t.Helper()
	s.flushLogs(ctx, t)
	var n uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(memory_usage) FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?", qid).Scan(&n); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return n
}

// exceptionCode is the ClickHouse exception code err carries, or 0.
func exceptionCode(err error) int32 {
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		return ex.Code
	}
	return 0
}

// decodeJSON decodes body into out.
func decodeJSON(t *testing.T, body string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}
