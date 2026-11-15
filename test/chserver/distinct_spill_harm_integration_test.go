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

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// Harmed-shape probe seed. Every seeded span carries, in each column one of
// the withheld Tempo shapes reads, its own unique value harmValueBytes long:
// the span name, kind and status; the materialized rpc.method span attribute
// and k8s.namespace.name resource attribute; one span and one resource
// attribute key.
const (
	harmSpans      = 1_000_000
	harmValueBytes = 256
	// harmProbeBudget bounds the seed and every probe of
	// TestDistinctSpill_WithheldShapesAnswer.
	harmProbeBudget = 15 * time.Minute
	// harmCalibrationCap is the cap a shape's unspilled peak is measured
	// under: far above every shape's peak, so nothing spills or aborts.
	harmCalibrationCap = 4 << 30
	// harmSpanKey / harmResourceKey are the materialized attribute keys the
	// seed fills, schema.DefaultMaterializedSpanAttributeColumns' and
	// DefaultMaterializedResourceAttributeColumns' string-typed keys.
	harmSpanKey     = "rpc.method"
	harmResourceKey = "k8s.namespace.name"
)

// harmCapFactors are the caps, as multiples of a shape's unspilled peak, at
// which TestDistinctSpill_WithheldShapesAnswer looks for the spilled
// DISTINCT aborting, in the order it tries them. Every one lies between the
// peak and twice it. 1.11 comes first because every withheld shape has
// aborted spilled there; the rest widen the search across the band, nearest
// the peak first.
var harmCapFactors = []float64{1.11, 1.2, 1.4, 1.6}

// harmCalibrationRuns is how many times a shape's unspilled peak is
// measured; the probe sizes its caps from the largest, since one statement's
// peak varies from run to run.
const harmCalibrationRuns = 3

// harmedShape is one Tempo tag-discovery request whose statement's DISTINCT
// a spill harms, so the handler withholds the DISTINCT threshold from it.
type harmedShape struct {
	name   string
	path   string
	params url.Values
	// answers decodes the response body into the number of values it lists.
	answers func(t *testing.T, body string) int
	want    int
}

// tagValuesAnswers counts the values of a /api/v2/search/tag/{name}/values
// response.
func tagValuesAnswers(t *testing.T, body string) int {
	var resp tempo.SearchTagValuesResponseV2
	decodeJSON(t, body, &resp)
	return len(resp.TagValues)
}

// tagNamesAnswers counts the names of a /api/search/tags response.
func tagNamesAnswers(t *testing.T, body string) int {
	var resp tempo.SearchTagsResponse
	decodeJSON(t, body, &resp)
	return len(resp.TagNames)
}

