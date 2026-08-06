package chplan

// SampleColumns names the four columns of the canonical sample shape a
// PromQL-facing consumer expects a plan to expose: (MetricName,
// Attributes, Timestamp, Value). The names are configuration — the
// schema override config can rename any of them — so every classifier
// below takes them rather than hard-coding the OTel-CH defaults.
type SampleColumns struct {
	MetricName string
	Attributes string
	Timestamp  string
	Value      string
}

// IsDerivedShape reports whether n's output schema lacks the canonical
// MetricName column, i.e. exposes only `(group keys…, timestamp,
// value)`.
//
// This is the ONE classifier for that question. Both the HTTP layer
// (which wraps a plan in the canonical chclient.Sample projection) and
// the chsql emitter (which canonicalises each VectorSetOp arm) route
// through it, because the two answers must be the same answer: they
// describe one property of one IR node, and a disagreement between them
// is a wrong-SQL bug rather than a difference of opinion. When they were
// two switches they drifted — the emitter grew *MetricsAggregate and
// *MetricsHistogramOverTime arms the HTTP copy never got — while the
// docstring on each asserted they were in sync.
//
// The reducing and metrics-pipeline nodes are derived: their SELECT
// projects the group keys plus the reduced value, so MetricName never
// exists in that scope and a consumer must SYNTHESISE it as the empty
// string rather than reference it (ClickHouse rejects the reference with
// code 47, `Unknown expression identifier 'MetricName'`).
//
// Filter is transparent — it constrains rows, never columns.
//
// A Project ON TOP of a derived shape stays derived unless its own
// projections name all four canonical columns as outputs: that is the
// LWR-style `Project [MetricName, Attributes, TimeUnix, Value]` shape
// lowered for canonical-shape consumers. The value-rewrite Project that
// `projectValueOverInner` emits (clamp / abs / unary minus / the
// out-of-range quantile_over_time fold) carries only `[Attributes, …,
// Value]` over a derived inner and must stay classified as derived.
//
// Every other node — Scan, RangeLWR, a nested VectorSetOp (whose emit
// wraps it in its own canonical-column SELECT), … — is canonical.
func IsDerivedShape(n Node, cols SampleColumns) bool {
	switch v := n.(type) {
	case *RangeWindow,
		*RangeWindowNative,
		*Aggregate,
		*MetricsAggregate,
		*MetricsHistogramOverTime:
		return true
	case *Filter:
		return IsDerivedShape(v.Input, cols)
	case *Project:
		if ProjectExposesCanonical(v, cols) {
			return false
		}
		return IsDerivedShape(v.Input, cols)
	}
	return false
}

// ProjectExposesCanonical reports whether p's projections name ALL four
// canonical column outputs. Anything less disqualifies it: a two-output
// value-rewrite Project (`Attributes`, `Value`) sits over an inner scope
// that carries neither MetricName nor the timestamp, so treating it as
// canonical emits a reference to a column the subquery never exposes.
func ProjectExposesCanonical(p *Project, cols SampleColumns) bool {
	needed := map[string]bool{
		cols.MetricName: false,
		cols.Attributes: false,
		cols.Timestamp:  false,
		cols.Value:      false,
	}
	for _, proj := range p.Projections {
		name := ProjectionOutputName(proj)
		if _, ok := needed[name]; ok {
			needed[name] = true
		}
	}
	for _, ok := range needed {
		if !ok {
			return false
		}
	}
	return true
}

// ProjectionOutputName returns the column name a Projection exposes: the
// explicit Alias when set, otherwise the bare-ColumnRef name when the
// Expr is a column reference passed through under its own name. A
// computed Expr with no Alias names no output column and returns "" —
// the conservative answer for every caller, all of which ask "does this
// projection expose column X".
func ProjectionOutputName(p Projection) string {
	if p.Alias != "" {
		return p.Alias
	}
	if ref, ok := p.Expr.(*ColumnRef); ok {
		return ref.Name
	}
	return ""
}

// ProjectionOutputsColumn reports whether p exposes an output named col.
// An empty col names no column, so nothing exposes it — without that
// guard a computed unaliased projection (output name "") would answer
// yes to a caller that lost track of its column name.
func ProjectionOutputsColumn(p Projection, col string) bool {
	return col != "" && ProjectionOutputName(p) == col
}
