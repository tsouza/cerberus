//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/test/perf/smoke"
)

// analyzerBuilds are the builds the native-histogram analyzer probe runs on:
// the last 26.8 LTS build, which still lets a query disable the analyzer, and
// a 26.9 build, on which the analyzer is mandatory and enable_analyzer=0 is
// rejected with SETTING_CONSTRAINT_VIOLATION (code 452).
var analyzerBuilds = []string{
	"clickhouse/clickhouse-server:26.8.10.6-alpine",
	"clickhouse/clickhouse-server:26.9.25.15-alpine",
}

// Native-histogram probe shape: analyzerSeries series of analyzerMetric,
// seeded through analyzerEvalTime, queried over analyzerRateWindow.
const (
	analyzerMetric     = "analyzer_probe_latency_exp_hist"
	analyzerSeries     = 20
	analyzerSeedSpan   = 30 * time.Minute
	analyzerRateWindow = "15m"
	analyzerRangeSpan  = 10 * time.Minute
	analyzerRangeStep  = "300"
)

// analyzerMandatorySince is the first release line whose analyzer cannot be
// disabled, stated here independently of the chopt.AnalyzerMandatorySince the
// probe checks.
var analyzerMandatorySince = chopt.Version{Major: 26, Minor: 9}

// analyzerSelections are the optimization selections the probe resolves
// under: the default, and "off", which enables no feature and must still stamp
// enable_analyzer=0 on a build that permits it, because that stamp follows the
// build, not the selection.
var analyzerSelections = []string{chopt.SelectionAuto, "off"}

// analyzerEvalTime is the instant the probe evaluates at; the seed ends there.
var analyzerEvalTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// analyzerShape is one native-histogram query carrying the analyzer hazard:
// histogram_quantile reaches chplan.HistogramQuantileNative, and the
// range-mode histogram_count reaches the exponential RangeBucketFanout.
type analyzerShape struct {
	name string
	path func() string
}

var analyzerShapes = []analyzerShape{
	{"histogram_quantile", func() string {
		params := url.Values{}
		params.Set("query", fmt.Sprintf("histogram_quantile(0.9, sum(rate(%s[%s])))", analyzerMetric, analyzerRateWindow))
		params.Set("time", strconv.FormatInt(analyzerEvalTime.Unix(), 10))
		return "/api/v1/query?" + params.Encode()
	}},
	{"histogram_count_range", func() string {
		params := url.Values{}
		params.Set("query", fmt.Sprintf("histogram_count(rate(%s[%s]))", analyzerMetric, analyzerRateWindow))
		params.Set("start", strconv.FormatInt(analyzerEvalTime.Add(-analyzerRangeSpan).Unix(), 10))
		params.Set("end", strconv.FormatInt(analyzerEvalTime.Unix(), 10))
		params.Set("step", analyzerRangeStep)
		return "/api/v1/query_range?" + params.Encode()
	}},
}

// TestNativeHistogram_AnalyzerPostureAcrossBuilds drives native-histogram
// PromQL through the production Prometheus handler under the settings rules
// each analyzerSelections entry resolves on the build, and requires on every
// build that each query answers with data. It also checks the version policy
// chopt records, under every selection alike: the resolved set reports the
// analyzer mandatory exactly from 26.9; below it, system.query_log shows the
// query ran with the analyzer disabled, and from it the query must not carry
// enable_analyzer=0, which the server refuses.
func TestNativeHistogram_AnalyzerPostureAcrossBuilds(t *testing.T) {
	for _, image := range analyzerBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image)
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("%s: apply DDL: %v", s.image, err)
			}
			if _, err := smoke.SeedNativeHistogramAtScale(ctx, s.admin.Conn(), analyzerMetric, analyzerSeries,
				analyzerEvalTime.Add(-analyzerSeedSpan), analyzerEvalTime); err != nil {
				t.Fatalf("%s: seed: %v", s.image, err)
			}

			client := s.client(t, adminUser, adminPassword, chclient.Config{})
			for _, selection := range analyzerSelections {
				t.Run(selection, func(t *testing.T) {
					set := chopttest.ResolveEnabledSet(ctx, t, s.admin, selection)
					legacy := !set.AnalyzerMandatory()
					if mandatory := s.version.AtLeast(analyzerMandatorySince); legacy == mandatory {
						t.Fatalf("selection %q resolves the legacy analyzer = %v on %s; want it exactly below 26.9", selection, legacy, s.version)
					}
					rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
					handler := prom.New(client, schema.DefaultOTelMetrics(), nil)
					handler.Engine.SetSettings(rules)
					mux := http.NewServeMux()
					handler.Mount(mux)

					for _, shape := range analyzerShapes {
						t.Run(shape.name, func(t *testing.T) {
							qid := fmt.Sprintf("analyzer-probe-%s-%s-%d", selection, shape.name, time.Now().UnixNano())
							var resp struct {
								Data struct {
									Result []any `json:"result"`
								} `json:"data"`
							}
							// serveJSON fails the test on any status but 200: a
							// server refusing enable_analyzer=0 answers 502.
							serveJSON(chclient.WithQueryID(ctx, qid), t, mux, shape.path(), &resp)
							if len(resp.Data.Result) == 0 {
								t.Fatalf("%s on %s: empty result; want the seeded series", shape.name, s.version)
							}
							if legacy {
								chopttest.AssertQuerySettingStamped(ctx, t, s.admin.Conn(), qid, "enable_analyzer", "0")
								return
							}
							assertAnalyzerNotDisabled(ctx, t, s, qid)
						})
					}
				})
			}
		})
	}
}

// assertAnalyzerNotDisabled requires that no query the request dispatched
// under qid ran with the analyzer disabled. A served request on a build that
// refuses enable_analyzer=0 already shows that; this names it directly.
func assertAnalyzerNotDisabled(ctx context.Context, t *testing.T, s *server, qid string) {
	t.Helper()
	s.flushLogs(ctx, t)
	var finished, disabled uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT count(), countIf(Settings['allow_experimental_analyzer'] = '0' OR Settings['enable_analyzer'] = '0') "+
			"FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?", qid).Scan(&finished, &disabled); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if finished == 0 {
		t.Fatalf("query_id %s: no QueryFinish row in system.query_log; the probe did not reach the server", qid)
	}
	if disabled != 0 {
		t.Errorf("query_id %s on %s: %d query(ies) ran with the analyzer disabled; want none from 26.9", qid, s.version, disabled)
	}
}
