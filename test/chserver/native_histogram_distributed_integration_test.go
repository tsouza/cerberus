//go:build integration

package chserver

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/test/perf/smoke"
)

// legacyAnalyzerDistributedImage is the build the legacy-analyzer
// Distributed probe runs on: a line below
// chopt.NativeHistogramAnalyzerFasterSince, so the engine stamps
// enable_analyzer=0 on a native-histogram plan there.
const legacyAnalyzerDistributedImage = "clickhouse/clickhouse-server:26.6.1.1193-alpine"

// The two-shard topology: one server, two local databases standing in for
// the two shards' data. Each replica names its shard's database as its
// default_database, and the Distributed tables pass an empty database, so a
// shard's subquery reads that shard's database alone.
const (
	distributedCluster = "two_local_shards"
	distributedShardA  = "shard_a"
	distributedShardB  = "shard_b"
	distributedDB      = "dist"
)

// Probe shape: distributedSeries series of distributedMetric, seeded through
// distributedEvalTime and split between the shards by pod.
const (
	distributedMetric     = "distributed_probe_latency_exp_hist"
	distributedSeries     = 20
	distributedSeedSpan   = 30 * time.Minute
	distributedRateWindow = "15m"
	distributedPhi        = "0.9"
)

// distributedEvalTime is the instant the probe evaluates at; the seed ends there.
var distributedEvalTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// distributedMaxRelativeError bounds how far the Distributed answer may sit
// from the single-table one. The shards return partial sums the initiator
// adds in a different order than one table's scan does, so the two answers
// may differ in the last bits of a float64; a wrong pushdown moves the
// quantile by whole buckets.
const distributedMaxRelativeError = 1e-9

// minShardSubqueries is the number of per-shard subqueries a fan-out over the
// two-shard cluster dispatches at the least: one per shard.
const minShardSubqueries = 2

// twoLocalShardsConfig defines distributedCluster in the server config: two
// shards, both this server, each reading its own database. The replicas carry
// the probe's credentials, because the default profile below sends every
// shard's subquery over the network as a real cluster does.
func twoLocalShardsConfig() testcontainers.ContainerCustomizer {
	replica := func(db string) string {
		return "<shard><replica><host>127.0.0.1</host><port>9000</port>" +
			"<user>" + adminUser + "</user><password>" + adminPassword + "</password>" +
			"<default_database>" + db + "</default_database></replica></shard>"
	}
	config := "<clickhouse><remote_servers><" + distributedCluster + ">" +
		replica(distributedShardA) + replica(distributedShardB) +
		"</" + distributedCluster + "></remote_servers></clickhouse>"
	profile := "<clickhouse><profiles><default><prefer_localhost_replica>0</prefer_localhost_replica></default></profiles></clickhouse>"
	return testcontainers.WithFiles(
		testcontainers.ContainerFile{
			Reader:            strings.NewReader(config),
			ContainerFilePath: "/etc/clickhouse-server/config.d/two-local-shards.xml",
			FileMode:          configFileMode,
		},
		testcontainers.ContainerFile{
			Reader:            strings.NewReader(profile),
			ContainerFilePath: "/etc/clickhouse-server/users.d/remote-shard-reads.xml",
			FileMode:          configFileMode,
		},
	)
}

// configFileMode is the permission bits of a configuration file copied into
// the container: readable by the server's own user.
const configFileMode = 0o644

