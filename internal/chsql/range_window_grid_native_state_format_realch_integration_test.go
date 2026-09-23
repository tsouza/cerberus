//go:build integration

// Real-ClickHouse coverage of the native timeSeries*ToGrid aggregate-state
// format across a rolling ClickHouse upgrade of a Distributed deployment.
//
// The family writes a format-version byte at the head of every partial
// aggregation state and refuses any other version with INCORRECT_DATA. The
// format moved twice — to 3 in 26.7 (ClickHouse #106724) and to 4 in 26.8
// (ClickHouse #115920). A state therefore cannot cross between two servers on
// different minors, and ClickHouse ships one exactly when an aggregate sits at
// the same query level as a Distributed table: each shard aggregates its
// rows and the initiator merges the states.
//
// Cerberus never emits that shape. Every scan renders as its own
// `(SELECT ... FROM <table> WHERE ...)` subquery, so the Distributed table
// sends rows and the whole aggregation — plain, -State and -Merge alike —
// runs on the initiator. The native path is therefore insensitive to a
// version skew between the participants, and this suite proves it on real
// servers rather than trusting the planner:
//
//  1. a control query with the aggregate directly over the Distributed table
//     FAILS with INCORRECT_DATA on every cross-minor rig and succeeds on every
//     single-minor rig, so each rig really does straddle (or not) a format
//     boundary and would expose a shipped state;
//  2. cerberus's own native SQL for rate, rate with the deferred label-shaping
//     recollapse (the -State/-Merge pair), and the bare-selector resample
//     answers exactly the fan-out reference from EACH initiator of every rig —
//     old initiator with a new shard, new initiator with an old shard, and
//     the homogeneous states before and after the upgrade;
//  3. the production lowering table, resolved from the initiator's own probes
//     the way cmd/cerberus resolves it, routes rate native and answers the
//     reference on every rig;
//  4. the timeSeriesLastTwoSamples state the downsample tier persists
//     round-trips between the two servers of every rig in both directions,
//     while a timeSeriesRateToGrid state does not across a minor boundary —
//     the persisted tier does not share the family's versioned format.
//
// The reference is the fan-out answer, which carries no aggregate state; every
// rig's fan-out answer must equal every other rig's.
//
// The dataset spreads each series across both shards and carries finite
// values, a NaN, a Prometheus stale-marker NaN payload, samples on window
// boundaries, a cross-shard unequal finite duplicate, and two raw series that
// the label shaping collapses into one output series. A NaN-bearing duplicate
// is deliberately absent: its survivor is version-dependent by design (see
// range_window_grid_native_nan_duplicate_realch_integration_test.go), so the
// rigs would disagree for a reason unrelated to the state format.
//
// Gated behind the `integration` build tag; run by the
// `ts-grid-state-format-integration` Justfile recipe.
package chsql_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	promparser "github.com/prometheus/prometheus/promql/parser"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/network"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// The pinned builds, each standing for the state-format byte it writes —
// measured on that exact build, not read off a changelog heading. The Justfile
// pre-pulls the same literals (CH_TS_STATE_*_IMAGE), held equal by
// TestIntegrationImagePinsMatchTheJustfile.
const (
	// tsStateV2Image: 26.6.8.7, the latest 26.6 build — format 2.
	tsStateV2Image = "clickhouse/clickhouse-server:26.6.8.7-alpine"
	// tsStateV3FirstImage: 26.7.1.1315, the first 26.7 release build —
	// format 3.
	tsStateV3FirstImage = "clickhouse/clickhouse-server:26.7.1.1315-alpine"
	// tsStateV3Image: 26.7.13.12, the latest 26.7 build published as an
	// image — format 3.
	tsStateV3Image = "clickhouse/clickhouse-server:26.7.13.12-alpine"
	// tsStateV4Image: 26.8.1.2041, the first 26.8 release build — format 4.
	tsStateV4Image = "clickhouse/clickhouse-server:26.8.1.2041-alpine"
)

