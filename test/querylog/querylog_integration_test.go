//go:build integration

package querylog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/testcontainers/testcontainers-go/network"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

const (
	distributedQuery = "SELECT toString(count()) FROM samples_all WHERE x % 7 = 0"
	shardQuery       = "SELECT toString(count()) FROM samples WHERE x % 11 = 0"
)

// repeatPolls is how many reconciliation passes a case runs where it asserts
// that a row is accounted once: a repeated read of an already-read row must
// not add an observation.
const repeatPolls = 3

// pageProbeLimit is the one-row page the pagination case walks the log with,
// and wholeReadLimit a page larger than everything that case dispatched.
const (
	pageProbeLimit = 1
	wholeReadLimit = 100
)

// upgradeRows is the table the pre-upgrade server holds.
const upgradeRows = 1000

// workload names the two stamped queries one case dispatches on shard A.
// Every case stamps its own shapes, so cases sharing a rig never read each
// other's rows.
type workload struct {
	// prefix is the case's own shape prefix; it starts with the "cerb:"
	// prefix the reader selects on.
	prefix string
	// captured is a Distributed read dispatched with actuals capture armed:
	// the packet path observes it, and its remote child runs on shard B.
	captured string
	// foreign is a shard-local read stamped without capture — a query another
	// cerberus process dispatched, which only the query log can show this one.
	foreign string
}

func newWorkload(name string) workload {
	prefix := "cerb:querylog;" + name + ";"
	return workload{prefix: prefix, captured: prefix + "captured", foreign: prefix + "foreign"}
}

// twoShardRig is two data shards behind samples_all.
type twoShardRig struct {
	a, b *node
}

func startTwoShardRig(ctx context.Context, t *testing.T, image string, configs ...string) twoShardRig {
	t.Helper()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })
	configs = append([]string{"cluster.xml"}, configs...)
	rig := twoShardRig{
		a: startNode(ctx, t, image, nodeOptions{configs: configs, network: nw, alias: aliasA}),
		b: startNode(ctx, t, image, nodeOptions{configs: configs, network: nw, alias: aliasB}),
	}
	rig.a.seedShard(ctx, t)
	rig.b.seedShard(ctx, t)
	return rig
}

// dispatch runs w's two stamped queries on shard A — the captured one armed on
// tracker — and flushes both shards' logs.
func (r twoShardRig) dispatch(ctx context.Context, t *testing.T, w workload, tracker *actuals.Tracker) {
	t.Helper()
	dispatch(ctx, t, r.a.admin, tracker, w.captured, distributedQuery)
	dispatch(ctx, t, r.a.admin, nil, w.foreign, shardQuery)
	r.a.flushLogs(ctx, t)
	r.b.flushLogs(ctx, t)
}

// initiatorRows is read_rows of shape's initiator row in n's own log.
func (n *node) initiatorRows(ctx context.Context, t *testing.T, shape string) uint64 {
	t.Helper()
	return n.uint64Of(ctx, t,
		"SELECT sum(read_rows) FROM system.query_log WHERE type = 'QueryFinish' AND is_initial_query = 1 AND log_comment = ?", shape)
}

// TestFloorBuild measures the supported floor, which has no union table.
func TestFloorBuild(t *testing.T) {
	ctx := t.Context()
	rig := startTwoShardRig(ctx, t, floorImage)
	t.Run("packet path covers remote shards", func(t *testing.T) { casePacketPathCoversRemoteShards(ctx, t, rig) })
	t.Run("remote child rows are not queries", func(t *testing.T) { caseRemoteChildRowsAreNotQueries(ctx, t, rig) })
	t.Run("local log misses after failover", func(t *testing.T) { caseLocalLogMissesAfterFailover(ctx, t, rig) })
	t.Run("absent union falls back to local", func(t *testing.T) { caseUnionAbsentFallsBackToLocal(ctx, t, rig) })
	t.Run("routed request is one observation in another process", func(t *testing.T) {
		caseRoutedRequestIsOneObservationInAnotherProcess(ctx, t, rig, rig.a.admin, false)
	})
}

