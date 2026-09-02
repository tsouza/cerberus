//go:build integration

// Real-ClickHouse characterisation of the duplicate-timestamp SURVIVOR the
// native timeSeries*ToGrid family actually elects, and of the survivor the
// array-fold fan-out elects for the same rows (cerberus issue #2798).
//
// # What this pins, and why it is a class-level suite rather than one probe
//
// Cerberus states ONE duplicate-timestamp rule for every range function
// (cerberus issue #2914, PR #2920), implemented by dedupWindowPairsByTsFrag
// and stated in full on that function's doc: one sample per distinct
// (series, timestamp), tie-broken to the max value under ClickHouse's total
// order over Float64 — the order in which NaN ranks GREATEST.
//
// The native family cannot keep the second half of that rule. Its own
// documented rule is the opposite one ("a NaN value loses to any other
// value"), and it delivers NEITHER deterministically: the collapse is a
// running "replace the current best only when the candidate compares
// greater" fold, and IEEE754 makes every comparison against a NaN false, so
// the survivor is decided by which row the scan happens to visit first.
//
// Three properties are therefore pinned here, all against a real server at
// the family's own 25.9 registry floor (chopt.FeatureTSGridRange and
// siblings), because the finding is a property of the ClickHouse builtin and
// no amount of cerberus-side reasoning can establish it:
//
//  1. TestTSGridFamily_NaNDuplicateSurvivorIsOrderDependent_RealCH — for
//     EVERY member of the emitter's own nativeTSGridFn registry, which
//     sample survives a NaN-bearing duplicate under each of the two
//     encounter orders. The case list is ratcheted against that registry, so
//     a family member added later cannot ship unprobed.
//  2. TestFanoutDedup_NaNDuplicateSurvivorIsOrderIndependent_RealCH — the
//     production dedupWindowPairsByTsFrag Frag itself, rendered and executed,
//     elects the SAME survivor under both encounter orders. This is the half
//     of the contract cerberus does deliver, and it is what makes the
//     divergence a divergence rather than two equally unspecified paths.
//  3. TestRate_NativeGrid_NaNDuplicate_DivergesFromFanout_RealCH — the gap
//     end to end through cerberus's own lowering and emitter, over one
//     MergeTree table holding two series with the IDENTICAL sample multiset
//     and opposite physical row order.
//
// These tests are a characterisation of third-party behaviour, so their
// direction of failure is the point: they go red when ClickHouse's fold
// changes. A red here is the signal to re-derive nativeTSGridFn's and
// chopt.FeatureTSGrid*'s posture docs against the new behaviour — and, if
// the fold became order-independent, to close #2798.
//
// Needs a real ClickHouse >= 25.9 and Docker; gated behind the `integration`
// build tag, run by the `ts-grid-nan-duplicate-integration` Justfile recipe.
// In-package (`package chsql`, unlike its `chsql_test` sibling
// range_window_group_array_realch_integration_test.go) precisely so the
// family sweep can be driven by the unexported nativeTSGridFn registry and
// the fan-out probe can execute the unexported dedupWindowPairsByTsFrag Frag
// itself rather than a hand-copied transcription of either.
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

// nanDupImage pins a ClickHouse at the timeSeries*ToGrid family's own 25.9
// registry floor (chopt.FeatureTSGridRange and siblings). A plain literal
// rather than an import of internal/chopt, mirroring tsGridGroupArrayImage's
// rationale in the sibling integration file: this suite's only dependency on
// the family's floor is the image tag its container runs.
const nanDupImage = "clickhouse/clickhouse-server:25.9-alpine"

// The one grid point every probe evaluates, and the two samples that share a
// timestamp inside its window. nanDupDupTS carries BOTH nanDupFiniteValue and
// a NaN; nanDupLaterTS carries a single unambiguous sample, so every
// aggregate has the >= 2 samples it needs to return non-NULL.
const (
	nanDupAnchor    = "2026-01-01 00:01:00"
	nanDupDupTS     = "2026-01-01 00:00:20"
	nanDupLaterTS   = "2026-01-01 00:00:50"
	nanDupStepSec   = 60
	nanDupWindowSec = 60

	nanDupFiniteValue = 25.0
	nanDupLaterValue  = 40.0

	// nanDupPredictOffsetSec is predict_linear's fifth parametric argument
	// (the forecast horizon). Any whole-second horizon works; 600 matches
	// the horizon the native predict_linear fixtures already use.
	nanDupPredictOffsetSec = 600
)

