package chplan

// CanonicalMapFunc is the ClickHouse function that establishes the
// key-order invariant every series-identity key depends on. Named so
// [CanonicalAttributesExpr] and its own idempotence check cannot drift
// apart.
const CanonicalMapFunc = "mapSort"

// CanonicalAttributesExpr wraps a label Map in ClickHouse
// [CanonicalMapFunc], establishing the invariant every series-identity
// key in the engine depends on: the attributes Map is ordered by key.
//
// A CH Map compares and groups POSITIONALLY over its (keys, values)
// arrays, so map('job','api','dc','eu') and map('dc','eu','job','api')
// are unequal despite carrying the same label set. Divergent key order
// reaches the store straight from ingestion — the OTel-CH exporter
// preserves OTLP wire order and never sorts — so two deliveries of one
// logical stream can land under two physical orders. Every consumer that
// keys on the whole Map (a RangeWindow's per-series partition, an
// Aggregate's GROUP BY, `without(...)`, a topk PARTITION BY, the
// vector-join arms) would then see two series where there is one:
// `count()` doubles, `sum()` adds the same series twice, and a range
// function sees one sample per partition and returns empty. The wrong
// answer is silent — it presents as a shifted value, not as a visibly
// doubled series.
//
// mapSort is order-only and idempotent, so it can never merge two
// genuinely different label sets, and re-wrapping an already-canonical
// map is a no-op — which is why callers may wrap defensively without
// paying a second sort.
//
// This lives in the plan IR rather than in one head's lowering because
// all three heads bind the same invariant against the same Map columns;
// each head's lowering wraps its own identity projection with it.
func CanonicalAttributesExpr(expr Expr) Expr {
	if call, ok := expr.(*FuncCall); ok && call.Name == CanonicalMapFunc {
		return expr
	}
	return &FuncCall{Name: CanonicalMapFunc, Args: []Expr{expr}}
}