const (
	tsStateCluster  = "cerberus_ts_state"
	tsStateDatabase = "otel"
	tsStateUser     = "cerberus"
	tsStatePassword = "cerberus"

	// clickhouseIncorrectData is ClickHouse's INCORRECT_DATA error code, the
	// one a timeSeries*ToGrid state carrying a foreign format version raises.
	clickhouseIncorrectData = 117

	tsStateStartTimeout = 10 * time.Minute
)

// The query grid: three points one minute apart, each evaluating a
// one-minute window.
var (
	tsStateStart = time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	tsStateEnd   = time.Date(2026, 1, 1, 0, 3, 0, 0, time.UTC)
	tsStateStep  = time.Minute
)

// tsStateRow is one seeded sample. node picks the shard (0 or 1) whose LOCAL
// table receives it, so a series' placement is deliberate rather than left to
// the Distributed table's rand() sharding key.
type tsStateRow struct {
	node  int
	attrs string // a ClickHouse map literal
	ts    string
	value string // a Float64 SQL expression
}

// tsStateStaleNaN is Prometheus's stale-marker payload (value.StaleNaN,
// 0x7ff0000000000002) as a ClickHouse Float64 expression.
const tsStateStaleNaN = "reinterpretAsFloat64(toUInt64(9218868437227405314))"

// tsStateRows is the dataset. Every series alternates between the two shards
// so each output value needs states from both.
var tsStateRows = []tsStateRow{
	// finite: a counter, with samples ON the grid points 00:01:00, 00:02:00,
	// 00:03:00 (the right edge of one window, excluded from the next) and on
	// 00:00:00 (the left edge of the first window, excluded from it).
	{0, "map('job','finite')", "2026-01-01 00:00:00", "5.0"},
	{1, "map('job','finite')", "2026-01-01 00:00:15", "10.0"},
	{0, "map('job','finite')", "2026-01-01 00:00:45", "20.0"},
	{1, "map('job','finite')", "2026-01-01 00:01:00", "30.0"},
	{0, "map('job','finite')", "2026-01-01 00:01:30", "40.0"},
	{1, "map('job','finite')", "2026-01-01 00:02:00", "50.0"},
	{0, "map('job','finite')", "2026-01-01 00:02:30", "60.0"},
	{1, "map('job','finite')", "2026-01-01 00:03:00", "70.0"},
	// nan: an ordinary NaN inside the second window.
	{0, "map('job','nan')", "2026-01-01 00:00:20", "10.0"},
	{1, "map('job','nan')", "2026-01-01 00:00:50", "20.0"},
	{0, "map('job','nan')", "2026-01-01 00:01:20", "nan"},
	{1, "map('job','nan')", "2026-01-01 00:01:40", "40.0"},
	{0, "map('job','nan')", "2026-01-01 00:02:10", "50.0"},
	{1, "map('job','nan')", "2026-01-01 00:02:50", "60.0"},
	// stale: a stale-marker NaN payload closing the third window.
	{1, "map('job','stale')", "2026-01-01 00:00:20", "10.0"},
	{0, "map('job','stale')", "2026-01-01 00:00:50", "20.0"},
	{1, "map('job','stale')", "2026-01-01 00:01:30", "30.0"},
	{0, "map('job','stale')", "2026-01-01 00:02:20", "40.0"},
	{1, "map('job','stale')", "2026-01-01 00:02:50", tsStateStaleNaN},
	// dup: an unequal finite duplicate split across the shards. Every
	// supported build keeps the greater value.
	{0, "map('job','dup')", "2026-01-01 00:00:20", "10.0"},
	{1, "map('job','dup')", "2026-01-01 00:00:50", "20.0"},
	{0, "map('job','dup')", "2026-01-01 00:01:30", "40.0"},
	{1, "map('job','dup')", "2026-01-01 00:01:30", "45.0"},
	{0, "map('job','dup')", "2026-01-01 00:01:50", "50.0"},
	// recollapse: `a.b` and `a_b` both shape to the Prometheus label `a_b`,
	// so these two raw series are one output series.
	{0, "map('job','recollapse','a.b','x')", "2026-01-01 00:00:20", "10.0"},
	{1, "map('job','recollapse','a_b','x')", "2026-01-01 00:00:50", "20.0"},
	{1, "map('job','recollapse','a.b','x')", "2026-01-01 00:01:20", "30.0"},
	{0, "map('job','recollapse','a_b','x')", "2026-01-01 00:01:50", "40.0"},
	{0, "map('job','recollapse','a.b','x')", "2026-01-01 00:02:20", "50.0"},
	{1, "map('job','recollapse','a_b','x')", "2026-01-01 00:02:40", "60.0"},
}

