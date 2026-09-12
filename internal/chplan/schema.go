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

// HistogramField identifies one physical histogram component independently
// of its configured storage name and public histogram-payload role.
type HistogramField uint8

const (
	// HistogramFieldNone marks a column with no histogram identity.
	HistogramFieldNone HistogramField = iota
	HistogramFieldCount
	HistogramFieldSum
	HistogramFieldScale
	HistogramFieldZeroThreshold
	HistogramFieldZeroCount
	HistogramFieldPositiveOffset
	HistogramFieldPositiveBucketCounts
	HistogramFieldNegativeOffset
	HistogramFieldNegativeBucketCounts
	HistogramFieldBucketCounts
	HistogramFieldExplicitBounds
)

func (field HistogramField) valid() bool {
	return field >= HistogramFieldCount && field <= HistogramFieldExplicitBounds
}

// Column is one output. An empty Name denotes an unaliased expression whose
// driver-assigned name cannot be derived without rendering SQL.
type Column struct {
	Name           string
	Role           ColumnRole
	HistogramField HistogramField
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

// FindHistogramField returns the uniquely identified histogram column.
// Invalid, missing, duplicate, unnamed, wrongly-role-tagged, and physically
// name-aliased identities fail closed.
func (s Schema) FindHistogramField(field HistogramField) (Column, bool) {
	if !field.valid() {
		return Column{}, false
	}
	var found Column
	seen := false
	for _, column := range s.Columns {
		if column.HistogramField != field {
			continue
		}
		if seen || column.Name == "" || column.Role != RoleHistogramField {
			return Column{}, false
		}
		for _, candidate := range s.Columns {
			if candidate.Name == column.Name && candidate.HistogramField != column.HistogramField {
				return Column{}, false
			}
		}
		found, seen = column, true
	}
	return found, seen
}

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
		c.HistogramField = canonicalHistogramField(name)
	}
	return c
}

func canonicalHistogramField(name string) HistogramField {
	switch name {
	case HistogramCountColumn:
		return HistogramFieldCount
	case HistogramSumColumn:
		return HistogramFieldSum
	case HistogramScaleColumn:
		return HistogramFieldScale
	case HistogramZeroThresholdColumn:
		return HistogramFieldZeroThreshold
	case HistogramZeroCountColumn:
		return HistogramFieldZeroCount
	case HistogramPositiveOffsetColumn:
		return HistogramFieldPositiveOffset
	case HistogramPositiveBucketCountsColumn:
		return HistogramFieldPositiveBucketCounts
	case HistogramNegativeOffsetColumn:
		return HistogramFieldNegativeOffset
	case HistogramNegativeBucketCountsColumn:
		return HistogramFieldNegativeBucketCounts
	default:
		return HistogramFieldNone
	}
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
	return Schema{Columns: []Column{{Name: metric, Role: RoleMetricName}, {Name: attributes, Role: RoleAttributes}, {Name: timestamp, Role: RoleTimestamp}, {Name: value, Role: RoleValue}}}
}

func histogramColumns() []Column {
	return []Column{
		{Name: HistogramCountColumn, Role: RoleHistogramField, HistogramField: HistogramFieldCount},
		{Name: HistogramSumColumn, Role: RoleHistogramField, HistogramField: HistogramFieldSum},
		{Name: HistogramScaleColumn, Role: RoleHistogramField, HistogramField: HistogramFieldScale},
		{Name: HistogramZeroThresholdColumn, Role: RoleHistogramField, HistogramField: HistogramFieldZeroThreshold},
		{Name: HistogramZeroCountColumn, Role: RoleHistogramField, HistogramField: HistogramFieldZeroCount},
		{Name: HistogramPositiveOffsetColumn, Role: RoleHistogramField, HistogramField: HistogramFieldPositiveOffset},
		{Name: HistogramPositiveBucketCountsColumn, Role: RoleHistogramField, HistogramField: HistogramFieldPositiveBucketCounts},
		{Name: HistogramNegativeOffsetColumn, Role: RoleHistogramField, HistogramField: HistogramFieldNegativeOffset},
		{Name: HistogramNegativeBucketCountsColumn, Role: RoleHistogramField, HistogramField: HistogramFieldNegativeBucketCounts},
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
		if (column.Role == RoleHistogramField) != column.HistogramField.valid() {
			return SampleKindInvalid
		}
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
				if column.Name == field.Name && column.HistogramField == field.HistogramField {
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

// LiveSampleKind refines a node's physical sample schema with the only
// value-domain proof represented in the plan: a discriminator-zero filter over
// a mixed payload contains float rows only while retaining the mixed columns.
// Filter and OrderBy preserve that proof; every other node is a proof barrier.
func LiveSampleKind(n Node) SampleKind {
	if n == nil {
		return SampleKindOpaque
	}
	kind := n.RowType().SampleKind()
	for {
		if filter, ok := n.(*Filter); ok {
			if predicate, ok := filter.Predicate.(*LitBool); ok && !predicate.V {
				return SampleKindFloat
			}
		}
		if kind == SampleKindMixed && IsMixedFloatNarrowing(n) {
			return SampleKindFloat
		}
		switch node := n.(type) {
		case *Filter:
			n = node.Input
		case *OrderBy:
			n = node.Input
		default:
			return kind
		}
	}
}

func samplePublicRole(role ColumnRole) bool {
	switch role {
	case RoleMetricName, RoleAttributes, RoleTimestamp, RoleAnchor, RoleValue, RoleHistogramField, RoleDiscriminator:
		return true
	default:
		return false
	}
}

// RowShapeFromSchema folds a validated physical sample contract into the
// diagnostic row-shape vocabulary. Opaque, open, incomplete, and invalid
// schemas retain the sample default; that default is not proof of live floats.
func RowShapeFromSchema(s Schema) RowShape {
	switch s.SampleKind() {
	case SampleKindMixed:
		return MixedRowShape
	case SampleKindHistogram:
		return HistogramRowShape
	case SampleKindFloat:
		if s.Has(RoleAttributes) && s.Has(RoleAnchor) {
			return GridWindowRowShape
		}
		if s.Has(RoleAttributes) && !s.Has(RoleTimestamp) && !s.Has(RoleAnchor) {
			return ReducedWindowRowShape
		}
	}
	return SampleRowShape
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
