//go:build integration

// Real-ClickHouse characterisation of the duplicate-timestamp SURVIVOR every
// native timeSeries* aggregate cerberus emits elects, on each side of
// ClickHouse #115920, and of the survivor cerberus's own fan-out elects for
// the same rows.
//
// # The two upstream contracts
//
// Every native member collapses a duplicate (series, timestamp) inside the
// ClickHouse builtin. Two contracts exist, and which one a server delivers is
// a property of its build:
//
//   - scan order (every build before #115920): the collapse is a running
//     "replace the current best only when the candidate compares greater"
//     fold, and IEEE754 makes every comparison against a NaN false, so on a
//     NaN-bearing duplicate the survivor follows encounter order. The
//     whole-window members keep a FIRST-visited NaN; the trailing-pair
//     members (irate, idelta, timeSeriesLastTwoSamples) keep a LAST-visited
//     one. Merging partial states follows the same rule, with the state
//     merge order standing in for row order.
//   - NaN loses (#115920): the greatest value wins and a NaN loses to any
//     other value, whatever the order. An all-NaN duplicate stays NaN.
//
// Both contracts keep the greater of two unequal FINITE duplicates, and both
// treat a Prometheus stale-marker payload (value.StaleNaN) exactly like an
// ordinary NaN — the builtins compare floats, never NaN payload bits.
//
// The pinned servers, each measured rather than read off a changelog heading:
// 25.9 (the family's registry floor) and 26.7.13.12 (the latest 26.7 build;
// #115920 is not an ancestor of any v26.7.* release tag and was never
// backported) deliver scan order; 26.8.1.2041 (the first 26.8 release)
// delivers NaN loses.
//
// # Cerberus's fan-out
//
// dedupWindowPairsByTsFrag keeps the greatest sample under ClickHouse's total
// order over Float64, in which NaN ranks GREATEST — on every server, and
// independent of encounter order. It agrees with both upstream contracts on
// unequal finite duplicates and on all-NaN duplicates, and disagrees with
// NaN-loses on every NaN-versus-finite duplicate: the fan-out elects the NaN,
// a #115920 server elects the finite sample. Against scan order it agrees
// only when the encounter order happens to favour the NaN.
//
// # What is pinned
//
//  1. TestTSGridFamily_DuplicateSurvivor_RealCH — for every member of the
//     emitter's nativeTSGridFn registry plus the other native aggregates
//     cerberus emits (resample, timeSeriesLastTwoSamples,
//     timeSeriesGroupArray and its -If form), on every pinned server: NaN-versus-finite,
//     stale-versus-finite, all-NaN (NaN/NaN, NaN/stale) and unequal finite
//     duplicates, each under both insertion orders AND both partial-state
//     merge orders, asserted against the server's contract.
//  2. TestFanoutDedup_DuplicateSurvivorIsOrderIndependent_RealCH — the
//     production dedupWindowPairsByTsFrag Frag, rendered and executed on
//     every pinned server, elects the same survivor under both encounter
//     orders for every collision kind.
//  3. TestRate_NativeGrid_NaNDuplicate_AgainstFanout_RealCH — end to end
//     through cerberus's own lowering and emitter over one MergeTree table
//     holding two series with the identical sample multiset and opposite
//     physical row order: order-dependent on a scan-order server,
//     deterministic but opposite to the fan-out on a NaN-loses server.
//
// Needs Docker; gated behind the `integration` build tag, run by the
// `ts-grid-nan-duplicate-integration` Justfile recipe. In-package so the sweep
// is driven by the unexported nativeTSGridFn registry and the fan-out probe
// executes the unexported dedupWindowPairsByTsFrag Frag itself.
package chsql

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	promparser "github.com/prometheus/prometheus/promql/parser"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// nanDupContract is the duplicate-timestamp rule a server build delivers.
type nanDupContract int

const (
	// contractScanOrder: the pre-#115920 fold, decided by encounter order
	// on a NaN-bearing duplicate.
	contractScanOrder nanDupContract = iota
	// contractNaNLoses: ClickHouse #115920 — greatest value wins, NaN loses.
	contractNaNLoses
)

func (c nanDupContract) String() string {
	if c == contractNaNLoses {
		return "NaN-loses"
	}
	return "scan-order"
}