// tsStateLocalDDL / tsStateDistributedDDL mirror internal/schema/ddl's
// multi-shard split for the sum table: a per-shard local table plus a
// Distributed wrapper under the name every query reads.
const (
	tsStateLocalDDL = `
CREATE TABLE otel_metrics_sum_local (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix)`
	tsStateDistributedDDL = `
CREATE TABLE otel_metrics_sum AS otel_metrics_sum_local
ENGINE = Distributed(` + tsStateCluster + `, ` + tsStateDatabase + `, otel_metrics_sum_local, rand())`
)

// tsStateNode is one server of a rig.
type tsStateNode struct {
	alias  string
	image  string
	db     *sql.DB
	client *chclient.Client
}

// tsStateShape is one representative native query shape: the PromQL, the
// lowering table that forces the native arm, and the native aggregate the
// emitted SQL must name.
type tsStateShape struct {
	name     string
	query    string
	native   promql.RangeLowerers
	nativeFn string
}

var tsStateShapes = []tsStateShape{
	{
		name:     "rate",
		query:    "rate(requests_total[1m])",
		native:   promql.RangeLowerers{Rate: promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}}},
		nativeFn: "timeSeriesRateToGrid(",
	},
	{
		name:  "rate_recollapse",
		query: "rate(requests_total[1m])",
		native: promql.RangeLowerers{Rate: promql.NativeRateLowerer{
			Fallback: promql.FanoutRateLowerer{}, Recollapse: true,
		}},
		nativeFn: "timeSeriesRateToGridMerge(",
	},
	{
		name:     "resample",
		query:    "requests_total",
		native:   promql.RangeLowerers{Staleness: promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}}},
		nativeFn: "timeSeriesResampleToGridWithStaleness(",
	},
}

// tsStateAnswer maps an output series (its Attributes as JSON) to its
// per-timestamp values.
type tsStateAnswer map[string]map[int64]float64

