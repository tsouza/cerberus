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

// The seed: classicLayoutSeries series of classicLayoutBounds finite bounds
// (1 to classicLayoutBounds) plus +Inf, classicLayoutSamples samples each,
// classicLayoutSpacing apart inside the five minutes before
// classicMergeEvalTime. Every bucket of every sample counts one observation.
const (
	classicLayoutSeries  = 3000
	classicLayoutBounds  = 15
	classicLayoutSamples = 20
	classicLayoutSpacing = 15 * time.Second
)

// The two-level threshold setting, the value the layout is stamped with
// (8 MiB, which this seed's aggregate state passes), and the value under
// which the same statement aggregates single-level: 0 unsets the byte
// threshold, and the layout's series stay under the row threshold.
const (
	classicLayoutTwoLevelSetting = "group_by_two_level_threshold_bytes"
	classicLayoutTwoLevelStamp   = "8388608"
	classicLayoutSingleLevel     = 0
)

// classicLayoutMaxPeakFraction bounds the served statement's peak against the
// same statement aggregated single-level. On this shape the served statement
// peaked at 51 MiB on 26.3.17.56, and single-level at 307 MiB (#4519).
const classicLayoutMaxPeakFraction = 0.5

// TestClassicBucketLayout_InstantMergeAggregatesTwoLevel serves the instant
// `sum by (le) (<bucket>)` through the production handler on 26.3.17.56,
// against a schema whose Flags column was probed, so the selection carries
// the stale-marker layout. It checks each `le` sum against the seed, that the
// dispatched statement carries the two-level stamp, and that it peaks at no
// more than classicLayoutMaxPeakFraction of the same statement aggregated
// single-level. Single-level, the layout's aggregate hands its whole result
// up as one block, and one thread fans every series out (#4519).
func TestClassicBucketLayout_InstantMergeAggregatesTwoLevel(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, classicBuild263)
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	metric := seedClassicLayout(ctx, t, s)
	mux := classicLayoutMux(ctx, t, s)

	qid := fmt.Sprintf("classic-layout-%s-%d", s.version, time.Now().UnixNano())
	code, body := serveClassicLayout(chclient.WithQueryID(ctx, qid), mux, metric)
	assertClassicLayoutAnswer(t, s, code, body)
	served := s.classicKernelBlockRecord(ctx, t, qid)
	if got := served.settings[classicLayoutTwoLevelSetting]; got != classicLayoutTwoLevelStamp {
		t.Fatalf("%s: the served statement carries %s=%q; want %q", s.version, classicLayoutTwoLevelSetting, got, classicLayoutTwoLevelStamp)
	}

	replayed, singleLevel := s.classicReplay(ctx, t, served, qid+"-single-level", classicLayoutTwoLevelSetting, classicLayoutSingleLevel)
	if replayed != classicLayoutBounds+1 {
		t.Fatalf("%s: the statement single-level returned %d rows; want %d", s.version, replayed, classicLayoutBounds+1)
	}

	t.Logf("%s: peak %d bytes served, %d single-level", s.version, served.peak, singleLevel.peak)
	if float64(served.peak) > classicLayoutMaxPeakFraction*float64(singleLevel.peak) {
		t.Fatalf("%s: the served statement peaked at %d bytes against %d single-level; want at most %v of it",
			s.version, served.peak, singleLevel.peak, classicLayoutMaxPeakFraction)
	}
}

// classicLayoutMux mounts the production Prometheus handler over s as
// classicMergeMux does, with the auto selection, against a metrics schema
// whose Flags column was probed, as cmd/cerberus resolves it on a table that
// carries the column.
func classicLayoutMux(ctx context.Context, t *testing.T, s *server) *http.ServeMux {
	t.Helper()
	client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: classicMergeQueryMemoryBytes})
	set := chopttest.ResolveEnabledSet(ctx, t, s.admin, "auto")
	metrics := schema.DefaultOTelMetrics()
	metrics.FlagsColumnProbed = true
	handler := prom.New(client, metrics, nil)
	handler.Engine.SetSettings(choptwire.SettingsRules(set, metrics, schema.DefaultOTelTraces(), schema.DefaultOTelLogs()))
	handler.SetLowerers(choptwire.RangeLowerers(set))
	handler.ResourceBounds = promql.DefaultResourceBounds()
	mux := http.NewServeMux()
	handler.Mount(mux)
	return mux
}

// seedClassicLayout inserts the seed under its own metric name and returns
// that name.
func seedClassicLayout(ctx context.Context, t *testing.T, s *server) string {
	t.Helper()
	metric := fmt.Sprintf("classic_layout_%d_%d", classicLayoutSeries, classicLayoutBounds)
	s.exec(ctx, t, fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d))),
  toDateTime64('%s', 9) - toIntervalSecond((number %% %d) * %d),
  arrayWithConstant(%d, toUInt64(1)),
  arrayMap(j -> toFloat64(j), range(1, %d))
FROM numbers(%d)`,
		metric, classicLayoutSamples,
		classicMergeEvalTime.Format("2006-01-02 15:04:05"), classicLayoutSamples, int(classicLayoutSpacing/time.Second),
		classicLayoutBounds+1,
		classicLayoutBounds+1,
		classicLayoutSeries*classicLayoutSamples))
	return metric
}

// serveClassicLayout issues the instant `sum by (le) (<bucket>)` over metric
// at classicMergeEvalTime and returns the status and body.
func serveClassicLayout(ctx context.Context, mux http.Handler, metric string) (int, string) {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("sum by (le) (%s_bucket)", metric))
	params.Set("time", strconv.FormatInt(classicMergeEvalTime.Unix(), 10))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// assertClassicLayoutAnswer checks one series per `le`: bound j's cumulative
// count is j in every series' latest sample, and +Inf's is
// classicLayoutBounds+1, so each sums to that count times
// classicLayoutSeries.
func assertClassicLayoutAnswer(t *testing.T, s *server, code int, body string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s; want the merge answered", s.version, code, body)
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	decodeJSON(t, body, &resp)
	if len(resp.Data.Result) != classicLayoutBounds+1 {
		t.Fatalf("%s: %d series in the answer; want %d", s.version, len(resp.Data.Result), classicLayoutBounds+1)
	}
	for _, series := range resp.Data.Result {
		le := series.Metric["le"]
		rung := classicLayoutBounds + 1
		if le != "+Inf" {
			bound, err := strconv.Atoi(le)
			if err != nil {
				t.Fatalf("%s: series %v carries no integer le label", s.version, series.Metric)
			}
			rung = bound
		}
		raw, _ := series.Value[1].(string)
		got, err := strconv.ParseFloat(raw, 64)
		if want := float64(rung * classicLayoutSeries); err != nil || got != want {
			t.Fatalf("%s: le=%s answered %q; want %v", s.version, le, raw, want)
		}
	}
}
