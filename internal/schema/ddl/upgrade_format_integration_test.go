//go:build integration

package ddl_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/network"
	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// The on-disk format contract for ClickHouse upgrades (docs/helm-clickhouse.md
// § "On-disk format across ClickHouse upgrades") proved on real servers. The
// three images are the three reader generations the contract names.
const (
	// textIndexRollbackFloorImage is the bundled chart's server line: the
	// oldest server a bundled rollout rolls back to. It reads every
	// text-index format but not the packed skip-index archive 26.8 writes.
	textIndexRollbackFloorImage = "clickhouse/clickhouse-server:26.6-alpine"
	// textIndexUpgradeTargetImage writes v2_with_positions text indexes and
	// packs small skip indices into skp_idx.packed by default.
	textIndexUpgradeTargetImage = "clickhouse/clickhouse-server:26.8-alpine"
	// textIndexV0ReaderImage is the newest server line that reads only
	// v0_initial text-index parts.
	textIndexV0ReaderImage = "clickhouse/clickhouse-server:26.5-alpine"

	textIndexDatabase    = "otel"
	textIndexZooPath     = "/clickhouse/databases/otel"
	textIndexLegacyTable = "otel_logs_legacy"
	textIndexService     = "textidx"
	textIndexToken       = "timeout"
	// textIndexMatchEvery: one row in this many carries the token.
	textIndexMatchEvery = 10
	textIndexBatchRows  = 100
	// textIndexCreateIndex / textIndexUpgradeIndex are the index names
	// renderLogsTable and renderAddBodyTextIndex install.
	textIndexCreateIndex  = "idx_lower_body"
	textIndexUpgradeIndex = "idx_body_text"
	textIndexV0Pin        = "v0_initial"
	// chartValues holds the chart's MergeTree settings pass-through, whose
	// default carries the rollback pin every 26.8 replica runs with.
	chartValues = "../../../deploy/helm/cerberus/values.yaml"

	textIndexStartTimeout = 5 * time.Minute
	textIndexStopTimeout  = time.Minute
	// textIndexReadTimeout bounds a statement that waits on both replicas
	// (mutations_sync / alter_sync = 2): a replica that cannot fetch its
	// peer's part never finishes, and the test fails here instead of hanging.
	textIndexReadTimeout = 2 * time.Minute
	textIndexQueryWindow = time.Hour
	textIndexLineLimit   = 1000
	chDataDir            = "/var/lib/clickhouse"
	unsupportedFormatErr = "Unsupported version of sparse index"
	unsupportedPackedErr = "Unknown format (1) of packed data"
)

// upgradeFormatTables are the cerberus tables the mixed-version test writes,
// merges and reads back: both logs shapes plus a trace and a metric table, each
// carrying the minmax / bloom_filter / tokenbf skip indices 26.8 packs.
var upgradeFormatTables = []string{"otel_logs", textIndexLegacyTable, "otel_traces", "otel_metrics_gauge"}

// textIndexLogQL are the LogQL line filters whose answers must not depend on
// which server wrote the part: a substring filter (served through the text
// index) and a regex filter (a plain row scan).
var textIndexLogQL = []string{
	`{service_name="` + textIndexService + `"} |= "Timeout"`,
	`{service_name="` + textIndexService + `"} |~ "Time.ut"`,
}

// textIndexKeeperNode runs the embedded Keeper the mixed-version cluster
// coordinates through, and fetches every merged part instead of merging, so
// the parts it serves are the ones its newer peer wrote.
const textIndexKeeperNode = `<clickhouse>
    <keeper_server>
        <tcp_port>9181</tcp_port>
        <server_id>1</server_id>
        <log_storage_path>/var/lib/clickhouse/coordination/log</log_storage_path>
        <snapshot_storage_path>/var/lib/clickhouse/coordination/snapshots</snapshot_storage_path>
        <coordination_settings>
            <operation_timeout_ms>60000</operation_timeout_ms>
            <session_timeout_ms>30000</session_timeout_ms>
        </coordination_settings>
        <raft_configuration>
            <server><id>1</id><hostname>localhost</hostname><port>9234</port></server>
        </raft_configuration>
    </keeper_server>
    <zookeeper><node><host>localhost</host><port>9181</port></node></zookeeper>
    <macros><shard>01</shard><replica>old</replica></macros>
    <interserver_http_host>old</interserver_http_host>
    <merge_tree><always_fetch_merged_part>1</always_fetch_merged_part></merge_tree>
</clickhouse>`

