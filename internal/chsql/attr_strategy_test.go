package chsql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// renderExpr renders x against a Builder carrying strategies (nil is the
// pre-#2777 default: AttrStrategyMap everywhere) and returns the rendered
// SQL and bound args.
func renderExpr(t *testing.T, strategies chsql.AttrStrategies, x chplan.Expr) (string, []any) {
	t.Helper()
	b := chsql.NewBuilderWithAttrStrategies(strategies)
	if err := b.Expr(x); err != nil {
		t.Fatalf("render: %v", err)
	}
	sql, args, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return sql, args
}

// TestAttrStrategy_MapPathByteIdentical is the refactor guard cerberus
// issue #2777 requires: proof that threading AttrStrategies through
// chsql.Builder / chsql.QueryBuilder's construction did not change ONE
// BYTE of the SQL emitted for the Map path — the only path every one of
// chsql's pre-existing call sites exercises. This is DELIBERATELY separate
// from, and is NOT evidence for, the JSON-path tests below: it only proves
// the pre-#2777 behaviour survived the strategy-threading refactor
// unchanged.
//
// Four constructions of the identical chplan trees are compared against
// the exact literal pre-#2777 SQL shape:
//   - chsql.NewBuilder() (no strategies field even settable before this
//     change existed) — the zero-value / "never heard of AttrStrategies"
//     caller every one of chsql's ~50+ existing NewQuery() sites is,
//   - NewBuilderWithAttrStrategies(nil),
//   - NewBuilderWithAttrStrategies with an explicit AttrStrategyMap entry
//     for the column, and
//   - NewBuilderWithAttrStrategies with a non-nil map that has NO entry at
//     all for the column (the real production shape: a per-signal
//     strategies map that only ever carries entries for columns preflight
//     actually detected as JSON — every other column resolves via the
//     map-miss zero value).
//
// All four must render `<col>[?]` for MapAccess and
// `mapContains(<col>, ?)` for FnMapContainsKey — the exact shape
// TestAttrStrategy_MapPathByteIdentical's git history shows chsql emitted
// before cerberus issue #2777 touched this file.
func TestAttrStrategy_MapPathByteIdentical(t *testing.T) {
	t.Parallel()

	mapAccess := &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: "ResourceAttributes"},
		Key: &chplan.LitString{V: "service.name"},
	}
	mapContains := &chplan.FuncCall{
		Fn:   chplan.FnMapContainsKey,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: "ResourceAttributes"}, &chplan.LitString{V: "service.name"}},
	}

	const wantMapAccessSQL = "`ResourceAttributes`[?]"
	const wantMapContainsSQL = "mapContains(`ResourceAttributes`, ?)"

	variants := []struct {
		name       string
		strategies chsql.AttrStrategies
	}{
		{"legacy NewBuilder (no strategies field settable)", nil},
		{"NewBuilderWithAttrStrategies(nil)", nil},
		{"explicit AttrStrategyMap for the column", chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyMap}},
		{"non-nil map with no entry for the column", chsql.AttrStrategies{"SomeOtherColumn": chsql.AttrStrategyJSON}},
	}

	for _, v := range variants {
		t.Run("MapAccess/"+v.name, func(t *testing.T) {
			t.Parallel()
			sql, args := renderExpr(t, v.strategies, mapAccess)
			if sql != wantMapAccessSQL {
				t.Errorf("MapAccess SQL = %q, want %q", sql, wantMapAccessSQL)
			}
			if len(args) != 1 || args[0] != "service.name" {
				t.Errorf("MapAccess args = %v, want [service.name]", args)
			}
		})
		t.Run("FnMapContainsKey/"+v.name, func(t *testing.T) {
			t.Parallel()
			sql, args := renderExpr(t, v.strategies, mapContains)
			if sql != wantMapContainsSQL {
				t.Errorf("FnMapContainsKey SQL = %q, want %q", sql, wantMapContainsSQL)
			}
			if len(args) != 1 || args[0] != "service.name" {
				t.Errorf("FnMapContainsKey args = %v, want [service.name]", args)
			}
		})
	}

	// chsql.NewBuilder() itself (not NewBuilderWithAttrStrategies) is the
	// literal call every pre-existing production site makes — confirm it
	// independently, not just via the nil-strategies variant above.
	t.Run("MapAccess/chsql.NewBuilder()", func(t *testing.T) {
		t.Parallel()
		b := chsql.NewBuilder()
		if err := b.Expr(mapAccess); err != nil {
			t.Fatalf("render: %v", err)
		}
		sql, _, err := b.Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if sql != wantMapAccessSQL {
			t.Errorf("MapAccess SQL = %q, want %q", sql, wantMapAccessSQL)
		}
	})
}

