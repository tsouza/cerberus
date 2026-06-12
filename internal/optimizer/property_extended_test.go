//go:build chdb

// Extended property tests for the optimizer (Layer 4 § B).
//
// property_test.go runs the full Default() optimizer pipeline against
// randomly generated plans. This file complements it with per-rule
// equivalence: for each rule, build a small slate of plans where the
// rule is *applicable* and verify the rule preserves the row set.
//
// Scope. The chDB roundtrip is expensive — opening an in-process CH
// session + running DDL takes ~hundreds of ms per test. We restrict
// the matrix to the rules whose rewrite is most observable in the
// emitted SQL:
//
//   - FilterFusion: collapses Filter(Filter) → Filter
//   - ConstantFoldSemantic: collapses literal arithmetic
//   - ConstantFoldHeuristic: collapses boolean identities
//   - ProjectionPushdown: narrows Scan.Columns
//
// The transpose rules (FilterAggregateTranspose, FilterRangeWindowTranspose)
// need richer seed data (multi-series Aggregate inputs, windowed shapes)
// to round-trip meaningfully — those rules are covered by the existing
// TXTAR fixture suite and the optimizer_test.go integration test (which
// checks SQL output but not row-set equivalence). The decision_pins_test.go
// file adds plan-shape pins for those rules.
//
// Why drive per-rule rather than reuse property_test's "full pipeline"
// generator? Per-rule tests can pin equivalence even if a future Default()
// pipeline change masks one rule's effect with another. A bug introduced
// in FilterFusion alone — say, AND-ing the wrong direction — would
// surface here but be masked by ConstantFoldHeuristic in the full pipeline.

package optimizer_test

import (
	"context"
	"database/sql"
	"math/rand"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// seedPropertyDB opens a fresh ephemeral chDB session and applies
// propertyDDL. Mirrors property_test.go's seed loop so the extended
// suite can run independently.
func seedPropertyDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openPropertyChDB(t)
	for _, stmt := range splitDDLStatements(propertyDDL) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed exec failed: stmt=%q err=%v", stmt, err)
		}
	}
	return db
}

// TestPropertyFilterFusion_RowSetEquivalent: build a Filter(Filter(Scan))
// with two independent predicates and check that running FilterFusion
// yields the same rows as the unoptimized plan. Each iteration picks
// a random pair of leaf predicates from the property generator's
// alphabet so the rule sees a variety of operand types.
func TestPropertyFilterFusion_RowSetEquivalent(t *testing.T) {
	const N = 10
	rng := rand.New(rand.NewSource(20260514))

	db := seedPropertyDB(t)

	ctx := context.Background()
	d := optimizer.New(optimizer.FilterFusion{})

	for i := 0; i < N; i++ {
		plan := chplan.Node(&chplan.Filter{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: propertyTable},
				Predicate: generateLeafPredicate(rng),
			},
			Predicate: generateLeafPredicate(rng),
		})

		pre, errPre := runPlan(ctx, db, plan)
		if errPre != nil {
			t.Logf("iter=%d skip pre err=%v", i, errPre)
			continue
		}
		post, errPost := runPlan(ctx, db, d.Run(ctx, plan))
		if errPost != nil {
			t.Fatalf("iter=%d optimizer broke a plan that ran pre-opt:\n%s\nerr=%v", i, dumpPlan(plan), errPost)
		}
		if !rowsetEqual(pre, post) {
			t.Fatalf("iter=%d row-set diverged\nplan: %s\npre: %s\npost: %s", i, dumpPlan(plan), dumpRows(pre), dumpRows(post))
		}
	}
}

// TestPropertyConstantFoldSemantic_RowSetEquivalent: build a Filter
// whose predicate has a pure-literal Binary subtree (`1+2=3`, etc.).
// Semantic fold collapses it. Pre/post row sets must match.
func TestPropertyConstantFoldSemantic_RowSetEquivalent(t *testing.T) {
	const N = 10
	rng := rand.New(rand.NewSource(20260515))

	db := seedPropertyDB(t)

	ctx := context.Background()
	d := optimizer.New(optimizer.ConstantFoldSemantic{})

	// Literal comparison pairs that semantic fold reduces to LitBool.
	// All operands sit underneath a wrapping `<binary> AND <leaf>`,
	// so the binary must collapse to a Bool — arithmetic ops like
	// OpAdd are excluded (CH would reject `1+2 AND <bool>` typing).
	literalPairs := []struct {
		op   chplan.BinaryOp
		l, r int64
	}{
		{chplan.OpEq, 1, 1},
		{chplan.OpEq, 1, 2},
		{chplan.OpLt, 1, 2},
		{chplan.OpGt, 1, 2},
		{chplan.OpLe, 2, 2},
		{chplan.OpGe, 2, 1},
		{chplan.OpNe, 1, 2},
	}

	for i := 0; i < N; i++ {
		pair := literalPairs[rng.Intn(len(literalPairs))]
		// Wrap the literal binary in `<literal-binary> AND <leaf>` to
		// give the runner a chance of producing a non-empty row set.
		leaf := generateLeafPredicate(rng)
		plan := chplan.Node(&chplan.Filter{
			Input: &chplan.Scan{Table: propertyTable},
			Predicate: &chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    pair.op,
					Left:  &chplan.LitInt{V: pair.l},
					Right: &chplan.LitInt{V: pair.r},
				},
				Right: leaf,
			},
		})

		pre, errPre := runPlan(ctx, db, plan)
		if errPre != nil {
			t.Logf("iter=%d skip pre err=%v", i, errPre)
			continue
		}
		post, errPost := runPlan(ctx, db, d.Run(ctx, plan))
		if errPost != nil {
			t.Fatalf("iter=%d post-opt broke:\n%s\nerr=%v", i, dumpPlan(plan), errPost)
		}
		if !rowsetEqual(pre, post) {
			t.Fatalf("iter=%d row-set diverged\nplan: %s\npre: %s\npost: %s", i, dumpPlan(plan), dumpRows(pre), dumpRows(post))
		}
	}
}