// TestUnionBuildWithoutSection measures a union-capable build whose operator
// never configured create_union_system_log_tables.
func TestUnionBuildWithoutSection(t *testing.T) {
	ctx := t.Context()
	rig := startTwoShardRig(ctx, t, unionImage)
	t.Run("absent union falls back to local", func(t *testing.T) { caseUnionAbsentFallsBackToLocal(ctx, t, rig) })
}

// TestUnionBuild measures 26.8 with system.all_query_log over the cluster.
func TestUnionBuild(t *testing.T) {
	ctx := t.Context()
	rig := startTwoShardRig(ctx, t, unionImage, "union-cluster.xml")
	t.Run("packet path covers remote shards", func(t *testing.T) { casePacketPathCoversRemoteShards(ctx, t, rig) })
	t.Run("remote child rows are not queries", func(t *testing.T) { caseRemoteChildRowsAreNotQueries(ctx, t, rig) })
	t.Run("local log misses after failover", func(t *testing.T) { caseLocalLogMissesAfterFailover(ctx, t, rig) })
	t.Run("union finds observations after failover", func(t *testing.T) { caseUnionFindsObservationsAfterFailover(ctx, t, rig) })
	t.Run("union read pages stably", func(t *testing.T) { caseUnionReadPagesStably(ctx, t, rig) })
	t.Run("union needs no cluster-wide grant", func(t *testing.T) { caseUnionNeedsNoClusterWideGrant(ctx, t, rig) })
	t.Run("routed request is one observation in another process", func(t *testing.T) {
		caseRoutedRequestIsOneObservationInAnotherProcess(ctx, t, rig, failoverClient(t, rig.b), true)
	})
	t.Run("http dispatches are observed from the query log", func(t *testing.T) { caseHTTPDispatchesObservedFromQueryLog(ctx, t, rig) })
}

// casePacketPathCoversRemoteShards: the native packet path records one
// observation of a Distributed read whose read_rows is the whole query —
// both shards — on the dispatching connection, whichever servers ran the
// pieces.
func casePacketPathCoversRemoteShards(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("packet")
	tracker := actuals.NewTracker(actualsConfig())
	rig.dispatch(ctx, t, w, tracker)

	if n, rows := observations(tracker, w.captured); n != 1 || rows != 2*shardRows {
		t.Fatalf("packet path: %d observations of %v rows, want 1 of %d (both shards)", n, rows, 2*shardRows)
	}
}

// caseRemoteChildRowsAreNotQueries: a Distributed read leaves, in the other
// shard's local log, a child row with its own query_id, the propagated
// log_comment and half the rows. The reader never takes it for a query.
func caseRemoteChildRowsAreNotQueries(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("child")
	tracker := actuals.NewTracker(actualsConfig())
	rig.dispatch(ctx, t, w, tracker)

	childRows := rig.b.uint64Of(ctx, t,
		"SELECT sum(read_rows) FROM system.query_log WHERE type = 'QueryFinish' AND is_initial_query = 0 AND log_comment = ?",
		w.captured)
	if childRows != shardRows {
		t.Fatalf("shard B logged %d child read_rows for the stamped Distributed read, want %d — "+
			"without a stamped child row there is nothing for the initiator filter to exclude", childRows, shardRows)
	}

	// A second cerberus process reading shard B's own log: the child row is
	// the only row of this shape there, and it is not a query.
	other := actuals.NewTracker(actualsConfig())
	reconcile(ctx, t, rig.b.admin, other, false, repeatPolls)
	if n, _ := observations(other, w.captured); n != 0 {
		t.Fatalf("the reader recorded %d observations from shard B's remote child row, want 0", n)
	}
	// This process: the packet observation stays the only one.
	reconcile(ctx, t, rig.b.admin, tracker, false, repeatPolls)
	if n, rows := observations(tracker, w.captured); n != 1 || rows != 2*shardRows {
		t.Fatalf("after reading shard B: %d observations of %v rows, want the packet path's 1 of %d", n, rows, 2*shardRows)
	}
}

