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
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// bucketSpillFloorBuild is the build the bucket fan-out spill probe runs
// on: the supported floor, where mapConcat, like mapSort, returns a Map type
// unequal to the one its spilled blocks are read back as (auto resolves
// chopt.FeatureCanonicalMapTypeCast in).
const bucketSpillFloorBuild = "clickhouse/clickhouse-server:24.8.14.39-alpine"

// Bucket fan-out spill probe shape. Each series carries one histogram row a
// minute across the hour the range query reads, so the range lookback's
// GROUP BY (MetricName, Attributes, anchor) holds one entry per series per
// anchor; at bucketSpillSeries series that state is larger than half of
// bucketSpillQueryMemoryBytes, the threshold the engine stamps as
// max_bytes_before_external_group_by, so the aggregation spills, and the
// merge of the spilled parts fits under the cap.
const (
	bucketSpillMetric           = "spill_probe_duration"
	bucketSpillSeries           = 10_000
	bucketSpillQueryMemoryBytes = 512 << 20
	bucketSpillNamespaces       = 13
	bucketSpillSamples          = 60
	bucketSpillSampleSecs       = 60
	bucketSpillSampleOffset     = 1
	bucketSpillMatchedBound     = "0.5"
	bucketSpillMatchedCount     = "2"
	bucketSpillPoints           = bucketSpillSamples
)

// bucketSpillPromQL selects one bucket of the classic histogram. Its lowering
// fans each histogram row out into its (le, cumulative count) pairs, keeps
// the matched one, and merges `le` into Attributes with mapConcat; the range
// lookback then groups by that Attributes.
const bucketSpillPromQL = bucketSpillMetric + `_bucket{le="` + bucketSpillMatchedBound + `"}`

// bucketSpillStart is the start of the range the probe reads; the samples
// land bucketSpillSampleOffset seconds after each minute of the hour after it.
var bucketSpillStart = time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

// TestSpill_BucketFanoutAttributesOnTheFloor drives a range query over a
// classic-histogram bucket selection whose range-lookback GROUP BY spills to
// disk, through the production Prometheus handler, on the floor build, on a
// client whose per-query memory cap makes the engine's own spill threshold
// fire and under the server strategies the build's auto optimizations
// resolve. It requires that the query answers every series at every anchor
// with the matched bucket's cumulative count, and that it did spill, so a
// pass is not a query that stayed in memory.
func TestSpill_BucketFanoutAttributesOnTheFloor(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, bucketSpillFloorBuild)
	seedBucketSpillProbe(ctx, t, s)

	client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: bucketSpillQueryMemoryBytes})
	set, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Mode: chopt.Permissive}, s.version)
	if err != nil {
		t.Fatalf("resolve auto against %s: %v", s.version, err)
	}
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	choptwire.SetServerStrategies(cell, set)
	handler := prom.New(client, schema.DefaultOTelMetrics(), nil)
	handler.Strategies = cell
	mux := http.NewServeMux()
	handler.Mount(mux)

	qid := fmt.Sprintf("bucket-spill-probe-%d", time.Now().UnixNano())
	end := bucketSpillStart.Add(bucketSpillSamples * bucketSpillSampleSecs * time.Second)
	params := url.Values{}
	params.Set("query", bucketSpillPromQL)
	params.Set("start", strconv.FormatInt(bucketSpillStart.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))
	params.Set("step", strconv.Itoa(bucketSpillSampleSecs))
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	// serveJSON fails the test on any status but 200: on a build
	// whose spilled read-back rejects the grouped Attributes, that
	// is a 502.
	serveJSON(chclient.WithQueryID(ctx, qid), t, mux, "/api/v1/query_range?"+params.Encode(), &resp)
	if len(resp.Data.Result) != bucketSpillSeries {
		t.Fatalf("%s on %s: %d series, want %d", bucketSpillPromQL, s.version, len(resp.Data.Result), bucketSpillSeries)
	}
	for _, r := range resp.Data.Result {
		if r.Metric["le"] != bucketSpillMatchedBound {
			t.Fatalf("%s on %s: series %v carries le %q, want %q", bucketSpillPromQL, s.version, r.Metric, r.Metric["le"], bucketSpillMatchedBound)
		}
		if len(r.Values) != bucketSpillPoints {
			t.Fatalf("%s on %s: series %v has %d points, want %d", bucketSpillPromQL, s.version, r.Metric, len(r.Values), bucketSpillPoints)
		}
		for _, v := range r.Values {
			if len(v) != promSampleFields || v[1] != bucketSpillMatchedCount {
				t.Fatalf("%s on %s: series %v point %v, want the matched bucket's cumulative count %s",
					bucketSpillPromQL, s.version, r.Metric, v, bucketSpillMatchedCount)
			}
		}
	}

	s.flushLogs(ctx, t)
	var spilledParts uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT sum(ProfileEvents[?]) FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?",
		spillKeyAggregationSpills, qid).Scan(&spilledParts); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	if spilledParts == 0 {
		t.Fatalf("%s on %s did not spill its aggregation (%s = 0); the probe no longer exercises the spilled read-back",
			bucketSpillPromQL, s.version, spillKeyAggregationSpills)
	}
}

// seedBucketSpillProbe applies cerberus's metrics DDL and writes
// bucketSpillSamples histogram rows per series, one a minute across the hour
// after bucketSpillStart, over bounds (0.1, 0.5, 1) with one observation in
// each of the four buckets, so the cumulative count at le=0.5 is 2.
func seedBucketSpillProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_metrics_histogram (ServiceName, MetricName, Attributes, StartTimeUnix, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, AggregationTemporality)
SELECT
    'svc', '%s',
    map('pod', concat('pod-', toString(series)),
        'namespace', concat('ns', toString(series %% %d))),
    toDateTime64(%d, 9),
    toDateTime64(%d + sample * %d, 9),
    4, 1, [1, 1, 1, 1], [0.1, 0.5, 1], 2
FROM (SELECT number AS series FROM numbers(%d)) AS s
CROSS JOIN (SELECT number AS sample FROM numbers(%d)) AS k`,
		bucketSpillMetric, bucketSpillNamespaces,
		bucketSpillStart.Unix(), bucketSpillStart.Unix()+bucketSpillSampleOffset, bucketSpillSampleSecs,
		bucketSpillSeries, bucketSpillSamples))
}
