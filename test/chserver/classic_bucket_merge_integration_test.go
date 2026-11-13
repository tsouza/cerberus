//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// classicMergeBuilds are the builds the classic-histogram bucket-merge
// ceiling is pinned on: the bundled 26.8 LTS build and a 26.6 build, the
// oldest line the ceiling's calibration covers.
var classicMergeBuilds = []string{
	"clickhouse/clickhouse-server:26.8.36.3-alpine",
	"clickhouse/clickhouse-server:26.6.1.1193-alpine",
}

// classicMergeQueryMemoryBytes is the per-query memory cap every probe runs
// under: CERBERUS_CH_QUERY_MAX_MEMORY's 1 GiB default, the cap the merge
// ceiling is calibrated against.
const classicMergeQueryMemoryBytes = 1 << 30

// classicMergeEvalTime is the instant every probe evaluates at; each series'
// samples sit inside the five minutes before it.
var classicMergeEvalTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// classicMergeRejectedPeakBytes bounds the peak memory of a refused probe
// whose refusal is checked for timing. The guard counts the merge's entries
// before the merge Aggregate reads a row, so a shape just past the ceiling
// is refused after the scan and the per-series stage but before any merge
// work: such shapes peak at 125-200 MiB on both builds, while the sumMap
// merge on the same data, refused only after its aggregate, peaks above
// 700 MiB. A quarter of the cap separates the two.
const classicMergeRejectedPeakBytes = classicMergeQueryMemoryBytes / 4

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
// merge. checkPeak marks a refused shape whose peak memory must sit under
// classicMergeRejectedPeakBytes.
type classicMergeShape struct {
	name                           string
	series, width, samples, groups int
	sumMap, checkPeak              bool
}

// classicMergeAdmitted are admitted shapes, each under the 2,982,616-entry
// ceiling: single-sample at 58,000 x 51 entries through both merge
// constructions, the fullest window a series carries (fourteen samples) at
// 20,000 x 50, and 57,600 x 51 entries split over six merge groups. Each must
// answer with the quantile the seed implies.
var classicMergeAdmitted = []classicMergeShape{
	{name: "single-sample", series: 58_000, width: 50, samples: 1, groups: 1},
	{name: "multi-sample", series: 20_000, width: 50, samples: 14, groups: 1},
	{name: "multi-group", series: 57_600, width: 50, samples: 1, groups: 6},
	{name: "summap", series: 58_000, width: 50, samples: 1, groups: 1, sumMap: true},
}

// classicMergeRejected are shapes past the ceiling, each of which must be
// refused with the merge guard's own message rather than by the server:
// 60,000 x 51 = 3,060,000 entries single- and two-sample and through the
// sumMap merge; 100,000 x 51 through the sumMap merge, which the server
// cannot merge under the cap; and six merge groups of 40,000 x 51 entries
// each, every group under the ceiling while the query is four times past it.
var classicMergeRejected = []classicMergeShape{
	{name: "single-sample", series: 60_000, width: 50, samples: 1, groups: 1, checkPeak: true},
	{name: "multi-sample", series: 60_000, width: 50, samples: 2, groups: 1, checkPeak: true},
	{name: "summap", series: 60_000, width: 50, samples: 1, groups: 1, sumMap: true, checkPeak: true},
	{name: "summap-large", series: 100_000, width: 50, samples: 1, groups: 1, sumMap: true},
	{name: "multi-group", series: 240_000, width: 50, samples: 1, groups: 6},
}

// TestClassicBucketMerge_DisjointLayoutCeilingAcrossBuilds drives
// `histogram_quantile(0.5, sum by (le) (sum_over_time(<bucket>[5m])))` (or
// `by (le, grp)`) through the production Prometheus handler, under the 1 GiB
// query memory cap and the default merge ceiling, on each build. An admitted
// shape must answer with the quantile the disjoint seed implies; a shape
// past the ceiling must be refused by the bucket-merge guard, not by the
// server running out of memory, and a shape just past it before the merge
// runs.
func TestClassicBucketMerge_DisjointLayoutCeilingAcrossBuilds(t *testing.T) {
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
			for _, shape := range classicMergeAdmitted {
				t.Run("admitted/"+shape.name, func(t *testing.T) {
					metric := s.seedClassicMergeShape(ctx, t, shape)
					code, body := serveClassicMerge(ctx, muxes[shape.sumMap], metric, shape)
					if code != http.StatusOK {
						t.Fatalf("%s %+v: HTTP %d: %s; want the admitted shape answered", s.version, shape, code, body)
					}
					assertClassicMergeQuantile(t, shape, body)
				})
			}
			for _, shape := range classicMergeRejected {
				t.Run("rejected/"+shape.name, func(t *testing.T) {
					metric := s.seedClassicMergeShape(ctx, t, shape)
					qid := classicMergeQueryID(s, shape)
					code, body := serveClassicMerge(chclient.WithQueryID(ctx, qid), muxes[shape.sumMap], metric, shape)
					if code != http.StatusUnprocessableEntity || !strings.Contains(body, chplan.ClassicBucketMergeBudgetMessage) {
						t.Fatalf("%s %+v: HTTP %d: %s; want 422 carrying the bucket-merge guard's message",
							s.version, shape, code, body)
					}
					if !shape.checkPeak {
						return
					}
					if peak := s.exceptionPeakMemory(ctx, t, qid); peak >= classicMergeRejectedPeakBytes {
						t.Fatalf("%s %+v: refused at peak memory_usage %d bytes; want the guard to refuse before the merge, below %d bytes",
							s.version, shape, peak, classicMergeRejectedPeakBytes)
					}
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
