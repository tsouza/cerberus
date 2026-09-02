package chsql

import (
	"context"
	"errors"
	"slices"
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
		// checkFusedVariants.go:92 — the fused emitter drives only the
		// *_over_time array reducers, which take no scalar parameters.
		// A scalar argument here is untested territory: the guard's
		// condition is EVALUATED on every fused Emit (both operands are
		// always 0 in every other case above), so an `||`→`&&` flip
		// would still show false everywhere else and only this case
		// exercises the true branch.
		"scalar argument present": func(r *chplan.RangeWindow) {
			r.Scalars = []float64{1}
		},
		// checkFusedVariants.go:95 — the fused emitter consults no
		// temporality column (each arm's own reducer never reads
		// windowTemporalityRef). Same coverage gap as the scalar case.
		"temporality column present": func(r *chplan.RangeWindow) {
			r.TemporalityColumn = "AggregationTemporality"
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

// TestFusedVariantValueLayout pins the arm-to-slot many-to-one mapping. The
// tuple carries each input value column once, while reducers retain arm order
// and may read the same slot.
func TestFusedVariantValueLayout(t *testing.T) {
	t.Parallel()
	r := fusedTestWindow()
	r.Variants[1].ValueColumn = "Value_0"
	r.Variants = append(r.Variants, chplan.RangeWindowVariant{
		Func: "min_over_time", ValueColumn: "Value_1", Label: "2",
	})

	columns, slots := fusedVariantValueLayout(r)
	wantColumns := []string{"Value_0", "Value_1"}
	wantSlots := []int{0, 0, 1}
	if !slices.Equal(columns, wantColumns) {
		t.Errorf("value columns = %v, want %v", columns, wantColumns)
	}
	if !slices.Equal(slots, wantSlots) {
		t.Errorf("arm slots = %v, want %v", slots, wantSlots)
	}
	if _, _, err := Emit(context.Background(), r); err != nil {
		t.Fatalf("Emit shared-value fused window: %v", err)
	}
}

// TestEmitFusedVariants_InstantVsMatrixDispatch kills the
// CONDITIONALS_NEGATION / CONDITIONALS_BOUNDARY mutants at
// range_window_variants.go:`r.OuterRange > 0` in
// emitRangeWindowVariants. The instant shape (OuterRange == 0) has no
// anchor grid at all — no `anchor_ts` column anywhere in the emitted
// SQL — while the matrix shape fans out across one. A `>` → `<=` flip
// would route the instant case into the matrix emitter (or vice
// versa); every other test in this file exercises only the matrix
// side (fusedTestWindow's default OuterRange > 0), so this is the only
// coverage of the instant branch existing before this change.
func TestEmitFusedVariants_InstantVsMatrixDispatch(t *testing.T) {
	t.Parallel()

	t.Run("OuterRange == 0 → instant, no anchor grid", func(t *testing.T) {
		t.Parallel()
		r := fusedTestWindow()
		r.OuterRange = 0
		sql, _, err := Emit(context.Background(), r)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if strings.Contains(sql, "anchor_ts") {
			t.Errorf("instant fused window (OuterRange=0) must not fan out across anchors\nSQL: %s", sql)
		}
	})

	t.Run("OuterRange > 0 → matrix, anchor grid present", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), fusedTestWindow())
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "anchor_ts") {
			t.Errorf("matrix fused window (OuterRange>0) must fan out across anchors\nSQL: %s", sql)
		}
	})
}

// TestEmitFusedVariantsMatrix_TimestampColumnAnchorTsAlias kills the
// CONDITIONALS_NEGATION mutant at range_window_variants.go:`r.TimestampColumn != "anchor_ts"`
// (`r.TimestampColumn != "anchor_ts"`). The matrix outer layer always
// selects the raw `anchor_ts` column; it additionally re-projects it
// under the schema timestamp column's own alias UNLESS that alias
// would just be "anchor_ts" again, in which case the duplicate
// projection must be skipped. A `!=` → `==` flip inverts which case
// gets the extra projection.
func TestEmitFusedVariantsMatrix_TimestampColumnAnchorTsAlias(t *testing.T) {
	t.Parallel()

	t.Run(`TimestampColumn != "anchor_ts" → extra alias projected`, func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), fusedTestWindow()) // TimestampColumn: "Timestamp"
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "anchor_ts AS `Timestamp`") {
			t.Errorf("expected the schema-timestamp re-projection `anchor_ts AS `Timestamp`` (line 351 flipped?)\nSQL: %s", sql)
		}
	})

	t.Run(`TimestampColumn == "anchor_ts" → no duplicate projection`, func(t *testing.T) {
		t.Parallel()
		r := fusedTestWindow()
		r.TimestampColumn = "anchor_ts"
		sql, _, err := Emit(context.Background(), r)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if strings.Contains(sql, "anchor_ts AS") {
			t.Errorf("expected no duplicate anchor_ts re-projection when TimestampColumn==\"anchor_ts\" (line 351 flipped?)\nSQL: %s", sql)
		}
	})
}