// TestNativeHistogram_LegacyAnalyzerOverDistributed runs the native-histogram
// quantile (histogram_quantile over sum(rate())) through the production
// Prometheus handler against Distributed tables over two shards, on a build
// below 26.8, where the engine runs that plan under the legacy analyzer. The
// statement nests the merge and window fold in derived tables (FROM (SELECT
// …)) over a single read of the Distributed table, so the outer TimeUnix
// predicate is pushed down through those levels into each shard's query
// under enable_analyzer=0 — the class of rewrite ClickHouse#29332 got wrong
// over a Distributed table. It requires that system.query_log
// shows the query ran with enable_analyzer=0 and fanned out to both shards,
// and that its answer equals the same query's over the unsharded table.
func TestNativeHistogram_LegacyAnalyzerOverDistributed(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, legacyAnalyzerDistributedImage, twoLocalShardsConfig())
	if s.version.AtLeast(chopt.NativeHistogramAnalyzerFasterSince) {
		t.Fatalf("premise: %s runs %s, not below %s; the probe needs a build that takes the legacy analyzer stamp",
			s.image, s.version, chopt.NativeHistogramAnalyzerFasterSince)
	}

	// The unsharded reference: every series in the default database.
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}
	if _, err := smoke.SeedNativeHistogramAtScale(ctx, s.admin.Conn(), distributedMetric, distributedSeries,
		distributedEvalTime.Add(-distributedSeedSpan), distributedEvalTime); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The shards: the same tables in each shard database, the reference's
	// rows split between them by pod, and a Distributed table over both under
	// every metrics table name.
	metrics := schema.DefaultOTelMetrics()
	expTable := metrics.ExpHistogramTable
	for _, db := range []string{distributedShardA, distributedShardB} {
		if err := ddl.ApplyWithConfig(ctx, s.admin.Conn(), ddl.Config{Database: db}, []ddl.Signal{ddl.Metrics}); err != nil {
			t.Fatalf("apply DDL in %s: %v", db, err)
		}
	}
	for i, db := range []string{distributedShardA, distributedShardB} {
		s.exec(ctx, t, "INSERT INTO "+db+"."+expTable+" SELECT * FROM "+serverDB+"."+expTable+
			" WHERE MetricName = ? AND cityHash64(Attributes['pod']) % 2 = ?", distributedMetric, i)
		var rows uint64
		if err := s.admin.Conn().QueryRow(ctx, "SELECT count() FROM "+db+"."+expTable).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", db, err)
		}
		if rows == 0 {
			t.Fatalf("premise: shard %s holds no rows; the split must give both shards data", db)
		}
	}
	s.exec(ctx, t, "CREATE DATABASE "+distributedDB)
	for _, table := range []string{metrics.GaugeTable, metrics.SumTable, metrics.HistogramTable, expTable, metrics.SummaryTable} {
		s.exec(ctx, t, "CREATE TABLE "+distributedDB+"."+table+" AS "+distributedShardA+"."+table+
			" ENGINE = Distributed("+distributedCluster+", '', "+table+")")
	}

	set := chopttest.ResolveEnabledSet(ctx, t, s.admin, chopt.SelectionAuto)
	rules := choptwire.SettingsRules(set, metrics, schema.DefaultOTelTraces(), schema.DefaultOTelLogs())

	for _, q := range distributedQueries() {
		t.Run(q.name, func(t *testing.T) {
			reference := distributedInstant(ctx, t, s, rules, serverDB, q.name+"-reference", q.promql)
			sharded := distributedInstant(ctx, t, s, rules, distributedDB, q.name+"-distributed", q.promql)

			chopttest.AssertQuerySettingStamped(ctx, t, s.admin.Conn(), sharded.queryID, "enable_analyzer", "0")
			var shardQueries uint64
			if err := s.admin.Conn().QueryRow(ctx,
				"SELECT count() FROM system.query_log WHERE type = 'QueryFinish' AND NOT is_initial_query AND initial_query_id = ?",
				sharded.queryID).Scan(&shardQueries); err != nil {
				t.Fatalf("read shard subqueries of %s: %v", sharded.queryID, err)
			}
			if shardQueries < minShardSubqueries {
				t.Fatalf("query_id %s dispatched %d shard subqueries; want at least %d, one per shard", sharded.queryID, shardQueries, minShardSubqueries)
			}
			t.Logf("unsharded %v, Distributed %v, %d shard subqueries", reference.value, sharded.value, shardQueries)
			if diff := math.Abs(sharded.value-reference.value) / math.Abs(reference.value); diff > distributedMaxRelativeError {
				t.Errorf("Distributed answer %v differs from the unsharded answer %v (relative %g)", sharded.value, reference.value, diff)
			}
		})
	}
}

// distributedQuery is one probe query over the seeded native histogram.
type distributedQuery struct {
	name   string
	promql string
}

// distributedQueries are the probe queries. The quantile is the datashard e2e
// lane's shape 1. Every seeded series has the same bucket shape, so the
// quantile is the same over any subset of them; histogram_count over the same
// merged rate() adds up every series, so it also fails when a shard's rows
// are lost or counted twice.
func distributedQueries() []distributedQuery {
	merged := fmt.Sprintf("sum(rate(%s[%s]))", distributedMetric, distributedRateWindow)
	return []distributedQuery{
		{"histogram_quantile", fmt.Sprintf("histogram_quantile(%s, %s)", distributedPhi, merged)},
		{"histogram_count", fmt.Sprintf("histogram_count(%s)", merged)},
	}
}

// distributedAnswer is one instant sample and the query id it ran under.
type distributedAnswer struct {
	value   float64
	queryID string
}

// distributedInstant serves promql as an instant query through a Prometheus
// handler reading database db and returns its single sample.
func distributedInstant(ctx context.Context, t *testing.T, s *server, rules engine.SettingsRules, db, label, promql string) distributedAnswer {
	t.Helper()
	client := s.client(t, adminUser, adminPassword, chclient.Config{Database: db})
	handler := prom.New(client, schema.DefaultOTelMetrics(), nil)
	handler.Engine.SetSettings(rules)
	mux := http.NewServeMux()
	handler.Mount(mux)

	params := url.Values{}
	params.Set("query", promql)
	params.Set("time", strconv.FormatInt(distributedEvalTime.Unix(), 10))
	qid := fmt.Sprintf("analyzer-distributed-%s-%d", label, time.Now().UnixNano())
	var resp struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	serveJSON(chclient.WithQueryID(ctx, qid), t, mux, "/api/v1/query?"+params.Encode(), &resp)
	if len(resp.Data.Result) != 1 || len(resp.Data.Result[0].Value) != 2 {
		t.Fatalf("%s: want one sample, got %+v", label, resp.Data.Result)
	}
	raw, ok := resp.Data.Result[0].Value[1].(string)
	if !ok {
		t.Fatalf("%s: sample value %v is not a string", label, resp.Data.Result[0].Value[1])
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || v == 0 {
		t.Fatalf("%s: sample value %q; want a finite non-zero value", label, raw)
	}
	return distributedAnswer{value: v, queryID: qid}
}