// nanDupServer is one pinned build and the contract it was measured to
// deliver. The Justfile pre-pulls the same literals (CH_TEST_IMAGE,
// CH_TS_STATE_V3_IMAGE, CH_TS_STATE_V4_IMAGE), held equal by
// TestIntegrationImagePinsMatchTheJustfile.
type nanDupServer struct {
	image    string
	contract nanDupContract
}

var nanDupServers = []nanDupServer{
	// The timeSeries*ToGrid family's 25.9 registry floor.
	{"clickhouse/clickhouse-server:25.9-alpine", contractScanOrder},
	// The latest 26.7 build published as an image.
	{"clickhouse/clickhouse-server:26.7.13.12-alpine", contractScanOrder},
	// The first 26.8 release, the first build carrying #115920.
	{"clickhouse/clickhouse-server:26.8.1.2041-alpine", contractNaNLoses},
}

// The grid point every grid probe evaluates, as epoch seconds (2026-01-01
// 00:01:00 UTC) because an aggregate's parameters must be literals for the
// -Merge form arrayReduce names. nanDupDupTS carries the colliding samples;
// nanDupLaterTS carries a single unambiguous sample so every rate-like member
// has the >= 2 samples it needs to answer.
const (
	nanDupAnchor      = "2026-01-01 00:01:00"
	nanDupAnchorEpoch = 1767225660
	nanDupDupTS       = "2026-01-01 00:00:20"
	nanDupLaterTS     = "2026-01-01 00:00:50"
	nanDupStepSec     = 60
	nanDupWindowSec   = 60
	// nanDupStalenessSec is the resample member's staleness window: wide
	// enough that the duplicate is the sample it carries to the grid point.
	nanDupStalenessSec = 300

	nanDupFiniteValue  = 25.0
	nanDupGreaterValue = 30.0
	nanDupLaterValue   = 40.0

	// nanDupPredictOffsetSec is predict_linear's fifth parametric argument
	// (the forecast horizon). 600 matches the native predict_linear
	// fixtures.
	nanDupPredictOffsetSec = 600
)

// The sample values a collision is built from, as SQL expressions.
const (
	nanDupNaN = "nan"
	// nanDupStale is Prometheus's value.StaleNaN (0x7ff0000000000002).
	nanDupStale = "reinterpretAsFloat64(toUInt64(9218868437227405314))"
)

var (
	nanDupFinite  = nanDupFloatLit(nanDupFiniteValue)
	nanDupGreater = nanDupFloatLit(nanDupGreaterValue)
)

// nanDupFold is the side a scan-order fold lands a NaN on.
type nanDupFold int

const (
	// foldFirstVisited: a running-max fold. A NaN already holding the slot
	// is never replaced, and a NaN arriving later never replaces a finite
	// best — the FIRST-visited sample survives a NaN-bearing duplicate.
	foldFirstVisited nanDupFold = iota
	// foldLastVisited: a trailing-pair fold. The same false comparison lands
	// on the other side — the LAST-visited sample survives.
	foldLastVisited
)

// nanDupMember is one native aggregate cerberus emits.
type nanDupMember struct {
	// params is the parenthesised parameter list, or "" for a
	// non-parametric aggregate.
	params string
	// extraArgs follows the (ts, val) arguments: the -If combinator's
	// predicate.
	extraArgs string
	// ts renders a timestamp literal in the type the aggregate is fed.
	ts func(string) string
	// withLater appends the unambiguous later sample. The resample member
	// omits it: it answers the latest sample, which would hide the
	// duplicate.
	withLater bool
	fold      nanDupFold
	// observable declares that the answer reveals which sample survived, on
	// every pinned server. A member not declared observable is still asserted
	// against its contract; the declaration only stops an observable member
	// from silently becoming uninformative.
	observable bool
}

func nanDupDateTime(s string) string   { return "toDateTime('" + s + "', 'UTC')" }
func nanDupDateTime64(s string) string { return "toDateTime64('" + s + "', 9, 'UTC')" }

func nanDupGridParams(extra string) string {
	return fmt.Sprintf("(%d, %d, %d, %d%s)", nanDupAnchorEpoch, nanDupAnchorEpoch, nanDupStepSec, nanDupWindowSec, extra)
}