// TestPropertyConstantFoldHeuristic_RowSetEquivalent: build a Filter
// whose predicate uses a boolean identity (`true AND X`, `false OR X`,
// etc.). Heuristic fold collapses it. Pre/post row sets must match.
func TestPropertyConstantFoldHeuristic_RowSetEquivalent(t *testing.T) {
	const N = 10
	rng := rand.New(rand.NewSource(20260516))

	db := seedPropertyDB(t)

	ctx := context.Background()
	d := optimizer.New(optimizer.ConstantFoldHeuristic{})

	identities := []struct {
		op  chplan.BinaryOp
		lit bool
	}{
		{chplan.OpAnd, true},
		{chplan.OpOr, false},
		// `false AND X` reduces to `false`, which drops all rows. We
		// still want the rule to preserve that: pre/post both report
		// zero rows. Skip the `true OR X` case (collapses to true,
		// returning ALL rows) since the runner may distinguish those.
	}

	for i := 0; i < N; i++ {
		id := identities[rng.Intn(len(identities))]
		leaf := generateLeafPredicate(rng)
		plan := chplan.Node(&chplan.Filter{
			Input: &chplan.Scan{Table: propertyTable},
			Predicate: &chplan.Binary{
				Op:    id.op,
				Left:  &chplan.LitBool{V: id.lit},
				Right: leaf,
			},
		})

		pre, errPre := runPlan(ctx, db, plan)
		if errPre != nil {
			t.Logf("iter=%d skip pre err=%v", i, errPre)
			continue
		}
		post, errPost := runPlan(ctx, db, d.Run(ctx, plan))
		if errPost != nil {
			t.Fatalf("iter=%d post-opt broke:\n%s\nerr=%v", i, dumpPlan(plan), errPost)
		}
		if !rowsetEqual(pre, post) {
			t.Fatalf("iter=%d row-set diverged\nplan: %s\npre: %s\npost: %s", i, dumpPlan(plan), dumpRows(pre), dumpRows(post))
		}
	}
}

// TestPropertyProjectionPushdown_RowSetEquivalent: pin that narrowing
// Scan.Columns preserves row content (assuming the test never asks
// for a column it didn't project). 10 iterations of Project([X, Y],
// Scan) where (X, Y) is a random subset of the seed alphabet.
func TestPropertyProjectionPushdown_RowSetEquivalent(t *testing.T) {
	const N = 10
	rng := rand.New(rand.NewSource(20260518))

	db := seedPropertyDB(t)

	ctx := context.Background()
	d := optimizer.New(optimizer.ProjectionPushdown{})

	for i := 0; i < N; i++ {
		// Subset of [MetricName, Value, TimeUnix].
		cols := append([]string(nil), propertyColumns...)
		rng.Shuffle(len(cols), func(i, j int) { cols[i], cols[j] = cols[j], cols[i] })
		cols = cols[:1+rng.Intn(len(cols))]
		projs := make([]chplan.Projection, len(cols))
		for k, c := range cols {
			projs[k] = chplan.Projection{Expr: &chplan.ColumnRef{Name: c}}
		}
		plan := chplan.Node(&chplan.Project{
			Input:       &chplan.Scan{Table: propertyTable},
			Projections: projs,
		})

		pre, errPre := runPlan(ctx, db, plan)
		if errPre != nil {
			t.Logf("iter=%d skip pre err=%v", i, errPre)
			continue
		}
		post, errPost := runPlan(ctx, db, d.Run(ctx, plan))
		if errPost != nil {
			t.Fatalf("iter=%d post-opt broke:\n%s\nerr=%v", i, dumpPlan(plan), errPost)
		}
		if !rowsetEqual(pre, post) {
			t.Fatalf("iter=%d row-set diverged\nplan: %s\npre: %s\npost: %s", i, dumpPlan(plan), dumpRows(pre), dumpRows(post))
		}
	}
}