// TestTSGridStateFormatBoundary_Distributed_RealCH walks a rolling upgrade
// across both format boundaries. Each case is one moment of it: the two
// homogeneous ends, each cross-minor mix (one rig covers both directions,
// since each node is the initiator in turn), a skip-a-minor mix, and patch
// skew inside one minor.
func TestTSGridStateFormatBoundary_Distributed_RealCH(t *testing.T) {
	v2, v3First, v3, v4 := tsStateV2Image, tsStateV3FirstImage, tsStateV3Image, tsStateV4Image

	cases := []struct {
		name   string
		images [2]string
		// crossesFormat is whether the two servers write different state
		// formats — the thing the control query must detect.
		crossesFormat bool
	}{
		{"homogeneous format 2 before the upgrade", [2]string{v2, v2}, false},
		{"format 2 and 3 mid-upgrade", [2]string{v2, v3}, true},
		{"patch skew inside format 3", [2]string{v3First, v3}, false},
		{"format 3 and 4 mid-upgrade", [2]string{v3, v4}, true},
		{"format 2 and 4 skipping a minor", [2]string{v2, v4}, true},
		{"homogeneous format 4 after the upgrade", [2]string{v4, v4}, false},
	}

	// The rigs are independent, so they boot in parallel; each records its
	// fan-out answer, and the answers are compared across rigs once all ran.
	fanouts := make([]tsStateAnswer, len(cases))
	t.Run("rigs", func(t *testing.T) {
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), tsStateStartTimeout)
				defer cancel()
				nodes := startTSStateCluster(ctx, t, tc.images)
				seedTSState(ctx, t, nodes)

				reference := runTSStateQuery(ctx, t, nodes[0], tsStateRateQuery, promql.RangeLowerers{})
				assertTSStateReferenceShape(t, reference)
				fanouts[i] = reference

				for _, initiator := range nodes {
					assertTSStateControl(ctx, t, initiator, peerOf(nodes, initiator), tc.crossesFormat)
					assertTSStateNativePaths(ctx, t, initiator, reference)
					assertTSStateProductionRouting(ctx, t, initiator, reference)
					assertTSStatePersistedTierState(ctx, t, initiator, peerOf(nodes, initiator), tc.crossesFormat)
				}
			})
		}
	})
	if t.Failed() {
		return
	}
	for i := 1; i < len(cases); i++ {
		if !tsStateAnswersEqual(fanouts[i], fanouts[0]) {
			t.Fatalf("the fan-out answers differently on %q than on %q — it carries no aggregate state, so the "+
				"dataset or a rig is broken:\n got  %v\n want %v", cases[i].name, cases[0].name, fanouts[i], fanouts[0])
		}
	}
}

// tsStateRateQuery is the PromQL the reference and the routing checks run.
const tsStateRateQuery = "rate(requests_total[1m])"

// tsStateControlSQL puts the aggregate at the same query level as the
// Distributed table — the one shape that makes every shard ship a partial
// state to the initiator. Cerberus never emits it; it exists to prove the rig
// would expose a shipped state.
const tsStateControlSQL = "SELECT `Attributes`, timeSeriesRateToGrid(toDateTime('2026-01-01 00:01:00', 'UTC'), " +
	"toDateTime('2026-01-01 00:03:00', 'UTC'), 60, 60)(`TimeUnix`, `Value`) FROM otel_metrics_sum GROUP BY `Attributes`"

// assertTSStateControl runs the control query from initiator. Across a format
// boundary it must fail with INCORRECT_DATA; on one format it must answer.
func assertTSStateControl(ctx context.Context, t *testing.T, initiator, peer *tsStateNode, crossesFormat bool) {
	t.Helper()
	_, err := queryTSStateRows(ctx, initiator.db, tsStateControlSQL+" SETTINGS "+chclient.SettingExperimentalTSGridAggregate+" = 1")
	var ex *clickhouse.Exception
	shipped := errors.As(err, &ex) && ex.Code == clickhouseIncorrectData
	switch {
	case crossesFormat && !shipped:
		t.Fatalf("control from %s (%s, shard peer %s): want INCORRECT_DATA (%d) from a state shipped across the "+
			"format boundary, got err=%v — the rig does not straddle the boundary it claims to",
			initiator.alias, initiator.image, peer.image, clickhouseIncorrectData, err)
	case !crossesFormat && err != nil:
		t.Fatalf("control from %s (%s, shard peer %s) failed on a single-format rig: %v",
			initiator.alias, initiator.image, peer.image, err)
	}
}

// assertTSStateNativePaths runs every native shape from initiator and asserts
// it answers the reference exactly, whatever version the shards run.
func assertTSStateNativePaths(ctx context.Context, t *testing.T, initiator *tsStateNode, reference tsStateAnswer) {
	t.Helper()
	for _, shape := range tsStateShapes {
		sqlStr, args := emitTSState(ctx, t, shape.query, shape.native)
		if !strings.Contains(sqlStr, shape.nativeFn) {
			t.Fatalf("%s: the native lowering did not emit %s — this would compare the fan-out with itself:\n%s",
				shape.name, shape.nativeFn, sqlStr)
		}
		got, err := queryTSState(ctx, initiator.db, sqlStr, args)
		if err != nil {
			t.Fatalf("%s from %s (%s): native query failed: %v", shape.name, initiator.alias, initiator.image, err)
		}
		want := reference
		if shape.query != tsStateRateQuery {
			if want, err = queryTSStateFanout(ctx, t, initiator, shape.query); err != nil {
				t.Fatalf("%s from %s: fan-out query: %v", shape.name, initiator.alias, err)
			}
		}
		if !tsStateAnswersEqual(got, want) {
			t.Fatalf("%s from %s (%s): native answer differs from the fan-out:\n native %v\n fanout %v",
				shape.name, initiator.alias, initiator.image, got, want)
		}
	}
}