// textIndexPeerNode is the upgraded replica; %s is its <merge_tree> block.
const textIndexPeerNode = `<clickhouse>
    <zookeeper><node><host>old</host><port>9181</port></node></zookeeper>
    <macros><shard>01</shard><replica>new</replica></macros>
    <interserver_http_host>new</interserver_http_host>
    %s
</clickhouse>`

type textIndexNode struct {
	label     string
	container *tcclickhouse.ClickHouseContainer
	conn      driver.Conn
	client    *chclient.Client
}

// TestUpgradeFormat_MixedVersionReplicas runs the bundled chart's supported
// rollout — a 26.6 replica beside a 26.8 replica carrying the chart's default
// MergeTree settings — for a fresh text-index logs schema, a legacy tokenbf
// logs schema upgraded in place, and the trace and metric tables. The 26.6
// replica must fetch and read every part the 26.8 replica merged, both must
// give identical LogQL answers, and the 26.8 replica must survive a restart
// and a rollback to 26.6 on its own disk with no part detached.
func TestUpgradeFormat_MixedVersionReplicas(t *testing.T) {
	ctx := context.Background()
	nw := textIndexNetwork(ctx, t)
	newVolume := textIndexVolume(ctx, t)
	peerConfig := fmt.Sprintf(textIndexPeerNode, chartMergeTreeSettings(t))

	old := startTextIndexNode(ctx, t, nw, textIndexRollbackFloorImage, "old", "", textIndexKeeperNode)
	peer := startTextIndexNode(ctx, t, nw, textIndexUpgradeTargetImage, "new", newVolume, peerConfig)

	fresh := ddl.Config{
		Database:         textIndexDatabase,
		DatabaseEngine:   ddl.DatabaseEngine{Replicated: true, ReplicatedZooPath: textIndexZooPath},
		TextIndexEnabled: true,
	}
	legacy := fresh
	legacy.Tables = ddl.Tables{Logs: textIndexLegacyTable}
	legacy.TextIndexEnabled = false

	// Fresh install on the pre-upgrade server; the upgraded replica attaches.
	for _, n := range []*textIndexNode{old, peer} {
		applySchema(ctx, t, n, fresh, ddl.All)
		applySchema(ctx, t, n, legacy, []ddl.Signal{ddl.Logs})
	}
	syncDatabase(ctx, t, old, peer)

	// Legacy tokenbf parts from both server versions, then the in-place
	// upgrade to the text index, run from the upgraded replica.
	insertLogs(ctx, t, old, textIndexLegacyTable, 0)
	insertLogs(ctx, t, peer, textIndexLegacyTable, 1)
	legacy.TextIndexEnabled = true
	applySchema(ctx, t, peer, legacy, []ddl.Signal{ddl.Logs})
	syncDatabase(ctx, t, old, peer)
	execOn(ctx, t, peer, "ALTER TABLE "+textIndexDatabase+"."+textIndexLegacyTable+" MATERIALIZE INDEX "+textIndexUpgradeIndex,
		clickhouse.Settings{"mutations_sync": 2})

	const priorBatches = 2
	const batches = 3
	for i := range batches {
		n := peer
		if i == 1 {
			n = old
		}
		insertLogs(ctx, t, n, "otel_logs", i)
		insertLogs(ctx, t, n, textIndexLegacyTable, priorBatches+i)
		insertSpans(ctx, t, n, i)
		insertGauges(ctx, t, n, i)
	}
	for _, table := range upgradeFormatTables {
		execOn(ctx, t, peer, "OPTIMIZE TABLE "+textIndexDatabase+"."+table+" FINAL", clickhouse.Settings{"alter_sync": 2})
	}
	syncReplicas(ctx, t, old, peer)
	for _, table := range upgradeFormatTables {
		assertFetchedMergedParts(ctx, t, old, table)
	}

	wantFresh := batches * textIndexBatchRows / textIndexMatchEvery
	wantLegacy := (priorBatches + batches) * textIndexBatchRows / textIndexMatchEvery
	baseline := map[string][]string{}
	check := func(stage string, nodes ...*textIndexNode) {
		t.Helper()
		for _, n := range nodes {
			assertNoDetachedParts(ctx, t, n)
			assertIndexReadable(ctx, t, n, "otel_logs", textIndexCreateIndex, wantFresh)
			assertIndexReadable(ctx, t, n, textIndexLegacyTable, textIndexUpgradeIndex, wantLegacy)
			assertSkipIndexesReadable(ctx, t, n)
			for _, table := range []string{"otel_logs", textIndexLegacyTable} {
				want := wantFresh
				if table == textIndexLegacyTable {
					want = wantLegacy
				}
				for _, q := range textIndexLogQL {
					lines := logQLLines(t, n, table, q)
					if len(lines) != want {
						t.Fatalf("%s: %s on %s: %s returned %d lines, want %d", stage, n.label, table, q, len(lines), want)
					}
					key := table + " " + q
					if prev, ok := baseline[key]; ok && strings.Join(prev, "\n") != strings.Join(lines, "\n") {
						t.Fatalf("%s: %s on %s: %s answered differently from the first replica", stage, n.label, table, q)
					}
					baseline[key] = lines
				}
			}
		}
	}
	check("mixed 26.6/26.8", old, peer)

	peer = restartTextIndexNode(ctx, t, nw, peer, textIndexUpgradeTargetImage, "new", newVolume, peerConfig)
	syncReplicas(ctx, t, peer)
	check("26.8 replica restarted", peer)

	// A rollback keeps the chart's settings: 26.6 knows the pin.
	peer = restartTextIndexNode(ctx, t, nw, peer, textIndexRollbackFloorImage, "new", newVolume, peerConfig)
	syncReplicas(ctx, t, peer)
	check("26.8 replica rolled back to 26.6", peer)
}

