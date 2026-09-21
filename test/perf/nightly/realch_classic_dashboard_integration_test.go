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
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/solver"
)

const (
	classicDashboardSeries          = 384
	classicDashboardSamples         = 131
	classicDashboardGroups          = 3
	classicDashboardInterval        = 30 * time.Second
	classicDashboardWindow          = 5 * time.Minute
	classicDashboardPanel           = time.Hour
	classicDashboardMetric          = "core_http_request_duration_seconds"
	classicDashboardEnvironment     = "prod-aws-us-east-1-default"
	classicDashboardResetBase       = 71
	classicDashboardResetVariation  = 5
	classicDashboardFactorVariation = 4
	classicDashboardExtraLabels     = 16
)

var (
	classicDashboardBounds = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048}
	classicDashboardMasses = []uint64{5, 10, 20, 30, 20, 8, 4, 2, 1, 1, 1, 1, 1}
)

func TestClassicDashboardRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, client := startTSGridInstantCH(ctx, t)
	conn := client.Conn()
	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO otel_metrics_histogram (MetricName, ServiceName, Attributes, ResourceAttributes, StartTimeUnix, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, AggregationTemporality)")
	if err != nil {
		t.Fatal(err)
	}
	for series := range classicDashboardSeries {
		attrs := map[string]string{"k8s_deployment_name": strconv.Itoa(series % classicDashboardGroups), "instance": strconv.Itoa(series)}
		resources := map[string]string{"deployment_environment_name": classicDashboardEnvironment}
		for label := range classicDashboardExtraLabels {
			resources[fmt.Sprintf("resource_%d", label)] = fmt.Sprintf("resource-value-%d", series)
		}
		for sample := range classicDashboardSamples {
			// Staggered resets make aggregate-before-rate observably incorrect.
			resetAt := classicDashboardResetBase + series%classicDashboardResetVariation
			factor := uint64((sample%resetAt + 1) * (series%classicDashboardFactorVariation + 1))
			counts := make([]uint64, len(classicDashboardMasses))
			var count uint64
			var sum float64
			for bucket, mass := range classicDashboardMasses {
				counts[bucket] = mass * factor
				count += counts[bucket]
				value := classicDashboardBounds[len(classicDashboardBounds)-1]
				if bucket < len(classicDashboardBounds) {
					value = classicDashboardBounds[bucket]
				}
				sum += value * float64(counts[bucket])
			}
			if err := batch.Append(classicDashboardMetric, "core", attrs, resources, base, base.Add(time.Duration(sample)*classicDashboardInterval), count, sum, counts, classicDashboardBounds, int32(2)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}
	start := base.Add(classicDashboardWindow)
	end := start.Add(classicDashboardPanel)
	inner := `sum by (le, k8s_deployment_name)(rate(core_http_request_duration_seconds_bucket{deployment_environment_name="prod-aws-us-east-1-default",k8s_deployment_name=~".*"}[5m]))`
	for _, mode := range []string{"auto", "auto,quantile_prom_histogram"} {
		t.Run(mode, func(t *testing.T) {
			set := chopttest.ResolveEnabledSet(ctx, t, client, mode)
			handler := prom.New(client, schema.DefaultOTelMetrics(), nil)
			handler.Lowerers = choptwire.RangeLowerers(set)
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
			rules.ResultCache = false
			handler.Engine.SetSettings(rules)
			solverConfig := solver.DefaultConfig()
			solverConfig.Mode = solver.ModeAuto
			handler.Engine.Solver = solver.New(solverConfig, engine.ChsqlEmitter{}, solver.ExecDeps{Client: client, Breaker: client})
			mux := http.NewServeMux()
			handler.Mount(mux)
			for _, quantile := range []bool{true, false} {
				for _, instant := range []bool{true, false} {
					t.Run(fmt.Sprintf("quantile=%t/instant=%t", quantile, instant), func(t *testing.T) {
						expr := inner
						if quantile {
							expr = "histogram_quantile(0.5," + inner + ")"
						}
						queryID := fmt.Sprintf("classic-dashboard-%s-%t-%t", mode, quantile, instant)
						params := url.Values{"query": {expr}, "start": {formatPromTime(start)}, "end": {formatPromTime(end)}, "step": {strconv.FormatFloat(classicDashboardInterval.Seconds(), 'f', -1, 64)}, "time": {formatPromTime(end)}}
						path := "/api/v1/query_range"
						if instant {
							path = "/api/v1/query"
						}
						req := httptest.NewRequest(http.MethodGet, path+"?"+params.Encode(), nil).WithContext(chclient.WithQueryID(ctx, queryID))
						rec := httptest.NewRecorder()
						before := time.Now()
						mux.ServeHTTP(rec, req)
						t.Logf("status=%d wall=%s body_bytes=%d route=%s", rec.Code, time.Since(before), rec.Body.Len(), rec.Header().Get("X-Cerberus-Route-Decision"))
						if rec.Code != http.StatusOK {
							t.Fatalf("%s", rec.Body.String())
						}
						checkClassicDashboardResult(t, rec.Body.Bytes(), quantile, instant, start, end, classicDashboardInterval, 0.5)
						if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
							t.Fatal(err)
						}
						var memory, duration, readRows, readBytes uint64
						var statement string
						if err := conn.QueryRow(ctx, "SELECT memory_usage, query_duration_ms, read_rows, read_bytes, query FROM system.query_log WHERE query_id = ? AND type = 'QueryFinish'", queryID).Scan(&memory, &duration, &readRows, &readBytes, &statement); err != nil {
							t.Fatal(err)
						}
						t.Logf("memory=%d duration_ms=%d read_rows=%d read_bytes=%d sql_bytes=%d", memory, duration, readRows, readBytes, len(statement))
					})
				}
			}
		})
	}
}

