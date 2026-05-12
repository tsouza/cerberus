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
}

func (*FieldAccess) exprNode() {}

func (f *FieldAccess) Equal(other Expr) bool {
	o, ok := other.(*FieldAccess)
	if !ok {
		return false
	}
	return f.Path == o.Path && f.Source.Equal(o.Source)
}