func queryTSStateFanout(ctx context.Context, t *testing.T, initiator *tsStateNode, query string) (tsStateAnswer, error) {
	t.Helper()
	sqlStr, args := emitTSState(ctx, t, query, promql.RangeLowerers{})
	if strings.Contains(sqlStr, "timeSeries") {
		t.Fatalf("the fan-out lowering of %q emitted a timeSeries aggregate:\n%s", query, sqlStr)
	}
	return queryTSState(ctx, initiator.db, sqlStr, args)
}

// assertTSStateProductionRouting resolves the optimization set from
// initiator's own version and capability probes exactly as cmd/cerberus does,
// wires the production lowering table, and asserts rate routes native and
// answers the reference.
func assertTSStateProductionRouting(ctx context.Context, t *testing.T, initiator *tsStateNode, reference tsStateAnswer) {
	t.Helper()
	version, err := initiator.client.ProbeVersion(ctx)
	if err != nil {
		t.Fatalf("probe version on %s: %v", initiator.alias, err)
	}
	set, _, err := chopt.Resolve(chopt.Config{
		Optimizations: chopt.SelectionAuto,
		Capability:    initiator.client.ProbeTSGridCapability(ctx),
	}, version)
	if err != nil {
		t.Fatalf("resolve on %s: %v", initiator.alias, err)
	}
	sqlStr, args := emitTSState(ctx, t, tsStateRateQuery, choptwire.RangeLowerers(set))
	if !strings.Contains(sqlStr, tsStateShapes[0].nativeFn) && !strings.Contains(sqlStr, tsStateShapes[1].nativeFn) {
		t.Fatalf("production routing on %s (%s, enabled %v) did not route rate native:\n%s",
			initiator.alias, initiator.image, set.IDs(), sqlStr)
	}
	got, err := queryTSState(ctx, initiator.db, sqlStr, args)
	if err != nil {
		t.Fatalf("production-routed rate from %s (%s): %v", initiator.alias, initiator.image, err)
	}
	if !tsStateAnswersEqual(got, reference) {
		t.Fatalf("production-routed rate from %s (%s) differs from the reference:\n got  %v\n want %v",
			initiator.alias, initiator.image, got, reference)
	}
}

// The persisted downsample tier stores AggregateFunction(timeSeriesLastTwoSamples,
// DateTime64(9), Float64). tsStateTierStateType is that column type, and the
// two probes below build one state from the same two samples on either server.
const (
	tsStateTierStateType = "AggregateFunction(timeSeriesLastTwoSamples, DateTime64(9), Float64)"
	tsStateTierStateSQL  = "SELECT hex(timeSeriesLastTwoSamplesState(ts, v)) FROM (" +
		"SELECT toDateTime64('2026-01-01 00:00:20', 9) AS ts, 25.0 AS v UNION ALL " +
		"SELECT toDateTime64('2026-01-01 00:00:50', 9), 40.0)"
	tsStateTierFinalSQL = "SELECT toString(finalizeAggregation(CAST(unhex(?) AS " + tsStateTierStateType + ")))"
	// The rate state's grid parameters are epoch seconds (2026-01-01
	// 00:01:00 UTC): an AggregateFunction type argument must be a literal.
	tsStateRateStateSQL = "SELECT hex(timeSeriesRateToGridState(1767225660, 1767225660, 60, 60)(ts, v)) FROM (" +
		"SELECT toDateTime('2026-01-01 00:00:20', 'UTC') AS ts, 25.0 AS v UNION ALL " +
		"SELECT toDateTime('2026-01-01 00:00:50', 'UTC'), 40.0)"
	tsStateRateFinalSQL = "SELECT toString(finalizeAggregation(CAST(unhex(?) AS AggregateFunction(" +
		"timeSeriesRateToGrid(1767225660, 1767225660, 60, 60), DateTime('UTC'), Float64))))"
)

