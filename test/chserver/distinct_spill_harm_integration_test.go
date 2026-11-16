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
	// under, and the memory limit a request's peak under a lower cap's
	// settings is measured with: far above every shape's peak, so nothing
	// spills or aborts.
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

// settingMaxThreads is ClickHouse's per-query thread count, and
// settingMaxMemoryUsage its per-query memory limit.
const (
	settingMaxThreads     = "max_threads"
	settingMaxMemoryUsage = "max_memory_usage"
)

// harmDispatchedHeadroom is how far above the request's own peak under a
// cap's settings — the largest of harmCalibrationRuns, memory limit lifted —
// a cap must lie for TestDistinctSpill_WithheldShapesAnswer to require the
// request to answer under it, and how far that peak may exceed the unspilled
// one. It exceeds the widest spread measured between one withheld
// statement's single-thread peaks across repeated runs, under either set of
// settings.
const harmDispatchedHeadroom = 1.10

// harmProbeThreads is the thread count TestDistinctSpill_WithheldShapesAnswer
// measures peaks and sends the request as dispatched with. Run in parallel, a
// DISTINCT's peak depends on how its per-thread sets happen to merge, and an
// occasional run peaks far above the rest; on one thread the peak repeats
// within a few percent of the largest of a few runs.
const harmProbeThreads = 1

// serverDefaultThreads stands for sending no max_threads, so the statement
// runs on the server's own default thread count, as every withheld statement
// does in production.
const serverDefaultThreads = 0

// harmManyThreads is a thread count the size a production server defaults
// to, its core count, well above the test container's two CPUs.
const harmManyThreads = 16

