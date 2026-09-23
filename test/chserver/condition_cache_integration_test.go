//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// conditionCacheBuilds are the pinned builds the condition-cache reproductions
// run against, with the outcome each upstream defect's reproduction must show
// there. The expectations are observations of the builds themselves; the test
// separately requires chopt's recorded UnsafeBuilds ranges to agree with them.
//
//   - 25.3.14.14: the supported floor. Skip-index reads before PREWHERE do not
//     exist yet; the row-policy attribution defect does.
//   - 26.3.12.3: last 26.3 release before either backport — both defects.
//   - 26.3.17.56: first 26.3 release carrying both backports.
//   - 26.6.1.1193: first release of the line both fixes merged into.
var conditionCacheBuilds = []struct {
	image                string
	skipIndexPoisons     bool
	rowPolicyPoisons     bool
	conditionCacheUnsafe bool
}{
	{"clickhouse/clickhouse-server:25.3.14.14-alpine", false, true, true},
	{"clickhouse/clickhouse-server:26.3.12.3-alpine", true, true, true},
	{"clickhouse/clickhouse-server:26.3.17.56-alpine", false, false, false},
	{"clickhouse/clickhouse-server:26.6.1.1193-alpine", false, false, false},
}

// Upstream defect references, matched against the Defect text of chopt's
// recorded UnsafeBuilds ranges.
const (
	skipIndexDefect = "ClickHouse#105686"
	rowPolicyDefect = "ClickHouse#107145"
)

// Seed shape. Both tables are ordered so the rows a probe's narrow predicate
// or row policy excludes fill whole granules (8192 rows), which is what makes
// a mis-attributed "no granule matches" verdict observable.
const (
	probeSpans        = 600_000
	probeServices     = 20
	probeSamples      = 600_000
	probeJobs         = 50
	tenantVisibleJobs = 5
	probeDurationNs   = 200_000_000 // 200ms, above the probes' 100ms threshold
	settingQCC        = chclient.SettingUseQueryConditionCache
	settingSkipIndex  = "use_skip_indexes"
	tenantUser        = "cc_tenant"
	tenantPassword    = "cc_tenant"
	probeMetric       = "ccprobe"
)

// probePromQL is a range-function selector over a metric whose name needs no
// OTel-to-Prometheus spelling alternatives, so its lowering reads the samples
// through PREWHERE (MetricName = ?) — the key a restricted user's identical
// query writes its row-policy-shaped verdict under. (A name with alternative
// spellings lowers to WHERE MetricName IN (...) instead, which ClickHouse
// moves to PREWHERE itself and does not attribute that way.)
const probePromQL = "max_over_time(" + probeMetric + "[5m])"

// probeWindow is the seeded hour; every span and sample lands inside it.
var (
	probeWindowStart = time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	probeWindowEnd   = probeWindowStart.Add(time.Hour)
)

// TraceQL probes. The narrow query lowers to PREWHERE (Duration > ?) WHERE
// (ResourceAttributes[?] = ?): the bloom skip index on the resource-attribute
// values drops whole marks before PREWHERE on a build with skip-index reads
// on, which is the ClickHouse#105686 trigger. The broad query shares that
// PREWHERE predicate, so it reads the entry the narrow one wrote.
const (
	narrowTraceQL = `{ resource.service.name = "s7" && duration > 100ms } | count_over_time()`
	broadTraceQL  = `{ duration > 100ms && resource.service.name != "none" } | count_over_time()`
)