// nanDupRegistryMembers covers every nativeTSGridFn entry; the sweep fails
// when the registry holds a function this map does not, so a member added
// later cannot ship unprobed.
var nanDupRegistryMembers = map[string]nanDupMember{
	"rate":     {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldFirstVisited, observable: true},
	"increase": {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldFirstVisited, observable: true},
	"delta":    {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldFirstVisited, observable: true},
	"deriv":    {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldFirstVisited, observable: true},
	"predict_linear": {
		params: nanDupGridParams(fmt.Sprintf(", %d", nanDupPredictOffsetSec)), ts: nanDupDateTime,
		withLater: true, fold: foldFirstVisited, observable: true,
	},
	// changes: a NaN-then-finite window counts one change more than a
	// finite-then-finite one only on builds before 26.7 (the leading-NaN
	// overcount chopt.FeatureTSGridChanges documents); from 26.7 on both
	// count one, so the survivor is not visible on every pinned server.
	"changes": {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldFirstVisited},
	// resets: a reset needs curr < prev, and every comparison against a NaN
	// is false, so every survivor counts zero.
	"resets": {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldFirstVisited},
	"irate":  {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldLastVisited, observable: true},
	"idelta": {params: nanDupGridParams(""), ts: nanDupDateTime, withLater: true, fold: foldLastVisited, observable: true},
}

// nanDupOtherMembers are the native aggregates cerberus emits outside the
// nativeTSGridFn registry, keyed by aggregate name.
var nanDupOtherMembers = map[string]nanDupMember{
	// The bare-selector resample (chopt.FeatureTSGridResample and
	// chopt.FeatureTSGridLastOverTime).
	nativeResampleFn: {
		params: fmt.Sprintf("(%d, %d, %d, %d)", nanDupAnchorEpoch, nanDupAnchorEpoch, nanDupStepSec, nanDupStalenessSec),
		ts:     nanDupDateTime64, fold: foldFirstVisited, observable: true,
	},
	// The persisted downsample tier's state (schema.DownsampleTierSamplesColumn).
	"timeSeriesLastTwoSamples": {ts: nanDupDateTime64, withLater: true, fold: foldLastVisited, observable: true},
	// The window-pairs assembly (chopt.FeatureTSGridGroupArray), and its
	// split-window -If form (nativeGroupArrayPairIfFrag) with an
	// always-true predicate.
	"timeSeriesGroupArray": {ts: nanDupDateTime64, withLater: true, fold: foldFirstVisited, observable: true},
	"timeSeriesGroupArrayIf": {
		extraArgs: ", toUInt8(1)", ts: nanDupDateTime64, withLater: true, fold: foldFirstVisited, observable: true,
	},
}

// nanDupCollision is one duplicate-timestamp shape: the two values that share
// nanDupDupTS.
type nanDupCollision struct {
	name string
	a, b string
}

var nanDupCollisions = []nanDupCollision{
	{"nan vs finite", nanDupNaN, nanDupFinite},
	{"stale vs finite", nanDupStale, nanDupFinite},
	{"nan vs nan", nanDupNaN, nanDupNaN},
	{"nan vs stale", nanDupNaN, nanDupStale},
	{"unequal finite", nanDupFinite, nanDupGreater},
}

func nanDupIsNaN(v string) bool { return v == nanDupNaN || v == nanDupStale }

// nanDupExpected is the sample the contract elects when first and second
// reach the collapse in that order (rows fed, or states merged). For an
// all-NaN collision it returns first: every NaN answers the same, which the
// sweep asserts separately.
func nanDupExpected(contract nanDupContract, fold nanDupFold, first, second string) string {
	switch {
	case !nanDupIsNaN(first) && !nanDupIsNaN(second):
		a, _ := strconv.ParseFloat(first, 64)
		b, _ := strconv.ParseFloat(second, 64)
		if a >= b {
			return first
		}
		return second
	case nanDupIsNaN(first) && nanDupIsNaN(second):
		return first
	case contract == contractNaNLoses:
		if nanDupIsNaN(first) {
			return second
		}
		return first
	case fold == foldFirstVisited:
		return first
	default:
		return second
	}
}

// nanDupConnect boots a ClickHouse at image and returns a pooled handle.
func nanDupConnect(ctx context.Context, t *testing.T, image string) *sql.DB {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		image,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse %s: %v", image, err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{host + ":" + port.Port()},
		Auth: clickhouse.Auth{
			Database: "otel",
			Username: "cerberus",
			Password: "cerberus",
		},
	})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

