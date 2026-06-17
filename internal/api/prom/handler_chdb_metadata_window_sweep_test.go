//go:build chdb

// Exploratory window-vs-freshness sweep for the Prometheus metadata
// endpoints (/series, /labels, /label/<name>/values). This is the
// metadata analogue of the rc.9 eval-instant sweep
// (test/spec/eval_instant_sweep.go): it seeds series at FIXED sample
// times anchored well in the past, then varies the REQUEST window
// [start,end] independently of wall-clock and asserts each endpoint
// returns exactly the series/labels/values whose sample falls in the
// window.
//
// Why this closes a real blind spot: every prior metadata chDB test
// seeds and queries at (or near) the same anchor, so a handler that
// ignores the request window — evaluating at `now` with a 5m staleness
// window (/series, /labels pre-fix) or clamping to an instant window at
// `end` (/label_values pre-fix) — still passed. Those exact defects are
// the rc.9 /series empty-window bug and its /labels + /label/<name>/values
// siblings. Anchoring the seed ~37 days before wall-clock means a
// now()-anchored handler returns EMPTY for every window here, so the
// sweep is red pre-fix and green only once all three endpoints honour the
// full closed [start,end] window (promql.LowerMetadataRange).

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"
)

// metaWindowSeedBase anchors the sweep's sample times. It is deliberately
// weeks before any plausible test wall-clock so a handler that evaluates a
// metadata request at `now` (the pre-fix /series + /labels behaviour)
// returns empty for EVERY window below — making the window-drop bug fail
// the sweep instead of hiding behind a now-aligned seed.
var metaWindowSeedBase = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

// metaWindowSeed builds the gauge/sum/histogram tables plus four `m`
// series (one per `job`), each with a single sample at a distinct time
// spread across a two-hour past span. A sample's job label uniquely
// identifies which sample time produced it, so the per-window oracle is
// exactly "the jobs whose sample time lies in [winStart,winEnd]".
func metaWindowSeed() (string, map[string]time.Time) {
	t0 := metaWindowSeedBase.Add(-2 * time.Hour)
	t1 := metaWindowSeedBase.Add(-1 * time.Hour)
	t2 := metaWindowSeedBase.Add(-10 * time.Minute)
	t3 := metaWindowSeedBase.Add(-30 * time.Second)
	jobTimes := map[string]time.Time{"t0": t0, "t1": t1, "t2": t2, "t3": t3}

	lit := func(t time.Time) string { return t.Format("2006-01-02 15:04:05.000000000") }
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('m', map('job', 't0'), toDateTime64('%s', 9), 1.0),
    ('m', map('job', 't1'), toDateTime64('%s', 9), 1.0),
    ('m', map('job', 't2'), toDateTime64('%s', 9), 1.0),
    ('m', map('job', 't3'), toDateTime64('%s', 9), 1.0);
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, IsMonotonic) VALUES
    ('mc_total', map('job', 't0'), toDateTime64('%s', 9), 1.0, true),
    ('mc_total', map('job', 't3'), toDateTime64('%s', 9), 1.0, true);`,
		lit(t0), lit(t1), lit(t2), lit(t3), lit(t0), lit(t3))
	return seed, jobTimes
}

// metaWindowCase is one row of the sweep: a request window and the set of
// jobs whose sample falls inside it. omitBounds drives the no-start/no-end
// case (whole-table scan; reference Prometheus's min/max-retention
// default).
type metaWindowCase struct {
	name        string
	start       time.Time
	end         time.Time
	omitBounds  bool
	expectJobs  []string
	description string
}

func metaWindowCases(jt map[string]time.Time) []metaWindowCase {
	sec := time.Second
	return []metaWindowCase{
		{
			name:        "W1_full",
			start:       jt["t0"].Add(-sec),
			end:         jt["t3"].Add(sec),
			expectJobs:  []string{"t0", "t1", "t2", "t3"},
			description: "window spans all four samples — pre-fix /series & /labels return empty (now-anchored), /label_values drops t0,t1,t2 (instant-at-end)",
		},
		{
			name:        "W2_early",
			start:       jt["t0"].Add(-sec),
			end:         jt["t1"].Add(sec),
			expectJobs:  []string{"t0", "t1"},
			description: "window ends ~1h in the past — pre-fix everything is empty (no sample in a now/instant-at-end window)",
		},
		{
			name:        "W3_late",
			start:       jt["t2"].Add(-sec),
			end:         jt["t3"].Add(sec),
			expectJobs:  []string{"t2", "t3"},
			description: "excludes t0,t1 — catches a handler that ignores `start` and returns all series",
		},
		{
			name:        "W4_outside",
			start:       jt["t3"].Add(time.Hour),
			end:         jt["t3"].Add(2 * time.Hour),
			expectJobs:  nil,
			description: "window entirely after every sample — must be empty; catches a handler that ignores the window and returns everything",
		},
		{
			name:        "W5_omitted",
			omitBounds:  true,
			expectJobs:  []string{"t0", "t1", "t2", "t3"},
			description: "no start/end — whole-table scan returns all series; pins the now64(9)-default break",
		},
	}
}

func (c metaWindowCase) query(base, path, metric, extra string) string {
	v := url.Values{}
	v.Set("match[]", metric)
	if !c.omitBounds {
		v.Set("start", fmt.Sprintf("%d", c.start.Unix()))
		v.Set("end", fmt.Sprintf("%d", c.end.Unix()))
	}
	if extra != "" {
		return fmt.Sprintf("%s%s?%s&%s", base, path, v.Encode(), extra)
	}
	return fmt.Sprintf("%s%s?%s", base, path, v.Encode())
}

// getMetaData GETs a metadata endpoint and returns the decoded `data`
// array as raw JSON messages (each is a string for /labels + /values, or
// an object for /series).
func getMetaData(t *testing.T, u string) []json.RawMessage {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", u, resp.StatusCode, body)
	}
	var parsed struct {
		Status string            `json:"status"`
		Error  string            `json:"error"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("GET %s: unmarshal: %v body=%s", u, err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("GET %s: status=%q err=%s", u, parsed.Status, parsed.Error)
	}
	return parsed.Data
}

