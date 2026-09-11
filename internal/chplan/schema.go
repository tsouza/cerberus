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

// SampleKind classifies a closed output schema by the sample payload it
// publishes. Opaque and invalid are deliberately separate: opaque means the
// schema makes no complete sample claim, while invalid means it makes a
// contradictory or ambiguous one that consumers must reject.
type SampleKind uint8

const (
	SampleKindOpaque SampleKind = iota
	SampleKindFloat
	SampleKindHistogram
	SampleKindMixed
	SampleKindInvalid
)

// String renders the sample-kind vocabulary used by invariant diagnostics.
func (k SampleKind) String() string {
	switch k {
	case SampleKindOpaque:
		return "opaque"
	case SampleKindFloat:
		return "float"
	case SampleKindHistogram:
		return "histogram"
	case SampleKindMixed:
		return "mixed"
	case SampleKindInvalid:
		return "invalid"
	default:
		return "unknown"
	}
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

// HistogramPayloadColumns returns the ordered canonical native-histogram
// payload published by HistogramProjection and understood by the decoder.
// The returned slice is independent and may be used to build projections.
func HistogramPayloadColumns() []Column { return histogramColumns() }

// HasHistogramPayload distinguishes the complete public native-histogram
// payload from raw storage fields and partial intermediate helper columns.
func (s Schema) HasHistogramPayload() bool {
	for _, want := range histogramColumns() {
		got, ok := s.ByName(want.Name)
		if !ok || got.Role != RoleHistogramField {
			return false
		}
	}
	return true
}

// SampleKind validates and classifies the public sample contract. Public
// roles must be named and unambiguous, and histogram fields must be the
// complete canonical payload. Names on opaque or other-role columns never
// create a sample contract, but they may not shadow a public sample output.
// Structurally valid open schemas remain opaque because undeclared outputs can
// invalidate an otherwise plausible contract.
func (s Schema) SampleKind() SampleKind {
	const samplePublicRoleCount = int(RoleDiscriminator) + 1
	const histogramPayloadColumnCount = 9

	var roleCount [samplePublicRoleCount]int
	var histogramSeen [histogramPayloadColumnCount]bool
	canonicalHistogram := histogramColumns()
	for _, column := range s.Columns {
		if !samplePublicRole(column.Role) {
			continue
		}
		if column.Name == "" {
			return SampleKindInvalid
		}
		nameCount := 0
		for _, candidate := range s.Columns {
			if candidate.Name == column.Name {
				nameCount++
			}
		}
		if nameCount != 1 {
			return SampleKindInvalid
		}
		roleCount[column.Role]++
		if column.Role == RoleHistogramField {
			matched := false
			for i, field := range canonicalHistogram {
				if column.Name == field.Name {
					histogramSeen[i] = true
					matched = true
					break
				}
			}
			if !matched {
				return SampleKindInvalid
			}
		}
	}

	for _, role := range [...]ColumnRole{
		RoleMetricName,
		RoleAttributes,
		RoleTimestamp,
		RoleAnchor,
		RoleValue,
		RoleDiscriminator,
	} {
		if roleCount[role] > 1 {
			return SampleKindInvalid
		}
	}

	hasHistogram := roleCount[RoleHistogramField] != 0
	hasDiscriminator := roleCount[RoleDiscriminator] != 0
	if hasHistogram {
		if roleCount[RoleHistogramField] != len(canonicalHistogram) {
			return SampleKindInvalid
		}
		for _, seen := range histogramSeen {
			if !seen {
				return SampleKindInvalid
			}
		}
	} else if hasDiscriminator {
		return SampleKindInvalid
	}

	if hasDiscriminator {
		for _, role := range [...]ColumnRole{RoleMetricName, RoleAttributes, RoleTimestamp, RoleValue} {
			if roleCount[role] != 1 {
				return SampleKindInvalid
			}
		}
	}
	if s.Open {
		return SampleKindOpaque
	}
	if hasDiscriminator {
		return SampleKindMixed
	}
	if hasHistogram {
		return SampleKindHistogram
	}
	if roleCount[RoleValue] == 1 {
		return SampleKindFloat
	}
	return SampleKindOpaque
}

func samplePublicRole(role ColumnRole) bool {
	switch role {
	case RoleMetricName, RoleAttributes, RoleTimestamp, RoleAnchor, RoleValue, RoleHistogramField, RoleDiscriminator:
		return true
	default:
		return false
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
	const floatDiscriminatorValue = 0
	f, ok := n.(*Filter)
	if !ok || f.Input == nil {
		return false
	}
	var discriminator string
	for _, column := range f.Input.RowType().Columns {
		if column.Role != RoleDiscriminator {
			continue
		}
		if discriminator != "" || column.Name == "" {
			return false
		}
		discriminator = column.Name
	}
	return discriminator != "" && discriminatorEquals(f.Predicate, discriminator, floatDiscriminatorValue)
}

func discriminatorEquals(expr Expr, name string, want int64) bool {
	b, ok := expr.(*Binary)
	if !ok {
		return false
	}
	if b.Op == OpAnd {
		return discriminatorEquals(b.Left, name, want) || discriminatorEquals(b.Right, name, want)
	}
	if b.Op != OpEq {
		return false
	}
	match := func(column, literal Expr) bool {
		c, cok := column.(*ColumnRef)
		v, vok := literal.(*LitInt)
		return cok && vok && c.Name == name && c.Qualifier == "" && v.V == want
	}
	return match(b.Left, b.Right) || match(b.Right, b.Left)
}