// assertTSStatePersistedTierState writes a timeSeriesLastTwoSamples state on
// peer and reads it on initiator — the path a persisted downsample-tier part
// takes when an upgraded replica fetches it or an upgraded server merges it.
// It must read back exactly what initiator computes itself. The
// timeSeriesRateToGrid state takes the same path as a contrast: across a
// format boundary it must be refused, which is what shows the tier's state is
// outside the family's versioned format rather than untested by the probe.
func assertTSStatePersistedTierState(ctx context.Context, t *testing.T, initiator, peer *tsStateNode, crossesFormat bool) {
	t.Helper()
	scalar := func(db *sql.DB, query string, args ...any) (string, error) {
		var out string
		err := db.QueryRowContext(ctx, query+" SETTINGS "+chclient.SettingExperimentalTSGridAggregate+" = 1", args...).Scan(&out)
		return out, err
	}
	foreign, err := scalar(peer.db, tsStateTierStateSQL)
	if err != nil {
		t.Fatalf("tier state on %s: %v", peer.alias, err)
	}
	local, err := scalar(initiator.db, tsStateTierStateSQL)
	if err != nil {
		t.Fatalf("tier state on %s: %v", initiator.alias, err)
	}
	fromForeign, err := scalar(initiator.db, tsStateTierFinalSQL, foreign)
	if err != nil {
		t.Fatalf("%s (%s) cannot read the timeSeriesLastTwoSamples state %s (%s) wrote: %v",
			initiator.alias, initiator.image, peer.alias, peer.image, err)
	}
	fromLocal, err := scalar(initiator.db, tsStateTierFinalSQL, local)
	if err != nil {
		t.Fatalf("%s reading its own tier state: %v", initiator.alias, err)
	}
	if fromForeign != fromLocal {
		t.Fatalf("%s (%s) reads %s's (%s) timeSeriesLastTwoSamples state as %s, its own as %s",
			initiator.alias, initiator.image, peer.alias, peer.image, fromForeign, fromLocal)
	}

	rateState, err := scalar(peer.db, tsStateRateStateSQL)
	if err != nil {
		t.Fatalf("rate state on %s: %v", peer.alias, err)
	}
	_, err = scalar(initiator.db, tsStateRateFinalSQL, rateState)
	var ex *clickhouse.Exception
	refused := errors.As(err, &ex) && ex.Code == clickhouseIncorrectData
	if crossesFormat && !refused {
		t.Fatalf("%s (%s) reading a timeSeriesRateToGrid state from %s (%s): want INCORRECT_DATA (%d), got err=%v",
			initiator.alias, initiator.image, peer.alias, peer.image, clickhouseIncorrectData, err)
	}
	if !crossesFormat && err != nil {
		t.Fatalf("%s (%s) cannot read the timeSeriesRateToGrid state %s (%s) wrote on one format: %v",
			initiator.alias, initiator.image, peer.alias, peer.image, err)
	}
}

