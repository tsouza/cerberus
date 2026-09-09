//go:build chdb

// Behavioural coverage for /index/volume's `aggregateBy` parameter
// (cerberus issue #3224). Upstream's two values produce two genuinely
// different response SHAPES, not two spellings of one:
//
//   - `series` (and the default) keys the result by the label SET — one
//     row per distinct series.
//   - `labels` keys it by the bare label NAME, summing the volume across
//     every value that label takes, and reports each row's metric as
//     `labels.FromStrings(name, "")` — a single label whose name is the
//     payload and whose value is empty
//     (pkg/ingester/instance.go:886-903, queryrange/volume.go:158-176).
//
// Cerberus emitted the series shape for both, so `aggregateBy=labels`
// answered one row per label SET where upstream answers one row per label
// NAME, with per-row volumes to match.
//
// Every expected number below comes from upstream's formula applied to
// the seed by hand — `labelVolumes[l.Name] += size` for every label the
// stream carries — never from cerberus's own output. These run against
// chDB rather than a stubQuerier because the whole fix lives in how
// ClickHouse explodes and re-aggregates the rows; a canned-row stub would
// assert nothing about it.

package loki_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
)

// volumeShapeSeed is the seed every test in this file shares: three log
// lines across two `service_name` values, one of which also carries a
// `pod` label. Bodies are deliberately distinct lengths so no two
// expected volumes can be confused for one another.
//
//	{service_name=a, env=prod}         10 bytes
//	{service_name=b, env=prod}          5 bytes
//	{service_name=a, env=prod, pod=p1}  2 bytes
//
// Upstream's `labels` aggregation over that seed (every label of a
// stream is charged the stream's whole size):
//
//	env          = 10 + 5 + 2 = 17
//	service_name = 10 + 5 + 2 = 17
//	pod          =           2 =  2
//
// Its `series` aggregation over the same seed keeps the three label sets
// apart at 10, 5 and 2. The two shapes therefore disagree on the metric
// of every row and on two of the three volumes, so neither can be
// mistaken for the other.
//
// It reuses map_key_order_chdb_test.go's seeding harness (same package,
// same build tag, same three-column otel_logs projection) rather than
// standing up a second one.
func volumeShapeSeed() []keyOrderSeedRow {
	return []keyOrderSeedRow{
		{body: "0123456789", mapSQL: "map('service_name','a','env','prod')"},
		{body: "01234", mapSQL: "map('service_name','b','env','prod')"},
		{body: "01", mapSQL: "map('service_name','a','env','prod','pod','p1')"},
	}
}

