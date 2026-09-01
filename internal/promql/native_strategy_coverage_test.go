package promql_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/test/spec"
)

// nativeStrategy binds ONE field of [promql.RangeLowerers] to the TXTAR
// marker section a fixture carries to opt into that field's
// ClickHouse-native strategy.
//
// The table this type builds is the spec lane's whole answer to a
// structural problem: `RangeLowerers` is a boot-wired dispatch table, and
// the TXTAR harness reaches only the strategy set some caller wired for
// it. A field with no row here is a strategy the golden corpus can never
// emit SQL for — its production shape is untested no matter how many
// fixtures exist, because every one of them lowers through the fan-out
// impl. That is the hollow-green shape where a feature turned OFF in the
// test lane lets the lane pass while proving nothing about the feature.
//
// So the table is not a convenience: it is the thing
// [TestNativeStrategies_CoverEveryRangeLowerersField] can reflect over.
// A hand-written `if` ladder cannot be enumerated, so a new field added
// without a matching arm would silently widen the hole. This can.
type nativeStrategy struct {
	// field is the RangeLowerers field this row wires, by name. The
	// completeness ratchet resolves it by reflection, so a typo or a
	// renamed field fails loudly rather than covering nothing.
	field string

	// section is the TXTAR marker section (any body, including empty) a
	// fixture carries to opt in.
	section string

	// wire sets this row's field on l to its native impl. It reads
	// `has` rather than the fixture itself so a strategy that composes
	// with a NESTED marker section can consult it — mirroring the boot
	// wiring in cmd/cerberus, where the same nesting decides the shape.
	wire func(l *promql.RangeLowerers, has func(section string) bool)
}

