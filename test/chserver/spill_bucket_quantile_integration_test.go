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
)

// bucketQuantileSpillPromQL reassembles the probe's classic histogram from
// its bucket series after arithmetic on them. The arithmetic keeps the
// buckets float series, so histogram_quantile groups every (series without
// `le`, anchor) bucket set by the label map with `le` filtered out — a
// mapFilter — and that GROUP BY spills at the probe's size.
const bucketQuantileSpillPromQL = `histogram_quantile(0.9, ` + bucketSpillMetric + `_bucket * 2)`

// bucketQuantileSpillAnswer is every series' 0.9-quantile at every anchor:
// the doubled cumulative counts (2, 4, 6, 8) rank 0.9 × 8 = 7.2 into the
// +Inf bucket, and histogram_quantile answers the highest finite bound, 1.
const bucketQuantileSpillAnswer = "1"

// TestSpill_BucketQuantileGroupOnTheFloor drives a range histogram_quantile
// over arithmetic on a classic histogram's bucket series whose GROUP BY over
// the bucket-free label map spills to disk, through the production
// Prometheus handler, on the floor build, on a client whose per-query memory
// cap makes the engine's own spill threshold fire and under the server
// strategies the build's auto optimizations resolve, with the stale-marker
// layout an established Flags column turns on. It requires every series'
// quantile at every anchor, and that the aggregation did spill, so a pass is
// not a query that stayed in memory. On a build whose spilled read-back
// rejects the grouped label map's type, the query answers 502.
func TestSpill_BucketQuantileGroupOnTheFloor(t *testing.T) {
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
	metrics := schema.DefaultOTelMetrics()
	metrics.FlagsColumnProbed = true
	handler := prom.New(client, metrics, nil)
	handler.Strategies = cell
	mux := http.NewServeMux()
	handler.Mount(mux)

	qid := fmt.Sprintf("bucket-quantile-spill-probe-%d", time.Now().UnixNano())
	end := bucketSpillStart.Add(bucketSpillSamples * bucketSpillSampleSecs * time.Second)
	params := url.Values{}
	params.Set("query", bucketQuantileSpillPromQL)
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
	serveJSON(chclient.WithQueryID(ctx, qid), t, mux, "/api/v1/query_range?"+params.Encode(), &resp)
	if len(resp.Data.Result) != bucketSpillSeries {
		t.Fatalf("%s on %s: %d series, want %d", bucketQuantileSpillPromQL, s.version, len(resp.Data.Result), bucketSpillSeries)
	}
	for _, r := range resp.Data.Result {
		if le, ok := r.Metric["le"]; ok {
			t.Fatalf("%s on %s: series %v keeps le %q", bucketQuantileSpillPromQL, s.version, r.Metric, le)
		}
		if len(r.Values) != bucketSpillPoints {
			t.Fatalf("%s on %s: series %v has %d points, want %d", bucketQuantileSpillPromQL, s.version, r.Metric, len(r.Values), bucketSpillPoints)
		}
		for _, v := range r.Values {
			if len(v) != promSampleFields || v[1] != bucketQuantileSpillAnswer {
				t.Fatalf("%s on %s: series %v point %v, want the quantile %s",
					bucketQuantileSpillPromQL, s.version, r.Metric, v, bucketQuantileSpillAnswer)
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
			bucketQuantileSpillPromQL, s.version, spillKeyAggregationSpills)
	}
}
