package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_VectorMatch_Cardinality covers the RC2 group_left /
// group_right cardinality + extra-label edges:
//
//   - group_left(<labels>) populates chplan.VectorJoin.Card +
//     Include and routes the named labels from the right side onto
//     the output via mapConcat at SQL emission time.
//   - group_right is the mirror.
//   - One-to-one matching on a subset of labels (`on(...)` or
//     `ignoring(...)`) embeds the runtime "many-to-many matching
//     not allowed" guard via throwIf(uniqExact(Attributes) > 1, ...)
//     so the cardinality violation surfaces as a CH error rather
//     than a silent cross-product.
//   - Default (full-Attributes) matching skips the runtime guard —
//     uniqueness is guaranteed by construction.
func TestLower_VectorMatch_Cardinality(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	t.Run("group_left populates Card + Include", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`up * on(job) group_left(env) info_metric`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		vj, ok := plan.(*chplan.VectorJoin)
		if !ok {
			t.Fatalf("expected *chplan.VectorJoin, got %T", plan)
		}
		if vj.Card != chplan.CardManyToOne {
			t.Errorf("Card = %v, want CardManyToOne", vj.Card)
		}
		if got, want := vj.Include, []string{"env"}; !equalStrings(got, want) {
			t.Errorf("Include = %v, want %v", got, want)
		}
	})

	t.Run("group_right populates Card + Include (mirror)", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`info_metric * on(job) group_right(env) up`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		vj, ok := plan.(*chplan.VectorJoin)
		if !ok {
			t.Fatalf("expected *chplan.VectorJoin, got %T", plan)
		}
		if vj.Card != chplan.CardOneToMany {
			t.Errorf("Card = %v, want CardOneToMany", vj.Card)
		}
		if got, want := vj.Include, []string{"env"}; !equalStrings(got, want) {
			t.Errorf("Include = %v, want %v", got, want)
		}
	})

	t.Run("group_left with multiple include labels", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`up * on(job) group_left(env, region) info_metric`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		vj := plan.(*chplan.VectorJoin)
		if got, want := vj.Include, []string{"env", "region"}; !equalStrings(got, want) {
			t.Errorf("Include = %v, want %v", got, want)
		}

		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		// Include labels surface as a mapConcat overlay onto the
		// left side's Attributes.
		if !strings.Contains(sql, "mapConcat(L.`Attributes`, mapFilter((k, v) -> k IN (?, ?), R.`Attributes`))") {
			t.Errorf("expected mapConcat overlay in SQL; got:\n%s", sql)
		}
		// Bound args carry both include labels.
		argStrs := stringArgs(args)
		if !containsAll(argStrs, []string{"env", "region"}) {
			t.Errorf("expected args to include env + region; got %v", argStrs)
		}
	})

	t.Run("on(...) one-to-one embeds runtime cardinality guard", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`up * on(job) up`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "throwIf(uniqExact(`Attributes`) > 1, ?)") {
			t.Errorf("expected throwIf-uniqExact guard in SQL; got:\n%s", sql)
		}
		const wantMsg = "many-to-many matching not allowed: matching labels must be unique on one side"
		argStrs := stringArgs(args)
		if !containsAll(argStrs, []string{wantMsg}) {
			t.Errorf("expected throwIf message in args; got %v", argStrs)
		}
	})

	t.Run("ignoring(...) one-to-one embeds runtime cardinality guard", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`up - ignoring(instance) up`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, "throwIf(uniqExact(`Attributes`) > 1, ?)") {
			t.Errorf("expected throwIf-uniqExact guard in SQL; got:\n%s", sql)
		}
	})

	t.Run("default full-Attributes match skips runtime guard", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`up + up`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		// Per-series argMax by full Attributes guarantees uniqueness;
		// no throwIf guard needed.
		if strings.Contains(sql, "throwIf") {
			t.Errorf("did not expect throwIf for full-Attributes match; got:\n%s", sql)
		}
	})

	t.Run("group_left on the many side keeps per-series granularity", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`up * on(job) group_left(env) info_metric`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		// The "many" (left) side aggregates over (MetricName, Attributes).
		// Per-side aggregation is wrapped in an outer Project that renames
		// `_join_*` aliases back to canonical names, so the GROUP BY
		// clause closes with `)) AS L` (inner agg + outer Project).
		if !strings.Contains(sql, "GROUP BY `MetricName`, `Attributes`)) AS L") {
			t.Errorf("expected left side per-series aggregation; got:\n%s", sql)
		}
		// The "one" (right) side aggregates by the matching key with
		// a uniqueness guard.
		if !strings.Contains(sql, "throwIf(uniqExact(`Attributes`) > 1, ?)") {
			t.Errorf("expected right-side cardinality guard; got:\n%s", sql)
		}
	})

	t.Run("group_right swaps the per-side aggregation roles", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`info_metric * on(job) group_right(env) up`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		// Right side is the "many" → per-series aggregation. The
		// per-side outer Project closes the inner agg's GROUP BY with
		// a second `)`.
		if !strings.Contains(sql, "GROUP BY `MetricName`, `Attributes`)) AS R") {
			t.Errorf("expected right side per-series aggregation; got:\n%s", sql)
		}
		// Output MetricName / TimeUnix come from R (the many side).
		if !strings.Contains(sql, "SELECT R.`MetricName`,") {
			t.Errorf("expected output to project R.MetricName; got:\n%s", sql)
		}
		if !strings.Contains(sql, "R.`TimeUnix`") {
			t.Errorf("expected output to project R.TimeUnix; got:\n%s", sql)
		}
	})
}

// TestLower_VectorMatch_ManyToMany asserts that a CardManyToMany
// VectorMatching (the parser-internal flag for set ops, but also
// reachable when the upstream parser ever surfaces it for arithmetic)
// errors with Prometheus's canonical "many-to-many matching not
// allowed" wording. The arithmetic-op path never produces
// CardManyToMany today; we drive the error path by hand-building a
// BinaryExpr with the flag set.
func TestLower_VectorMatch_ManyToMany(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	// Parse a vector/vector expression and tweak its VectorMatching.
	expr, err := p.ParseExpr(`up + up`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	bin, ok := expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("expected *parser.BinaryExpr, got %T", expr)
	}
	bin.VectorMatching = &parser.VectorMatching{Card: parser.CardManyToMany}

	if _, err := promql.Lower(context.Background(), expr, s); err == nil {
		t.Fatal("expected many-to-many error, got nil")
	} else if !strings.Contains(err.Error(), "many-to-many matching not allowed") {
		t.Errorf("error %q does not contain 'many-to-many matching not allowed'", err.Error())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringArgs(args []any) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsAll(haystack, needles []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}
