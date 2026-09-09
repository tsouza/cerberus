package traceql_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// An attribute a span never carried resolves to StaticNil in reference
// Tempo, and nothing matches nil: every binary comparison short-circuits
// to false (pkg/traceql/ast.go's Static.Equals / Static.NotEquals both
// return false on TypeNil, and pkg/traceql/ast_execute.go's
// BinaryOperation.execute returns StaticFalse for the operator/type pairs
// binaryTypesValid rejects — which for TypeNil is everything except `=`
// and `!=`, per that function's `case TypeNil, TypeStatus, TypeKind` arm
// in pkg/traceql/enum_operators.go), and every aggregate skips the span
// outright (the `if val.IsNil() { continue }` guard each of
// pkg/traceql/ast_execute.go's Aggregate.evaluate avg/max/min/sum arms
// opens its span loop with; engine_metrics.go's FloatizeAttribute answers
// TypeNil, which the over-time reducers skip).
//
// cerberus reads attributes out of a Map(String, String) carrier, whose
// subscript answers '' for a key the span does not have. '' is a perfectly
// good String as far as ClickHouse is concerned, so without the mechanisms
// pinned below an absent attribute silently behaves like a span that
// carried the empty string — matching `!=`, `!~`, `<`, `<=`, `>=`, a
// regex as permissive as `.*`, and `= ""` — or the number zero, dragging
// every aggregate down.
//
// The same mechanisms are pinned end-to-end against real rows, and against
// the live upstream engine, by the `*_absent_key` and `regex_match_*_fold`
// fixtures under test/spec/traceql/: each carries a span WITHOUT the
// attribute alongside spans that have it, and each is enrolled `oracle:
// tempo` so the expected row set is computed by reference rather than
// recorded from cerberus. These unit cases pin the emitted SHAPE, so the
// non-chDB lanes catch a regression too.