// nativeStrategies is the one place the spec lane's native-strategy
// wiring lives. Adding a field to [promql.RangeLowerers] without adding
// its row here fails [TestNativeStrategies_CoverEveryRangeLowerersField];
// adding a row whose section no fixture carries fails
// [TestNativeStrategies_HaveNativeSideFixtures].
var nativeStrategies = []nativeStrategy{
	{
		field:   "Rate",
		section: "experimental_ts_grid_range",
		wire: func(l *promql.RangeLowerers, has func(string) bool) {
			// `experimental_ts_grid_recollapse:` nests INSIDE this
			// section (mirroring the boot wiring in cmd/cerberus): the
			// two-stage label-shaping (-State/-Merge) shape only exists
			// on top of a native rate grid, so it is read only where one
			// is being built. `experimental_ts_grid_instant:` nests the
			// same way (cerberus issue #2748): the degenerate one-point
			// grid only exists on top of an already-eligible native rate
			// strategy.
			l.Rate = promql.NativeRateLowerer{
				Fallback:   promql.FanoutRateLowerer{},
				Recollapse: has("experimental_ts_grid_recollapse"),
				Instant:    has("experimental_ts_grid_instant"),
			}
		},
	},
	{
		field:   "Increase",
		section: "experimental_ts_grid_increase",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Increase = promql.NativeIncreaseLowerer{Fallback: promql.FanoutIncreaseLowerer{}}
		},
	},
	{
		field:   "Staleness",
		section: "experimental_ts_grid_resample",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}}
		},
	},
	{
		field:   "Changes",
		section: "experimental_ts_grid_changes",
		wire: func(l *promql.RangeLowerers, has func(string) bool) {
			// `experimental_ts_grid_instant:` nests INSIDE this section —
			// see the Rate row's own comment (cerberus issue #2748).
			l.Changes = promql.NativeChangesLowerer{
				Fallback: promql.FanoutChangesLowerer{},
				Instant:  has("experimental_ts_grid_instant"),
			}
		},
	},
	{
		field:   "Resets",
		section: "experimental_ts_grid_resets",
		wire: func(l *promql.RangeLowerers, has func(string) bool) {
			l.Resets = promql.NativeResetsLowerer{
				Fallback: promql.FanoutResetsLowerer{},
				Instant:  has("experimental_ts_grid_instant"),
			}
		},
	},
	{
		field:   "Deriv",
		section: "experimental_ts_grid_deriv",
		wire: func(l *promql.RangeLowerers, has func(string) bool) {
			l.Deriv = promql.NativeDerivLowerer{
				Fallback: promql.FanoutDerivLowerer{},
				Instant:  has("experimental_ts_grid_instant"),
			}
		},
	},
	{
		field:   "PredictLinear",
		section: "experimental_ts_grid_predict_linear",
		wire: func(l *promql.RangeLowerers, has func(string) bool) {
			l.PredictLinear = promql.NativePredictLinearLowerer{
				Fallback: promql.FanoutPredictLinearLowerer{},
				Instant:  has("experimental_ts_grid_instant"),
			}
		},
	},
	{
		field:   "Delta",
		section: "experimental_ts_grid_delta",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Delta = promql.NativeDeltaLowerer{Fallback: promql.FanoutDeltaLowerer{}}
		},
	},
	{
		field:   "Irate",
		section: "experimental_ts_grid_irate",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Irate = promql.NativeIrateLowerer{Fallback: promql.FanoutIrateLowerer{}}
		},
	},
	{
		field:   "Idelta",
		section: "experimental_ts_grid_idelta",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Idelta = promql.NativeIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}}
		},
	},
	{
		field:   "LastOverTime",
		section: "experimental_ts_grid_last_over_time",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.LastOverTime = promql.NativeLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}}
		},
	},
	{
		field:   "ClassicHistogram",
		section: "experimental_ts_grid_histogram",
		// Fallback chains straight to the bare fan-out — matching
		// cmd/cerberus's own nativeRangeLowerers chain exactly
		// (Native{Fallback: Fanout{}}). The window-slide link this row
		// used to route through was removed by #2511's root-cause
		// investigation (structural over-read of the base table — see
		// main.go's nativeRangeLowerers doc).
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.ClassicHistogram = promql.NativeClassicHistogramWindowLowerer{
				Fallback: promql.FanoutClassicHistogramWindowLowerer{},
			}
		},
	},
	{
		field:   "QuantileRankWalk",
		section: "experimental_quantile_prom_histogram",
		// No embedded Fallback: promql.QuantileRankWalkLowerer's own doc
		// explains why — every classic-histogram-quantile shape this
		// codebase builds is native-eligible, unlike every other row in
		// this table.
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.QuantileRankWalk = promql.NativeQuantileRankWalkLowerer{}
		},
	},
	{
		field:   "OverTime",
		section: "experimental_sorted_slab_over_time",
		// Unlike fixed_accumulator_extrapolated (deliberately NOT a row here
		// — see wireNativeStrategies' own comment — because Rate/Increase/
		// Delta already have a row each), sum_over_time/avg_over_time have
		// no OTHER row that ever sets l.OverTime, so the sorted-slab
		// strategy needs its own row to be reachable from a fixture at all.
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.OverTime = promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}}
		},
	},
	{
		field:   "ClassicBucketMerge",
		section: "experimental_classic_bucket_merge_summap",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.ClassicBucketMerge = promql.NativeClassicBucketMergeLowerer{
				Fallback: promql.FanoutClassicBucketMergeLowerer{},
			}
		},
	},
	{
		field:   "ExpHistogramMerge",
		section: "experimental_exp_histogram_merge_summap",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.ExpHistogramMerge = promql.NativeExpHistogramMergeLowerer{
				Fallback: promql.FanoutExpHistogramMergeLowerer{},
			}
		},
	},
	{
		field:   "ArgAndMaxFusion",
		section: "experimental_arg_and_max_fusion",
		// Unlike every other row, this sets a plain bool, not a Lowerer —
		// see RangeLowerers.ArgAndMaxFusion's own doc for why there is no
		// alternate strategy TYPE to wire, and wireNativeStrategies' own
		// post-loop block below for how the SAME marker section also
		// reaches chplan.RangeLWR's copy of the verdict
		// (FanoutStalenessLowerer.ArgAndMaxFusion) — a second field this
		// row deliberately does NOT touch, so the "exactly one field set"
		// check below stays meaningful for this row's own field.
		//
		// At the real cmd/cerberus boot wiring this same verdict also
		// reaches chplan.VectorJoin, but the spec lane's ONLY path to a
		// custom RangeLowerers table is the range_step: branch
		// (lower_test.go), which always sets ctx.step > 0 and therefore
		// StepAligned=true on any VectorJoin lowered under it — outside the
		// non-StepAligned scope this feature covers for that node. So this
		// row's fixture coverage is RangeLWR-only; VectorJoin's fused SQL
		// is pinned instead by internal/chsql's own
		// vector_join_argandmax_fusion_test.go (structural) and
		// vector_join_argandmax_fusion_chdb_test.go (differential).
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.ArgAndMaxFusion = true
		},
	},
	{
		field:   "VectorAgg",
		section: "experimental_ts_grid_vector_agg",
		// Like ArgAndMaxFusion, a plain bool: see
		// promql.RangeLowerers.VectorAgg's own doc for why there is no
		// alternate strategy TYPE to select between. A fixture carrying
		// ONLY this section is inert — lowerAggregate only consults it
		// after its input already lowered to a *chplan.RangeWindowGridNative,
		// so a fixture exercising the resulting SQL shape must ALSO carry
		// one of the range-function sections above (e.g.
		// experimental_ts_grid_changes) to build that native grid in the
		// first place — cerberus issue #2763.
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.VectorAgg = true
		},
	},
	{
		field:   "NativeGroupArray",
		section: "experimental_ts_grid_group_array",
		// Like ArgAndMaxFusion/VectorAgg, a plain bool: see
		// promql.RangeLowerers.NativeGroupArray's own doc for why there is
		// no alternate strategy TYPE to select between — it only changes
		// how rate/increase/delta's array-fold fallback tier assembles its
		// sample array, not which of that family's own Native /
		// FixedAccumulator / Fanout tiers fires.
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.NativeGroupArray = true
		},
	},
}