// TestConditionCache_CerberusShapesAcrossBuilds ports ClickHouse#105686's and
// ClickHouse#107145's reproductions to cerberus's own schema and emitted
// queries and runs each priming query and the later read on the same server.
// For every pinned build it establishes, through a client that pins the cache
// on or off per query:
//
//   - whether a narrow TraceQL query poisons the broad one (skip-index
//     attribution), and whether a restricted user reading the metrics table
//     directly poisons cerberus's PromQL read of it (row-policy attribution);
//   - that re-running a narrow query with its skip indexes switched off after
//     priming with them on stays correct — the index-selection change
//     ClickHouse#108548 fixed, which needs an index whose verdict can disagree
//     with the row predicate, and cerberus provisions none;
//
// then requires chopt's recorded ranges to agree with what the build did, and
// requires the production wiring — the resolved set, the engine settings
// rules and the client-wide override — to return the true answer through a
// poisoned cache on every build, with the effective use_query_condition_cache
// the query actually ran under read back from system.query_log. The same is
// checked under the "off" selection, the operator's explicit opt-out.
//
// No probe enables the query RESULT cache: the resolved sets below are
// required to leave result_cache out, so every answer is computed.
func TestConditionCache_CerberusShapesAcrossBuilds(t *testing.T) {
	for _, build := range conditionCacheBuilds {
		t.Run(build.image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			s := startServer(ctx, t, build.image)
			seedConditionCacheProbe(ctx, t, s)

			raw := newProbeMux(t, s, s.client(t, adminUser, adminPassword, chclient.Config{}), engine.SettingsRules{})
			tenant := s.client(t, tenantUser, tenantPassword, chclient.Config{})

			// --- server behaviour, with the cache pinned per query ---
			truthTraces := raw.traceCount(ctx, t, broadTraceQL, 0)
			if truthTraces != probeSpans {
				t.Fatalf("broad TraceQL count with the cache off = %v; want every seeded span (%d)", truthTraces, probeSpans)
			}
			s.dropConditionCache(ctx, t)
			raw.traceCount(ctx, t, narrowTraceQL, 1)
			skipIndexPoisoned := raw.traceCount(ctx, t, broadTraceQL, 1) != truthTraces
			if skipIndexPoisoned != build.skipIndexPoisons {
				t.Errorf("skip-index attribution: broad count after a narrow priming poisoned = %v; want %v",
					skipIndexPoisoned, build.skipIndexPoisons)
			}

			truthSeries := raw.promSeries(ctx, t, 0)
			if len(truthSeries) != probeJobs {
				t.Fatalf("PromQL selector with the cache off returned %d series; want %d", len(truthSeries), probeJobs)
			}
			s.dropConditionCache(ctx, t)
			if got := primeAsTenant(ctx, t, tenant); got != probeSamples/probeJobs*tenantVisibleJobs {
				t.Fatalf("restricted user counts %d samples; want %d (its row policy)", got, probeSamples/probeJobs*tenantVisibleJobs)
			}
			rowPolicyPoisoned := !sameSeries(raw.promSeries(ctx, t, 1), truthSeries)
			if rowPolicyPoisoned != build.rowPolicyPoisons {
				t.Errorf("row-policy attribution: unrestricted answer after a restricted priming poisoned = %v; want %v",
					rowPolicyPoisoned, build.rowPolicyPoisons)
			}

			s.dropConditionCache(ctx, t)
			truthNarrow := raw.traceCount(ctx, t, narrowTraceQL, 0)
			raw.traceCount(ctx, t, narrowTraceQL, 1)
			noIndexCtx := chclient.WithQuerySetting(ctx, settingSkipIndex, 0)
			if got := raw.traceCount(noIndexCtx, t, narrowTraceQL, 1); got != truthNarrow {
				t.Errorf("index-selection change: narrow count with skip indexes off after an indexed priming = %v; want %v", got, truthNarrow)
			}

			// --- chopt's recorded ranges agree with the build ---
			assertDefectRange(t, s.version, skipIndexDefect, skipIndexPoisoned)
			assertDefectRange(t, s.version, rowPolicyDefect, rowPolicyPoisoned)

			// --- the production wiring answers correctly through a poisoned cache ---
			for _, selection := range []string{chopt.SelectionAuto, "off"} {
				t.Run("selection="+selection, func(t *testing.T) {
					set := chopttest.ResolveEnabledSet(ctx, t, s.admin, selection)
					if got := set.KnownUnsafe(chopt.FeatureConditionCache); got != build.conditionCacheUnsafe {
						t.Fatalf("KnownUnsafe(condition_cache) on %s = %v; want %v", s.version, got, build.conditionCacheUnsafe)
					}
					rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
					if rules.ResultCache {
						t.Fatal("the resolved set enabled result_cache; every probe answer must be computed, not served from cache")
					}
					prodClient := s.client(t, adminUser, adminPassword, chclient.Config{})
					prodClient.SetQueryConditionCacheDisabled(choptwire.ConditionCacheDisabled(set))
					prod := newProbeMux(t, s, prodClient, rules)

					// The effective setting cerberus's query ran under: forced off
					// on a known-unsafe build under every selection, on where the
					// feature resolved in, and the server's own default under the
					// opt-out on a fixed build.
					want := s.defaultSetting(ctx, t, settingQCC)
					switch {
					case build.conditionCacheUnsafe:
						want = "0"
					case selection == chopt.SelectionAuto:
						want = "1"
					}

					s.dropConditionCache(ctx, t)
					raw.traceCount(ctx, t, narrowTraceQL, 1)
					qid := probeQueryID("traces", selection)
					got := prod.traceCount(chclient.WithQueryID(ctx, qid), t, broadTraceQL, -1)
					if got != truthTraces {
						t.Errorf("production TraceQL count through a primed cache = %v; want %v", got, truthTraces)
					}
					s.assertEffectiveSetting(ctx, t, qid, settingQCC, want)

					s.dropConditionCache(ctx, t)
					primeAsTenant(ctx, t, tenant)
					qid = probeQueryID("metrics", selection)
					if got := prod.promSeries(chclient.WithQueryID(ctx, qid), t, -1); !sameSeries(got, truthSeries) {
						t.Errorf("production PromQL answer through a restricted user's primed cache = %v; want %v", got, truthSeries)
					}
					s.assertEffectiveSetting(ctx, t, qid, settingQCC, want)
				})
			}
		})
	}
}

