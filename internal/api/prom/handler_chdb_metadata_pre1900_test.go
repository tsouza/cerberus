//go:build chdb

// chDB coverage for metadata windows whose bounds fall before 1900.
//
// A DateTime64(9) column holds instants back to 1677-09-21, but
// ClickHouse parses a time literal earlier than 1900-01-01 as
// 1900-01-01. A metadata bound in that range must therefore compare
// exactly some other way. The seed puts one sample in 1800 and one in
// 2000, and every case sits on a nanosecond edge of the 1800 sample, so a
// bound clamped to 1900 (or off by one nanosecond) changes the answer.

package prom_test

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

// pre1900Sample is the 1800 sample's instant; pre1900Modern the 2000 one.
var (
	pre1900Sample = time.Date(1800, time.January, 1, 0, 0, 0, 0, time.UTC)
	pre1900Modern = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// pre1900Seed seeds metric `m` with job="y1800" at [pre1900Sample] and
// job="y2000" at [pre1900Modern]. The 1800 value is inserted from its
// Unix nanoseconds: a toDateTime64 literal would itself clamp to 1900.
func pre1900Seed() string {
	return metaShapedMetricsDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('m', map('job', 'y1800'), fromUnixTimestamp64Nano(toInt64(%d)), 1.0),
    ('m', map('job', 'y2000'), fromUnixTimestamp64Nano(toInt64(%d)), 1.0);`,
		pre1900Sample.UnixNano(), pre1900Modern.UnixNano())
}

// TestMetadataPre1900Window_ChDB asserts every metadata enumeration
// shape answers exactly over a window with a bound before 1900: the
// matched path (promql.LowerMetadataRange), the no-match WHERE arms, and
// the now-anchored HAVING arms (no `end`).
func TestMetadataPre1900Window_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, pre1900Seed())
	rfc := func(t time.Time) string { return t.Format(time.RFC3339Nano) }
	ns := time.Nanosecond
	cases := []struct {
		name       string
		start, end string // "" omits the parameter
		want       []string
	}{
		{"start_at_1800_sample", rfc(pre1900Sample), rfc(pre1900Modern), []string{"y1800", "y2000"}},
		{"start_1ns_after_1800_sample", rfc(pre1900Sample.Add(ns)), rfc(pre1900Modern), []string{"y2000"}},
		{"end_at_1800_sample", "1700-01-01T00:00:00Z", rfc(pre1900Sample), []string{"y1800"}},
		{"end_1ns_before_1800_sample", "1700-01-01T00:00:00Z", rfc(pre1900Sample.Add(-ns)), nil},
		{"start_before_storage", "1600-01-01T00:00:00Z", rfc(pre1900Modern), []string{"y1800", "y2000"}},
		{"open_end_start_at_1800_sample", rfc(pre1900Sample), "", []string{"y1800", "y2000"}},
		{"open_end_start_1ns_after_1800_sample", rfc(pre1900Sample.Add(ns)), "", []string{"y2000"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			query := func(path string, match bool) string {
				v := url.Values{}
				if match {
					v.Set("match[]", "m")
				}
				if c.start != "" {
					v.Set("start", c.start)
				}
				if c.end != "" {
					v.Set("end", c.end)
				}
				return srv.URL + path + "?" + v.Encode()
			}
			if got := seriesJobs(t, getMetaData(t, query("/api/v1/series", true))); !setEqual(got, c.want) {
				t.Errorf("/series jobs = %v, want %v", got, c.want)
			}
			if got := stringData(t, getMetaData(t, query("/api/v1/label/job/values", true))); !setEqual(got, c.want) {
				t.Errorf("/label/job/values with match[] = %v, want %v", got, c.want)
			}
			if got := stringData(t, getMetaData(t, query("/api/v1/label/job/values", false))); !setEqual(got, c.want) {
				t.Errorf("/label/job/values without match[] = %v, want %v", got, c.want)
			}
			wantLabels := []string{"__name__"}
			if len(c.want) > 0 {
				wantLabels = append(wantLabels, "job")
			}
			if got := stringData(t, getMetaData(t, query("/api/v1/labels", true))); !setEqual(got, wantLabels) {
				t.Errorf("/labels with match[] = %v, want %v", got, wantLabels)
			}
		})
	}
}
