package chplan

import "slices"

// ColumnRole describes a column's public purpose, independently of its storage name.
type ColumnRole uint8

const (
	RoleOpaque ColumnRole = iota
	RoleMetricName
	RoleAttributes
	RoleTimestamp
	RoleAnchor
	RoleValue
	RoleHistogramField
	RoleDiscriminator
	RoleTraceID
	RoleSpanID
	RoleParentSpanID
)

// Column is one output. An empty Name denotes an unaliased expression whose
// driver-assigned name cannot be derived without rendering SQL.
type Column struct {
	Name string
	Role ColumnRole
}

// Schema describes a node's own output. Closed schemas preserve SELECT order.
// Open schemas describe known columns of a wildcard output; their order is not
// a claim about the physical table's order.
type Schema struct {
	Columns []Column
	Open    bool
}

// Find returns the first column carrying role, in declaration order.
func (s Schema) Find(role ColumnRole) (Column, bool) {
	for _, c := range s.Columns {
		if c.Role == role {
			return c, true
		}
	}
	return Column{}, false
}

// Has reports whether any output carries role.
func (s Schema) Has(role ColumnRole) bool { _, ok := s.Find(role); return ok }

// ByName looks up a named output; unnamed expressions never match.
func (s Schema) ByName(name string) (Column, bool) {
	if name != "" {
		for _, c := range s.Columns {
			if c.Name == name {
				return c, true
			}
		}
	}
	return Column{}, false
}

// Equal compares the complete schema, including declaration order.
func (s Schema) Equal(other Schema) bool {
	return s.Open == other.Open && slices.Equal(s.Columns, other.Columns)
}

func roleColumn(name string, input Schema, declared []Column) Column {
	if c, ok := (Schema{Columns: declared}).ByName(name); ok {
		return c
	}
	if c, ok := input.ByName(name); ok {
		return c
	}
	c := Column{Name: name}
	switch name {
	case RangeWindowAnchorColumn:
		c.Role = RoleAnchor
	case MixedDiscriminatorColumn:
		c.Role = RoleDiscriminator
	case HistogramCountColumn, HistogramSumColumn, HistogramScaleColumn,
		HistogramZeroCountColumn, HistogramZeroThresholdColumn,
		HistogramPositiveOffsetColumn, HistogramPositiveBucketCountsColumn,
		HistogramNegativeOffsetColumn, HistogramNegativeBucketCountsColumn:
		c.Role = RoleHistogramField
	}
	return c
}

func selectNames(input Schema, names []string, roles []Column) Schema {
	out := Schema{Columns: make([]Column, len(names))}
	for i, name := range names {
		out.Columns[i] = roleColumn(name, input, roles)
	}
	return out
}

func projectSchema(input Schema, projections []Projection, roles []Column) Schema {
	if len(projections) == 0 {
		return input
	}
	out := Schema{Columns: make([]Column, len(projections))}
	for i, p := range projections {
		out.Columns[i] = roleColumn(ProjectionOutputName(p), input, roles)
	}
	return out
}

func groupSchema(input Schema, keys []Expr, aliases []string, roles []Column) Schema {
	out := Schema{Columns: make([]Column, len(keys))}
	for i, key := range keys {
		p := Projection{Expr: key}
		if i < len(aliases) {
			p.Alias = aliases[i]
		}
		out.Columns[i] = roleColumn(ProjectionOutputName(p), input, roles)
	}
	return out
}

func appendReducers(out, input Schema, funcs []AggFunc, roles []Column) Schema {
	for _, f := range funcs {
		out.Columns = append(out.Columns, roleColumn(f.Alias, input, roles))
	}
	return out
}

func sampleSchema(metric, attributes, timestamp, value string) Schema {
	return Schema{Columns: []Column{{metric, RoleMetricName}, {attributes, RoleAttributes}, {timestamp, RoleTimestamp}, {value, RoleValue}}}
}

func histogramColumns() []Column {
	return []Column{
		{HistogramCountColumn, RoleHistogramField},
		{HistogramSumColumn, RoleHistogramField},
		{HistogramScaleColumn, RoleHistogramField},
		{HistogramZeroThresholdColumn, RoleHistogramField},
		{HistogramZeroCountColumn, RoleHistogramField},
		{HistogramPositiveOffsetColumn, RoleHistogramField},
		{HistogramPositiveBucketCountsColumn, RoleHistogramField},
		{HistogramNegativeOffsetColumn, RoleHistogramField},
		{HistogramNegativeBucketCountsColumn, RoleHistogramField},
	}
}

// RowShapeFromSchema folds physical columns into the legacy sample vocabulary.
// Opaque relational outputs have no sample contract and retain its default.
func RowShapeFromSchema(s Schema) RowShape {
	switch {
	case s.Has(RoleDiscriminator):
		return MixedRowShape
	case s.Has(RoleHistogramField):
		return HistogramRowShape
	case s.Has(RoleAnchor) && !s.Has(RoleMetricName):
		return GridWindowRowShape
	case s.Has(RoleValue) && !s.Has(RoleMetricName) && !s.Has(RoleTimestamp) && !s.Has(RoleAnchor):
		return ReducedWindowRowShape
	default:
		return SampleRowShape
	}
}

// IsMixedFloatNarrowing reports an explicit discriminator filter that retains
// only float rows while physically preserving the mixed payload columns.
func IsMixedFloatNarrowing(n Node) bool {
	f, ok := n.(*Filter)
	return ok && f.Input != nil && f.Input.RowType().Has(RoleDiscriminator) && discriminatorEquals(f.Predicate, 0)
}

func discriminatorEquals(expr Expr, want int64) bool {
	b, ok := expr.(*Binary)
	if !ok {
		return false
	}
	if b.Op == OpAnd {
		return discriminatorEquals(b.Left, want) || discriminatorEquals(b.Right, want)
	}
	if b.Op != OpEq {
		return false
	}
	match := func(column, literal Expr) bool {
		c, cok := column.(*ColumnRef)
		v, vok := literal.(*LitInt)
		return cok && vok && c.Name == MixedDiscriminatorColumn && v.V == want
	}
	return match(b.Left, b.Right) || match(b.Right, b.Left)
}