// wireNativeStrategies builds the dispatch table for a fixture from the
// marker sections it carries. Any field left nil after the loop below is
// resolved to its concrete fan-out impl at the lowering entry, so a fixture
// with no marker section lowers exactly as it did before the table existed.
func wireNativeStrategies(has func(section string) bool) promql.RangeLowerers {
	var l promql.RangeLowerers
	l.ClassicHistogram = promql.FanoutClassicHistogramWindowLowerer{}
	for _, ns := range nativeStrategies {
		if has(ns.section) {
			ns.wire(&l, has)
		}
	}
	// laginframe_adjacency (issue #2759) is changes/resets/irate/idelta's
	// improved FALLBACK, not a competitor to their native ts_grid strategy
	// above — mirroring cmd/cerberus's own nativeRangeLowerers composition
	// (Native{Fallback: LagAdjacency{Fallback: Fanout{}}}). It is
	// deliberately NOT a nativeStrategies row: a row's section is the ONLY
	// way to reach that row's field, and all four functions already have a
	// row gated on their own ts_grid section (irate/idelta's landed in
	// cerberus issue #2746). A fixture opts into the lag-adjacency SQL shape
	// ALONE (unshadowed by the native path) by carrying ONLY this section,
	// never alongside experimental_ts_grid_changes/resets/irate/idelta — the
	// two are mutually exclusive at this spec-fixture layer by convention,
	// documented on each fixture. (The composed native-wraps-lag-adjacency
	// shape is exercised directly in Go by
	// internal/chsql/range_window_lag_adjacency_chdb_test.go, not through
	// this table.)
	if has("experimental_lag_adjacency_changes") {
		l.Changes = promql.LagAdjacencyChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}
	}
	if has("experimental_lag_adjacency_resets") {
		l.Resets = promql.LagAdjacencyResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}
	}
	// irate/idelta gained their own native ts_grid strategy (cerberus issue
	// #2746) and moved into the nativeStrategies table above, exactly
	// mirroring changes/resets: laginframe_adjacency is now THEIR improved
	// fallback too, so its two existing fixtures
	// (lag_adjacency_irate_delta_temporality_range.txtar,
	// lag_adjacency_idelta_duplicate_ts.txtar) keep opting into the
	// lag-adjacency SQL shape ALONE via this override, unshadowed by the
	// native path.
	if has("experimental_lag_adjacency_irate") {
		l.Irate = promql.LagAdjacencyIrateLowerer{Fallback: promql.FanoutIrateLowerer{}}
	}
	if has("experimental_lag_adjacency_idelta") {
		l.Idelta = promql.LagAdjacencyIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}}
	}
	// fixed_accumulator_extrapolated (issue #2760) is Rate/Increase/Delta's
	// improved FALLBACK, not a competitor to their native ts_grid strategy
	// above — mirroring the laginframe_adjacency block just above (and
	// cmd/cerberus's own nativeRangeLowerers composition). Deliberately NOT a
	// nativeStrategies row for the same reason: Rate/Increase/Delta already
	// have a row gated on their own ts_grid section (Delta's own native
	// competitor, ts_grid_delta, landed via cerberus issue #2745). A fixture
	// opts into the fixed-accumulator SQL shape ALONE (unshadowed by the
	// native path) by carrying ONLY this section, never alongside
	// experimental_ts_grid_range/increase/delta.
	if has("experimental_fixed_accumulator_rate") {
		l.Rate = promql.FixedAccumulatorRateLowerer{Fallback: promql.FanoutRateLowerer{}}
	}
	if has("experimental_fixed_accumulator_increase") {
		l.Increase = promql.FixedAccumulatorIncreaseLowerer{Fallback: promql.FanoutIncreaseLowerer{}}
	}
	if has("experimental_fixed_accumulator_delta") {
		l.Delta = promql.FixedAccumulatorDeltaLowerer{Fallback: promql.FanoutDeltaLowerer{}}
	}
	// arg_and_max_fusion (issue #2764) reaches chplan.RangeLWR through
	// FanoutStalenessLowerer's OWN copy of the verdict — see the
	// ArgAndMaxFusion row's own doc above for why the table row only sets
	// the top-level bool and this second assignment lives here instead.
	// Overriding l.Staleness unconditionally mirrors cmd/cerberus's own
	// nativeRangeLowerers: RangeLWR.ArgAndMaxFusion only matters when
	// SampleTimestamp is requested, which forces NativeStalenessLowerer to
	// its Fallback regardless, so no fixture under this marker needs to
	// also carry experimental_ts_grid_resample.
	if has("experimental_arg_and_max_fusion") {
		l.Staleness = promql.FanoutStalenessLowerer{ArgAndMaxFusion: true}
	}
	return l
}