// assertTSStateReferenceShape guards the first rig's fan-out answer, which
// every later comparison trusts: every seeded series answers, the two raw
// recollapse series answer as one shaped series, and the finite counter
// answers at every grid point.
func assertTSStateReferenceShape(t *testing.T, ref tsStateAnswer) {
	t.Helper()
	for _, job := range []string{"finite", "nan", "stale", "dup", "recollapse"} {
		matches := 0
		for series := range ref {
			if strings.Contains(series, `"job":"`+job+`"`) {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("the reference answer has %d series for job=%s, want 1 — the fixture is broken: %v", matches, job, ref)
		}
	}
	for series, points := range ref {
		if strings.Contains(series, `"job":"recollapse"`) && !strings.Contains(series, `"a_b":"x"`) {
			t.Fatalf("recollapse series %s is not the shaped label set", series)
		}
		if strings.Contains(series, `"job":"finite"`) {
			for ts := tsStateStart; !ts.After(tsStateEnd); ts = ts.Add(tsStateStep) {
				if _, ok := points[ts.Unix()]; !ok {
					t.Fatalf("finite series has no point at %s: %v", ts, points)
				}
			}
		}
	}
}

// startTSStateCluster boots two ClickHouse servers on one private network,
// each defining the two-shard cluster over both aliases.
func startTSStateCluster(ctx context.Context, t *testing.T, images [2]string) [2]*tsStateNode {
	t.Helper()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })

	aliases := [2]string{"cha", "chb"}
	var replicas strings.Builder
	for _, alias := range aliases {
		fmt.Fprintf(&replicas, "<shard><replica><host>%s</host><port>9000</port><user>%s</user><password>%s</password></replica></shard>",
			alias, tsStateUser, tsStatePassword)
	}
	configPath := filepath.Join(t.TempDir(), "cluster.xml")
	clusterXML := fmt.Sprintf("<clickhouse><remote_servers><%[1]s>%[2]s</%[1]s></remote_servers></clickhouse>\n",
		tsStateCluster, replicas.String())
	if err := os.WriteFile(configPath, []byte(clusterXML), 0o600); err != nil {
		t.Fatalf("write cluster config: %v", err)
	}

	var nodes [2]*tsStateNode
	for i, alias := range aliases {
		container, err := tcclickhouse.Run(
			ctx, images[i],
			tcclickhouse.WithUsername(tsStateUser),
			tcclickhouse.WithPassword(tsStatePassword),
			tcclickhouse.WithDatabase(tsStateDatabase),
			tcclickhouse.WithConfigFile(configPath),
			network.WithNetwork([]string{alias}, nw),
		)
		if err != nil {
			t.Fatalf("start %s (%s): %v", alias, images[i], err)
		}
		t.Cleanup(func() { _ = container.Terminate(context.Background()) })
		host, err := container.Host(ctx)
		if err != nil {
			t.Fatalf("host %s: %v", alias, err)
		}
		port, err := container.MappedPort(ctx, "9000/tcp")
		if err != nil {
			t.Fatalf("port %s: %v", alias, err)
		}
		addr := host + ":" + port.Port()
		db := clickhouse.OpenDB(&clickhouse.Options{
			Addr: []string{addr},
			Auth: clickhouse.Auth{Database: tsStateDatabase, Username: tsStateUser, Password: tsStatePassword},
		})
		t.Cleanup(func() { _ = db.Close() })
		if err := db.PingContext(ctx); err != nil {
			t.Fatalf("ping %s: %v", alias, err)
		}
		client, err := chclient.New(chclient.Config{
			Addr: addr, Username: tsStateUser, Password: tsStatePassword, Database: tsStateDatabase,
		})
		if err != nil {
			t.Fatalf("chclient for %s: %v", alias, err)
		}
		t.Cleanup(func() { _ = client.Close() })
		nodes[i] = &tsStateNode{alias: alias, image: images[i], db: db, client: client}
	}
	return nodes
}

func seedTSState(ctx context.Context, t *testing.T, nodes [2]*tsStateNode) {
	t.Helper()
	for _, node := range nodes {
		for _, ddl := range []string{tsStateLocalDDL, tsStateDistributedDDL} {
			if _, err := node.db.ExecContext(ctx, ddl); err != nil {
				t.Fatalf("%s DDL: %v\n%s", node.alias, err, ddl)
			}
		}
	}
	for i, node := range nodes {
		var values []string
		for _, r := range tsStateRows {
			if r.node == i {
				values = append(values, fmt.Sprintf("('requests_total', %s, toDateTime64('%s', 9), %s)", r.attrs, r.ts, r.value))
			}
		}
		insert := "INSERT INTO otel_metrics_sum_local (MetricName, Attributes, TimeUnix, Value) VALUES " +
			strings.Join(values, ", ")
		if _, err := node.db.ExecContext(ctx, insert); err != nil {
			t.Fatalf("%s seed: %v", node.alias, err)
		}
	}
}

