//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// metadataSpillImage is the 26.9 build that carries the external DISTINCT
// (ClickHouse#116569) and that the distinct-spill probe also pins.
const metadataSpillImage = "clickhouse/clickhouse-server:26.9.25.15-alpine"

// metadataSpillRolledBackImage predates both version-gated spill thresholds:
// max_bytes_before_external_join (26.4) and max_bytes_before_external_distinct
// (26.9). The droppable-setting probe pins it too.
const metadataSpillRolledBackImage = "clickhouse/clickhouse-server:26.3.17.56-alpine"

// Probe shape. Every seeded span carries the span attribute
// metadataSpillTag with a unique value metadataSpillValueBytes long, so the
// tag-value discovery statement's `SELECT DISTINCT` holds one hash-set entry
// per span: at metadataSpillSpans spans that set outgrows
// metadataSpillQueryMemoryBytes, the per-query cap the client stamps as
// max_memory_usage.
//
// Tag values, not tag names: the names come from the LowCardinality key
// column, whose DISTINCT keeps a fixed-size hash per key whatever its length,
// so no seed this package can afford drives it past a cap the server can
// still spill under. The values are plain strings, held whole.
const (
	metadataSpillSpans            = 3_000_000
	metadataSpillValueBytes       = 96
	metadataSpillTag              = "session.id"
	metadataSpillQueryMemoryBytes = 512 << 20
	metadataSpillProbeBudget      = 15 * time.Minute
	settingExternalGroupBy        = "max_bytes_before_external_group_by"
	settingExternalSort           = "max_bytes_before_external_sort"
)

// metadataSpillWindow is the hour every seeded span lands in.
var (
	metadataSpillWindowStart = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	metadataSpillWindowEnd   = metadataSpillWindowStart.Add(time.Hour)
)

// TestMetadataSpill_TagValuesUnderCappedMemory drives a high-cardinality
// tag-value discovery request, a statement the Tempo handler dispatches
// without the engine, through the production handler under a capped
// max_memory_usage, with the engine's settings rules resolved from
// "auto,distinct_spill":
//
//   - with SettingsRules.DistinctSpill switched off the request fails and the
//     statement aborts with MEMORY_LIMIT_EXCEEDED (code 241): its DISTINCT
//     outgrows the cap, and the group_by/sort thresholds do not reach it;
//   - with it on the request completes with every seeded value, the statement
//     ran with every spill threshold the engine stamps at cap/2
//     (system.query_log's Settings), and its DISTINCT spilled
//     (ExternalDistinctWritePart).
func TestMetadataSpill_TagValuesUnderCappedMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), metadataSpillProbeBudget)
	defer cancel()
	s := startServer(ctx, t, metadataSpillImage)

	set, _, err := chopt.Resolve(chopt.Config{
		Optimizations: chopt.SelectionAuto + "," + chopt.FeatureDistinctSpill,
		Mode:          chopt.Permissive,
	}, s.version)
	if err != nil {
		t.Fatalf("resolve against %s: %v", s.version, err)
	}
	rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	if !rules.DistinctSpill || !rules.JoinSpill {
		t.Fatalf("rules resolved on %s carry DistinctSpill = %v, JoinSpill = %v; want both", s.version, rules.DistinctSpill, rules.JoinSpill)
	}
	seedMetadataSpillProbe(ctx, t, s, metadataSpillSpans)
	client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: metadataSpillQueryMemoryBytes})

	control := rules
	control.DistinctSpill = false
	qid := fmt.Sprintf("metadata-spill-off-%d", time.Now().UnixNano())
	code, body := runTagValuesProbe(chclient.WithQueryID(ctx, qid), t, client, control)
	if code == http.StatusOK {
		t.Fatalf("without the DISTINCT threshold the probe answered 200; it no longer outgrows the %d-byte cap, so the run below proves nothing",
			metadataSpillQueryMemoryBytes)
	}
	if got, msg := s.finishedException(ctx, t, qid); got != memoryLimitExceeded {
		t.Fatalf("without the DISTINCT threshold the probe failed with HTTP %d (%.200s) and server code %d (%s); want code %d",
			code, body, got, msg, memoryLimitExceeded)
	} else {
		t.Logf("without the DISTINCT threshold: HTTP %d, server code %d: %s", code, got, msg)
	}

	qid = fmt.Sprintf("metadata-spill-on-%d", time.Now().UnixNano())
	code, body = runTagValuesProbe(chclient.WithQueryID(ctx, qid), t, client, rules)
	if code != http.StatusOK {
		t.Fatalf("with the engine's thresholds the probe answered HTTP %d: %.500s", code, body)
	}
	var resp tempo.SearchTagValuesResponseV2
	decodeJSON(t, body, &resp)
	if len(resp.TagValues) != metadataSpillSpans {
		t.Errorf("with the engine's thresholds the probe returned %d tag values; want %d", len(resp.TagValues), metadataSpillSpans)
	}
	threshold := strconv.FormatInt(metadataSpillQueryMemoryBytes/2, 10)
	for _, name := range []string{
		settingExternalGroupBy, settingExternalSort,
		chopt.SettingMaxBytesBeforeExternalJoin, chopt.SettingMaxBytesBeforeExternalDistinct,
	} {
		s.assertEffectiveSetting(ctx, t, qid, name, threshold)
	}
	if parts := s.profileEvent(ctx, t, qid, externalDistinctWritePart); parts == 0 {
		t.Errorf("with the engine's thresholds the probe completed without spilling its DISTINCT (%s = 0)", externalDistinctWritePart)
	} else {
		t.Logf("with the engine's thresholds: %d tag values, %s = %d, peak memory %d of %d bytes",
			len(resp.TagValues), externalDistinctWritePart, parts, s.peakMemory(ctx, t, qid), metadataSpillQueryMemoryBytes)
	}
}