// TestVariantValsFrag_TupleSlotArithmetic kills the ARITHMETIC_BASE mutant
// at range_window_variants.go:`valueSlot + firstValueSlot`. Tuple
// slot 1 is always the timestamp, so value slot 0 must read tuple index 2
// and value slot 1 must read tuple index 3; a `+`→`-` flip would read
// negative/zero indices and a `+`→`*` flip would collide slot 0 onto index
// 0 instead of 2.
func TestVariantValsFrag_TupleSlotArithmetic(t *testing.T) {
	t.Parallel()
	render := func(f Frag) string {
		b := &Builder{}
		f(b)
		return b.String()
	}
	cases := []struct {
		valueSlot int
		wantIndex string
	}{
		{0, "tupleElement(p, 2)"},
		{1, "tupleElement(p, 3)"},
		{2, "tupleElement(p, 4)"},
	}
	for _, c := range cases {
		got := render(variantValsFrag(BareIdent("window_pairs"), c.valueSlot))
		if !strings.Contains(got, c.wantIndex) {
			t.Errorf("variantValsFrag(valueSlot=%d) missing %q (line 151 arithmetic flipped?)\ngot: %s",
				c.valueSlot, c.wantIndex, got)
		}
	}
}

// TestEmitFusedVariantsMatrix_AnchorCountArithmetic kills the
// ARITHMETIC_BASE mutants at range_window_variants.go:`r.OuterRange.Nanoseconds()/stepNS + 1`
// (`r.OuterRange.Nanoseconds()/stepNS + 1`). fusedTestWindow's OuterRange
// (1m) / Step (30s) + 1 = 3 anchors, surfacing as the sample-fanout's
// `least(3, ...)` upper clamp. A `/`→`*` flip overflows to a huge count;
// a `+`→`-` flip yields `least(1, ...)`.
func TestEmitFusedVariantsMatrix_AnchorCountArithmetic(t *testing.T) {
	t.Parallel()
	sql, _, err := Emit(context.Background(), fusedTestWindow())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "least(3, ") {
		t.Errorf("expected least(3, ...) anchor-count clamp (OuterRange/Step + 1 = 3) — line 293 arithmetic flipped?\nSQL: %s", sql)
	}
	if strings.Contains(sql, "least(1, ") {
		t.Errorf("found least(1, ...) — `+ 1` mutated to `- 1` (line 293)\nSQL: %s", sql)
	}
}

// TestGroupArrayVariantTupleFrag_EmptyValCols kills the ARITHMETIC_BASE
// mutant at range_window_variants.go:`len(valCols)+1` (`len(valCols)+1`, the capacity
// hint for the tuple-parts slice). With valCols empty, len(valCols)+1 == 1,
// a harmless capacity; a `+`->`-` flip instead computes -1, which panics
// make() immediately. Production never calls this with an empty valCols
// (checkFusedVariants requires every arm to name a ValueColumn), so this
// calls the helper directly to force the boundary.
func TestGroupArrayVariantTupleFrag_EmptyValCols(t *testing.T) {
	t.Parallel()
	got := groupArrayVariantTupleFrag("Timestamp", nil)
	b := &Builder{}
	got(b)
	sql, _, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(sql, "arraySort(groupArray(") {
		t.Errorf("unexpected SQL: %s", sql)
	}
}

// TestEmitFusedVariantsInstant_CompletesAllSteps kills the four
// CONDITIONALS_NEGATION mutants at range_window_variants.go:232:9, 236:9,
// 249:9 and 256:66 — the four `if err != nil { return err }` guards inside
// emitRangeWindowVariantsInstant. Every one of those checks genuinely
// succeeds on this well-formed plan, so a `!= nil` -> `== nil` mutant at any
// one of them turns "keep going on success" into "return nil immediately
// after this step succeeds" — the function would return before ever
// reaching arrayJoinVariants (the ARRAY JOIN unpivot near the end), so its
// absence from the SQL is the shared tripwire for all four sites.
func TestEmitFusedVariantsInstant_CompletesAllSteps(t *testing.T) {
	t.Parallel()
	r := fusedTestWindow()
	r.OuterRange = 0 // instant path
	sql, _, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "ARRAY JOIN") {
		t.Errorf("instant fused variants SQL missing ARRAY JOIN unpivot — an early return before completion?\nSQL: %s", sql)
	}
	for _, v := range r.Variants {
		if !strings.Contains(sql, v.ValueColumn) {
			t.Errorf("missing arm value column %q — Emit returned early?\nSQL: %s", v.ValueColumn, sql)
		}
	}
}

// TestEmitFusedVariantsMatrix_UngroupedRegroupKeysCapacity kills the
// ARITHMETIC_BASE mutant at range_window_variants.go:`len(groupFrags)+1`
// (`len(groupFrags)+1`, the capacity hint for regroupKeys). An ungrouped
// fused matrix window has zero groupFrags, so len(groupFrags)+1 == 1 (the
// anchor_ts key alone); a `+`->`-` flip computes -1, panicking make()
// before the query can even be assembled.
func TestEmitFusedVariantsMatrix_UngroupedRegroupKeysCapacity(t *testing.T) {
	t.Parallel()
	r := fusedTestWindow()
	r.GroupBy = nil // OuterRange > 0 stays (matrix path)
	sql, _, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "ARRAY JOIN") {
		t.Errorf("ungrouped matrix fused variants missing ARRAY JOIN\nSQL: %s", sql)
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