// tsStateSchema is the default metrics schema without the temporality column,
// which would otherwise send every rate() window off the native path.
func tsStateSchema() schema.Metrics {
	s := schema.DefaultOTelMetrics()
	s.AggregationTemporalityColumn = ""
	return s
}

func emitTSState(ctx context.Context, t *testing.T, query string, lowerers promql.RangeLowerers) (string, []any) {
	t.Helper()
	expr, err := promparser.NewParser(promparser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(ctx, expr, tsStateSchema(), tsStateStart, tsStateEnd, tsStateStep,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit %q: %v", query, err)
	}
	return sqlStr, args
}

func runTSStateQuery(ctx context.Context, t *testing.T, node *tsStateNode, query string, lowerers promql.RangeLowerers) tsStateAnswer {
	t.Helper()
	sqlStr, args := emitTSState(ctx, t, query, lowerers)
	got, err := queryTSState(ctx, node.db, sqlStr, args)
	if err != nil {
		t.Fatalf("%s on %s: %v", query, node.alias, err)
	}
	return got
}

func queryTSState(ctx context.Context, db *sql.DB, sqlStr string, args []any) (tsStateAnswer, error) {
	wrapped := fmt.Sprintf(
		"SELECT toJSONString(`Attributes`), toUnixTimestamp(`TimeUnix`), `Value` FROM (%s) SETTINGS %s = 1",
		sqlStr, chclient.SettingExperimentalTSGridAggregate,
	)
	rows, err := db.QueryContext(ctx, wrapped, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := tsStateAnswer{}
	for rows.Next() {
		var series string
		var ts int64
		var v float64
		if err := rows.Scan(&series, &ts, &v); err != nil {
			return nil, err
		}
		if out[series] == nil {
			out[series] = map[int64]float64{}
		}
		out[series][ts] = v
	}
	return out, rows.Err()
}

// tsStateAnswersEqual compares two answers exactly, with NaN equal to NaN:
// the same samples through the same arithmetic must produce the same bits
// whichever server merged them.
func tsStateAnswersEqual(a, b tsStateAnswer) bool {
	if len(a) != len(b) {
		return false
	}
	for series, pa := range a {
		pb, ok := b[series]
		if !ok || len(pa) != len(pb) {
			return false
		}
		for ts, va := range pa {
			vb, ok := pb[ts]
			if !ok {
				return false
			}
			if math.IsNaN(va) != math.IsNaN(vb) || (!math.IsNaN(va) && va != vb) {
				return false
			}
		}
	}
	return true
}

func (a tsStateAnswer) String() string {
	series := make([]string, 0, len(a))
	for s := range a {
		series = append(series, s)
	}
	sort.Strings(series)
	var b strings.Builder
	for _, s := range series {
		ts := make([]int64, 0, len(a[s]))
		for k := range a[s] {
			ts = append(ts, k)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		fmt.Fprintf(&b, "%s:", s)
		for _, k := range ts {
			fmt.Fprintf(&b, " %d=%v", k, a[s][k])
		}
		b.WriteString("; ")
	}
	return b.String()
}

func peerOf(nodes [2]*tsStateNode, n *tsStateNode) *tsStateNode {
	if nodes[0] == n {
		return nodes[1]
	}
	return nodes[0]
}

// queryTSStateRows runs query to completion and counts its rows. A Distributed
// query can fail after its first block arrives, so the error is only final
// once every row has been read.
func queryTSStateRows(ctx context.Context, db *sql.DB, query string) (int, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}