// TestUpgradeFormat_PackedSkipIndexNeedsThePin is why the chart's default
// carries packed_skip_index_max_bytes: 0. Without it a 26.8 merge packs the
// small skip indices into an archive 26.6 cannot read, and says so; with it,
// or after the documented rewrite under it, 26.6 reads the same part.
func TestUpgradeFormat_PackedSkipIndexNeedsThePin(t *testing.T) {
	ctx := context.Background()
	nw := textIndexNetwork(ctx, t)
	volume := textIndexVolume(ctx, t)
	cfg := ddl.Config{Database: textIndexDatabase, TextIndexEnabled: true}

	n := startTextIndexNode(ctx, t, nw, textIndexUpgradeTargetImage, "single", volume, "")
	applySchema(ctx, t, n, cfg, []ddl.Signal{ddl.Traces})
	for i := range 3 {
		insertSpans(ctx, t, n, i)
	}
	execOn(ctx, t, n, "OPTIMIZE TABLE "+textIndexDatabase+".otel_traces FINAL", nil)

	n = restartTextIndexNode(ctx, t, nw, n, textIndexRollbackFloorImage, "single", volume, "")
	if err := skipIndexProbe(ctx, n); err == nil || !strings.Contains(err.Error(), unsupportedPackedErr) {
		t.Fatalf("26.6 reading an unpinned 26.8 merge: err = %v, want %q", err, unsupportedPackedErr)
	}

	pinned := "<clickhouse>" + chartMergeTreeSettings(t) + "</clickhouse>"
	n = restartTextIndexNode(ctx, t, nw, n, textIndexUpgradeTargetImage, "single", volume, pinned)
	execOn(ctx, t, n, "OPTIMIZE TABLE "+textIndexDatabase+".otel_traces FINAL", nil)

	n = restartTextIndexNode(ctx, t, nw, n, textIndexRollbackFloorImage, "single", volume, pinned)
	if err := skipIndexProbe(ctx, n); err != nil {
		t.Fatalf("26.6 reading a 26.8 merge rewritten under the pin: %v", err)
	}
	assertNoDetachedParts(ctx, t, n)
}