// TestComparisonGuardsAttributeExistence pins the
// `mapContains(<carrier>, <key>) AND ...` conjunct that keeps a span
// without the attribute from satisfying a comparison. Reference answers
// false for every comparison against an absent operand; cerberus's Map
// subscript hands back ” instead, which is an ordinary string.
func TestComparisonGuardsAttributeExistence(t *testing.T) {
	t.Parallel()

	plain := schema.DefaultOTelTraces()
	materialized := schema.DefaultOTelTraces()
	materialized.MaterializedSpanAttributeColumns = map[string]string{
		"rpc.method":       "__cerberus_materialized_rpc.method",
		"http.status_code": "__cerberus_materialized_http.status_code",
	}

	for _, tc := range []struct {
		name  string
		query string
		s     schema.Traces
		// want is a SQL substring the emitted predicate must contain.
		want string
	}{
		{
			name:  "span_scope_not_equal",
			query: `{ span.color != "red" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] != ?)",
		},
		{
			name:  "span_scope_not_regex",
			query: `{ span.color !~ "red" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND NOT match(`SpanAttributes`[?], ?)",
		},
		{
			name:  "resource_scope_not_equal",
			query: `{ resource.color != "red" }`,
			s:     plain,
			want:  "mapContains(`ResourceAttributes`, ?) AND (`ResourceAttributes`[?] != ?)",
		},
		{
			// The unscoped read is a span-then-resource coalesce, so the
			// span exists for this attribute iff EITHER carrier has the key.
			name:  "unscoped_not_equal_guards_both_carriers",
			query: `{ .color != "red" }`,
			s:     plain,
			want:  "(mapContains(`SpanAttributes`, ?) OR mapContains(`ResourceAttributes`, ?)) AND (if(",
		},
		{
			// ast/rewrite.go folds `!= && !=` into NOT IN; without its own
			// guard that fold would be a free bypass of the rule above.
			name:  "and_chain_folded_to_not_in",
			query: `{ span.color != "red" && span.color != "blue" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND not((`SpanAttributes`[?] IN (?, ?)))",
		},
		{
			// Both operands read the map, so both must exist — reference
			// returns false if EITHER side resolves to nil.
			name:  "both_operands_guarded",
			query: `{ span.a != span.b }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] != `SpanAttributes`[?])",
		},
		{
			// A MaterializedColumnKindString column is declared
			// LowCardinality(String) DEFAULT <map>[<key>], so it inherits
			// the map's '' default and needs the same guard.
			name:  "string_materialized_column_still_guarded",
			query: `{ span.rpc.method != "x" }`,
			s:     materialized,
			want:  "mapContains(`SpanAttributes`, ?)",
		},
		{
			// `.*` matches ”, so without the guard every span answered
			// this — the single most over-matching shape in the set.
			name:  "regex_match",
			query: `{ span.color =~ ".*" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND match(`SpanAttributes`[?], ?)",
		},
		{
			// ” sorts below every non-empty string.
			name:  "less_than",
			query: `{ span.color < "m" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] < ?)",
		},
		{
			name:  "less_or_equal",
			query: `{ span.color <= "m" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] <= ?)",
		},
		{
			// `>= ""` is true for ”; `> "m"` is not, and is guarded anyway
			// — see needsAbsenceGuard on why one member of an ordering
			// family is not exempted on a property of the collation.
			name:  "greater_or_equal",
			query: `{ span.color >= "m" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] >= ?)",
		},
		{
			name:  "greater_than",
			query: `{ span.color > "m" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] > ?)",
		},
		{
			// The sharp case for the guard's own correctness: `= ""` is
			// what a user writes to find spans carrying the attribute with
			// an empty value, and it must find only those.
			name:  "equality_against_the_empty_literal",
			query: `{ span.color = "" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] = ?)",
		},
		{
			// Neither side is a literal, so nothing settles whether ” could
			// satisfy the comparison — and with BOTH absent it would.
			name:  "equality_between_two_attributes",
			query: `{ span.a = span.b }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] = `SpanAttributes`[?])",
		},
		{
			// The `||` chain of `=` folds to IN, which inherits the hazard.
			name:  "or_chain_folded_to_in",
			query: `{ span.color = "" || span.color = "blue" }`,
			s:     plain,
			want:  "mapContains(`SpanAttributes`, ?) AND (`SpanAttributes`[?] IN (?, ?))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr := emitTraceQL(t, tc.query, tc.s)
			if !strings.Contains(sqlStr, tc.want) {
				t.Fatalf("emitted SQL is missing the existence guard %q; got: %s", tc.want, sqlStr)
			}
		})
	}
}

// TestComparisonsThatNeedNoExistenceGuard is the negative half: the guard
// must fire ONLY where the ” default can produce a wrong match. Three
// shapes already carry reference's nil semantics natively, and a guard on
// them would be dead SQL on a hot path.
func TestComparisonsThatNeedNoExistenceGuard(t *testing.T) {
	t.Parallel()

	plain := schema.DefaultOTelTraces()
	materialized := schema.DefaultOTelTraces()
	materialized.MaterializedSpanAttributeColumns = map[string]string{
		"http.status_code": "__cerberus_materialized_http.status_code",
	}

	for _, tc := range []struct {
		name  string
		query string
		s     schema.Traces
	}{
		{
			// toFloat64OrNull already answers NULL for a missing key, and a
			// comparison against NULL is NULL, which WHERE drops.
			name:  "numeric_coercion_already_nulls",
			query: `{ span.size != 5 }`,
			s:     plain,
		},
		{
			// Nullable(Int32) DEFAULT toInt32OrNull(<map>[<key>]) — the same
			// NULL, computed once at ingest.
			name:  "numeric_materialized_column_already_nulls",
			query: `{ span.http.status_code != 5 }`,
			s:     materialized,
		},
		{
			// Intrinsics read real columns and are never nil upstream.
			name:  "intrinsic_duration",
			query: `{ duration != 5ms }`,
			s:     plain,
		},
		{
			name:  "intrinsic_name",
			query: `{ name != "GET" }`,
			s:     plain,
		},
		{
			// ” never equals a non-empty literal, so the probe would be
			// dead SQL — and equality against a literal is the predicate a
			// materialized attribute column exists to keep off the map.
			// See needsAbsenceGuard.
			name:  "equality_against_a_non_empty_literal",
			query: `{ span.color = "blue" }`,
			s:     plain,
		},
		{
			// The mirror image: ” is not != "" either.
			name:  "inequality_against_the_empty_literal",
			query: `{ span.color != "" }`,
			s:     plain,
		},
		{
			// The bool literal stringifies to "true", a non-empty literal.
			name:  "equality_against_a_bool_literal",
			query: `{ span.cache.hit = true }`,
			s:     plain,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr := emitTraceQL(t, tc.query, tc.s)
			if strings.Contains(sqlStr, "mapContains") {
				t.Fatalf("no existence guard is needed here, but one was emitted; got: %s", sqlStr)
			}
		})
	}
}

// TestAggregateSkipsSpansMissingTheAttribute pins the two halves of the
// spanset-aggregate rule: the NULL coercion that makes ClickHouse skip a
// span the way reference's `if val.IsNil() { continue }` does, and the
// isNotNull filter that drops a trace whose every span was skipped, the way
// reference's `if sum == nil { continue }` drops the spanset.
func TestAggregateSkipsSpansMissingTheAttribute(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, tc := range []struct {
		name  string
		query string
		agg   string
	}{
		{"avg", `{} | avg(span.size) > 5`, "avg(toFloat64OrNull(`SpanAttributes`[?]))"},
		{"min", `{} | min(span.size) > 5`, "min(toFloat64OrNull(`SpanAttributes`[?]))"},
		{"max", `{} | max(span.size) > 5`, "max(toFloat64OrNull(`SpanAttributes`[?]))"},
		{"sum", `{} | sum(span.size) > 5`, "sum(toFloat64OrNull(`SpanAttributes`[?]))"},
		{"resource_scope", `{} | avg(resource.replicas) > 5`, "avg(toFloat64OrNull(`ResourceAttributes`[?]))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr := emitTraceQL(t, tc.query, s)
			if !strings.Contains(sqlStr, tc.agg) {
				t.Fatalf("aggregate must skip attribute-less spans via toFloat64OrNull (want %q); got: %s", tc.agg, sqlStr)
			}
			if !strings.Contains(sqlStr, "WHERE isNotNull(`Value`)") {
				t.Fatalf("a trace whose every span lacked the attribute must be dropped, not reported with a NULL/zero Value; got: %s", sqlStr)
			}
		})
	}
}

// TestAggregateOverAlwaysPresentInputKeepsEveryTrace is the negative half of
// the rule above: `count()` and the intrinsic aggregates read values that
// exist on every span, so neither the NULL coercion nor the drop-filter
// applies and no trace may be filtered out of the answer.
func TestAggregateOverAlwaysPresentInputKeepsEveryTrace(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"count", `{} | count() > 5`},
		{"duration_intrinsic", `{} | avg(duration) > 100ms`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr := emitTraceQL(t, tc.query, s)
			if strings.Contains(sqlStr, "isNotNull") {
				t.Fatalf("this aggregate can never be NULL, so no drop-filter belongs in its SQL; got: %s", sqlStr)
			}
		})
	}
}

// TestMetricsOverTimeSkipsSpansMissingTheAttribute is the metrics-pipeline
// twin: `*_over_time(attr)` and `quantile_over_time(attr, q)` read the same
// Map carrier through the same coercion, and reference funnels them through
// FloatizeAttribute, which answers TypeNil — the NaN sentinel the reducers
// skip — for a missing key.
func TestMetricsOverTimeSkipsSpansMissingTheAttribute(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"max_over_time", `{} | max_over_time(span.latency_ms)`, "max(toFloat64OrNull(`SpanAttributes`[?]))"},
		{"min_over_time", `{} | min_over_time(span.latency_ms)`, "min(toFloat64OrNull(`SpanAttributes`[?]))"},
		{"sum_over_time", `{} | sum_over_time(span.latency_ms)`, "sum(toFloat64OrNull(`SpanAttributes`[?]))"},
		{"avg_over_time", `{} | avg_over_time(span.latency_ms)`, "avg(toFloat64OrNull(`SpanAttributes`[?]))"},
		{
			"quantile_over_time",
			`{} | quantile_over_time(span.latency_ms, 0.95)`,
			"quantileExactInclusive(toFloat64(?))(toFloat64OrNull(`SpanAttributes`[?]))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr := emitTraceQL(t, tc.query, s)
			if !strings.Contains(sqlStr, tc.want) {
				t.Fatalf("metrics aggregate must skip attribute-less spans via toFloat64OrNull (want %q); got: %s", tc.want, sqlStr)
			}
		})
	}
}

// TestRegexArrayFoldLowersLikeItsScalarSpelling covers the two array
// operators this repo's own parser folds an `||` chain of `=~` and an `&&`
// chain of `!~` into (ast/rewrite.go's arrayFoldRules). Before the lowering
// existed, the fold produced an operator mapBinaryOp had no case for, so
// the folded spelling answered `traceql: operator operator(40) is
// unsupported` where the unfolded one answered normally — cerberus
// rejecting a query on the strength of its own rewrite.
//
// The expected per-element fragment is DERIVED from the scalar lowering at
// test time rather than written out here, so the folded and unfolded paths
// cannot drift on how a TraceQL regex renders (the `^(?:…)$` anchoring
// lives in the chsql emitter and must stay the only place that decides it).
func TestRegexArrayFoldLowersLikeItsScalarSpelling(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	// The two scalar spellings the fold is built out of. Their emitted
	// predicates are the fragments the folded form must contain.
	scalarMatch := guardedPredicateBody(t, emitTraceQL(t, `{ span.flavor =~ "van.*" }`, s))
	scalarNotMatch := guardedPredicateBody(t, emitTraceQL(t, `{ span.flavor !~ "van.*" }`, s))

	for _, tc := range []struct {
		name  string
		query string
		// combined is the whole predicate the fold must emit, built from
		// the scalar fragment plus its second element and the combining
		// operator reference's toElementOp/matchAll pair implies.
		combined string
	}{
		{
			// OpRegexMatchAny -> element op OpRegex, matchAll false, so
			// `matchCount > 0`: an OR (the `matchAll` / `matchCount`
			// pair in pkg/traceql/ast_execute.go's BinaryOperation.execute
			// array branch).
			name:     "match_any_is_an_or_of_the_scalar_form",
			query:    `{ span.flavor =~ "van.*" || span.flavor =~ "man.*" }`,
			combined: "(match(`SpanAttributes`[?], ?) OR match(`SpanAttributes`[?], ?))",
		},
		{
			// OpRegexMatchNone -> element op OpNotRegex, matchAll true, so
			// `matchCount == elemCount`: an AND (same branch).
			name:     "match_none_is_an_and_of_the_scalar_form",
			query:    `{ span.flavor !~ "van.*" && span.flavor !~ "man.*" }`,
			combined: "NOT match(`SpanAttributes`[?], ?) AND NOT match(`SpanAttributes`[?], ?)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr := emitTraceQL(t, tc.query, s)
			if !strings.Contains(sqlStr, tc.combined) {
				t.Fatalf("folded regex array must combine the per-element matches as %q; got: %s", tc.combined, sqlStr)
			}
			// Each element renders exactly as the scalar spelling does.
			scalar := scalarMatch
			if strings.Contains(tc.query, "!~") {
				scalar = scalarNotMatch
			}
			if !strings.Contains(sqlStr, scalar) {
				t.Fatalf("folded regex array must render each element like its scalar spelling %q; got: %s", scalar, sqlStr)
			}
			// And the whole thing carries the existence probe: match-any is
			// true for a pattern that matches '', match-none is true for
			// every pattern that does not.
			if !strings.Contains(sqlStr, "mapContains(`SpanAttributes`, ?) AND ") {
				t.Fatalf("folded regex array must guard attribute existence; got: %s", sqlStr)
			}
		})
	}
}