// nanDupServerTimeout bounds one server's boot plus its whole sweep.
const nanDupServerTimeout = 10 * time.Minute

// forEachNaNDupServer runs fn against every pinned server in parallel.
func forEachNaNDupServer(t *testing.T, fn func(ctx context.Context, t *testing.T, db *sql.DB, server nanDupServer)) {
	t.Helper()
	for _, server := range nanDupServers {
		t.Run(server.image, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), nanDupServerTimeout)
			defer cancel()
			fn(ctx, t, nanDupConnect(ctx, t, server.image), server)
		})
	}
}

// nanDupFloatLit renders a Float64 SQL literal. A bare `%v` renders 25.0 as
// `25`, which ClickHouse types UInt8 — and the timeSeries* family rejects a
// non-Float64 value argument outright (ILLEGAL_TYPE_OF_ARGUMENT), so the
// fractional digit is load-bearing rather than cosmetic.
func nanDupFloatLit(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// nanDupSampleRows renders the UNION ALL row source for one member.
// valuesAtDupTS are the samples sharing nanDupDupTS, IN ENCOUNTER ORDER,
// followed by the later sample when the member takes one.
func nanDupSampleRows(m nanDupMember, valuesAtDupTS ...string) string {
	parts := make([]string, 0, len(valuesAtDupTS)+1)
	for _, v := range valuesAtDupTS {
		parts = append(parts, fmt.Sprintf("SELECT %s AS ts, toFloat64(%s) AS val", m.ts(nanDupDupTS), v))
	}
	if m.withLater {
		parts = append(parts, fmt.Sprintf("SELECT %s AS ts, toFloat64(%s) AS val",
			m.ts(nanDupLaterTS), nanDupFloatLit(nanDupLaterValue)))
	}
	return "(" + strings.Join(parts, " UNION ALL ") + ")"
}

// nanDupScalar runs query with the experimental setting scoped to the
// statement: a pooled *sql.DB gives no guarantee a later query reuses the
// connection a session-level SET landed on. max_threads=1 keeps the UNION ALL
// arms in the order they are written.
func nanDupScalar(ctx context.Context, t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	full := query + " SETTINGS max_threads = 1, " + chclient.SettingExperimentalTSGridAggregate + " = 1"
	var got string
	if err := db.QueryRowContext(ctx, full).Scan(&got); err != nil {
		t.Fatalf("%v\nSQL: %s", err, full)
	}
	return got
}

// nanDupFeed answers agg over rows fed in the order given.
func nanDupFeed(ctx context.Context, t *testing.T, db *sql.DB, agg string, m nanDupMember, values ...string) string {
	t.Helper()
	return nanDupScalar(ctx, t, db, fmt.Sprintf("SELECT toString(%s%s(ts, val%s)) FROM %s",
		agg, m.params, m.extraArgs, nanDupSampleRows(m, values...)))
}

// nanDupMerge builds one partial state holding first (plus the later sample)
// and one holding second, and answers their merge in that order — the order a
// Distributed initiator or the deferred-shaping recollapse merges states in.
func nanDupMerge(ctx context.Context, t *testing.T, db *sql.DB, agg string, m nanDupMember, first, second string) string {
	t.Helper()
	stateOf := func(rows string) string {
		return fmt.Sprintf("(SELECT %sState%s(ts, val%s) FROM %s)", agg, m.params, m.extraArgs, rows)
	}
	onlyDup := m
	onlyDup.withLater = false
	return nanDupScalar(ctx, t, db, fmt.Sprintf("SELECT toString(arrayReduce('%sMerge%s', [%s, %s]))",
		agg, m.params, stateOf(nanDupSampleRows(m, first)), stateOf(nanDupSampleRows(onlyDup, second))))
}

// TestTSGridFamily_DuplicateSurvivor_RealCH asserts, for every native member
// and every pinned server, that each collision's survivor is the one the
// server's contract elects — under both insertion orders and both
// partial-state merge orders.
//
// The survivor is read off the answer by comparing it with the answer the
// same member gives over the window holding each candidate alone, so a case
// asserts which SAMPLE survived rather than transcribing a float.
func TestTSGridFamily_DuplicateSurvivor_RealCH(t *testing.T) {
	if len(nativeTSGridFn) == 0 {
		t.Fatal("nativeTSGridFn is empty — the ratchet below would vacuously pass")
	}
	members := map[string]nanDupMember{}
	for fn, agg := range nativeTSGridFn {
		m, ok := nanDupRegistryMembers[fn]
		if !ok {
			t.Errorf("nativeTSGridFn registers %q but nanDupRegistryMembers has no entry for it — every native "+
				"member must have its duplicate-timestamp survivor measured", fn)
			continue
		}
		members[agg.Fn] = m
	}
	for fn := range nanDupRegistryMembers {
		if _, ok := nativeTSGridFn[fn]; !ok {
			t.Errorf("nanDupRegistryMembers declares %q, which nativeTSGridFn does not register", fn)
		}
	}
	for agg, m := range nanDupOtherMembers {
		members[agg] = m
	}
	aggs := make([]string, 0, len(members))
	for agg := range members {
		aggs = append(aggs, agg)
	}
	sort.Strings(aggs)

	forEachNaNDupServer(t, func(ctx context.Context, t *testing.T, db *sql.DB, server nanDupServer) {
		orderDependent := 0
		for _, agg := range aggs {
			m := members[agg]
			alone := map[string]string{}
			for _, v := range []string{nanDupNaN, nanDupStale, nanDupFinite, nanDupGreater} {
				alone[v] = nanDupFeed(ctx, t, db, agg, m, v)
			}
			if alone[nanDupNaN] != alone[nanDupStale] {
				t.Errorf("%s answers a stale-marker payload (%s) differently from an ordinary NaN (%s)",
					agg, alone[nanDupStale], alone[nanDupNaN])
				continue
			}
			if m.observable && (alone[nanDupNaN] == alone[nanDupFinite] || alone[nanDupFinite] == alone[nanDupGreater]) {
				t.Errorf("%s is declared observable, but its single-sample answers do not tell the candidates "+
					"apart (nan=%s finite=%s greater=%s)", agg, alone[nanDupNaN], alone[nanDupFinite], alone[nanDupGreater])
				continue
			}
			for _, c := range nanDupCollisions {
				for _, order := range [][2]string{{c.a, c.b}, {c.b, c.a}} {
					want := alone[nanDupExpected(server.contract, m.fold, order[0], order[1])]
					if got := nanDupFeed(ctx, t, db, agg, m, order[0], order[1]); got != want {
						t.Errorf("%s fed %s then %s (%s): got %s, want %s under the %s contract",
							agg, order[0], order[1], c.name, got, want, server.contract)
					}
					if got := nanDupMerge(ctx, t, db, agg, m, order[0], order[1]); got != want {
						t.Errorf("%s merged %s's state then %s's (%s): got %s, want %s under the %s contract",
							agg, order[0], order[1], c.name, got, want, server.contract)
					}
				}
			}
			if m.observable && nanDupExpected(server.contract, m.fold, nanDupNaN, nanDupFinite) !=
				nanDupExpected(server.contract, m.fold, nanDupFinite, nanDupNaN) {
				orderDependent++
			}
		}
		// The two contracts must actually differ where they claim to: a
		// scan-order server shows order dependence on its observable members,
		// and a NaN-loses server shows none.
		if server.contract == contractScanOrder && orderDependent == 0 {
			t.Errorf("%s is pinned as scan-order, but no observable member is order dependent", server.image)
		}
		if server.contract == contractNaNLoses && orderDependent != 0 {
			t.Errorf("%s is pinned as NaN-loses, but %d observable members are order dependent", server.image, orderDependent)
		}
	})
}

// TestFanoutDedup_DuplicateSurvivorIsOrderIndependent_RealCH executes the
// PRODUCTION dedupWindowPairsByTsFrag Frag — rendered from the emitter's own
// constructor, not transcribed — on every pinned server, over both encounter
// orders of every collision, and asserts one survivor per collision: the
// greatest under ClickHouse's total order, in which NaN ranks greatest.
func TestFanoutDedup_DuplicateSurvivorIsOrderIndependent_RealCH(t *testing.T) {
	dedupSQL, err := Render(dedupWindowPairsByTsFrag(
		Call("arraySort", Call("groupArray", Tuple(Col("ts"), Col("val")))),
	))
	if err != nil {
		t.Fatalf("render dedupWindowPairsByTsFrag: %v", err)
	}
	rowsOnly := nanDupMember{ts: nanDupDateTime64, withLater: true}

	forEachNaNDupServer(t, func(ctx context.Context, t *testing.T, db *sql.DB, _ nanDupServer) {
		run := func(values ...string) string {
			return nanDupScalar(ctx, t, db, fmt.Sprintf("SELECT toString(%s) FROM %s",
				dedupSQL, nanDupSampleRows(rowsOnly, values...)))
		}
		for _, c := range nanDupCollisions {
			ab, ba := run(c.a, c.b), run(c.b, c.a)
			if ab != ba {
				t.Errorf("%s: dedupWindowPairsByTsFrag is insertion-order DEPENDENT: %q vs %q", c.name, ab, ba)
				continue
			}
			// The total order ranks NaN greatest, so the NaN — or, for two
			// finite values, the greater one — is the survivor.
			survivor := c.b
			if nanDupIsNaN(c.a) {
				survivor = c.a
			}
			if want := run(survivor); ab != want {
				t.Errorf("%s: kept %q, want %q — one sample per timestamp, the total order's greatest", c.name, ab, want)
			}
		}
	})
}

// nanDupMetricsDDL is the OTel-CH sum table the end-to-end case seeds. Only
// the columns the default schema's rate() lowering reads.
const nanDupMetricsDDL = `
CREATE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64,
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix)
`

// The three series the end-to-end case seeds. nanFirst and nanSecond hold the
// IDENTICAL sample multiset and differ only in the physical order of the two
// rows sharing one timestamp; plain has no duplicate at all, so a lowering
// that silently stopped producing rows cannot pass this test vacuously.
const (
	nanDupJobNaNFirst  = "nanfirst"
	nanDupJobNaNSecond = "nansecond"
	nanDupJobPlain     = "plain"
)

// TestRate_NativeGrid_NaNDuplicate_AgainstFanout_RealCH runs cerberus's OWN
// emitted SQL for `rate(requests_total[1m])` down both lowerings, on every
// pinned server, over one table holding two series with the identical sample
// multiset in opposite physical order:
//
//   - the fan-out answers both series NaN on every server — its survivor is a
//     function of the multiset, and the total order ranks NaN greatest;
//   - on a scan-order server the native path answers NaN only where the NaN
//     row is physically first, so it disagrees with itself as well as with
//     the fan-out;
//   - on a NaN-loses server the native path answers both series with the
//     finite survivor's rate — deterministic, and opposite to the fan-out.
//
// The control series pins that both lowerings agree where no duplicate exists.
func TestRate_NativeGrid_NaNDuplicate_AgainstFanout_RealCH(t *testing.T) {
	anchor, err := time.Parse(time.DateTime, nanDupAnchor)
	if err != nil {
		t.Fatalf("parse anchor: %v", err)
	}
	forEachNaNDupServer(t, func(ctx context.Context, t *testing.T, db *sql.DB, server nanDupServer) {
		if _, err := db.ExecContext(ctx, nanDupMetricsDDL); err != nil {
			t.Fatalf("create table: %v", err)
		}
		seed := fmt.Sprintf(`
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('requests_total', map('job', '%[1]s'), toDateTime64('%[4]s', 9), nan),
    ('requests_total', map('job', '%[1]s'), toDateTime64('%[4]s', 9), %[6]s),
    ('requests_total', map('job', '%[1]s'), toDateTime64('%[5]s', 9), %[7]s),
    ('requests_total', map('job', '%[2]s'), toDateTime64('%[4]s', 9), %[6]s),
    ('requests_total', map('job', '%[2]s'), toDateTime64('%[4]s', 9), nan),
    ('requests_total', map('job', '%[2]s'), toDateTime64('%[5]s', 9), %[7]s),
    ('requests_total', map('job', '%[3]s'), toDateTime64('%[4]s', 9), %[6]s),
    ('requests_total', map('job', '%[3]s'), toDateTime64('%[5]s', 9), %[7]s)
`, nanDupJobNaNFirst, nanDupJobNaNSecond, nanDupJobPlain,
			nanDupDupTS, nanDupLaterTS, nanDupFinite, nanDupFloatLit(nanDupLaterValue))
		if _, err := db.ExecContext(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}

		fanout := nanDupRunRate(ctx, t, db, anchor, false)
		native := nanDupRunRate(ctx, t, db, anchor, true)
		for _, job := range []string{nanDupJobNaNFirst, nanDupJobNaNSecond, nanDupJobPlain} {
			if _, ok := fanout[job]; !ok {
				t.Fatalf("fan-out returned no row for job=%s — the fixture is broken", job)
			}
			if _, ok := native[job]; !ok {
				t.Fatalf("native returned no row for job=%s — the fixture is broken", job)
			}
		}

		// The control: no duplicate timestamp, so both lowerings must agree.
		// It holds exactly the two samples a finite survivor leaves, so its
		// rate is the finite-survivor answer below.
		finiteRate := native[nanDupJobPlain]
		if f := fanout[nanDupJobPlain]; math.IsNaN(finiteRate) || math.Abs(f-finiteRate) > 1e-9 {
			t.Fatalf("job=%s carries no duplicate timestamp yet native=%v and fan-out=%v disagree",
				nanDupJobPlain, finiteRate, f)
		}

		if f1, f2 := fanout[nanDupJobNaNFirst], fanout[nanDupJobNaNSecond]; !math.IsNaN(f1) || !math.IsNaN(f2) {
			t.Fatalf("fan-out answered job=%s %v and job=%s %v — dedupWindowPairsByTsFrag elects the NaN "+
				"(the total order ranks it greatest), so both windows answer NaN",
				nanDupJobNaNFirst, f1, nanDupJobNaNSecond, f2)
		}

		wantFirst, wantSecond := finiteRate, finiteRate
		if server.contract == contractScanOrder {
			// rate's fold keeps a FIRST-visited NaN.
			wantFirst = math.NaN()
		}
		for job, want := range map[string]float64{nanDupJobNaNFirst: wantFirst, nanDupJobNaNSecond: wantSecond} {
			got := native[job]
			if math.IsNaN(got) != math.IsNaN(want) || (!math.IsNaN(want) && math.Abs(got-want) > 1e-9) {
				t.Errorf("native job=%s = %v, want %v under the %s contract", job, got, want, server.contract)
			}
		}
	})
}

// nanDupRunRate lowers and emits `rate(requests_total[1m])` as a single-point
// range query at anchor, down the native timeSeries*ToGrid lowering or the
// fan-out, and returns the per-job value. A range query rather than the
// instant shape: the native lowering is a query_range strategy, so an instant
// query would silently fall back to the fan-out.
func nanDupRunRate(ctx context.Context, t *testing.T, db *sql.DB, anchor time.Time, native bool) map[string]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(fmt.Sprintf("rate(requests_total[%ds])", nanDupWindowSec))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var lowerers promql.RangeLowerers
	if native {
		lowerers.Rate = promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}}
	}
	plan, err := promql.LowerAtRangeOpts(ctx, expr, nanDupSchema(),
		anchor, anchor, time.Duration(nanDupStepSec)*time.Second,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	sqlStr, args, err := Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	if emitted := strings.Contains(sqlStr, nativeTSGridFn["rate"].Fn); emitted != native {
		t.Fatalf("native=%v but the emitted SQL names %s: %v — the differential would compare one lowering "+
			"with itself:\n%s", native, nativeTSGridFn["rate"].Fn, emitted, sqlStr)
	}

	wrapped := fmt.Sprintf(
		"SELECT toJSONString(`Attributes`) AS job_json, `Value` FROM (%s) SETTINGS %s = 1",
		sqlStr, chclient.SettingExperimentalTSGridAggregate,
	)
	rows, err := db.QueryContext(ctx, wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]float64{}
	for rows.Next() {
		var jobJSON string
		var v float64
		if err := rows.Scan(&jobJSON, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[nanDupJobLabel(jobJSON)] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	return out
}

// nanDupSchema is the default OTel-CH metrics schema with the
// AggregationTemporality column cleared: a schema that declares it forces
// every rate() window off the native path (see nativeTSGridMatrixNode), which
// would make the native arm above silently the fan-out.
func nanDupSchema() schema.Metrics {
	s := schema.DefaultOTelMetrics()
	s.AggregationTemporalityColumn = ""
	return s
}

// nanDupJobLabel pulls the job value out of the JSON-encoded Attributes map
// (`{"job":"a"}`).
func nanDupJobLabel(jsonStr string) string {
	const key = `"job":"`
	i := strings.Index(jsonStr, key)
	if i < 0 {
		return ""
	}
	rest := jsonStr[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return rest
	}
	return rest[:j]
}