// TestUpgradeFormat_TextIndexRollbackBelowReaderFloor proves the one unsupported
// rollback and its recovery: a 26.5 server cannot read the parts 26.8 writes
// by default and says so, and the documented procedure — pin v0_initial on the
// newer server, rewrite the index, roll back without the pin — makes the same
// data readable and the same LogQL answers come back.
func TestUpgradeFormat_TextIndexRollbackBelowReaderFloor(t *testing.T) {
	ctx := context.Background()
	nw := textIndexNetwork(ctx, t)
	volume := textIndexVolume(ctx, t)
	cfg := ddl.Config{Database: textIndexDatabase, TextIndexEnabled: true}
	want := 3 * textIndexBatchRows / textIndexMatchEvery

	n := startTextIndexNode(ctx, t, nw, textIndexUpgradeTargetImage, "single", volume, "")
	applySchema(ctx, t, n, cfg, []ddl.Signal{ddl.Logs})
	for i := range 3 {
		insertLogs(ctx, t, n, "otel_logs", i)
	}
	execOn(ctx, t, n, "OPTIMIZE TABLE "+textIndexDatabase+".otel_logs FINAL", nil)
	assertIndexReadable(ctx, t, n, "otel_logs", textIndexCreateIndex, want)

	n = restartTextIndexNode(ctx, t, nw, n, textIndexV0ReaderImage, "single", volume, "")
	err := indexProbe(ctx, n, "otel_logs", textIndexCreateIndex, nil)
	if err == nil || !strings.Contains(err.Error(), unsupportedFormatErr) {
		t.Fatalf("26.5 reading 26.8 default parts: err = %v, want %q", err, unsupportedFormatErr)
	}

	pin := "<clickhouse><merge_tree><text_index_serialization_version>" + textIndexV0Pin +
		"</text_index_serialization_version></merge_tree></clickhouse>"
	n = restartTextIndexNode(ctx, t, nw, n, textIndexUpgradeTargetImage, "single", volume, pin)
	execOn(ctx, t, n, "ALTER TABLE "+textIndexDatabase+".otel_logs MATERIALIZE INDEX "+textIndexCreateIndex,
		clickhouse.Settings{"mutations_sync": 1})

	n = restartTextIndexNode(ctx, t, nw, n, textIndexV0ReaderImage, "single", volume, "")
	assertIndexReadable(ctx, t, n, "otel_logs", textIndexCreateIndex, want)
	for _, q := range textIndexLogQL {
		if got := len(logQLLines(t, n, "otel_logs", q)); got != want {
			t.Fatalf("26.5 after the v0 rewrite: %s returned %d lines, want %d", q, got, want)
		}
	}
}

func textIndexNetwork(ctx context.Context, t *testing.T) *testcontainers.DockerNetwork {
	t.Helper()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })
	return nw
}

// textIndexVolume names a Docker volume that outlives the server containers
// mounting it, so a restart or a rollback reopens the same parts.
func textIndexVolume(ctx context.Context, t *testing.T) string {
	t.Helper()
	name := "cerberus-textidx-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		cli, err := testcontainers.NewDockerClientWithOpts(ctx)
		if err != nil {
			t.Errorf("docker client: %v", err)
			return
		}
		defer cli.Close()
		if _, err := cli.VolumeRemove(context.Background(), name, mobyclient.VolumeRemoveOptions{Force: true}); err != nil {
			t.Errorf("remove volume %s: %v", name, err)
		}
	})
	return name
}