// TestRegexArrayFoldOverANumericMaterializedColumn pins that each element of
// the fold takes the same toString(...) stringify the scalar regex path
// applies: match() cannot evaluate against the Nullable(Int32) column a
// routed numeric attribute reads from, so without it the folded spelling
// would abort the query ClickHouse-side where the unfolded one answers.
func TestRegexArrayFoldOverANumericMaterializedColumn(t *testing.T) {
	t.Parallel()

	on := schema.DefaultOTelTraces()
	on.MaterializedSpanAttributeColumns = map[string]string{
		"http.status_code": "__cerberus_materialized_http.status_code",
	}
	const col = "`__cerberus_materialized_http.status_code`"

	sqlStr := emitTraceQL(t, `{ span.http.status_code =~ "5.." || span.http.status_code =~ "4.." }`, on)
	if strings.Count(sqlStr, "toString("+col+")") != 2 {
		t.Fatalf("every element of the fold must stringify the numeric materialized column; got: %s", sqlStr)
	}
}

// TestUnbackedAttributeMembershipIsFalseInBothPolarities pins the constant
// absentAttributePredicate answers for a carrier the OTel-CH schema does not
// materialise.
//
// Reference does not compute a membership and negate it: binaryTypesValid
// admits a TypeNil operand for `=` / `!=` only (its
// `case TypeNil, TypeStatus, TypeKind` arm in
// pkg/traceql/enum_operators.go), so OpIn AND OpNotIn both fail
// BinaryOperation.execute's type check, which returns StaticFalse outright
// (pkg/traceql/ast_execute.go).
//
// The NOT IN half is the one that was wrong, and ast/rewrite.go is what made
// it reachable from ordinary query text: it folds `!= && !=` into OpNotIn,
// so `{ instrumentation.foo != "a" }` answered false (correct, via
// lowerAbsentFieldBinary) while `{ instrumentation.foo != "a" &&
// instrumentation.foo != "b" }` answered TRUE for every span in the table.
func TestUnbackedAttributeMembershipIsFalseInBothPolarities(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"unfolded_inequality", `{ instrumentation.foo != "a" }`},
		{"and_chain_folded_to_not_in", `{ instrumentation.foo != "a" && instrumentation.foo != "b" }`},
		{"unfolded_equality", `{ instrumentation.foo = "a" }`},
		{"or_chain_folded_to_in", `{ instrumentation.foo = "a" || instrumentation.foo = "b" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlStr, args := emitTraceQLWithArgs(t, tc.query, s)
			if !strings.Contains(sqlStr, "WHERE ?") {
				t.Fatalf("an unbacked carrier must fold to a constant predicate; got: %s", sqlStr)
			}
			if len(args) != 1 || args[0] != false {
				t.Fatalf("the constant must be false in BOTH polarities, got args %#v (SQL: %s)", args, sqlStr)
			}
		})
	}
}

// guardedPredicateBody returns the comparison a guarded single-table query
// emits, with the leading `mapContains(...) AND ` existence probe stripped —
// i.e. the fragment the fold must reproduce per element, read out of a real
// scalar lowering instead of hardcoded here. It fails the test if the scalar
// query did not carry a guard, so the helper cannot silently start returning
// the whole predicate if the guard is ever dropped.
func guardedPredicateBody(t *testing.T, sqlStr string) string {
	t.Helper()
	const where = " WHERE "
	i := strings.LastIndex(sqlStr, where)
	if i < 0 {
		t.Fatalf("emitted SQL has no WHERE clause to read a predicate out of: %s", sqlStr)
	}
	pred := sqlStr[i+len(where):]
	const guard = "mapContains("
	if !strings.HasPrefix(pred, guard) {
		t.Fatalf("expected a guarded scalar predicate to strip, got: %s", pred)
	}
	const sep = " AND "
	j := strings.Index(pred, sep)
	if j < 0 {
		t.Fatalf("guarded predicate has no conjunct after the probe: %s", pred)
	}
	return pred[j+len(sep):]
}
