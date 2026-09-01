package chplan

// FieldAccess is a chplan expression for resolving dotted-path
// attribute references like TraceQL's `.service.name`,
// `resource.k8s.pod.name`, or `span.http.status_code`.
//
// Conceptually it's a generalised MapAccess: the Source carries the
// outer column to dereference (the carrier map), and Path is the
// (possibly multi-segment) key. The emitter renders it as
// `Source[<dotted-key>]` since the OTel-CH attribute maps store the
// dotted form verbatim as the key — `Attributes['http.status_code']`,
// not nested `Attributes['http']['status_code']`.
//
// Distinct from MapAccess so the lowering layer can express
// scope-aware resolution (resource. vs span. vs scope.) without
// stringifying the AST first.
type FieldAccess struct {
	Source Expr
	Path   string

	// MaterializedColumn, when non-empty, names a schema-provisioned
	// top-level column that is value-identical to Source[Path] (see
	// schema.Traces.MaterializedSpanAttributeColumns /
	// MaterializedResourceAttributeColumns) and that the emitter should
	// reference directly instead of rendering the Map subscript — see
	// chsql's exprFieldAccess. Kept on FieldAccess itself, rather than
	// swapped for a bare ColumnRef at lowering time, so every existing
	// FieldAccess-aware pass (the numeric/bool coercion helpers in
	// internal/traceql/lower.go, PREWHERE promotion in
	// internal/chsql/prewhere.go) keeps treating a materialized
	// attribute reference exactly like an unmaterialized one — only the
	// final SQL rendering differs.
	MaterializedColumn string

	// MaterializedColumnNumeric tags MaterializedColumn as a real
	// ClickHouse numeric type (schema.MaterializedColumnKindNumeric, e.g.
	// Nullable(Int32) for http.status_code) rather than the default
	// LowCardinality(String) (cerberus issue #2869). Always false when
	// MaterializedColumn is empty — an unmaterialized FieldAccess always
	// reads the String-valued attribute map.
	//
	// internal/traceql/lower.go's coerceNumericFieldAccess /
	// coerceBoolFieldAccess consult this to skip the toFloat64OrNull /
	// bool-to-string wraps a String-typed carrier needs: a numeric column
	// is already comparable/arithmetic-able as-is, and wrapping it would
	// be redundant at best (a needless cast) and lossy at worst (an
	// unwanted Int32->Float64 widening). A plain bool, not a fuller kind
	// enum, because coercion only ever needs this one yes/no answer —
	// see schema.MaterializedColumnKind for the fuller type-family
	// classification DDL/preflight consume instead.
	MaterializedColumnNumeric bool
}

func (*FieldAccess) exprNode() {}

func (f *FieldAccess) Equal(other Expr) bool {
	o, ok := other.(*FieldAccess)
	if !ok {
		return false
	}
	return f.Path == o.Path && f.MaterializedColumn == o.MaterializedColumn &&
		f.MaterializedColumnNumeric == o.MaterializedColumnNumeric && f.Source.Equal(o.Source)
}