// TestDistinctSpill_WithheldShapesAnswer drives every Tempo tag-discovery
// shape the handler withholds the DISTINCT spill threshold from, over 1M
// unique 256-byte values, through the production handler on ClickHouse 26.9
// with the engine's settings rules resolved from "auto,distinct_spill". For
// each shape it measures the statement's unspilled peak P under a cap far
// above it, the largest of harmCalibrationRuns runs, then finds a cap between
// P and 2P at which the DISTINCT threshold forced onto the statement at half
// the cap makes it abort with MEMORY_LIMIT_EXCEEDED (code 241) while carrying
// that threshold — failing if there is none, since a shape a
// spill no longer harms must not go without the threshold — and requires the
// request as dispatched to answer every seeded value under that same cap,
// running with the group_by/sort/join thresholds at cap/2 and without the
// DISTINCT one (system.query_log's Settings).
func TestDistinctSpill_WithheldShapesAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), harmProbeBudget)
	defer cancel()
	s := startServer(ctx, t, metadataSpillImage)
	rules := resolveDistinctSpillRules(t, s)
	traces := seedHarmProbe(ctx, t, s)

	tagValues := func(tag string) string { return "/api/v2/search/tag/" + tag + "/values" }
	scope := func(name string) url.Values { return url.Values{"scope": {name}} }
	// A span and a resource map each hold the probe's unique key and the
	// materialized key, so each lists one name more than there are spans.
	const materializedKeyPerMap = 1
	for _, shape := range []harmedShape{
		{"intrinsic name values", tagValues("name"), url.Values{}, tagValuesAnswers, harmSpans},
		{"intrinsic kind values", tagValues("kind"), url.Values{}, tagValuesAnswers, harmSpans},
		{"intrinsic status values", tagValues("status"), url.Values{}, tagValuesAnswers, harmSpans},
		{"materialized span values", tagValues("span." + harmSpanKey), url.Values{}, tagValuesAnswers, harmSpans},
		{"materialized resource values", tagValues("resource." + harmResourceKey), url.Values{}, tagValuesAnswers, harmSpans},
		{"span tag names", "/api/search/tags", scope("span"), tagNamesAnswers, harmSpans + materializedKeyPerMap},
		{"resource tag names", "/api/search/tags", scope("resource"), tagNamesAnswers, harmSpans + materializedKeyPerMap},
	} {
		t.Run(shape.name, func(t *testing.T) {
			run := func(ctx context.Context, memCap int64) (int, string) {
				client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: memCap})
				params := url.Values{}
				maps.Copy(params, shape.params)
				return runTempoProbeOn(ctx, t, client, rules, traces, shape.path, params)
			}

			peak := int64(0)
			for range harmCalibrationRuns {
				qid := fmt.Sprintf("harm-peak-%d", time.Now().UnixNano())
				code, body := run(chclient.WithQueryID(ctx, qid), harmCalibrationCap)
				if code != http.StatusOK {
					t.Fatalf("under the %d-byte calibration cap the probe answered HTTP %d: %.500s", harmCalibrationCap, code, body)
				}
				if got := shape.answers(t, body); got != shape.want {
					t.Fatalf("under the calibration cap the probe returned %d values; want %d", got, shape.want)
				}
				peak = max(peak, int64(s.peakMemory(ctx, t, qid)))
			}

			memCap := int64(0)
			for _, factor := range harmCapFactors {
				candidate := int64(factor * float64(peak))
				qid := fmt.Sprintf("harm-forced-%d", time.Now().UnixNano())
				forced := chclient.WithQuerySetting(chclient.WithQueryID(ctx, qid), chopt.SettingMaxBytesBeforeExternalDistinct, candidate/2)
				if code, _ := run(forced, candidate); code == http.StatusOK {
					t.Logf("at %.2f x the %d-byte peak (%d bytes) the forced-spill probe completed", factor, peak, candidate)
					continue
				}
				got, msg := s.finishedException(ctx, t, qid)
				if got != memoryLimitExceeded {
					t.Fatalf("at %.2f x the peak the forced-spill probe failed with server code %d (%s); want code %d",
						factor, got, msg, memoryLimitExceeded)
				}
				if !s.abortedCarrying(ctx, t, qid, chopt.SettingMaxBytesBeforeExternalDistinct) {
					t.Fatalf("at %.2f x the peak the forced-spill probe aborted without carrying %s, so its abort says nothing about a spill",
						factor, chopt.SettingMaxBytesBeforeExternalDistinct)
				}
				t.Logf("at %.2f x the %d-byte peak (%d bytes) the forced-spill probe aborted: %.200s", factor, peak, candidate, msg)
				memCap = candidate
				break
			}
			if memCap == 0 {
				t.Fatalf("no cap in %v x the %d-byte peak made the forced-spill probe abort: a spill no longer harms this shape, so it must keep the DISTINCT threshold",
					harmCapFactors, peak)
			}

			qid := fmt.Sprintf("harm-dispatched-%d", time.Now().UnixNano())
			code, body := run(chclient.WithQueryID(ctx, qid), memCap)
			if code != http.StatusOK {
				t.Fatalf("as dispatched under the %d-byte cap the probe answered HTTP %d: %.500s", memCap, code, body)
			}
			if got := shape.answers(t, body); got != shape.want {
				t.Errorf("as dispatched the probe returned %d values; want %d", got, shape.want)
			}
			threshold := strconv.FormatInt(memCap/2, 10)
			for _, name := range []string{settingExternalGroupBy, settingExternalSort, chopt.SettingMaxBytesBeforeExternalJoin} {
				s.assertEffectiveSetting(ctx, t, qid, name, threshold)
			}
			if s.finishedCarries(ctx, t, qid, chopt.SettingMaxBytesBeforeExternalDistinct) {
				t.Errorf("as dispatched the statement carries %s; want it withheld from a DISTINCT a spill harms",
					chopt.SettingMaxBytesBeforeExternalDistinct)
			}
			t.Logf("as dispatched: answered under %d bytes, %.2f x the %d-byte unspilled peak, at a peak of %d",
				memCap, float64(memCap)/float64(peak), peak, s.peakMemory(ctx, t, qid))
		})
	}
}

// abortedCarrying reports whether the statement query qid aborted with
// carried the setting name explicitly.
func (s *server) abortedCarrying(ctx context.Context, t *testing.T, qid, name string) bool {
	t.Helper()
	s.flushLogs(ctx, t)
	var (
		present uint8
		rows    uint64
	)
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(mapContains(Settings, ?)), count() FROM system.query_log WHERE type = 'ExceptionWhileProcessing' AND query_id = ?",
		name, qid).Scan(&present, &rows); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if rows == 0 {
		t.Fatalf("query_log has no aborted query %s", qid)
	}
	return present != 0
}

// seedHarmProbe applies cerberus's traces DDL with the default materialized
// attribute columns and writes harmSpans spans inside the probe window, each
// carrying its own harmValueBytes-long value in every column a withheld shape
// reads. It returns the traces schema the handler reads them under.
func seedHarmProbe(ctx context.Context, t *testing.T, s *server) schema.Traces {
	t.Helper()
	traces := schema.DefaultOTelTraces()
	traces.MaterializedSpanAttributeColumns = schema.DefaultMaterializedSpanAttributeColumns()
	traces.MaterializedResourceAttributeColumns = schema.DefaultMaterializedResourceAttributeColumns()
	if err := ddl.ApplyWithConfig(ctx, s.admin.Conn(), ddl.Config{
		TraceMaterializedAttributesEnabled:   true,
		MaterializedSpanAttributeColumns:     traces.MaterializedSpanAttributeColumns,
		MaterializedResourceAttributeColumns: traces.MaterializedResourceAttributeColumns,
	}, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	// unique renders the row's own value, tagged so no two columns share one.
	unique := func(tag string) string {
		return fmt.Sprintf("leftPad(toString(number), %d, '%s')", harmValueBytes, tag)
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
    %[2]s AS SpanName,
    %[3]s AS SpanKind,
    'api' AS ServiceName,
    map('%[4]s', %[5]s, %[6]s, 'v') AS ResourceAttributes,
    map('%[7]s', %[8]s, %[9]s, 'v') AS SpanAttributes,
    1000 AS Duration,
    %[10]s AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM numbers(%[11]d)`,
		metadataSpillWindowStart.Unix(),
		unique("n"), unique("k"),
		harmResourceKey, unique("r"), unique("R"),
		harmSpanKey, unique("m"), unique("S"),
		unique("s"),
		harmSpans))
	s.exec(ctx, t, "OPTIMIZE TABLE otel_traces FINAL")
	return traces
}