// volumeSamples issues one /index/volume request and decodes the
// Prometheus-vector envelope into its samples.
func volumeSamples(t *testing.T, srvURL, query string) []loki.VectorSample {
	t.Helper()
	start, end := keyOrderWindow()
	var parsed struct {
		Data loki.QueryData `json:"data"`
	}
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/volume?query=%%7Benv%%3D%%22prod%%22%%7D&start=%d&end=%d&%s`,
		srvURL, start, end, query,
	), &parsed)

	raw, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var samples []loki.VectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	return samples
}

// labelNameVolumes folds the samples of an `aggregateBy=labels` response
// into name → bytes, failing the test on any sample that is not the
// single-label, empty-value metric that mode is defined to produce. That
// failure is the shape half of the assertion: a series-shaped row carries
// a populated multi-label metric and lands here, not in a value mismatch.
func labelNameVolumes(t *testing.T, samples []loki.VectorSample) map[string]uint64 {
	t.Helper()
	out := make(map[string]uint64, len(samples))
	for _, s := range samples {
		if len(s.Metric) != 1 {
			t.Fatalf("aggregateBy=labels must report one bare label NAME per sample, "+
				"got a %d-label metric %+v", len(s.Metric), s.Metric)
		}
		for name, value := range s.Metric {
			if value != "" {
				t.Fatalf("aggregateBy=labels must leave the value slot empty "+
					"(upstream labels.FromStrings(name, \"\")); got %q=%q", name, value)
			}
			raw, ok := s.Value[1].(string)
			if !ok {
				t.Fatalf("volume must be a string-encoded count; got %T (%v)", s.Value[1], s.Value[1])
			}
			n, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				t.Fatalf("volume %q for %q: %v", raw, name, err)
			}
			if _, dup := out[name]; dup {
				t.Fatalf("label %q reported twice; aggregateBy=labels sums a label's "+
					"values into ONE row", name)
			}
			out[name] = n
		}
	}
	return out
}

// TestIndexVolume_ChDB_AggregateByLabels pins the per-label-name shape.
//
// Without the fix the endpoint answers the three seeded label SETS
// ({service_name=a,env=prod}=10, {service_name=b,env=prod}=5,
// {service_name=a,env=prod,pod=p1}=2), so every assertion below fires:
// the metrics carry two or three labels with populated values, and no
// row reports 17.
func TestIndexVolume_ChDB_AggregateByLabels(t *testing.T) {
	srv, _ := seedKeyOrderServer(t, volumeShapeSeed())

	got := labelNameVolumes(t, volumeSamples(t, srv.URL, "aggregateBy=labels"))
	want := map[string]uint64{
		"env":          17,
		"service_name": 17,
		"pod":          2,
	}
	if len(got) != len(want) {
		t.Fatalf("aggregateBy=labels must report one row per distinct label NAME "+
			"(%d here); got %d rows: %v", len(want), len(got), got)
	}
	for name, bytes := range want {
		if got[name] != bytes {
			t.Errorf("label %q volume: got %d want %d (upstream charges every label of a "+
				"stream the stream's whole size); all=%v", name, got[name], bytes, got)
		}
	}
}

// TestIndexVolume_ChDB_AggregateByLabelsTargetLabels covers the
// `targetLabels` restriction of the same shape. Upstream keeps only the
// requested names and still sums across their values, so one row of 17 —
// where the unfixed series shape answers TWO rows ({service_name=a}=12
// and {service_name=b}=5), splitting the very sum this mode exists to
// produce.
func TestIndexVolume_ChDB_AggregateByLabelsTargetLabels(t *testing.T) {
	srv, _ := seedKeyOrderServer(t, volumeShapeSeed())

	got := labelNameVolumes(t, volumeSamples(t, srv.URL,
		"aggregateBy=labels&targetLabels=service_name"))
	want := map[string]uint64{"service_name": 17}
	if len(got) != len(want) {
		t.Fatalf("targetLabels=service_name must collapse to ONE service_name row; "+
			"got %d: %v", len(got), got)
	}
	if got["service_name"] != want["service_name"] {
		t.Errorf("service_name volume: got %d want %d; all=%v",
			got["service_name"], want["service_name"], got)
	}
}

// TestIndexVolume_ChDB_AggregateBySeriesUnchanged is the other half of
// the contract: the default and the explicit `series` value must keep
// answering the label-SET shape over the same seed. Routing "labels" to
// its own SQL must not disturb the branch it was previously sharing.
func TestIndexVolume_ChDB_AggregateBySeriesUnchanged(t *testing.T) {
	for _, mode := range []string{"", "aggregateBy=series"} {
		t.Run("mode="+mode, func(t *testing.T) {
			srv, _ := seedKeyOrderServer(t, volumeShapeSeed())

			samples := volumeSamples(t, srv.URL, mode)
			// The three seeded label sets, keyed by their service_name +
			// pod identity, with the byte volume of each.
			want := map[string]string{
				"a/":   "10",
				"b/":   "5",
				"a/p1": "2",
			}
			got := make(map[string]string, len(samples))
			for _, s := range samples {
				if s.Metric["env"] != "prod" {
					t.Fatalf("series shape must keep the full label set with its VALUES; got %+v", s.Metric)
				}
				got[s.Metric["service_name"]+"/"+s.Metric["pod"]] = fmt.Sprint(s.Value[1])
			}
			if len(got) != len(want) {
				t.Fatalf("series shape must report one row per label SET (%d here); got %d: %v",
					len(want), len(got), got)
			}
			for key, bytes := range want {
				if got[key] != bytes {
					t.Errorf("series %q volume: got %q want %q; all=%v", key, got[key], bytes, got)
				}
			}
		})
	}
}