// nanDupSurvivor names which of the two samples sharing nanDupDupTS won the
// collapse. It is derived from the aggregate's answer by comparing it against
// the two duplicate-free baselines the same aggregate returns over the same
// window — so a case declares a statement about the DEDUP CONTRACT ("the NaN
// survived") rather than transcribing whatever float the arithmetic happens
// to produce.
type nanDupSurvivor int

const (
	// survivorUnobservable marks an aggregate whose two duplicate-free
	// baselines are equal, so its answer cannot reveal which sample won.
	survivorUnobservable nanDupSurvivor = iota
	survivorNaN
	survivorFinite
)

func (s nanDupSurvivor) String() string {
	switch s {
	case survivorNaN:
		return "NaN"
	case survivorFinite:
		return "finite"
	default:
		return "unobservable"
	}
}

// nanDupCase declares, for one nativeTSGridFn member, the extra parametric
// arguments its aggregate takes and the survivor it elects under each of the
// two encounter orders.
type nanDupCase struct {
	// extraParams is appended to the shared
	// `(start, end, step_s, window_s)` parameter list. Empty for every
	// member except predict_linear, whose fifth argument is its horizon.
	extraParams string
	// nanFirstSurvivor / nanSecondSurvivor are the samples that win when the
	// NaN row is fed to the aggregate before / after the finite row.
	nanFirstSurvivor  nanDupSurvivor
	nanSecondSurvivor nanDupSurvivor
	// why records what makes this member's outcome what it is, so a future
	// red carries its own diagnosis.
	why string
}

// orderDependent reports whether the two encounter orders elect different
// samples. Derived rather than declared: a case that declared BOTH the two
// survivors and a redundant "is it order dependent" flag could disagree with
// itself.
func (c nanDupCase) orderDependent() bool {
	return c.nanFirstSurvivor != c.nanSecondSurvivor
}

// nanDupCases covers every nativeTSGridFn member. The sweep below fails when
// the registry holds a function this map does not, which is what makes the
// coverage a ratchet rather than a snapshot.
//
// Two sub-groups fall out of the measurements, and the split is the
// substantive finding — the family does NOT fold uniformly:
//
//   - The whole-window members (rate / increase / delta / changes / resets /
//     deriv / predict_linear) keep the FIRST-visited sample when it is a NaN:
//     nothing compares greater than a NaN, so a NaN that is already the
//     current best can never be replaced, and a NaN arriving later can never
//     replace a finite current best.
//   - The instant members (irate / idelta) invert it: they keep the
//     LAST-visited sample when it is a NaN. Their fold reduces the window to
//     its trailing pair rather than sweeping a running maximum, so the same
//     false comparison lands on the opposite side.
//
// Neither sub-group matches the family's own documented "a NaN value loses to
// any other value" rule, which would require survivorFinite in BOTH columns
// for every member.
var nanDupCases = map[string]nanDupCase{
	"rate": {
		nanFirstSurvivor:  survivorNaN,
		nanSecondSurvivor: survivorFinite,
		why:               "running max fold; a NaN current best is never replaced and never replaces",
	},
	"increase": {
		nanFirstSurvivor:  survivorNaN,
		nanSecondSurvivor: survivorFinite,
		why:               "shares timeSeriesRateToGrid with rate, so it shares rate's fold exactly",
	},
	"changes": {
		nanFirstSurvivor:  survivorNaN,
		nanSecondSurvivor: survivorFinite,
		why:               "running max fold; the surviving sample shifts the transition count",
	},
	"resets": {
		nanFirstSurvivor:  survivorUnobservable,
		nanSecondSurvivor: survivorUnobservable,
		why: "a counter reset needs curr < prev, and every comparison against a NaN is false, " +
			"so both survivors yield the same zero count and the fold is not observable through the answer",
	},
	"deriv": {
		nanFirstSurvivor:  survivorNaN,
		nanSecondSurvivor: survivorFinite,
		why:               "running max fold; the surviving sample enters the least-squares fit",
	},
	"predict_linear": {
		extraParams:       fmt.Sprintf(", %d", nanDupPredictOffsetSec),
		nanFirstSurvivor:  survivorNaN,
		nanSecondSurvivor: survivorFinite,
		why:               "running max fold; the surviving sample enters the same least-squares fit deriv uses",
	},
	"delta": {
		nanFirstSurvivor:  survivorNaN,
		nanSecondSurvivor: survivorFinite,
		why:               "running max fold; the surviving sample is one end of the differenced pair",
	},
	"irate": {
		nanFirstSurvivor:  survivorFinite,
		nanSecondSurvivor: survivorNaN,
		why:               "trailing-pair fold, INVERTED against the whole-window members: the LAST-visited NaN wins",
	},
	"idelta": {
		nanFirstSurvivor:  survivorFinite,
		nanSecondSurvivor: survivorNaN,
		why:               "trailing-pair fold, inverted for the same reason irate's is",
	},
}

