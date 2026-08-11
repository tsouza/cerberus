package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rangeFuncVocabulary is every range function emitRangeWindow dispatches on.
// It is the universe TestFusibleWindowFuncMatchesEmitter classifies, and it
// must stay in step with that switch — a function added there and omitted here
// is simply unclassified, which the sibling assertions below cannot see.
var rangeFuncVocabulary = []string{
	"rate", "irate", "increase", "delta", "idelta",
	"sum_over_time", "avg_over_time", "min_over_time", "max_over_time",
	"count_over_time", "first_over_time", "last_over_time",
	"stddev_over_time", "stdvar_over_time", "present_over_time", "mad_over_time",
	"ts_of_first_over_time", "ts_of_last_over_time",
	"ts_of_max_over_time", "ts_of_min_over_time",
	"quantile_over_time", "log_rate", "predict_linear", "holt_winters",
	"deriv", "resets", "changes",
}

// TestFusibleWindowFuncMatchesEmitter pins chplan.FusibleWindowFunc — the
// predicate a lowering consults before building a fused multi-arm window —
// against the emitter that has to render the result.
//
// The load-bearing direction is "declared fusible ⟹ the emitter reduces it":
// a lowering that fuses an arm the emitter cannot render turns a working query
// into an error, and that direction is checked exhaustively over the declared
// set, so it cannot regress. The converse ("the emitter reduces it ⟹ declared
// fusible") is checked over rangeFuncVocabulary; a miss there costs a scan,
// not an answer.
func TestFusibleWindowFuncMatchesEmitter(t *testing.T) {
	t.Parallel()
	vals := BareIdent("window_vals")
	for _, fn := range rangeFuncVocabulary {
		fusible := chplan.FusibleWindowFunc(fn)
		_, err := overTimeArrayValueFrag(fn, vals)
		reduces := err == nil
		if fusible != reduces {
			t.Errorf("fn %q: FusibleWindowFunc=%v but emitter reduces=%v (err=%v)",
				fn, fusible, reduces, err)
		}
	}
}

// TestFusibleWindowFuncDeclaresOnlyRenderableFuncs is the exhaustive half of
// the contract: every function the predicate declares fusible must render,
// independent of whether rangeFuncVocabulary happens to list it.
func TestFusibleWindowFuncDeclaresOnlyRenderableFuncs(t *testing.T) {
	t.Parallel()
	// Any string the predicate accepts must reduce. Iterating the vocabulary
	// plus a few near-miss names keeps the check honest without reflection.
	candidates := append([]string{}, rangeFuncVocabulary...)
	candidates = append(candidates, "", "bytes_over_time", "absent_over_time", "nonesuch")
	for _, fn := range candidates {
		if !chplan.FusibleWindowFunc(fn) {
			continue
		}
		if _, err := overTimeArrayValueFrag(fn, BareIdent("window_vals")); err != nil {
			t.Errorf("fn %q declared fusible but the emitter rejects it: %v", fn, err)
		}
	}
}

// fusedTestWindow builds a minimal, well-formed fused matrix window over a
// two-column row-shape input.
func fusedTestWindow() *chplan.RangeWindow {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindow{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_logs"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "ResourceAttributes"}, Alias: "ResourceAttributes"},
				{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
				{Expr: &chplan.LitInt{V: 1}, Alias: "Value_0"},
				{Expr: &chplan.ColumnRef{Name: "Len"}, Alias: "Value_1"},
			},
		},
		Range:           5 * time.Minute,
		Step:            30 * time.Second,
		OuterRange:      time.Minute,
		Start:           start,
		End:             start.Add(time.Minute),
		TimestampColumn: "Timestamp",
		ValueColumn:     "Value",
		VariantColumn:   "__variant__",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "ResourceAttributes"}},
		Variants: []chplan.RangeWindowVariant{
			{Func: "count_over_time", ValueColumn: "Value_0", Label: "0"},
			{Func: "sum_over_time", ValueColumn: "Value_1", Label: "1"},
		},
	}
}