// caseLocalLogMissesAfterFailover: once the reconciler's connection fails
// over to shard B, a query shard A ran is invisible to the local source.
func caseLocalLogMissesAfterFailover(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("local")
	tracker := actuals.NewTracker(actualsConfig())
	rig.dispatch(ctx, t, w, tracker)

	reconcile(ctx, t, failoverClient(t, rig.b), tracker, false, repeatPolls)
	if n, _ := observations(tracker, w.foreign); n != 0 {
		t.Fatalf("the local log on shard B showed %d observations of a query shard A ran, want 0", n)
	}
	// The same reader on shard A does see it: the query is in a log, just not
	// in the log the failed-over connection reads.
	reconcile(ctx, t, rig.a.admin, tracker, false, 1)
	if n, rows := observations(tracker, w.foreign); n != 1 || uint64(rows) != rig.a.initiatorRows(ctx, t, w.foreign) {
		t.Fatalf("shard A's own log: %d observations of %v rows, want 1 of the logged read_rows", n, rows)
	}
}

// caseUnionFindsObservationsAfterFailover is the acceptance for the union
// source across two data shards: after the reconciler's connection fails
// over to shard B, system.all_query_log still finds the query shard A ran,
// records it once however often it is read, never adds a second observation
// of the packet-observed Distributed read, and — for a process with no packet
// observation of that read — records its initiator's whole-query totals once,
// never the remote child's fraction.
func caseUnionFindsObservationsAfterFailover(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("union")
	tracker := actuals.NewTracker(actualsConfig())
	rig.dispatch(ctx, t, w, tracker)

	reader := failoverClient(t, rig.b)
	if got := reader.ProbeQueryLogUnionCapability(ctx); got != chopt.CapabilityAvailable {
		t.Fatalf("union probe with create_union_system_log_tables = %s, want available", got)
	}

	reconcile(ctx, t, reader, tracker, true, repeatPolls)
	if n, rows := observations(tracker, w.foreign); n != 1 || uint64(rows) != rig.a.initiatorRows(ctx, t, w.foreign) {
		t.Fatalf("union after failover: %d observations of %v rows, want 1 of the logged read_rows", n, rows)
	}
	if n, rows := observations(tracker, w.captured); n != 1 || rows != 2*shardRows {
		t.Fatalf("packet-observed read after union polls: %d observations of %v rows, want the packet path's 1 of %d",
			n, rows, 2*shardRows)
	}

	other := actuals.NewTracker(actualsConfig())
	reconcile(ctx, t, reader, other, true, repeatPolls)
	if n, rows := observations(other, w.captured); n != 1 || rows != 2*shardRows {
		t.Fatalf("another process via the union: %d observations of %v rows, want the initiator's 1 of %d",
			n, rows, 2*shardRows)
	}
}