// nanDupConnect boots a ClickHouse at nanDupImage and returns a pooled
// handle. A local copy of the sibling file's realCHConnect rather than a
// shared helper: that one lives in `package chsql_test` and this suite must
// be in-package to reach nativeTSGridFn and dedupWindowPairsByTsFrag.
func nanDupConnect(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		nanDupImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

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

// nanDupFloatLit renders a Float64 SQL literal. A bare `%v` renders 25.0 as
// `25`, which ClickHouse types UInt8 — and the timeSeries*ToGrid family
// rejects a non-Float64 value argument outright (ILLEGAL_TYPE_OF_ARGUMENT),
// so the fractional digit is load-bearing rather than cosmetic.
func nanDupFloatLit(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// nanDupSampleRows renders the UNION ALL row source the sweep feeds one
// aggregate. valuesAtDupTS are the samples sharing nanDupDupTS, IN ENCOUNTER
// ORDER — the whole point of the probe — followed by the unambiguous
// nanDupLaterTS sample.
func nanDupSampleRows(valuesAtDupTS ...string) string {
	parts := make([]string, 0, len(valuesAtDupTS)+1)
	for i, v := range valuesAtDupTS {
		if i == 0 {
			parts = append(parts, fmt.Sprintf(
				"SELECT toDateTime('%s') AS ts, %s AS val", nanDupDupTS, v,
			))
			continue
		}
		parts = append(parts, fmt.Sprintf(
			"SELECT toDateTime('%s'), %s", nanDupDupTS, v,
		))
	}
	parts = append(parts, fmt.Sprintf(
		"SELECT toDateTime('%s'), %s", nanDupLaterTS, nanDupFloatLit(nanDupLaterValue),
	))
	return "(" + strings.Join(parts, " UNION ALL ") + ")"
}

// nanDupGrid runs one family member over rows and returns its grid as a
// string. The experimental setting is scoped to the statement rather than the
// session: a pooled *sql.DB gives no guarantee a later query reuses the
// connection a session-level SET landed on.
func nanDupGrid(ctx context.Context, t *testing.T, db *sql.DB, agg, extraParams, rows string) string {
	t.Helper()
	query := fmt.Sprintf(
		"SELECT toString(%s(toDateTime('%s'), toDateTime('%s'), %d, %d%s)(ts, val)) FROM %s SETTINGS %s = 1",
		agg, nanDupAnchor, nanDupAnchor, nanDupStepSec, nanDupWindowSec, extraParams, rows,
		chclient.SettingExperimentalTSGridAggregate,
	)
	var got string
	if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
		t.Fatalf("%s: %v\nSQL: %s", agg, err, query)
	}
	return got
}

// TestTSGridFamily_NaNDuplicateSurvivorIsOrderDependent_RealCH measures, for
// every nativeTSGridFn member, which sample survives a NaN-bearing duplicate
// timestamp under each encounter order — the finding cerberus issue #2798
// tracks, established here family-wide rather than inferred from the two
// members #2746's chDB sweep happened to probe.
//
// The survivor is DERIVED, not transcribed: each member is first run over the
// two duplicate-free windows (the NaN alone at the shared timestamp, then the
// finite sample alone), and the duplicate answer is matched against those two
// baselines. A member whose baselines coincide is declared unobservable and
// asserted to be so, which is what stops `resets` from silently counting as
// evidence of order-independence.
func TestTSGridFamily_NaNDuplicateSurvivorIsOrderDependent_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db := nanDupConnect(ctx, t)

	if len(nativeTSGridFn) == 0 {
		t.Fatal("nativeTSGridFn is empty — the ratchet below would vacuously pass")
	}
	for fn := range nativeTSGridFn {
		if _, ok := nanDupCases[fn]; !ok {
			t.Errorf("nativeTSGridFn registers %q but nanDupCases has no case for it — every native "+
				"family member must have its duplicate-timestamp survivor measured (cerberus issue #2798)", fn)
		}
	}
	for fn := range nanDupCases {
		if _, ok := nativeTSGridFn[fn]; !ok {
			t.Errorf("nanDupCases declares %q, which nativeTSGridFn does not register — a stale case "+
				"would keep asserting against an aggregate cerberus no longer emits", fn)
		}
	}

	names := make([]string, 0, len(nanDupCases))
	for fn := range nanDupCases {
		names = append(names, fn)
	}
	sort.Strings(names)

	orderDependentMembers := 0
	for _, fn := range names {
		c := nanDupCases[fn]
		agg, ok := nativeTSGridFn[fn]
		if !ok {
			continue // already reported above
		}
		t.Run(fn, func(t *testing.T) {
			nanOnly := nanDupGrid(ctx, t, db, agg.Fn, c.extraParams,
				nanDupSampleRows("nan"))
			finiteOnly := nanDupGrid(ctx, t, db, agg.Fn, c.extraParams,
				nanDupSampleRows(nanDupFloatLit(nanDupFiniteValue)))

			classify := func(got string) nanDupSurvivor {
				if nanOnly == finiteOnly {
					return survivorUnobservable
				}
				switch got {
				case nanOnly:
					return survivorNaN
				case finiteOnly:
					return survivorFinite
				default:
					t.Fatalf("%s: duplicate answer %q matches NEITHER duplicate-free baseline "+
						"(nan-only=%q, finite-only=%q) — the collapse kept something other than one of "+
						"the two samples, which no reading of the documented rule allows",
						agg.Fn, got, nanOnly, finiteOnly)
					return survivorUnobservable
				}
			}

			if c.nanFirstSurvivor == survivorUnobservable || c.nanSecondSurvivor == survivorUnobservable {
				if nanOnly != finiteOnly {
					t.Fatalf("%s: case declares the survivor unobservable (%s), but the two "+
						"duplicate-free baselines DIFFER (nan-only=%q, finite-only=%q) — the answer "+
						"does reveal the survivor, so the case must declare which one it is",
						agg.Fn, c.why, nanOnly, finiteOnly)
				}
			} else if nanOnly == finiteOnly {
				t.Fatalf("%s: case declares survivors (%s / %s) the answer cannot distinguish — both "+
					"duplicate-free baselines are %q",
					agg.Fn, c.nanFirstSurvivor, c.nanSecondSurvivor, nanOnly)
			}

			nanFirst := classify(nanDupGrid(ctx, t, db, agg.Fn, c.extraParams,
				nanDupSampleRows("nan", nanDupFloatLit(nanDupFiniteValue))))
			nanSecond := classify(nanDupGrid(ctx, t, db, agg.Fn, c.extraParams,
				nanDupSampleRows(nanDupFloatLit(nanDupFiniteValue), "nan")))

			if nanFirst != c.nanFirstSurvivor {
				t.Errorf("%s: NaN fed FIRST — survivor is %s, case declares %s (%s)",
					agg.Fn, nanFirst, c.nanFirstSurvivor, c.why)
			}
			if nanSecond != c.nanSecondSurvivor {
				t.Errorf("%s: NaN fed SECOND — survivor is %s, case declares %s (%s)",
					agg.Fn, nanSecond, c.nanSecondSurvivor, c.why)
			}
			if got, want := nanFirst != nanSecond, c.orderDependent(); got != want {
				t.Errorf("%s: order dependence = %v, case implies %v — if ClickHouse's fold became "+
					"order-independent, cerberus issue #2798 can close and every doc citing it must be "+
					"re-derived", agg.Fn, got, want)
			}
		})
		if c.orderDependent() {
			orderDependentMembers++
		}
	}

	if orderDependentMembers == 0 {
		t.Fatal("no nativeTSGridFn member is declared order dependent — this suite exists to pin " +
			"cerberus issue #2798's finding, and would be vacuous with every case declared insensitive")
	}
}