// TestNativeStrategies_CoverEveryRangeLowerersField is the ratchet: it
// fails when [promql.RangeLowerers] gains a strategy field the spec lane
// has no way to select.
//
// It checks the binding in BOTH directions, and by BEHAVIOUR rather than
// by name list. Each row is applied to a zero table and the result read
// back by reflection: the row's named field must have become non-nil and
// every other field must have stayed nil. So a row that names a field it
// does not actually set — the copy-paste failure this table's shape
// invites — fails here rather than silently wiring the wrong strategy
// into every fixture that carries its section.
func TestNativeStrategies_CoverEveryRangeLowerersField(t *testing.T) {
	t.Parallel()

	tableType := reflect.TypeOf(promql.RangeLowerers{})

	fields := make(map[string]bool, tableType.NumField())
	for i := range tableType.NumField() {
		fields[tableType.Field(i).Name] = false
	}

	for _, ns := range nativeStrategies {
		covered, known := fields[ns.field]
		if !known {
			t.Errorf("nativeStrategies row %q names no field of promql.RangeLowerers", ns.field)
			continue
		}
		if covered {
			t.Errorf("field %s is claimed by more than one nativeStrategies row", ns.field)
			continue
		}
		fields[ns.field] = true

		// Apply this row alone, with no nested section present, and read
		// the result back: exactly its own field must be set.
		var got promql.RangeLowerers
		ns.wire(&got, func(string) bool { return false })
		v := reflect.ValueOf(got)
		for i := range tableType.NumField() {
			name := tableType.Field(i).Name
			// Every strategy field is an interface (nilable); ArgAndMaxFusion
			// is a plain bool (there is no alternate NODE shape to select
			// between, only an emission-detail bit read directly off the
			// same node — see RangeLowerers.ArgAndMaxFusion's own doc), so
			// "set" reads its zero value instead of IsNil, which panics on
			// a non-interface Kind.
			field := v.Field(i)
			var set bool
			if field.Kind() == reflect.Bool {
				set = field.Bool()
			} else {
				set = !field.IsNil()
			}
			switch {
			case name == ns.field && !set:
				t.Errorf("nativeStrategies row %q (section %q) left field %s nil",
					ns.field, ns.section, name)
			case name != ns.field && set:
				t.Errorf("nativeStrategies row %q (section %q) also set unrelated field %s",
					ns.field, ns.section, name)
			}
		}
	}

	for name, covered := range fields {
		if !covered {
			t.Errorf("promql.RangeLowerers.%s has no nativeStrategies row, so no TXTAR fixture "+
				"can lower through it — every spec fixture would exercise the fan-out impl "+
				"instead, and the SQL this strategy emits in production would go unpinned. "+
				"Add a row naming the marker section, and a fixture under %s carrying it.",
				name, fixtureDir)
		}
	}
}

// TestNativeStrategies_HaveNativeSideFixtures is the other half of the
// ratchet. A wired strategy nothing opts into is still unpinned SQL, so
// having a row is not enough — some fixture must actually carry its
// marker section.
//
// This is what makes `RangeLowerers` gaining a field a build failure
// rather than a silent widening: the field needs a row (test above) AND
// the row needs a fixture (here), which together mean the new strategy's
// emitted SQL lands in the golden corpus.
func TestNativeStrategies_HaveNativeSideFixtures(t *testing.T) {
	t.Parallel()

	carried := sectionsCarriedByFixtures(t, fixtureDir)

	for _, ns := range nativeStrategies {
		if n := carried[ns.section]; n == 0 {
			t.Errorf("no fixture under %s carries the %q section, so promql.RangeLowerers.%s "+
				"is wired but never selected and the SQL it emits is unpinned",
				fixtureDir, ns.section, ns.field)
		}
	}
}

// sectionsCarriedByFixtures returns, per section name, how many fixtures
// under dir carry it.
func sectionsCarriedByFixtures(t *testing.T, dir string) map[string]int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", dir, err)
	}
	out := map[string]int{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txtar") {
			continue
		}
		c, err := spec.Load(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("load fixture %s: %v", e.Name(), err)
		}
		for name := range c.Sections() {
			out[name]++
		}
	}
	return out
}