func stringData(t *testing.T, raw []json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err != nil {
			t.Fatalf("decode string element %s: %v", r, err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func seriesJobs(t *testing.T, raw []json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		var m map[string]string
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatalf("decode series element %s: %v", r, err)
		}
		if m["__name__"] != "m" {
			t.Errorf("series missing __name__=m: %v", m)
		}
		out = append(out, m["job"])
	}
	sort.Strings(out)
	return out
}

func setEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestMetadataWindowSweep_ChDB is the core sweep. For each window it
// asserts every metadata endpoint returns exactly the in-window series.
func TestMetadataWindowSweep_ChDB(t *testing.T) {
	seed, jt := metaWindowSeed()
	srv, _ := newChDBServer(t, seed)

	for _, c := range metaWindowCases(jt) {
		t.Run(c.name, func(t *testing.T) {
			want := c.expectJobs
			nonEmpty := len(want) > 0

			// /api/v1/series — the endpoint the rc.9 bug was reported on.
			// Oracle: the series whose sample is in [start,end].
			gotSeries := seriesJobs(t, getMetaData(t, c.query(srv.URL, "/api/v1/series", "m", "")))
			if !setEqual(gotSeries, want) {
				t.Errorf("/series jobs = %v, want %v  (%s)", gotSeries, want, c.description)
			}

			// /api/v1/label/job/values — the /label_values sibling
			// (instant-at-end pre-fix). Oracle: same in-window job set.
			gotValues := stringData(t, getMetaData(t, c.query(srv.URL, "/api/v1/label/job/values", "m", "")))
			if !setEqual(gotValues, want) {
				t.Errorf("/label/job/values = %v, want %v  (%s)", gotValues, want, c.description)
			}

			// /api/v1/labels — the now-wallclock sibling. `__name__` is
			// always prepended; `job` appears IFF an in-window series
			// carries it. Full set-equality (not just job-presence) so a
			// stray extra key would also fail.
			gotLabels := stringData(t, getMetaData(t, c.query(srv.URL, "/api/v1/labels", "m", "")))
			wantLabels := []string{"__name__"}
			if nonEmpty {
				wantLabels = []string{"__name__", "job"}
			}
			if !setEqual(gotLabels, wantLabels) {
				t.Errorf("/labels = %v, want %v  (%s)", gotLabels, wantLabels, c.description)
			}

			// /api/v1/label/__name__/values — existence of the metric in
			// the window.
			gotNames := stringData(t, getMetaData(t, c.query(srv.URL, "/api/v1/label/__name__/values", "m", "")))
			wantNames := []string{}
			if nonEmpty {
				wantNames = []string{"m"}
			}
			if !setEqual(gotNames, wantNames) {
				t.Errorf("/label/__name__/values = %v, want %v  (%s)", gotNames, wantNames, c.description)
			}
		})
	}
}

// TestMetadataWindowSweep_SingleTable_ChDB covers the single-table
// lowering seam: a suffixed `_total` counter resolves to otel_metrics_sum
// alone (not the Gauge∪Sum scan the unsuffixed `m` uses), so the
// metadataFullRange branch on the primary lowerVectorSelector return path
// is exercised against a single physical table. mc_total has samples at
// t0 and t3. (The companion-union seam in lowerCompanionUnion is covered
// separately by TestMetadataWindowSweep_HistogramCompanion_ChDB.)
func TestMetadataWindowSweep_SingleTable_ChDB(t *testing.T) {
	seed, jt := metaWindowSeed()
	srv, _ := newChDBServer(t, seed)

	sec := time.Second
	cases := []struct {
		name       string
		start, end time.Time
		want       []string
	}{
		{"full", jt["t0"].Add(-sec), jt["t3"].Add(sec), []string{"t0", "t3"}},
		{"late_only_t3", jt["t2"].Add(-sec), jt["t3"].Add(sec), []string{"t3"}},
		{"early_only_t0", jt["t0"].Add(-sec), jt["t1"].Add(sec), []string{"t0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cc := metaWindowCase{start: c.start, end: c.end}
			got := stringData(t, getMetaData(t, cc.query(srv.URL, "/api/v1/label/job/values", "mc_total", "")))
			if !setEqual(got, c.want) {
				t.Errorf("/label/job/values match[]=mc_total = %v, want %v", got, c.want)
			}
			gotSeries := getMetaData(t, cc.query(srv.URL, "/api/v1/series", "mc_total", ""))
			if len(gotSeries) != len(c.want) {
				t.Errorf("/series match[]=mc_total returned %d series, want %d", len(gotSeries), len(c.want))
			}
		})
	}
}

// TestMetadataWindowSweep_HistogramCompanion_ChDB covers the SECOND,
// distinct lowering seam — lowerCompanionUnion — which a classic-histogram
// `_count` / `_sum` selector routes through (histogram arm UNION-ALL
// literal-suffixed value-table arms). This is the exact shape of the GCP
// production metric the rc.9 bug was reported on
// (loadbalancing_…_https_request_count, a DELTA histogram), so its window
// handling MUST be verified with real in/out-of-window rows: a regression
// dropping the window on this arm would pass the gauge sweep above green.
func TestMetadataWindowSweep_HistogramCompanion_ChDB(t *testing.T) {
	jt := map[string]time.Time{
		"t0": metaWindowSeedBase.Add(-2 * time.Hour),
		"t1": metaWindowSeedBase.Add(-1 * time.Hour),
		"t2": metaWindowSeedBase.Add(-10 * time.Minute),
		"t3": metaWindowSeedBase.Add(-30 * time.Second),
	}
	lit := func(t time.Time) string { return t.Format("2006-01-02 15:04:05.000000000") }
	// `mh` lives in otel_metrics_histogram at all four sample times, one
	// per job. All three tables exist so the companion union (histogram +
	// literal-name value arms) resolves; the gauge/sum tables stay empty.
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds) VALUES
    ('mh', map('job', 't0'), toDateTime64('%s', 9), 1, 1.0, [1, 0], [0.5]),
    ('mh', map('job', 't1'), toDateTime64('%s', 9), 1, 1.0, [1, 0], [0.5]),
    ('mh', map('job', 't2'), toDateTime64('%s', 9), 1, 1.0, [1, 0], [0.5]),
    ('mh', map('job', 't3'), toDateTime64('%s', 9), 1, 1.0, [1, 0], [0.5]);`,
		lit(jt["t0"]), lit(jt["t1"]), lit(jt["t2"]), lit(jt["t3"]))
	srv, _ := newChDBServer(t, seed)

	for _, c := range metaWindowCases(jt) {
		t.Run(c.name, func(t *testing.T) {
			// `mh_count` is the companion-union selector. Its job values are
			// the jobs of the in-window mh rows — the same window oracle.
			got := stringData(t, getMetaData(t, c.query(srv.URL, "/api/v1/label/job/values", "mh_count", "")))
			if !setEqual(got, c.expectJobs) {
				t.Errorf("/label/job/values match[]=mh_count = %v, want %v  (%s)", got, c.expectJobs, c.description)
			}
		})
	}
}

// TestMetadataWindowSweep_ResourceAttr_ChDB reproduces the exact
// production trigger: label_values(metric, deployment_environment_name),
// where the label is a ResourceAttributes-map key merged into Attributes
// by augmentSelectorAttributes BEFORE the window wrap. The merge-then-
// window path is only exercised when the seed carries a ResourceAttributes
// column AND the request window is offset from the (past-anchored) samples
// — which no prior resource-attr test does (they seed at time.Now()).
func TestMetadataWindowSweep_ResourceAttr_ChDB(t *testing.T) {
	jt := map[string]time.Time{
		"t0": metaWindowSeedBase.Add(-2 * time.Hour),
		"t1": metaWindowSeedBase.Add(-1 * time.Hour),
		"t2": metaWindowSeedBase.Add(-10 * time.Minute),
		"t3": metaWindowSeedBase.Add(-30 * time.Second),
	}
	// env value per sample time, so the in-window env set is the oracle.
	envOf := map[string]string{"t0": "env0", "t1": "env1", "t2": "env2", "t3": "env3"}
	lit := func(t time.Time) string { return t.Format("2006-01-02 15:04:05.000000000") }
	row := func(job string) string {
		return fmt.Sprintf("('rm', map('job', '%s'), map('deployment.environment.name', '%s'), toDateTime64('%s', 9), 1.0)",
			job, envOf[job], lit(jt[job]))
	}
	seed := resourceAttrGaugeDDL + resourceAttrSumDDL + resourceAttrHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, Attributes, ResourceAttributes, TimeUnix, Value) VALUES
    %s, %s, %s, %s;`, row("t0"), row("t1"), row("t2"), row("t3"))
	srv, _ := newChDBServer(t, seed)

	for _, c := range metaWindowCases(jt) {
		t.Run(c.name, func(t *testing.T) {
			wantEnvs := make([]string, 0, len(c.expectJobs))
			for _, j := range c.expectJobs {
				wantEnvs = append(wantEnvs, envOf[j])
			}
			got := stringData(t, getMetaData(t, c.query(srv.URL, "/api/v1/label/deployment_environment_name/values", "rm", "")))
			if !setEqual(got, wantEnvs) {
				t.Errorf("/label/deployment_environment_name/values match[]=rm = %v, want %v  (%s)", got, wantEnvs, c.description)
			}
		})
	}
}

