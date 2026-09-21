//go:build integration

package nightly

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

const (
	nativeDashboardSeries          = 96
	nativeDashboardDefaultSeries   = 6
	nativeDashboardSamples         = 131
	nativeDashboardGroups          = 3
	nativeDashboardMinBuckets      = 100
	nativeDashboardBucketVariation = 60
	nativeDashboardMinScale        = 3
	nativeDashboardScaleVariation  = 18
	nativeDashboardInterval        = 30 * time.Second
	nativeDashboardWindow          = 5 * time.Minute
	nativeDashboardPanel           = time.Hour
	nativeDashboardMetric          = "cerberus_queries_duration_exp_hist"
	// The stress case measures physical query memory, not the conservative
	// admission estimate. Only that case raises the logical fold-work budget;
	// the server's independent 1 GiB cap remains unchanged.
	nativeDashboardStressFoldBudget = 250_000_000
	// Measured folded-state boundary peaks at ~423 MB on the 96-series case.
	// Leave room for supported-version and allocator variation, but keep this
	// below the original expression's >1 GiB peak.
	nativeDashboardMaxMemory         = 512 * 1024 * 1024
	nativeDashboardRelativeTolerance = 1e-10
)

// TestNativeDashboardRealCH pins the complete dense native-histogram dashboard,
// not a sparse selector. Both admission-default and larger configured workloads
// must return every expected label/anchor/value under a measured memory ceiling.
func TestNativeDashboardRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	image := os.Getenv("NATIVE_DASHBOARD_CH_IMAGE")
	if image == "" {
		image = tsGridInstantCHImage
	}
	_, client := startNightlyCH(ctx, t, image)
	conn := client.Conn()
	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO otel_metrics_exponential_histogram (MetricName, ServiceName, Attributes, ResourceAttributes, StartTimeUnix, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts, AggregationTemporality)")
	if err != nil {
		t.Fatal(err)
	}
	for series := range nativeDashboardSeries {
		width := nativeDashboardMinBuckets + series%nativeDashboardBucketVariation
		scale := int32(nativeDashboardMinScale + series%nativeDashboardScaleVariation)
		attrs := map[string]string{"cerberus_ql": strconv.Itoa(series % nativeDashboardGroups), "instance": strconv.Itoa(series)}
		unitSum := 0.0
		for bucket := range width {
			// Each observation is at its bucket's geometric midpoint.
			unitSum += math.Exp2((float64(bucket) + 0.5) * math.Exp2(-float64(scale)))
		}
		for sample := range nativeDashboardSamples {
			counts := make([]uint64, width)
			for bucket := range counts {
				counts[bucket] = uint64(sample + 1)
			}
			count := uint64(width * (sample + 1))
			if err := batch.Append(nativeDashboardMetric, "cerberus", attrs, map[string]string{}, base, base.Add(time.Duration(sample)*nativeDashboardInterval), count, unitSum*float64(sample+1), scale, uint64(0), int32(0), counts, int32(0), []uint64{}, int32(2)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}
	start := base.Add(nativeDashboardWindow)
	end := start.Add(nativeDashboardPanel)
	set := chopttest.ResolveEnabledSet(ctx, t, client, "auto")
	for _, workload := range []struct {
		name       string
		series     int
		selector   string
		foldBudget int64
	}{
		{name: "defaults", series: nativeDashboardDefaultSeries, selector: nativeDashboardMetric + "{instance=~\"0|1|2|3|4|5\"}"},
		{name: "dense", series: nativeDashboardSeries, selector: nativeDashboardMetric, foldBudget: nativeDashboardStressFoldBudget},
	} {
		t.Run(workload.name, func(t *testing.T) {
			handler := prom.New(client, schema.DefaultOTelMetrics(), nil)
			handler.Lowerers = choptwire.RangeLowerers(set)
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
			rules.ResultCache = false
			handler.Engine.SetSettings(rules)
			handler.Engine.RangeBucketFanoutFoldCostMaxUnits = workload.foldBudget
			mux := http.NewServeMux()
			handler.Mount(mux)
			for _, instant := range []bool{true, false} {
				t.Run(fmt.Sprintf("instant=%t", instant), func(t *testing.T) {
					queryID := fmt.Sprintf("native-dashboard-%s-%t", workload.name, instant)
					expr := "histogram_quantile(0.50, sum by (cerberus_ql)(rate(" + workload.selector + "[5m])))"
					params := url.Values{"query": {expr}, "start": {formatPromTime(start)}, "end": {formatPromTime(end)}, "step": {strconv.FormatFloat(nativeDashboardInterval.Seconds(), 'f', -1, 64)}, "time": {formatPromTime(end)}}
					path := "/api/v1/query_range"
					if instant {
						path = "/api/v1/query"
					}
					req := httptest.NewRequest(http.MethodGet, path+"?"+params.Encode(), nil).WithContext(chclient.WithQueryID(ctx, queryID))
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
					}
					var response struct {
						Status string `json:"status"`
						Data   struct {
							ResultType string `json:"resultType"`
							Result     []struct {
								Metric map[string]string   `json:"metric"`
								Value  []json.RawMessage   `json:"value"`
								Values [][]json.RawMessage `json:"values"`
							} `json:"result"`
						} `json:"data"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					wantType := "matrix"
					if instant {
						wantType = "vector"
					}
					if response.Status != "success" || response.Data.ResultType != wantType || len(response.Data.Result) != nativeDashboardGroups {
						t.Fatalf("unexpected response: %s", rec.Body.String())
					}
					seen := make(map[int]bool)
					for _, result := range response.Data.Result {
						group, err := strconv.Atoi(result.Metric["cerberus_ql"])
						if err != nil || group < 0 || group >= nativeDashboardGroups || seen[group] || len(result.Metric) != 1 {
							t.Fatalf("unexpected or duplicate labels: %v", result.Metric)
						}
						seen[group] = true
						samples := result.Values
						wantAnchors := int(nativeDashboardPanel/nativeDashboardInterval) + 1
						first := start
						if instant {
							samples = [][]json.RawMessage{result.Value}
							wantAnchors = 1
							first = end
						}
						if len(samples) != wantAnchors {
							t.Fatalf("group %d: got %d anchors, want %d", group, len(samples), wantAnchors)
						}
						wantValue := nativeDashboardMedian(workload.series, group)
						for i, sample := range samples {
							if len(sample) != 2 {
								t.Fatalf("malformed sample: %s", sample)
							}
							var timestamp float64
							var encodedValue string
							if err := json.Unmarshal(sample[0], &timestamp); err != nil {
								t.Fatal(err)
							}
							if err := json.Unmarshal(sample[1], &encodedValue); err != nil {
								t.Fatal(err)
							}
							value, err := strconv.ParseFloat(encodedValue, 64)
							wantTime := float64(first.Add(time.Duration(i) * nativeDashboardInterval).Unix())
							if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value-wantValue) > nativeDashboardRelativeTolerance*math.Abs(wantValue) || timestamp != wantTime {
								t.Fatalf("group %d anchor %d: got (%v,%s), want (%v,%v)", group, i, timestamp, encodedValue, wantTime, wantValue)
							}
						}
					}
					if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
						t.Fatal(err)
					}
					var memory, duration, readRows, sqlBytes uint64
					if err := conn.QueryRow(ctx, "SELECT memory_usage, query_duration_ms, read_rows, length(query) FROM system.query_log WHERE query_id = ? AND type = 'QueryFinish'", queryID).Scan(&memory, &duration, &readRows, &sqlBytes); err != nil {
						t.Fatal(err)
					}
					t.Logf("image=%s memory=%d duration_ms=%d read_rows=%d sql_bytes=%d", image, memory, duration, readRows, sqlBytes)
					if memory == 0 || memory > nativeDashboardMaxMemory || readRows == 0 {
						t.Fatalf("invalid or excessive resource use: memory=%d read_rows=%d", memory, readRows)
					}
				})
			}
		})
	}
}

// Each series adds one observation per bucket per interval, so the common rate
// factor cancels from the quantile. Independently merge integer bucket masses
// at the group's coarsest scale, then interpolate the median in log space.
func nativeDashboardMedian(seriesCount, group int) float64 {
	scale := nativeDashboardMinScale + group
	masses := make([]int, nativeDashboardMinBuckets+nativeDashboardBucketVariation)
	total := 0
	for series := group; series < seriesCount; series += nativeDashboardGroups {
		seriesScale := nativeDashboardMinScale + series%nativeDashboardScaleVariation
		for bucket := range nativeDashboardMinBuckets + series%nativeDashboardBucketVariation {
			masses[bucket>>(seriesScale-scale)]++
			total++
		}
	}
	rank := float64(total) / 2
	cumulative := 0
	for bucket, mass := range masses {
		if mass != 0 && float64(cumulative+mass) >= rank {
			fraction := (rank - float64(cumulative)) / float64(mass)
			return math.Exp2((float64(bucket) + fraction) * math.Exp2(-float64(scale)))
		}
		cumulative += mass
	}
	panic("invalid native histogram fixture")
}
