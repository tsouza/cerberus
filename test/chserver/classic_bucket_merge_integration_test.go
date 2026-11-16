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

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// classicBuild268 is the bundled 26.8 LTS build, one of the two builds the
// classic-histogram quantile's memory behaviour is pinned on. Each build is
// its own single-line constant, the form ch-server-safety-shards.mjs resolves
// a family shard's images from.
const classicBuild268 = "clickhouse/clickhouse-server:26.8.36.3-alpine"

// classicBuild266 is a 26.6 build, the oldest supported line, the other build
// the classic-histogram quantile's memory behaviour is pinned on.
const classicBuild266 = "clickhouse/clickhouse-server:26.6.1.1193-alpine"

// classicMergeBuilds are classicBuild268 and classicBuild266.
var classicMergeBuilds = []string{classicBuild268, classicBuild266}

// classicMergeQueryMemoryBytes is the per-query memory cap every probe runs
// under: CERBERUS_CH_QUERY_MAX_MEMORY's 1 GiB default, the cap the
// classic-histogram calibration is measured against.
const classicMergeQueryMemoryBytes = 1 << 30

// classicMergeEvalTime is the instant every probe evaluates at; each series'
// samples sit inside the five minutes before it.
var classicMergeEvalTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// classicMergeSampleSpacing separates one series' consecutive samples, so a
// series of up to fourteen samples stays inside the [5m] window.
const classicMergeSampleSpacing = 20 * time.Second

// classicMergeShape is one disjoint-layout seed: series series, each with its
// own width-wide ExplicitBounds layout `[i*width+1 .. i*width+width]` (no two
// series share a bound — the worst case for the merge, where the merged
// layout is as wide as the total bucket volume), each carrying samples
// in-window samples of one observation per bucket. Series i carries the
// label grp=i%groups; a shape with groups > 1 is queried `by (le, grp)`, one
// merge group per value. sumMap selects the classic_bucket_merge_summap
// merge. want is how the handler answers it on each build under the 1 GiB
// cap.
type classicMergeShape struct {
	name                           string
	series, width, samples, groups int
	sumMap                         bool
	want                           map[string]classicWindowOutcome
}

// classicMergeShapes are the merges pinned on each build, at two threads. No
// ceiling bounds the merge, so each either answers with the quantile the
// seed implies or fails at the per-query cap with the classic-histogram
// memory-limit 422 and code 241. The first four sit within the
// 2,982,616-entry ceiling the merge once carried; the rest are past it:
//   - 60,000 x 51 = 3,060,000 entries, single- and two-sample, answer on
//     both builds (554-654 MiB, 3 runs each). Through the sumMap merge it
//     answers on 26.8 (712-713 MiB) and on 26.6 peaks at 948-964 MiB, above
//     90% of the cap, so 26.6 is pinned inside the band (the rule of
//     classicWindowSeriesProbes);
//   - 100,000 x 51 through the sumMap merge runs out of memory on both
//     builds (3 of 3 runs);
//   - sixty merge groups of 5,000 x 51 entries, 15,300,000 in all, five
//     times the old ceiling, answer on both builds (665-702 MiB on 26.8,
//     551-579 MiB on 26.6, 3 runs each).
var classicMergeShapes = []classicMergeShape{
	{name: "single-sample", series: 58_000, width: 50, samples: 1, groups: 1, want: classicWindowOnBoth(classicWindowAnswered)},
	{name: "multi-sample", series: 20_000, width: 50, samples: 14, groups: 1, want: classicWindowOnBoth(classicWindowAnswered)},
	{name: "multi-group", series: 57_600, width: 50, samples: 1, groups: 6, want: classicWindowOnBoth(classicWindowAnswered)},
	{name: "summap", series: 58_000, width: 50, samples: 1, groups: 1, sumMap: true, want: classicWindowOnBoth(classicWindowAnswered)},
	{name: "single-sample-60000", series: 60_000, width: 50, samples: 1, groups: 1, want: classicWindowOnBoth(classicWindowAnswered)},
	{name: "multi-sample-60000", series: 60_000, width: 50, samples: 2, groups: 1, want: classicWindowOnBoth(classicWindowAnswered)},
	{
		name: "summap-60000", series: 60_000, width: 50, samples: 1, groups: 1, sumMap: true,
		want: map[string]classicWindowOutcome{classicBuild268: classicWindowAnswered, classicBuild266: classicWindowInBand},
	},
	{name: "summap-100000", series: 100_000, width: 50, samples: 1, groups: 1, sumMap: true, want: classicWindowOnBoth(classicWindowFailsAtCap)},
	{name: "multi-group-300000", series: 300_000, width: 50, samples: 1, groups: 60, want: classicWindowOnBoth(classicWindowAnswered)},
}