// metadataRolledBackSpans is the rolled-back probe's seed: only the request's
// outcome and the settings it ran with are under test there, not memory.
const metadataRolledBackSpans = 10

// TestMetadataSpill_RolledBackNodeAnswers drives tag-value discovery with the
// settings rules a 26.9 fleet resolves (join_spill and distinct_spill in)
// against a node that knows neither threshold, the state a rolling rollback
// leaves. The node rejects the statement for the unknown setting, and the
// client sends it again without the version-gated thresholds, so the request
// still answers, and the statement the node ran keeps the group_by/sort
// thresholds every supported build knows.
func TestMetadataSpill_RolledBackNodeAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := startServer(ctx, t, metadataSpillRolledBackImage)
	seedMetadataSpillProbe(ctx, t, s, metadataRolledBackSpans)
	client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: metadataSpillQueryMemoryBytes})
	rec := &rejections{}
	client.SetUnknownNameHook(rec.record)

	rules := engine.SettingsRules{JoinSpill: true, DistinctSpill: true}
	qid := fmt.Sprintf("metadata-spill-rolled-back-%d", time.Now().UnixNano())
	code, body := runTagValuesProbe(chclient.WithQueryID(ctx, qid), t, client, rules)
	if code != http.StatusOK {
		t.Fatalf("on %s the probe answered HTTP %d: %.500s; want the statement retried without the thresholds the node lacks",
			s.version, code, body)
	}
	var resp tempo.SearchTagValuesResponseV2
	decodeJSON(t, body, &resp)
	if len(resp.TagValues) != metadataRolledBackSpans {
		t.Errorf("on %s the probe returned %d tag values; want %d", s.version, len(resp.TagValues), metadataRolledBackSpans)
	}
	got := rec.take()
	if len(got) == 0 {
		t.Errorf("on %s no rejection was reported; the node knows a threshold it predates", s.version)
	}
	for _, rej := range got {
		if !rej.Retried {
			t.Errorf("on %s: rejection %+v was not retried", s.version, rej)
		}
	}
	threshold := strconv.FormatInt(metadataSpillQueryMemoryBytes/2, 10)
	for _, name := range []string{settingExternalGroupBy, settingExternalSort} {
		s.assertEffectiveSetting(ctx, t, qid, name, threshold)
	}
	for _, name := range []string{chopt.SettingMaxBytesBeforeExternalJoin, chopt.SettingMaxBytesBeforeExternalDistinct} {
		if s.finishedCarries(ctx, t, qid, name) {
			t.Errorf("on %s the statement that finished carries %s, a setting the node does not know", s.version, name)
		}
	}
}

// seedMetadataSpillProbe applies cerberus's traces DDL and writes spans
// spans inside the probe window, span n carrying the single span attribute
// metadataSpillTag, whose value is n left-padded to metadataSpillValueBytes.
func seedMetadataSpillProbe(ctx context.Context, t *testing.T, s *server, spans int) {
	t.Helper()
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    toDateTime64(%[1]d, 9) + toIntervalMillisecond(1 + number %% 3599999) AS Timestamp,
    leftPad(hex(number), 32, '0') AS TraceId,
    leftPad(hex(number), 16, '0') AS SpanId,
    '' AS ParentSpanId,
    'op' AS SpanName,
    'Server' AS SpanKind,
    'api' AS ServiceName,
    map('service.name', 'api') AS ResourceAttributes,
    map('%[2]s', leftPad(toString(number), %[3]d, 'v')) AS SpanAttributes,
    1000 AS Duration,
    'Unset' AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM numbers(%[4]d)`,
		metadataSpillWindowStart.Unix(), metadataSpillTag, metadataSpillValueBytes, spans))
	s.exec(ctx, t, "OPTIMIZE TABLE otel_traces FINAL")
}

// runTagValuesProbe issues /api/v2/search/tag/span.<metadataSpillTag>/values
// over the seeded window through a Tempo handler on client whose engine runs
// under rules, and returns the response status and body.
func runTagValuesProbe(ctx context.Context, t *testing.T, client *chclient.Client, rules engine.SettingsRules) (int, string) {
	t.Helper()
	th := tempo.New(client, schema.DefaultOTelTraces(), "chserver-metadata-spill", nil)
	th.Engine.SetSettings(rules)
	mux := http.NewServeMux()
	th.Mount(mux)
	params := url.Values{}
	params.Set("start", strconv.FormatInt(metadataSpillWindowStart.Unix(), 10))
	params.Set("end", strconv.FormatInt(metadataSpillWindowEnd.Unix(), 10))
	path := "/api/v2/search/tag/span." + metadataSpillTag + "/values?" + params.Encode()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// finishedCarries reports whether the statement query qid finished with
// carried the setting name explicitly.
func (s *server) finishedCarries(ctx context.Context, t *testing.T, qid, name string) bool {
	t.Helper()
	s.flushLogs(ctx, t)
	var (
		present uint8
		rows    uint64
	)
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(mapContains(Settings, ?)), count() FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?",
		name, qid).Scan(&present, &rows); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if rows == 0 {
		t.Fatalf("query_log has no finished query %s", qid)
	}
	return present != 0
}