// caseUnionReadPagesStably walks the union with one-row pages and requires
// the exact row set a single read returns, in the same order, with no row
// twice: the cursor's tuple comparison and the settle horizon, evaluated by a
// real server across union members.
func caseUnionReadPagesStably(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("pages")
	for range repeatPolls {
		rig.dispatch(ctx, t, w, nil)
	}
	reader := rig.b.admin
	req := chclient.QueryLogActualsRequest{
		Union:         true,
		After:         chclient.QueryLogCursor{EventTime: time.Now().Add(-actualsConfig().QueryLogLookback)},
		ShapeIDPrefix: w.prefix,
		Limit:         wholeReadLimit,
	}
	whole, err := reader.QueryLogActuals(ctx, req)
	if err != nil {
		t.Fatalf("single read: %v", err)
	}
	if want := 2 * repeatPolls; len(whole) != want {
		t.Fatalf("single read returned %d initiator rows, want %d", len(whole), want)
	}

	var paged []chclient.QueryLogActualRow
	page := req
	page.Limit = pageProbeLimit
	for {
		rows, err := reader.QueryLogActuals(ctx, page)
		if err != nil {
			t.Fatalf("paged read: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		paged = append(paged, rows...)
		last := rows[len(rows)-1]
		page.After = chclient.QueryLogCursor{EventTime: last.EventTime, Hostname: last.Hostname, QueryID: last.QueryID}
		if len(paged) > len(whole) {
			t.Fatalf("paging returned more rows (%d) than a single read (%d): a row was read twice", len(paged), len(whole))
		}
	}
	if len(paged) != len(whole) {
		t.Fatalf("paging returned %d rows, a single read %d", len(paged), len(whole))
	}
	for i := range whole {
		if paged[i] != whole[i] {
			t.Fatalf("row %d: paged %+v, single read %+v", i, paged[i], whole[i])
		}
	}

	held := req
	held.SettleDelay = time.Hour
	if rows, err := reader.QueryLogActuals(ctx, held); err != nil || len(rows) != 0 {
		t.Fatalf("an hour's settle delay returned %d rows (err %v), want none: every row is younger than that", len(rows), err)
	}
}

// caseUnionAbsentFallsBackToLocal: where the union table does not exist, the
// probe reports the refusal, the union read is refused with
// ErrQueryLogUnionRefused, and the reconciler still reads the local log.
func caseUnionAbsentFallsBackToLocal(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("absent")
	tracker := actuals.NewTracker(actualsConfig())
	rig.dispatch(ctx, t, w, tracker)

	reader := rig.a.admin
	if got := reader.ProbeQueryLogUnionCapability(ctx); got != chopt.CapabilityForbidden {
		t.Fatalf("union probe without the union table = %s, want forbidden", got)
	}
	_, err := reader.QueryLogActuals(ctx, chclient.QueryLogActualsRequest{Union: true, ShapeIDPrefix: w.prefix, Limit: pageProbeLimit})
	if !errors.Is(err, chclient.ErrQueryLogUnionRefused) {
		t.Fatalf("union read without the table: err %v, want ErrQueryLogUnionRefused", err)
	}
	reconcile(ctx, t, reader, tracker, true, repeatPolls)
	if n, _ := observations(tracker, w.foreign); n != 1 {
		t.Fatalf("union refused: the local fallback recorded %d observations of shard A's query, want 1", n)
	}
}

// caseUnionNeedsNoClusterWideGrant pins the deployment contract: a user
// holding only SELECT on system.query_log runs the local source, the union
// probe refuses it, and adding SELECT on system.all_query_log alone — no
// REMOTE, no grant on any other server — makes the union readable, because
// the table reaches the other replicas with the cluster's own credentials.
func caseUnionNeedsNoClusterWideGrant(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("grants")
	rig.dispatch(ctx, t, w, nil)

	const user, password = "actuals_reader", "actuals_reader"
	rig.b.exec(ctx, t, "CREATE USER "+user+" IDENTIFIED WITH plaintext_password BY '"+password+"'")
	rig.b.exec(ctx, t, "GRANT SELECT ON system.query_log TO "+user)
	reader := newClient(t, chclient.Config{Addr: rig.b.addr}, user, password)

	tracker := actuals.NewTracker(actualsConfig())
	reconcile(ctx, t, reader, tracker, false, 1)
	if n, _ := observations(tracker, w.foreign); n != 0 {
		t.Fatalf("local read by the restricted user on shard B recorded %d observations of shard A's query, want 0", n)
	}
	if got := reader.ProbeQueryLogUnionCapability(ctx); got != chopt.CapabilityForbidden {
		t.Fatalf("union probe without SELECT on system.all_query_log = %s, want forbidden", got)
	}

	rig.b.exec(ctx, t, "GRANT SELECT ON system.all_query_log TO "+user)
	if got := reader.ProbeQueryLogUnionCapability(ctx); got != chopt.CapabilityAvailable {
		t.Fatalf("union probe with SELECT on system.all_query_log alone = %s, want available", got)
	}
	reconcile(ctx, t, reader, tracker, true, 1)
	if n, _ := observations(tracker, w.foreign); n != 1 {
		t.Fatalf("restricted user via the union recorded %d observations of shard A's query, want 1", n)
	}
}

// TestUnionFindsRowsRotatedByUpgrade is the acceptance for a rotated log: a
// server upgraded from the supported floor to 26.8 renames its query_log to
// query_log_0 because the table's schema changed. The local source misses
// every query logged before the upgrade; the union finds them, once, and the
// query the packet path observed before the upgrade is still accounted once.
func TestUnionFindsRowsRotatedByUpgrade(t *testing.T) {
	ctx := t.Context()
	volume := "cerberus-querylog-" + randomSuffix(t)
	w := newWorkload("upgrade")

	before := startNode(ctx, t, floorImage, nodeOptions{volume: volume})
	before.exec(ctx, t, "CREATE TABLE samples (x UInt64) ENGINE = MergeTree ORDER BY x")
	before.exec(ctx, t, "INSERT INTO samples SELECT number FROM numbers("+strconv.Itoa(upgradeRows)+")")
	tracker := actuals.NewTracker(actualsConfig())
	dispatch(ctx, t, before.admin, tracker, w.captured, "SELECT toString(count()) FROM samples")
	dispatch(ctx, t, before.admin, nil, w.foreign, "SELECT toString(sum(x)) FROM samples")
	before.flushLogs(ctx, t)
	before.stop(t)

	after := startNode(ctx, t, unionImage, nodeOptions{configs: []string{"union-local.xml"}, volume: volume, removeVolume: true})
	after.flushLogs(ctx, t)
	if rotated := after.uint64Of(ctx, t,
		"SELECT count() FROM system.tables WHERE database = 'system' AND name = 'query_log_0'"); rotated != 1 {
		t.Fatalf("the upgrade to %s did not rotate query_log into query_log_0", unionImage)
	}

	reconcile(ctx, t, after.admin, tracker, false, repeatPolls)
	if n, _ := observations(tracker, w.foreign); n != 0 {
		t.Fatalf("the local log after the upgrade showed %d observations of a pre-upgrade query, want 0", n)
	}
	reconcile(ctx, t, after.admin, tracker, true, repeatPolls)
	if n, _ := observations(tracker, w.foreign); n != 1 {
		t.Fatalf("the union after the upgrade recorded %d observations of the pre-upgrade query, want 1", n)
	}
	if n, _ := observations(tracker, w.captured); n != 1 {
		t.Fatalf("the packet-observed pre-upgrade query has %d observations after the union read, want 1", n)
	}
}

// randomSuffix is a short random hex string for a per-run resource name.
func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// statementRows is read_rows of every finished initiator row of shape in n's
// own log whose statement is statement, keyed by query id.
func (n *node) statementRows(ctx context.Context, t *testing.T, shape, statement string) map[string]uint64 {
	t.Helper()
	rows, err := n.admin.Conn().Query(ctx,
		"SELECT query_id, read_rows FROM system.query_log WHERE type = 'QueryFinish' AND is_initial_query = 1 AND log_comment = ? AND query = ?",
		shape, statement)
	if err != nil {
		t.Fatalf("%s: initiator rows of %s: %v", n.image, shape, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]uint64)
	for rows.Next() {
		var id string
		var readRows uint64
		if err := rows.Scan(&id, &readRows); err != nil {
			t.Fatalf("%s: scan: %v", n.image, err)
		}
		out[id] = readRows
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: initiator rows of %s: %v", n.image, shape, err)
	}
	return out
}

// caseRoutedRequestIsOneObservationInAnotherProcess is the acceptance for
// cerberus issue #3669. One tracker dispatches a routed request of
// routedShards shard statements through the solver's executor on shard A;
// each statement is a top-level query of its own in the log, carrying a
// fraction of the request's rows. A second tracker — another process, which
// holds no packet claim on those statements — reads the log through reader
// and records at most one observation, whose rows are the request's total.
func caseRoutedRequestIsOneObservationInAnotherProcess(ctx context.Context, t *testing.T, rig twoShardRig, reader *chclient.Client, union bool) {
	w := newWorkload("routed")
	shape := w.prefix + "routed"
	dispatcher := actuals.NewTracker(actualsConfig())
	ids := dispatchRouted(ctx, t, rig.a.admin, dispatcher, shape)
	rig.a.flushLogs(ctx, t)
	rig.b.flushLogs(ctx, t)

	// The log holds one initiator row per shard statement, each a fraction of
	// the request: exactly the rows a reader would record as routedShards
	// whole queries without the request's identity.
	logged := rig.a.statementRows(ctx, t, shape, routedShardSQL)
	if len(logged) != routedShards {
		t.Fatalf("shard A logged %d initiator rows for the routed request, want one per shard (%d): %v", len(logged), routedShards, logged)
	}
	var total uint64
	for _, id := range ids {
		readRows, ok := logged[id]
		if !ok || readRows != shardRows {
			t.Fatalf("shard statement %s logged read_rows %d (present %v), want %d", id, readRows, ok, shardRows)
		}
		total += readRows
	}

	if n, rows := observations(dispatcher, shape); n != 1 || uint64(rows) != total {
		t.Fatalf("dispatching process, packet path: %d observations of %v rows, want 1 of %d", n, rows, total)
	}

	other := actuals.NewTracker(actualsConfig())
	reconcile(ctx, t, reader, other, union, repeatPolls)
	if n, rows := observations(other, shape); n != 1 || uint64(rows) != total {
		t.Fatalf("another process reading the log: %d observations of %v rows, want 1 of the request's %d", n, rows, total)
	}

	reconcile(ctx, t, reader, dispatcher, union, repeatPolls)
	if n, rows := observations(dispatcher, shape); n != 1 || uint64(rows) != total {
		t.Fatalf("dispatching process after reading the log: %d observations of %v rows, want the packet path's 1 of %d", n, rows, total)
	}
}

// caseHTTPDispatchesObservedFromQueryLog is the acceptance for cerberus issue
// #3668. Over HTTP the server streams no progress packets, so the packet path
// has nothing to observe: a single statement and a routed request dispatched
// over HTTP with capture armed leave no packet observation and no claim, and
// the same process's query-log reader records each once with the real
// read_rows the log carries.
func caseHTTPDispatchesObservedFromQueryLog(ctx context.Context, t *testing.T, rig twoShardRig) {
	w := newWorkload("http")
	single, routed := w.prefix+"single", w.prefix+"routed"
	httpClient := newClient(t, chclient.Config{Addr: rig.a.httpAddr, Protocol: clickhouse.HTTP}, adminUser, adminPassword)
	tracker := actuals.NewTracker(actualsConfig())
	dispatch(ctx, t, httpClient, tracker, single, shardQuery)
	dispatchRouted(ctx, t, httpClient, tracker, routed)
	rig.a.flushLogs(ctx, t)

	for _, shape := range []string{single, routed} {
		if n, rows := observations(tracker, shape); n != 0 {
			t.Fatalf("%s over HTTP: the packet path recorded %d observations of %v rows, want none — "+
				"it has no progress packets to observe", shape, n, rows)
		}
	}

	// The real totals the log carries — never zero: every statement scans the
	// whole shard-local table.
	singleStatement := rig.a.statementRows(ctx, t, single, shardQuery)
	if len(singleStatement) != 1 {
		t.Fatalf("the HTTP statement logged %d finished initiator rows, want 1", len(singleStatement))
	}
	var singleRows uint64
	for _, readRows := range singleStatement {
		singleRows = readRows
	}
	if singleRows < shardRows {
		t.Fatalf("the HTTP statement logged read_rows %d, want at least the %d-row table it scans", singleRows, shardRows)
	}
	shardStatements := rig.a.statementRows(ctx, t, routed, routedShardSQL)
	if len(shardStatements) != routedShards {
		t.Fatalf("the HTTP routed request logged %d finished initiator rows, want one per shard (%d)", len(shardStatements), routedShards)
	}
	var routedRows uint64
	for _, readRows := range shardStatements {
		routedRows += readRows
	}
	// The HTTP transport opens a connection by running its hello under the
	// dispatch's own query_id and log_comment, so the log holds a second
	// finished initiator row with the statement's identity that is not the
	// statement: the row the reader must not take for it.
	if hello := rig.a.uint64Of(ctx, t,
		"SELECT count() FROM system.query_log WHERE type = 'QueryFinish' AND is_initial_query = 1 AND log_comment = ? AND query != ? AND query_id IN (?)",
		single, shardQuery, slices.Collect(maps.Keys(singleStatement))); hello == 0 {
		t.Fatalf("the first statement over a fresh HTTP connection logged no connection hello under its identity")
	}

	reconcile(ctx, t, rig.a.admin, tracker, false, repeatPolls)
	if n, rows := observations(tracker, single); n != 1 || uint64(rows) != singleRows {
		t.Fatalf("HTTP statement after reading the log: %d observations of %v rows, want 1 of the logged %d", n, rows, singleRows)
	}
	if n, rows := observations(tracker, routed); n != 1 || uint64(rows) != routedRows {
		t.Fatalf("HTTP routed request after reading the log: %d observations of %v rows, want 1 of the logged total %d",
			n, rows, routedRows)
	}
}