// TestFanoutDedup_NaNDuplicateSurvivorIsOrderIndependent_RealCH executes the
// PRODUCTION dedupWindowPairsByTsFrag Frag — rendered from the emitter's own
// constructor, not transcribed — over the same two encounter orders, and
// asserts it elects the same survivor either way.
//
// This is the half of cerberus's duplicate-timestamp contract that IS
// deliverable, and it is what makes the native family's behaviour a
// divergence rather than two equally unspecified paths: `arraySort` orders
// Float64 under a total order in which NaN ranks greatest, so the
// last-of-run keep is a function of the sample multiset alone.
//
// It fails if dedupWindowPairsByTsFrag is ever swapped for a fold that
// inherits ClickHouse's IEEE754 comparison — including the self-deduping
// native timeSeriesGroupArray aggregate (chopt.FeatureTSGridGroupArray),
// whose AutoSelect: false posture rests on exactly this difference.
func TestFanoutDedup_NaNDuplicateSurvivorIsOrderIndependent_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db := nanDupConnect(ctx, t)

	dedupSQL, err := Render(dedupWindowPairsByTsFrag(
		Call("arraySort", Call("groupArray", Tuple(Col("ts"), Col("val")))),
	))
	if err != nil {
		t.Fatalf("render dedupWindowPairsByTsFrag: %v", err)
	}

	run := func(rows string) string {
		var got string
		query := fmt.Sprintf("SELECT toString(%s) FROM %s", dedupSQL, rows)
		if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
			t.Fatalf("dedup probe: %v\nSQL: %s", err, query)
		}
		return got
	}

	finite := nanDupFloatLit(nanDupFiniteValue)
	nanFirst := run(nanDupSampleRows("nan", finite))
	nanSecond := run(nanDupSampleRows(finite, "nan"))

	if nanFirst != nanSecond {
		t.Fatalf("dedupWindowPairsByTsFrag is insertion-order DEPENDENT: nan-first=%q nan-second=%q. "+
			"Cerberus's duplicate-timestamp contract (see the Frag's own doc) promises a survivor that "+
			"is a function of the sample multiset alone", nanFirst, nanSecond)
		return
	}
	if strings.Count(nanFirst, "(") != 2 {
		t.Fatalf("dedupWindowPairsByTsFrag kept %q — want exactly two tuples, one per distinct "+
			"timestamp (the cardinality half of the contract)", nanFirst)
	}
	if !strings.Contains(strings.ToLower(nanFirst), "nan") {
		t.Fatalf("dedupWindowPairsByTsFrag kept %q — want the NaN to survive: arraySort ranks NaN "+
			"greatest, so the last-of-run keep elects it, and that is the representative half of the "+
			"contract cerberus commits to", nanFirst)
	}
	if strings.Contains(nanFirst, finite) {
		t.Fatalf("dedupWindowPairsByTsFrag kept %q — the finite duplicate must NOT survive alongside "+
			"the NaN; both rows surviving is the over-count dedupWindowPairsByTsFrag exists to prevent",
			nanFirst)
	}
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