// TestMetadataWindowSweep_Boundary_ChDB pins the CLOSED interval — a
// sample exactly at `start` or exactly at `end` is INCLUDED — so an
// `OpGe`/`OpLe` → `OpGt`/`OpLt` regression (off-by-one at the window edge)
// fails. Whole-second sample times keep the unix-second bounds exact.
func TestMetadataWindowSweep_Boundary_ChDB(t *testing.T) {
	seed, jt := metaWindowSeed()
	srv, _ := newChDBServer(t, seed)
	t2 := jt["t2"]

	cases := []struct {
		name       string
		start, end time.Time
		wantT2     bool
	}{
		{"start_eq_sample", t2, t2.Add(time.Hour), true},                     // sample at start → included
		{"end_eq_sample", t2.Add(-time.Hour), t2, true},                      // sample at end → included
		{"zero_width_at_sample", t2, t2, true},                               // start==end==sample → included
		{"just_after_sample", t2.Add(time.Second), t2.Add(time.Hour), false}, // start > sample → excluded
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cc := metaWindowCase{start: c.start, end: c.end}
			got := stringData(t, getMetaData(t, cc.query(srv.URL, "/api/v1/label/job/values", "m", "")))
			if hasT2 := contains(got, "t2"); hasT2 != c.wantT2 {
				t.Errorf("boundary %s: hasT2=%v want %v (got=%v)", c.name, hasT2, c.wantT2, got)
			}
		})
	}
}