func startTextIndexNode(ctx context.Context, t *testing.T, nw *testcontainers.DockerNetwork, image, alias, volume, configXML string) *textIndexNode {
	t.Helper()
	startCtx, cancel := context.WithTimeout(ctx, textIndexStartTimeout)
	defer cancel()
	label := alias + " " + image
	opts := []testcontainers.ContainerCustomizer{
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		network.WithNetwork([]string{alias}, nw),
	}
	if configXML != "" {
		path := filepath.Join(t.TempDir(), "node.xml")
		if err := os.WriteFile(path, []byte(configXML), 0o644); err != nil {
			t.Fatalf("%s: write config: %v", label, err)
		}
		opts = append(opts, tcclickhouse.WithConfigFile(path))
	}
	if volume != "" {
		opts = append(opts, testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.TypeVolume, Source: volume, Target: chDataDir})
		}))
	}
	c, err := tcclickhouse.Run(startCtx, image, opts...)
	if err != nil {
		t.Fatalf("%s: start: %v", label, err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(startCtx)
	if err != nil {
		t.Fatalf("%s: host: %v", label, err)
	}
	port, err := c.MappedPort(startCtx, "9000/tcp")
	if err != nil {
		t.Fatalf("%s: port: %v", label, err)
	}
	addr := host + ":" + port.Port()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        clickhouse.Auth{Database: "default", Username: "cerberus", Password: "cerberus"},
		ReadTimeout: textIndexReadTimeout,
	})
	if err != nil {
		t.Fatalf("%s: open: %v", label, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Ping(startCtx); err != nil {
		t.Fatalf("%s: ping: %v", label, err)
	}
	client, err := chclient.New(chclient.Config{Addr: addr, Database: textIndexDatabase, Username: "cerberus", Password: "cerberus"})
	if err != nil {
		t.Fatalf("%s: chclient: %v", label, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &textIndexNode{label: label, container: c, conn: conn, client: client}
}

// restartTextIndexNode stops n gracefully and brings image up in its place on
// the same volume and network alias.
func restartTextIndexNode(ctx context.Context, t *testing.T, nw *testcontainers.DockerNetwork, n *textIndexNode, image, alias, volume, configXML string) *textIndexNode {
	t.Helper()
	_ = n.client.Close()
	_ = n.conn.Close()
	timeout := textIndexStopTimeout
	if err := n.container.Stop(ctx, &timeout); err != nil {
		t.Fatalf("%s: stop: %v", n.label, err)
	}
	if err := n.container.Terminate(ctx); err != nil {
		t.Fatalf("%s: remove: %v", n.label, err)
	}
	return startTextIndexNode(ctx, t, nw, image, alias, volume, configXML)
}

func applySchema(ctx context.Context, t *testing.T, n *textIndexNode, cfg ddl.Config, signals []ddl.Signal) {
	t.Helper()
	if err := ddl.ApplyWithConfig(ctx, n.conn, cfg, signals); err != nil {
		t.Fatalf("%s: apply schema %+v: %v", n.label, cfg.Tables, err)
	}
}

// chartMergeTreeSettings renders the chart's default
// clickhouse.bundled.settings the way configmap-config.yaml does.
func chartMergeTreeSettings(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(chartValues)
	if err != nil {
		t.Fatalf("read %s: %v", chartValues, err)
	}
	var values struct {
		ClickHouse struct {
			Bundled struct {
				Settings map[string]any `yaml:"settings"`
			} `yaml:"bundled"`
		} `yaml:"clickhouse"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse %s: %v", chartValues, err)
	}
	if len(values.ClickHouse.Bundled.Settings) == 0 {
		t.Fatalf("%s: clickhouse.bundled.settings carries no default; the pinned rollout would be vacuous", chartValues)
	}
	keys := make([]string, 0, len(values.ClickHouse.Bundled.Settings))
	for k := range values.ClickHouse.Bundled.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("<merge_tree>")
	for _, k := range keys {
		fmt.Fprintf(&b, "<%s>%v</%s>", k, values.ClickHouse.Bundled.Settings[k], k)
	}
	b.WriteString("</merge_tree>")
	return b.String()
}

func execOn(ctx context.Context, t *testing.T, n *textIndexNode, stmt string, settings clickhouse.Settings) {
	t.Helper()
	if settings != nil {
		ctx = clickhouse.Context(ctx, clickhouse.WithSettings(settings))
	}
	if err := n.conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("%s: %s: %v", n.label, stmt, err)
	}
}

func syncDatabase(ctx context.Context, t *testing.T, nodes ...*textIndexNode) {
	t.Helper()
	for _, n := range nodes {
		execOn(ctx, t, n, "SYSTEM SYNC DATABASE REPLICA "+textIndexDatabase, nil)
	}
}

func syncReplicas(ctx context.Context, t *testing.T, nodes ...*textIndexNode) {
	t.Helper()
	for _, n := range nodes {
		for _, table := range upgradeFormatTables {
			execOn(ctx, t, n, "SYSTEM SYNC REPLICA "+textIndexDatabase+"."+table, nil)
		}
	}
}

// insertLogs writes one part of textIndexBatchRows log rows. batch keeps every
// part's timestamps and bodies distinct.
func insertLogs(ctx context.Context, t *testing.T, n *textIndexNode, table string, batch int) {
	t.Helper()
	base := time.Now().Add(-textIndexQueryWindow / 2).Add(time.Duration(batch*textIndexBatchRows) * time.Second)
	stmt := fmt.Sprintf(`INSERT INTO %s.%s (Timestamp, ServiceName, SeverityText, Body, ResourceAttributes)
SELECT toDateTime64(%d, 9) + toIntervalSecond(number), '%s', 'INFO',
       concat('request ', toString(%d + number), if(number %% %d = 0, ' failed: upstream Timeout', ' ok')),
       map('service.name', '%s')
FROM numbers(%d)`,
		textIndexDatabase, table, base.Unix(), textIndexService, batch*textIndexBatchRows, textIndexMatchEvery, textIndexService, textIndexBatchRows)
	execOn(ctx, t, n, stmt, nil)
}

// insertSpans writes one part of spans into otel_traces.
func insertSpans(ctx context.Context, t *testing.T, n *textIndexNode, batch int) {
	t.Helper()
	base := time.Now().Add(-textIndexQueryWindow / 2).Add(time.Duration(batch*textIndexBatchRows) * time.Second)
	stmt := fmt.Sprintf(`INSERT INTO %s.otel_traces (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration, ResourceAttributes, SpanAttributes)
SELECT toDateTime64(%d, 9) + toIntervalSecond(number), hex(%d + number), hex(number), '%s', concat('op-', toString(number %% %d)),
       number * 1000, map('service.name', '%s'), map('http.route', concat('/r', toString(number %% %d)))
FROM numbers(%d)`,
		textIndexDatabase, base.Unix(), batch*textIndexBatchRows, textIndexService, textIndexMatchEvery, textIndexService, textIndexMatchEvery, textIndexBatchRows)
	execOn(ctx, t, n, stmt, nil)
}

// insertGauges writes one part of gauge points into otel_metrics_gauge.
func insertGauges(ctx context.Context, t *testing.T, n *textIndexNode, batch int) {
	t.Helper()
	base := time.Now().Add(-textIndexQueryWindow / 2).Add(time.Duration(batch*textIndexBatchRows) * time.Second)
	stmt := fmt.Sprintf(`INSERT INTO %s.otel_metrics_gauge (ServiceName, MetricName, Attributes, ResourceAttributes, TimeUnix, Value)
SELECT '%s', concat('gauge_', toString(number %% %d)), map('shard', toString(number %% %d)), map('service.name', '%s'),
       toDateTime64(%d, 9) + toIntervalSecond(number), toFloat64(number)
FROM numbers(%d)`,
		textIndexDatabase, textIndexService, textIndexMatchEvery, textIndexMatchEvery, textIndexService, base.Unix(), textIndexBatchRows)
	execOn(ctx, t, n, stmt, nil)
}

// assertNoDetachedParts fails when the server set any part aside as broken —
// what a server does with a part whose format it cannot load.
func assertNoDetachedParts(ctx context.Context, t *testing.T, n *textIndexNode) {
	t.Helper()
	query, args := chsql.NewQuery().
		Select(chsql.Call("count")).
		From(chsql.Qual("system", "detached_parts")).
		Where(chsql.Eq(chsql.Col("database"), chsql.Lit(textIndexDatabase))).
		Build()
	var detached uint64
	if err := n.conn.QueryRow(ctx, query, args...).Scan(&detached); err != nil {
		t.Fatalf("%s: read detached parts: %v", n.label, err)
	}
	if detached != 0 {
		t.Fatalf("%s: %d parts detached", n.label, detached)
	}
}

// skipIndexProbe reads otel_traces through its TraceId bloom filter, forced,
// so the skip-index files of every part are opened.
func skipIndexProbe(ctx context.Context, n *textIndexNode) error {
	query, args := chsql.NewQuery().
		Select(chsql.Call("count")).
		From(chsql.Qual(textIndexDatabase, "otel_traces")).
		Where(chsql.Eq(chsql.Col("TraceId"), chsql.Lit("0"))).
		Build()
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"force_data_skipping_indices": "idx_trace_id"}))
	var got uint64
	return n.conn.QueryRow(ctx, query, args...).Scan(&got)
}

func assertSkipIndexesReadable(ctx context.Context, t *testing.T, n *textIndexNode) {
	t.Helper()
	if err := skipIndexProbe(ctx, n); err != nil {
		t.Fatalf("%s: otel_traces via idx_trace_id: %v", n.label, err)
	}
}

// assertFetchedMergedParts proves the older replica is serving parts its peer
// wrote: it never merges (always_fetch_merged_part), so every merged part it
// holds was fetched byte for byte.
func assertFetchedMergedParts(ctx context.Context, t *testing.T, n *textIndexNode, table string) {
	t.Helper()
	query, args := chsql.NewQuery().
		Select(chsql.Call("count"), chsql.Call("min", chsql.Col("level"))).
		From(chsql.Qual("system", "parts")).
		Where(chsql.And(
			chsql.Eq(chsql.Col("database"), chsql.Lit(textIndexDatabase)),
			chsql.Eq(chsql.Col("table"), chsql.Lit(table)),
			chsql.Col("active"),
		)).
		Build()
	var parts uint64
	var minLevel uint32
	if err := n.conn.QueryRow(ctx, query, args...).Scan(&parts, &minLevel); err != nil {
		t.Fatalf("%s: read parts of %s: %v", n.label, table, err)
	}
	if parts == 0 || minLevel == 0 {
		t.Fatalf("%s: %s has %d active parts with min level %d; want only merged parts fetched from the peer", n.label, table, parts, minLevel)
	}
}

// indexProbe counts token matches with the named text index forced, so a
// server that cannot read the index format fails instead of scanning rows.
func indexProbe(ctx context.Context, n *textIndexNode, table, index string, count *uint64) error {
	query, args := chsql.NewQuery().
		Select(chsql.Call("count")).
		From(chsql.Qual(textIndexDatabase, table)).
		Where(chsql.Call("hasToken", chsql.Call("lower", chsql.Col("Body")), chsql.Lit(textIndexToken))).
		Build()
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"force_data_skipping_indices": index}))
	var got uint64
	if err := n.conn.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		return err
	}
	if count != nil {
		*count = got
	}
	return nil
}

func assertIndexReadable(ctx context.Context, t *testing.T, n *textIndexNode, table, index string, want int) {
	t.Helper()
	var got uint64
	if err := indexProbe(ctx, n, table, index, &got); err != nil {
		t.Fatalf("%s: %s via %s: %v", n.label, table, index, err)
	}
	if got != uint64(want) {
		t.Fatalf("%s: %s via %s matched %d rows, want %d", n.label, table, index, got, want)
	}
}

// logQLLines runs q through cerberus's Loki head against n and returns the
// returned log lines, sorted.
func logQLLines(t *testing.T, n *textIndexNode, table, q string) []string {
	t.Helper()
	s := schema.DefaultOTelLogs()
	s.LogsTable = table
	h := loki.New(n.client, s, nil)
	h.TextIndexLineFilter = true
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	end := time.Now()
	params := url.Values{
		"query":     {q},
		"start":     {strconv.FormatInt(end.Add(-textIndexQueryWindow).UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {strconv.Itoa(textIndexLineLimit)},
		"direction": {"forward"},
	}
	resp, err := http.Get(srv.URL + "/loki/api/v1/query_range?" + params.Encode())
	if err != nil {
		t.Fatalf("%s: %s: %v", n.label, q, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: %s: read: %v", n.label, q, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s: HTTP %d: %s", n.label, q, resp.StatusCode, body)
	}
	var out struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: %s: decode: %v", n.label, q, err)
	}
	var lines []string
	for _, stream := range out.Data.Result {
		for _, v := range stream.Values {
			lines = append(lines, v[1])
		}
	}
	sort.Strings(lines)
	return lines
}