// TestEmitFusedVariantsReadsInputOnce is the point of the whole shape: the
// emitted SQL must reference the scanned table exactly once, however many arms
// the window carries. A regression that re-expands the arms would show up here
// as a second FROM.
func TestEmitFusedVariantsReadsInputOnce(t *testing.T) {
	t.Parallel()
	for _, arms := range []int{2, 3, 4} {
		r := fusedTestWindow()
		r.Variants = r.Variants[:2]
		projections := r.Input.(*chplan.Project).Projections
		for i := 2; i < arms; i++ {
			alias := "Value_" + string(rune('0'+i))
			projections = append(projections, chplan.Projection{
				Expr: &chplan.ColumnRef{Name: "Len"}, Alias: alias,
			})
			r.Variants = append(r.Variants, chplan.RangeWindowVariant{
				Func: "max_over_time", ValueColumn: alias, Label: string(rune('0' + i)),
			})
		}
		r.Input.(*chplan.Project).Projections = projections

		sql, _, err := Emit(context.Background(), r)
		if err != nil {
			t.Fatalf("%d arms: Emit: %v", arms, err)
		}
		if got := strings.Count(sql, "`otel_logs`"); got != 1 {
			t.Errorf("%d arms: scanned table appears %d times, want exactly 1\nSQL: %s",
				arms, got, sql)
		}
		// Every arm must still reach the output.
		for _, v := range r.Variants {
			if !strings.Contains(sql, v.ValueColumn) {
				t.Errorf("%d arms: arm value column %q missing from SQL", arms, v.ValueColumn)
			}
		}
		// The unpivot is what turns one grouped row into one row per arm.
		if !strings.Contains(sql, "ARRAY JOIN") {
			t.Errorf("%d arms: no ARRAY JOIN unpivot in SQL: %s", arms, sql)
		}
	}
}

// TestEmitFusedVariantsRejectsIllFormed pins the fail-closed gate: a fused
// window the emitter cannot render correctly must error rather than quietly
// emit one arm's answer for every arm.
func TestEmitFusedVariantsRejectsIllFormed(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*chplan.RangeWindow){
		"single arm": func(r *chplan.RangeWindow) {
			r.Variants = r.Variants[:1]
		},
		"no variant column": func(r *chplan.RangeWindow) {
			r.VariantColumn = ""
		},
		"no value column": func(r *chplan.RangeWindow) {
			r.ValueColumn = ""
		},
		"no timestamp column": func(r *chplan.RangeWindow) {
			r.TimestampColumn = ""
		},
		"arms share a value column": func(r *chplan.RangeWindow) {
			r.Variants[1].ValueColumn = r.Variants[0].ValueColumn
		},
		"arm with no value column": func(r *chplan.RangeWindow) {
			r.Variants[1].ValueColumn = ""
		},
		"identity window": func(r *chplan.RangeWindow) {
			r.Identity = true
		},
		"unreducible arm function": func(r *chplan.RangeWindow) {
			r.Variants[1].Func = "rate"
		},
		"stepless anchor grid": func(r *chplan.RangeWindow) {
			r.Step = 0
		},
	}
	for name, mutate := range cases {
		r := fusedTestWindow()
		mutate(r)
		if _, _, err := Emit(context.Background(), r); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: got err %v, want ErrUnsupported", name, err)
		}
	}
}

// TestEmitFusedVariantsReusesSingleArmReducers pins that each arm's reducer is
// the SAME expression the single-arm path emits, over that arm's own values
// array — the property that keeps the fused and unfused answers equal without
// a second implementation of any function's semantics.
func TestEmitFusedVariantsReusesSingleArmReducers(t *testing.T) {
	t.Parallel()
	r := fusedTestWindow()
	sql, _, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for i, v := range r.Variants {
		vals := BareIdent(variantWindowValsAlias(i))
		want, err := overTimeArrayValueFrag(v.Func, vals)
		if err != nil {
			t.Fatalf("arm %d: %v", i, err)
		}
		b := &Builder{}
		want(b)
		wantSQL, _, err := b.Build()
		if err != nil {
			t.Fatalf("arm %d: build: %v", i, err)
		}
		if !strings.Contains(sql, wantSQL) {
			t.Errorf("arm %d (%s): reducer %q not found in emitted SQL\nSQL: %s",
				i, v.Func, wantSQL, sql)
		}
	}
}