// assertDefectRange requires chopt's condition_cache UnsafeBuilds to cover
// version for defect exactly when the reproduction poisoned the cache.
func assertDefectRange(t *testing.T, version chopt.Version, defect string, poisoned bool) {
	t.Helper()
	var feature chopt.Feature
	for _, f := range chopt.Registry() {
		if f.ID == chopt.FeatureConditionCache {
			feature = f
		}
	}
	recorded, covered := false, false
	for _, r := range feature.UnsafeBuilds {
		if !strings.Contains(r.Defect, defect) {
			continue
		}
		recorded = true
		covered = covered || r.Contains(version)
	}
	if !recorded {
		t.Fatalf("chopt records no condition_cache range for %s", defect)
	}
	if covered != poisoned {
		t.Errorf("chopt says %s affects %s = %v, but the reproduction poisoned = %v", defect, version, covered, poisoned)
	}
}

// probeQueryID is a unique query_id for one production-wiring dispatch.
func probeQueryID(shape, selection string) string {
	return fmt.Sprintf("cc-probe-%s-%s-%d", shape, selection, time.Now().UnixNano())
}

// seedConditionCacheProbe applies cerberus's own DDL, seeds the spans and
// samples the probes read, and creates the restricted user whose row policy
// hides every job but the first tenantVisibleJobs.
func seedConditionCacheProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Traces, ddl.Metrics}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    toDateTime64(%d, 9) + toIntervalMillisecond(1 + number %% 3599999) AS Timestamp,
    leftPad(hex(number), 32, '0') AS TraceId,
    leftPad(hex(number), 16, '0') AS SpanId,
    '' AS ParentSpanId,
    'op' AS SpanName,
    'Server' AS SpanKind,
    concat('s', toString(number %% %d)) AS ServiceName,
    map('service.name', concat('s', toString(number %% %d))) AS ResourceAttributes,
    map() AS SpanAttributes,
    %d AS Duration,
    'Unset' AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM numbers(%d)`,
		probeWindowStart.Unix(), probeServices, probeServices, probeDurationNs, probeSpans))

	// Spans land strictly after the window start: the metrics window is
	// left-open, like every cerberus range.
	//
	// One sample per (job, millisecond) inside the last five minutes of the
	// window, so an instant selector at the window end sees every job. The
	// sample's value is its job's index, which the row policy reads — a
	// column with no index, so the policy alone decides which granules the
	// restricted user sees.
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value)
SELECT
    'svc', '%s',
    map('job', concat('j', toString(number %% %d))),
    toDateTime64(%d, 9) - toIntervalMillisecond(number %% 240000),
    toFloat64(number %% %d)
FROM numbers(%d)`,
		probeMetric, probeJobs, probeWindowEnd.Unix(), probeJobs, probeSamples))

	s.exec(ctx, t, fmt.Sprintf("CREATE USER %s IDENTIFIED WITH plaintext_password BY '%s'", tenantUser, tenantPassword))
	s.exec(ctx, t, fmt.Sprintf("GRANT SELECT ON %s.* TO %s", serverDB, tenantUser))
	s.exec(ctx, t, fmt.Sprintf(
		"CREATE ROW POLICY cc_tenant_jobs ON %s.otel_metrics_gauge FOR SELECT USING toUInt64(Value) < %d TO %s",
		serverDB, tenantVisibleJobs, tenantUser,
	))
}

// tenantPrimingSQL is what another ClickHouse client — a dashboard reading the
// table directly as a restricted user — sends: the PREWHERE predicate
// cerberus's PromQL lowering emits for probeMetric, against the underlying
// table rather than through merge(). A restricted user's read through merge()
// applies its policy outside the attributing reader and writes nothing
// harmful, but a direct read does, and cerberus's own merge() read then
// consults that entry.
const tenantPrimingSQL = "SELECT count() FROM otel_metrics_gauge PREWHERE (`MetricName` = '" + probeMetric + "') SETTINGS " + settingQCC + " = 1"

// primeAsTenant runs tenantPrimingSQL as the restricted user and returns the
// count it saw.
func primeAsTenant(ctx context.Context, t *testing.T, tenant *chclient.Client) uint64 {
	t.Helper()
	var n uint64
	if err := tenant.Conn().QueryRow(ctx, tenantPrimingSQL).Scan(&n); err != nil {
		t.Fatalf("restricted-user priming: %v", err)
	}
	return n
}