// TestAttrStrategy_JSONMapAccess pins the JSON-strategy rendering of
// chplan.MapAccess (exprMapAccess's JSON branch), including its
// restriction to a bare ColumnRef with a literal key.
func TestAttrStrategy_JSONMapAccess(t *testing.T) {
	t.Parallel()

	strategies := chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyJSON}

	t.Run("literal key renders the dynamic subcolumn path", func(t *testing.T) {
		t.Parallel()
		mapAccess := &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "ResourceAttributes"},
			Key: &chplan.LitString{V: "http.status_code"},
		}
		sql, args := renderExpr(t, strategies, mapAccess)
		const want = "coalesce(`ResourceAttributes`.`http.status_code`.:String, '')"
		if sql != want {
			t.Errorf("SQL = %q, want %q", sql, want)
		}
		if len(args) != 0 {
			t.Errorf("args = %v, want none — the JSON path key is a compile-time literal, never bound", args)
		}
	})

	t.Run("non-literal key falls back to the Map subscript shape", func(t *testing.T) {
		t.Parallel()
		// ClickHouse's JSON dynamic-subcolumn path syntax is a
		// compile-time path expression, not a function argument — a
		// non-literal key cannot use it, so this MUST fall back to the
		// bracket-subscript shape (which will fail at CH query time
		// against a genuine JSON column — no worse than before this
		// issue's work, and no plan in this codebase currently builds
		// this shape against a bare attribute-map column).
		dynamicKey := &chplan.FuncCall{Fn: chplan.FnLower, Args: []chplan.Expr{&chplan.LitString{V: "X"}}}
		mapAccess := &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "ResourceAttributes"},
			Key: dynamicKey,
		}
		sql, _ := renderExpr(t, strategies, mapAccess)
		const want = "`ResourceAttributes`[lower(?)]"
		if sql != want {
			t.Errorf("SQL = %q, want %q", sql, want)
		}
	})

	t.Run("composed map operand falls back to the Map subscript shape", func(t *testing.T) {
		t.Parallel()
		// mapUpdate/mapConcat/map(...) — anything that ISN'T a bare
		// ColumnRef — really is a ClickHouse Map at the SQL level
		// regardless of the strategy of the column it was seeded from,
		// so it must never take the JSON branch even when the strategies
		// map happens to carry an entry matching no real column here.
		composedMap := &chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{&chplan.LitString{V: "k"}, &chplan.LitString{V: "v"}}}
		mapAccess := &chplan.MapAccess{
			Map: composedMap,
			Key: &chplan.LitString{V: "k"},
		}
		sql, _ := renderExpr(t, strategies, mapAccess)
		const want = "map(?, ?)[?]"
		if sql != want {
			t.Errorf("SQL = %q, want %q", sql, want)
		}
	})

	t.Run("qualified column renders the qualified path", func(t *testing.T) {
		t.Parallel()
		mapAccess := &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "ResourceAttributes", Qualifier: "L"},
			Key: &chplan.LitString{V: "service.name"},
		}
		sql, _ := renderExpr(t, strategies, mapAccess)
		const want = "coalesce(`L`.`ResourceAttributes`.`service.name`.:String, '')"
		if sql != want {
			t.Errorf("SQL = %q, want %q", sql, want)
		}
	})

	t.Run("key with an embedded backtick is safely escaped", func(t *testing.T) {
		t.Parallel()
		mapAccess := &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "ResourceAttributes"},
			Key: &chplan.LitString{V: "weird`key"},
		}
		sql, _ := renderExpr(t, strategies, mapAccess)
		const want = "coalesce(`ResourceAttributes`.`weird``key`.:String, '')"
		if sql != want {
			t.Errorf("SQL = %q, want %q", sql, want)
		}
	})
}

// TestAttrStrategy_JSONMapContainsKey pins the JSON-strategy rendering of
// chplan.FnMapContainsKey (exprMapContainsKey's JSON branch).
func TestAttrStrategy_JSONMapContainsKey(t *testing.T) {
	t.Parallel()

	strategies := chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyJSON}

	t.Run("JSON-strategy column renders has(JSONAllPaths(...))", func(t *testing.T) {
		t.Parallel()
		call := &chplan.FuncCall{
			Fn:   chplan.FnMapContainsKey,
			Args: []chplan.Expr{&chplan.ColumnRef{Name: "ResourceAttributes"}, &chplan.LitString{V: "http.status_code"}},
		}
		sql, args := renderExpr(t, strategies, call)
		const want = "has(JSONAllPaths(`ResourceAttributes`), ?)"
		if sql != want {
			t.Errorf("SQL = %q, want %q", sql, want)
		}
		if len(args) != 1 || args[0] != "http.status_code" {
			t.Errorf("args = %v, want [http.status_code] — unlike MapAccess, the key need not be a literal", args)
		}
	})

	t.Run("non-ColumnRef first argument falls back to mapContains", func(t *testing.T) {
		t.Parallel()
		composedMap := &chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{&chplan.LitString{V: "k"}, &chplan.LitString{V: "v"}}}
		call := &chplan.FuncCall{
			Fn:   chplan.FnMapContainsKey,
			Args: []chplan.Expr{composedMap, &chplan.LitString{V: "k"}},
		}
		sql, _ := renderExpr(t, strategies, call)
		const want = "mapContains(map(?, ?), ?)"
		if sql != want {
			t.Errorf("SQL = %q, want %q", sql, want)
		}
	})
}
