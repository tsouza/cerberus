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
			// is being built.
			l.Rate = promql.NativeRateLowerer{
				Fallback:   promql.FanoutRateLowerer{},
				Recollapse: has("experimental_ts_grid_recollapse"),
			}
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
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Changes = promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}
		},
	},
	{
		field:   "Resets",
		section: "experimental_ts_grid_resets",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Resets = promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}
		},
	},
	{
		field:   "Deriv",
		section: "experimental_ts_grid_deriv",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}}
		},
	},
	{
		field:   "PredictLinear",
		section: "experimental_ts_grid_predict_linear",
		wire: func(l *promql.RangeLowerers, _ func(string) bool) {
			l.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}}
		},
	},
}

// wireNativeStrategies builds the dispatch table for a fixture from the
// marker sections it carries. Any field left nil is resolved to its
// concrete fan-out impl at the lowering entry, so a fixture with no
// marker section lowers exactly as it did before the table existed.
func wireNativeStrategies(has func(section string) bool) promql.RangeLowerers {
	var l promql.RangeLowerers
	for _, ns := range nativeStrategies {
		if has(ns.section) {
			ns.wire(&l, has)
		}
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
			set := !v.Field(i).IsNil()
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