// dropConditionCache empties the server's query condition cache so the next
// priming query is the only writer a probe observes.
func (s *server) dropConditionCache(ctx context.Context, t *testing.T) {
	t.Helper()
	s.exec(ctx, t, "SYSTEM DROP QUERY CONDITION CACHE")
}

// defaultSetting is the value name takes on the administrative user's
// profile when a query does not set it.
func (s *server) defaultSetting(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	var v string
	if err := s.admin.Conn().QueryRow(ctx, "SELECT value FROM system.settings WHERE name = ?", name).Scan(&v); err != nil {
		t.Fatalf("read default of %s: %v", name, err)
	}
	return v
}

// assertEffectiveSetting requires query qid to have run with name = want.
// system.query_log.Settings records only values that differ from the
// profile's, so an absent key means the query ran under the default.
func (s *server) assertEffectiveSetting(ctx context.Context, t *testing.T, qid, name, want string) {
	t.Helper()
	s.flushLogs(ctx, t)
	var (
		value   string
		present uint8
		rows    uint64
	)
	err := s.admin.Conn().QueryRow(ctx,
		"SELECT any(Settings[?]), max(mapContains(Settings, ?)), count() FROM system.query_log "+
			"WHERE type = 'QueryFinish' AND query_id = ?",
		name, name, qid).Scan(&value, &present, &rows)
	if err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if rows == 0 {
		t.Fatalf("query_log has no finished query %s", qid)
	}
	if present == 0 {
		value = s.defaultSetting(ctx, t, name)
	}
	if value != want {
		t.Errorf("query %s ran with %s = %q; want %q", qid, name, value, want)
	}
}

// probeMux mounts the production Prometheus and Tempo handlers over one
// client, with rules as the engine's settings rules.
type probeMux struct {
	mux http.Handler
}

func newProbeMux(t *testing.T, s *server, client *chclient.Client, rules engine.SettingsRules) probeMux {
	t.Helper()
	mux := http.NewServeMux()
	ph := prom.New(client, schema.DefaultOTelMetrics(), nil)
	th := tempo.New(client, schema.DefaultOTelTraces(), "chserver-probe", nil)
	ph.Engine.SetSettings(rules)
	th.Engine.SetSettings(rules)
	ph.Mount(mux)
	th.Mount(mux)
	return probeMux{mux: mux}
}

// cacheCtx pins use_query_condition_cache on the request: 0 or 1, or leaves
// it to the handler's own wiring when qcc is negative.
func cacheCtx(base context.Context, qcc int) context.Context {
	if qcc < 0 {
		return base
	}
	return chclient.WithQuerySetting(base, settingQCC, qcc)
}

// traceCount sums a TraceQL instant metrics query over the probe window. ctx
// is the request's context, so a query id or per-query setting on it rides
// the dispatch.
func (p probeMux) traceCount(ctx context.Context, t *testing.T, q string, qcc int) float64 {
	t.Helper()
	params := url.Values{}
	params.Set("q", q)
	params.Set("start", strconv.FormatInt(probeWindowStart.Unix(), 10))
	params.Set("end", strconv.FormatInt(probeWindowEnd.Unix(), 10))
	var resp tempo.MetricsQueryInstantResponse
	serveJSON(cacheCtx(ctx, qcc), t, p.mux, "/api/metrics/query?"+params.Encode(), &resp)
	var total float64
	for _, series := range resp.Series {
		total += series.Value
	}
	return total
}

// promSampleFields is the length of a Prometheus instant-vector sample:
// [timestamp, value].
const promSampleFields = 2

// promSeries runs probePromQL at the window end and returns each series' job
// label mapped to its value@timestamp. ctx is the request's context.
func (p probeMux) promSeries(ctx context.Context, t *testing.T, qcc int) map[string]string {
	t.Helper()
	params := url.Values{}
	params.Set("query", probePromQL)
	params.Set("time", strconv.FormatInt(probeWindowEnd.Unix(), 10))
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	serveJSON(cacheCtx(ctx, qcc), t, p.mux, "/api/v1/query?"+params.Encode(), &resp)
	out := make(map[string]string, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		if len(r.Value) != promSampleFields {
			t.Fatalf("series %v: value %v is not a [timestamp, value] pair", r.Metric, r.Value)
		}
		out[r.Metric["job"]] = fmt.Sprintf("%v@%v", r.Value[1], r.Value[0])
	}
	return out
}

// sameSeries reports whether a and b carry the same series with the same values.
func sameSeries(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