// TestClassicBucketMerge_DisjointLayoutAcrossBuilds drives
// `histogram_quantile(0.5, sum by (le) (sum_over_time(<bucket>[5m])))` (or
// `by (le, grp)`) through the production Prometheus handler, under the 1 GiB
// query memory cap and the default resource bounds, on each build. Each shape
// gets the outcome it pins for the build: the quantile the disjoint seed
// implies, or HTTP 422 with the classic-histogram memory-limit message and
// ClickHouse's code 241 in query_log.
func TestClassicBucketMerge_DisjointLayoutAcrossBuilds(t *testing.T) {
	for _, shape := range classicMergeShapes {
		for _, image := range classicMergeBuilds {
			if want, ok := shape.want[image]; !ok || want == classicWindowRefusedPerSeries {
				t.Fatalf("%s on %s: outcome %v; want an answer or the per-query cap's 422", shape.name, image, want)
			}
		}
	}
	for _, image := range classicMergeBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image)
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			muxes := map[bool]*http.ServeMux{
				false: classicMergeMux(ctx, t, s, "auto"),
				true:  classicMergeMux(ctx, t, s, "auto,classic_bucket_merge_summap"),
			}
			for _, shape := range classicMergeShapes {
				want := shape.want[image]
				t.Run(want.String()+"/"+shape.name, func(t *testing.T) {
					metric := s.seedClassicMergeShape(ctx, t, shape)
					qid := classicMergeQueryID(s, shape)
					code, body := serveClassicMerge(chclient.WithQueryID(ctx, qid), muxes[shape.sumMap], metric, shape)
					if want == classicWindowAnswered || (want == classicWindowInBand && code == http.StatusOK) {
						if code != http.StatusOK {
							t.Fatalf("%s %+v: HTTP %d: %s; want the shape answered", s.version, shape, code, body)
						}
						assertClassicMergeQuantile(t, shape, body)
						return
					}
					assertClassicWindowRefusal(ctx, t, s, qid, classicWindowOutOfMemory, code, body)
				})
			}
		})
	}
}

// classicMergeMux mounts the production Prometheus handler over s, wired the
// way cmd/cerberus wires it: the optimizations selection resolves on the
// build, their settings, the default resource bounds and the 1 GiB query cap.
func classicMergeMux(ctx context.Context, t *testing.T, s *server, selection string) *http.ServeMux {
	t.Helper()
	client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: classicMergeQueryMemoryBytes})
	set := chopttest.ResolveEnabledSet(ctx, t, s.admin, selection)
	handler := prom.New(client, schema.DefaultOTelMetrics(), nil)
	handler.Engine.SetSettings(choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs()))
	handler.SetLowerers(choptwire.RangeLowerers(set))
	handler.ResourceBounds = promql.DefaultResourceBounds()
	mux := http.NewServeMux()
	handler.Mount(mux)
	return mux
}

// seedClassicMergeShape inserts shape under its own metric name and returns
// that name.
func (s *server) seedClassicMergeShape(ctx context.Context, t *testing.T, shape classicMergeShape) string {
	t.Helper()
	metric := fmt.Sprintf("classic_merge_%d_%d_%d_%d_%t", shape.series, shape.width, shape.samples, shape.groups, shape.sumMap)
	s.exec(ctx, t, fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d)), 'grp', toString(intDiv(number, %d) %% %d)),
  toDateTime64('%s', 9) - toIntervalSecond(((number %% %d) + 1) * %d),
  arrayWithConstant(%d, toUInt64(1)),
  arrayMap(j -> toFloat64(intDiv(number, %d) * %d + j), range(1, %d))
FROM numbers(%d)`,
		metric, shape.samples, shape.samples, shape.groups,
		classicMergeEvalTime.Format("2006-01-02 15:04:05"), shape.samples, int(classicMergeSampleSpacing/time.Second),
		shape.width+1,
		shape.samples, shape.width, shape.width+1,
		shape.series*shape.samples))
	return metric
}

// exceptionPeakMemory is the peak memory_usage query_log records for the
// refused query qid.
func (s *server) exceptionPeakMemory(ctx context.Context, t *testing.T, qid string) uint64 {
	t.Helper()
	s.flushLogs(ctx, t)
	var n uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT max(memory_usage) FROM system.query_log WHERE type = 'ExceptionWhileProcessing' AND query_id = ?", qid).Scan(&n); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if n == 0 {
		t.Fatalf("query_log holds no exception record for %s", qid)
	}
	return n
}

// classicMergeQueryID is a query id unique to one probe on one build.
func classicMergeQueryID(s *server, shape classicMergeShape) string {
	return fmt.Sprintf("classic-merge-%s-%d-%d-%d-%d", s.version, shape.series, shape.width, shape.samples, time.Now().UnixNano())
}

// serveClassicMerge issues shape's instant query over metric and returns the
// status and body.
func serveClassicMerge(ctx context.Context, mux http.Handler, metric string, shape classicMergeShape) (int, string) {
	by := "le"
	if shape.groups > 1 {
		by = "le, grp"
	}
	params := url.Values{}
	params.Set("query", fmt.Sprintf("histogram_quantile(0.5, sum by (%s) (sum_over_time(%s_bucket[5m])))", by, metric))
	params.Set("time", strconv.FormatInt(classicMergeEvalTime.Unix(), 10))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// assertClassicMergeQuantile checks the answer the disjoint seed implies. No
// two series share a bound, so each merged rung holds one series' cumulative
// count — at most samples x width — and the repaired ladder stays there until
// the +Inf rung jumps to the group's whole volume. The median of that volume
// lies past every finite rung, so each group's quantile is its highest
// finite bound: (i + 1) x width for the group's last series i.
func assertClassicMergeQuantile(t *testing.T, shape classicMergeShape, body string) {
	t.Helper()
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	decodeJSON(t, body, &resp)
	if len(resp.Data.Result) != shape.groups {
		t.Fatalf("%+v: %d series in the answer; want one per group: %s", shape, len(resp.Data.Result), body)
	}
	for _, r := range resp.Data.Result {
		group := 0
		if shape.groups > 1 {
			g, err := strconv.Atoi(r.Metric["grp"])
			if err != nil {
				t.Fatalf("%+v: answer series %v carries no numeric grp label", shape, r.Metric)
			}
			group = g
		}
		last := shape.series - shape.groups + group
		want := strconv.Itoa((last + 1) * shape.width)
		if got, _ := r.Value[1].(string); got != want {
			t.Fatalf("%+v: group %d quantile %q; want its highest merged bound %s", shape, group, got, want)
		}
	}
}
