package chsql

import "context"

// AttrStrategy selects how one attribute-map column is physically stored,
// and therefore how a per-key expression against it (chplan.MapAccess,
// chplan.FnMapContainsKey) must be rendered.
//
// cerberus issue #2777: the OTel ClickHouse Exporter's `json:true` schema
// variant types the logs/traces attribute-map columns (Attributes /
// ResourceAttributes / ScopeAttributes) as ClickHouse's native JSON rather
// than Map(String,String). internal/preflight's boot probe already detects
// this shape (isJSONAttrType) and boots with a WARNING instead of a FATAL
// (see preflight.go's Gate 2); this type is the other half — the resolved
// per-column DECISION that lets chsql render the right SQL shape once the
// probe has spoken, rather than re-deriving it per query.
type AttrStrategy int

const (
	// AttrStrategyMap is the zero value and the default: the column is a
	// genuine ClickHouse Map(String,String), and MapAccess / mapContains
	// render exactly as they always have — a bracket subscript / a plain
	// mapContains(...) call. Every Builder and QueryBuilder defaults to
	// this because AttrStrategies is nil unless a caller explicitly
	// resolves and passes one (see AttrStrategies.Lookup) — so every one
	// of chsql's ~50+ existing NewQuery() call sites, and every plan that
	// never touches a JSON-typed column, keeps emitting byte-identical SQL.
	// The refactor-guard test in attr_strategy_test.go pins this.
	AttrStrategyMap AttrStrategy = iota

	// AttrStrategyJSON means the column is ClickHouse's native JSON type.
	// exprMapAccess and the FnMapContainsKey render hook branch on it —
	// see their doc comments for the exact SQL shape and the
	// missing-vs-empty semantics decision. Only applies when the map/column
	// operand is a bare *chplan.ColumnRef (never a composed intermediate
	// Map expression built by mapUpdate/mapConcat/map(), which really is
	// a Map at the SQL level regardless of the strategy of the column it
	// was seeded from) — MapAccess additionally requires a literal
	// (*chplan.LitString) key, since ClickHouse's JSON dynamic-subcolumn
	// path syntax is a compile-time path expression, not a bound
	// parameter.
	//
	// Scope (cerberus issue #2777, this slice): wired for LOGS only.
	// Traces detection already ships (preflight's jsonAttrMapCompat), but
	// query-time rendering for traces is tracked separately as cerberus
	// issue #3062 — internal/engine never resolves a non-nil
	// AttrStrategies for a TraceQL request in this version, so a
	// JSON-typed traces attribute column still fails at query time
	// exactly as before (no behaviour change, no untested accidental
	// support). Metrics never sets this: attribute maps there carry
	// series identity and preflight FATALs on anything but Map.
	AttrStrategyJSON
)

// AttrStrategies resolves the AttrStrategy for one column, keyed by the
// column's bare name (schema.Logs.AttributesColumn etc — never
// table-qualified, and never disambiguated across signals: see the doc
// below on why that is safe).
//
// Why bare-name keying doesn't collide across signals: the OTel-CH default
// schema reuses "ResourceAttributes" / "ScopeAttributes" as the column name
// across metrics, logs AND traces, so a single global bare-name map fed
// from every signal's boot-probe findings would misapply one signal's
// strategy to another's identically-named column. AttrStrategies sidesteps
// this by being scoped PER SIGNAL at the call site instead: preflight
// resolves one map per signal (Result.LogsAttrStrategies /
// TracesAttrStrategies), and internal/engine injects only the map for the
// signal actually being queried (WithAttrStrategies(ctx, ...) called with
// LogsAttrStrategies for a LogQL request, nil for PromQL/TraceQL in this
// version) — so a chsql.Builder rendering one query only ever sees that one
// signal's resolved strategies, never a merged cross-signal map.
type AttrStrategies map[string]AttrStrategy

// Lookup returns the AttrStrategy configured for column, or
// AttrStrategyMap when s is nil or has no entry for it — the safe zero
// value that keeps every construction path that never heard of this
// mechanism (a bare NewBuilder()/NewQuery(), the ~50+ pre-existing chsql
// call sites) rendering exactly the SQL it always has.
func (s AttrStrategies) Lookup(column string) AttrStrategy {
	if s == nil {
		return AttrStrategyMap
	}
	return s[column]
}

// attrStrategiesCtxKey is the context key for WithAttrStrategies /
// attrStrategiesFromCtx. Mirrors the unexported-key pattern
// WithSpansTable/spansTableFromCtx (scan_resource_bound.go) and
// WithDeltaPrefixLookback/deltaPrefixLookbackFromCtx (range_window.go)
// already use for per-request emit-time configuration.
type attrStrategiesCtxKey struct{}

// WithAttrStrategies returns a context carrying the resolved
// AttrStrategies chsql.Emit renders the plan's attribute-map accesses
// against. internal/engine calls this once per request, scoped to the
// signal actually being queried (see AttrStrategies's doc) — a caller
// that never calls it (every non-engine chsql.Emit call, every direct
// QueryBuilder use) gets the nil default, i.e. AttrStrategyMap
// everywhere.
func WithAttrStrategies(ctx context.Context, s AttrStrategies) context.Context {
	return context.WithValue(ctx, attrStrategiesCtxKey{}, s)
}

// attrStrategiesFromCtx reads the AttrStrategies WithAttrStrategies set,
// or nil (AttrStrategyMap everywhere) when it was never called.
func attrStrategiesFromCtx(ctx context.Context) AttrStrategies {
	s, _ := ctx.Value(attrStrategiesCtxKey{}).(AttrStrategies)
	return s
}