func checkClassicDashboardResult(t *testing.T, body []byte, quantile, instant bool, start, end time.Time, step time.Duration, phi float64) {
	t.Helper()
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
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	wantSeries, wantLabels, wantType := classicDashboardGroups, 1, "matrix"
	if !quantile {
		wantSeries *= len(classicDashboardMasses)
		wantLabels++
	}
	if instant {
		wantType = "vector"
	}
	if response.Status != "success" || response.Data.ResultType != wantType || len(response.Data.Result) != wantSeries {
		t.Fatalf("unexpected response: status=%s type=%s series=%d want %d", response.Status, response.Data.ResultType, len(response.Data.Result), wantSeries)
	}
	seen := make(map[string]bool)
	for _, result := range response.Data.Result {
		group, err := strconv.Atoi(result.Metric["k8s_deployment_name"])
		key := result.Metric["k8s_deployment_name"] + "/" + result.Metric["le"]
		if err != nil || group < 0 || group >= classicDashboardGroups || len(result.Metric) != wantLabels || seen[key] {
			t.Fatalf("unexpected labels %v", result.Metric)
		}
		seen[key] = true
		want := classicDashboardQuantile(phi)
		if !quantile {
			bound, err := strconv.ParseFloat(result.Metric["le"], 64)
			if err != nil {
				t.Fatal(err)
			}
			var mass, factor uint64
			found := math.IsInf(bound, 1)
			for i, value := range classicDashboardMasses {
				mass += value
				if i < len(classicDashboardBounds) && classicDashboardBounds[i] == bound {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("unexpected bound %v", bound)
			}
			for series := group; series < classicDashboardSeries; series += classicDashboardGroups {
				factor += uint64(series%classicDashboardFactorVariation + 1)
			}
			want = float64(mass*factor) / classicDashboardInterval.Seconds()
		}
		samples, first := result.Values, start
		wantAnchors := int(end.Sub(start)/step) + 1
		if instant {
			samples = [][]json.RawMessage{result.Value}
			first = end
			wantAnchors = 1
		}
		if len(samples) != wantAnchors {
			t.Fatalf("%s: got %d anchors, want %d", key, len(samples), wantAnchors)
		}
		for i, sample := range samples {
			if len(sample) != 2 {
				t.Fatalf("invalid sample %s", sample)
			}
			var timestamp float64
			var encoded string
			if err := json.Unmarshal(sample[0], &timestamp); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(sample[1], &encoded); err != nil {
				t.Fatal(err)
			}
			value, err := strconv.ParseFloat(encoded, 64)
			const relativeTolerance = 1e-9
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value-want) > relativeTolerance*math.Abs(want) || timestamp != float64(first.Add(time.Duration(i)*step).Unix()) {
				t.Fatalf("%s anchor %d: got (%v,%s), want (%v,%v)", key, i, timestamp, encoded, first.Add(time.Duration(i)*step), want)
			}
		}
	}
}

func classicDashboardQuantile(phi float64) float64 {
	var total uint64
	for _, mass := range classicDashboardMasses {
		total += mass
	}
	rank := phi * float64(total)
	cumulative, low := 0.0, 0.0
	for i, mass := range classicDashboardMasses {
		if i == len(classicDashboardBounds) {
			return low
		}
		if cumulative+float64(mass) >= rank {
			return low + (classicDashboardBounds[i]-low)*(rank-cumulative)/float64(mass)
		}
		cumulative += float64(mass)
		low = classicDashboardBounds[i]
	}
	panic("invalid classic histogram fixture")
}