// TestRate_NativeGrid_NaNDuplicate_DivergesFromFanout_RealCH runs cerberus's
// OWN emitted SQL for `rate(requests_total[1m])` down both lowerings over one
// table holding two series with the identical sample multiset, and pins the
// gap cerberus issue #2798 tracks:
//
//   - the fan-out answers the two series IDENTICALLY (its survivor is a
//     function of the multiset), and
//   - the native grid path does NOT (its survivor is a function of physical
//     row order), so one of the two series gets an answer the contract does
//     not sanction.
//
// The control series pins that both lowerings agree where no duplicate
// timestamp exists at all, so the divergence cannot be an artefact of the two
// paths disagreeing generally.
func TestRate_NativeGrid_NaNDuplicate_DivergesFromFanout_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db := nanDupConnect(ctx, t)

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
		nanDupDupTS, nanDupLaterTS, nanDupFloatLit(nanDupFiniteValue), nanDupFloatLit(nanDupLaterValue))
	if _, err := db.ExecContext(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	anchor, err := time.Parse("2006-01-02 15:04:05", nanDupAnchor)
	if err != nil {
		t.Fatalf("parse anchor: %v", err)
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
	if f, n := fanout[nanDupJobPlain], native[nanDupJobPlain]; math.Abs(f-n) > 1e-9 {
		t.Fatalf("job=%s carries no duplicate timestamp yet native=%v and fan-out=%v disagree — the "+
			"divergence asserted below would not be attributable to the duplicate",
			nanDupJobPlain, n, f)
	}

	// The contract half cerberus delivers: the two duplicate-bearing series
	// hold the same sample multiset, so the fan-out must answer them the same.
	fFirst, fSecond := fanout[nanDupJobNaNFirst], fanout[nanDupJobNaNSecond]
	if !math.IsNaN(fFirst) || !math.IsNaN(fSecond) {
		t.Fatalf("fan-out answered job=%s %v and job=%s %v — cerberus's duplicate-timestamp contract "+
			"elects the NaN (arraySort ranks NaN greatest), so rate() over the surviving window is NaN "+
			"for both", nanDupJobNaNFirst, fFirst, nanDupJobNaNSecond, fSecond)
	}

	// The gap #2798 tracks: the native family's survivor follows physical row
	// order, so the two series answer differently.
	nFirst, nSecond := native[nanDupJobNaNFirst], native[nanDupJobNaNSecond]
	if math.IsNaN(nFirst) && math.IsNaN(nSecond) {
		t.Fatalf("the native timeSeries*ToGrid path answered both duplicate-bearing series NaN — "+
			"if ClickHouse's collapse became order-independent, cerberus issue #2798 can close and "+
			"every doc citing it must be re-derived (native: %s=%v %s=%v)",
			nanDupJobNaNFirst, nFirst, nanDupJobNaNSecond, nSecond)
	}
	if !math.IsNaN(nFirst) {
		t.Errorf("native job=%s = %v, want NaN: with the NaN row physically first, nothing compares "+
			"greater than it, so it survives the collapse", nanDupJobNaNFirst, nFirst)
	}
	if math.IsNaN(nSecond) {
		t.Errorf("native job=%s = NaN, want the finite-survivor answer: with the NaN row physically "+
			"second it can never displace the finite current best", nanDupJobNaNSecond)
	}
}

// nanDupRunRate lowers and emits `rate(requests_total[1m])` as an instant
// query at anchor, down the native timeSeries*ToGrid lowering or the fan-out,
// and returns the per-job value.
//
// An instant query rather than a range query: it evaluates exactly one grid
// point, so the answer is the survivor's contribution and nothing else, and
// the assertions above need no anchor bookkeeping.
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
	// A single-point range query rather than the instant shape: the native
	// timeSeries*ToGrid lowering is a query_range strategy, so an instant
	// query would silently fall back to the fan-out and compare the fan-out
	// with itself.
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
	if native && !strings.Contains(sqlStr, nativeTSGridFn["rate"].Fn) {
		t.Fatalf("native=true did not emit %s — the differential would compare the fan-out with "+
			"itself:\n%s", nativeTSGridFn["rate"].Fn, sqlStr)
	}
	if !native && strings.Contains(sqlStr, nativeTSGridFn["rate"].Fn) {
		t.Fatalf("native=false emitted %s — the fan-out arm is not the fan-out:\n%s",
			nativeTSGridFn["rate"].Fn, sqlStr)
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
// (`{"job":"a"}`). A local copy for the same reason nanDupConnect is one.
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