// harmForcedThreads are the thread counts the forced-spill probe must abort
// on at the chosen cap: the one the peaks are measured on, the server's
// default, and a production-sized one.
var harmForcedThreads = []int{harmProbeThreads, serverDefaultThreads, harmManyThreads}

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
// each shape, on harmProbeThreads threads, it measures the statement's
// unspilled peak P under a cap far above it, the largest of
// harmCalibrationRuns runs. It then takes the first cap in harmCapFactors x P
// that clears the request's own peak under that cap's settings (memory limit
// lifted, the largest of harmCalibrationRuns runs) by harmDispatchedHeadroom
// and at which the DISTINCT threshold forced onto the statement at half the
// cap makes it abort with MEMORY_LIMIT_EXCEEDED (code 241) while carrying
// that threshold, on every thread count in harmForcedThreads — failing if there is none, since a shape a spill no longer harms
// must not go without the threshold. It requires the request's peak under
// that cap's settings to stay within harmDispatchedHeadroom of P, and the
// request as dispatched to answer every seeded value under the cap, running
// with the group_by/sort/join thresholds at cap/2 and without the DISTINCT
// one. Every statement's thread count, memory limit and thresholds are read
// back from system.query_log's Settings.
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
			// run sends the request under memCap on the given thread count,
			// serverDefaultThreads leaving max_threads to the server.
			run := func(ctx context.Context, memCap int64, threads int) (int, string) {
				client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: memCap})
				params := url.Values{}
				maps.Copy(params, shape.params)
				if threads != serverDefaultThreads {
					ctx = chclient.WithQuerySetting(ctx, settingMaxThreads, threads)
				}
				return runTempoProbeOn(ctx, t, client, rules, traces, shape.path, params)
			}
			// assertRanWith requires statement qid's logType entry to record
			// the thread count, memLimit, and the group_by/sort/join
			// thresholds at half of memCap.
			assertRanWith := func(qid, logType string, memCap, memLimit int64, threads int) {
				t.Helper()
				threshold := strconv.FormatInt(memCap/2, 10)
				for _, name := range []string{settingExternalGroupBy, settingExternalSort, chopt.SettingMaxBytesBeforeExternalJoin} {
					s.assertEffectiveSettingAt(ctx, t, qid, logType, name, threshold)
				}
				s.assertEffectiveSettingAt(ctx, t, qid, logType, settingMaxMemoryUsage, strconv.FormatInt(memLimit, 10))
				wantThreads := strconv.Itoa(threads)
				if threads == serverDefaultThreads {
					wantThreads = s.defaultSetting(ctx, t, settingMaxThreads)
				}
				s.assertEffectiveSettingAt(ctx, t, qid, logType, settingMaxThreads, wantThreads)
			}
			// dispatchedPeak is the largest of harmCalibrationRuns peaks of
			// the request as dispatched under memCap — the spill thresholds
			// memCap sets — with only the memory limit itself lifted.
			dispatchedPeak := func(memCap int64) int64 {
				peak := int64(0)
				for range harmCalibrationRuns {
					qid := fmt.Sprintf("harm-dispatched-peak-%d", time.Now().UnixNano())
					lifted := chclient.WithQuerySetting(chclient.WithQueryID(ctx, qid), settingMaxMemoryUsage, int64(harmCalibrationCap))
					if code, body := run(lifted, memCap, harmProbeThreads); code != http.StatusOK {
						t.Fatalf("as dispatched under the %d-byte cap's settings, with the memory limit lifted, the probe answered HTTP %d: %.500s", memCap, code, body)
					}
					assertRanWith(qid, queryLogFinished, memCap, harmCalibrationCap, harmProbeThreads)
					peak = max(peak, int64(s.peakMemory(ctx, t, qid)))
				}
				return peak
			}
			// forcedSpillAborts reports whether the request, with the
			// DISTINCT threshold forced onto it at half of memCap, aborts with
			// MEMORY_LIMIT_EXCEEDED under memCap on the given thread count,
			// and fails the test if it fails any other way.
			forcedSpillAborts := func(memCap int64, threads int) bool {
				qid := fmt.Sprintf("harm-forced-%d", time.Now().UnixNano())
				forced := chclient.WithQuerySetting(chclient.WithQueryID(ctx, qid), chopt.SettingMaxBytesBeforeExternalDistinct, memCap/2)
				if code, _ := run(forced, memCap, threads); code == http.StatusOK {
					return false
				}
				got, msg := s.finishedException(ctx, t, qid)
				if got != memoryLimitExceeded {
					t.Fatalf("under the %d-byte cap on max_threads %d the forced-spill probe failed with server code %d (%s); want code %d",
						memCap, threads, got, msg, memoryLimitExceeded)
				}
				if !s.abortedCarrying(ctx, t, qid, chopt.SettingMaxBytesBeforeExternalDistinct) {
					t.Fatalf("under the %d-byte cap on max_threads %d the forced-spill probe aborted without carrying %s, so its abort says nothing about a spill",
						memCap, threads, chopt.SettingMaxBytesBeforeExternalDistinct)
				}
				assertRanWith(qid, queryLogAborted, memCap, memCap, threads)
				t.Logf("under the %d-byte cap on max_threads %d the forced-spill probe aborted: %.200s", memCap, threads, msg)
				return true
			}

			peak := int64(0)
			for range harmCalibrationRuns {
				qid := fmt.Sprintf("harm-peak-%d", time.Now().UnixNano())
				code, body := run(chclient.WithQueryID(ctx, qid), harmCalibrationCap, harmProbeThreads)
				if code != http.StatusOK {
					t.Fatalf("under the %d-byte calibration cap the probe answered HTTP %d: %.500s", harmCalibrationCap, code, body)
				}
				if got := shape.answers(t, body); got != shape.want {
					t.Fatalf("under the calibration cap the probe returned %d values; want %d", got, shape.want)
				}
				assertRanWith(qid, queryLogFinished, harmCalibrationCap, harmCalibrationCap, harmProbeThreads)
				peak = max(peak, int64(s.peakMemory(ctx, t, qid)))
			}

			memCap, capPeak := int64(0), int64(0)
			var rejected []string
		factors:
			for _, factor := range harmCapFactors {
				candidate := int64(factor * float64(peak))
				dispatched := dispatchedPeak(candidate)
				if float64(candidate) < harmDispatchedHeadroom*float64(dispatched) {
					rejected = append(rejected, fmt.Sprintf("%.2f x: the %d-byte cap does not clear the request's own %d-byte peak under its settings by %.2f x",
						factor, candidate, dispatched, harmDispatchedHeadroom))
					continue
				}
				for _, threads := range harmForcedThreads {
					if !forcedSpillAborts(candidate, threads) {
						rejected = append(rejected, fmt.Sprintf("%.2f x: under the %d-byte cap on max_threads %d the forced-spill probe completed",
							factor, candidate, threads))
						continue factors
					}
				}
				memCap, capPeak = candidate, dispatched
				break
			}
			for _, r := range rejected {
				t.Logf("rejected %s", r)
			}
			if memCap == 0 {
				t.Fatalf("no cap in %v x the %d-byte peak both cleared the request's own peak under its settings by %.2f x and made the forced-spill probe abort on every thread count in %v (rejections above): "+
					"a shape a spill no longer harms must keep the DISTINCT threshold",
					harmCapFactors, peak, harmDispatchedHeadroom, harmForcedThreads)
			}
			if float64(capPeak) > harmDispatchedHeadroom*float64(peak) {
				t.Errorf("as dispatched under the %d-byte cap's settings the request peaked at %d bytes, more than %.2f x its %d-byte unspilled peak: withholding the DISTINCT threshold no longer leaves the statement at its unspilled size",
					memCap, capPeak, harmDispatchedHeadroom, peak)
			}

			qid := fmt.Sprintf("harm-dispatched-%d", time.Now().UnixNano())
			code, body := run(chclient.WithQueryID(ctx, qid), memCap, harmProbeThreads)
			if code != http.StatusOK {
				t.Fatalf("as dispatched under the %d-byte cap the probe answered HTTP %d: %.500s", memCap, code, body)
			}
			if got := shape.answers(t, body); got != shape.want {
				t.Errorf("as dispatched the probe returned %d values; want %d", got, shape.want)
			}
			assertRanWith(qid, queryLogFinished, memCap, memCap, harmProbeThreads)
			if s.finishedCarries(ctx, t, qid, chopt.SettingMaxBytesBeforeExternalDistinct) {
				t.Errorf("as dispatched the statement carries %s; want it withheld from a DISTINCT a spill harms",
					chopt.SettingMaxBytesBeforeExternalDistinct)
			}
			answered := s.peakMemory(ctx, t, qid)
			t.Logf("as dispatched: answered under %d bytes (%.2f x the %d-byte unspilled peak) at a peak of %d, the cap %.3f x that peak",
				memCap, float64(memCap)/float64(peak), peak, answered, float64(memCap)/float64(answered))
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
