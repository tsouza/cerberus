package promql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

type sampleNamePolicy uint8

const (
	dropSampleName sampleNamePolicy = iota
	preserveSampleName
)

type samplePayloadPolicy uint8

const (
	floatSamplePayload samplePayloadPolicy = iota
	preserveMixedSamplePayload
)

// sampleProjectionPolicy belongs to the wrapper, not to the input schema.
// Physical roles identify columns; they do not decide PromQL name or payload
// semantics. Neither policy is inferred from a plan's node kind.
type sampleProjectionPolicy struct {
	name    sampleNamePolicy
	payload samplePayloadPolicy
}

// sampleProjectionLayout preserves the established projection boundary. A
// canonical projection over a grid and a direct grid can expose identical
// physical roles while requiring different SQL and HTTP timestamp handling.
type sampleProjectionLayout struct {
	canonical          bool
	anchored           bool
	materializeAliases bool
}

type sampleRoleRefs struct {
	MetricName *chplan.ColumnRef
	Attributes *chplan.ColumnRef
	Timestamp  *chplan.ColumnRef
	Anchor     *chplan.ColumnRef
	Value      *chplan.ColumnRef
}

type sampleRoleRewrite struct {
	attributes chplan.Expr
	value      chplan.Expr
}

type sampleTemporalRoles struct {
	metricName string
	attributes string
	timestamp  string
	anchor     string
	value      string
}

func (r sampleTemporalRoles) refs() sampleRoleRefs {
	refs := sampleRoleRefs{
		Attributes: &chplan.ColumnRef{Name: r.attributes},
		Value:      &chplan.ColumnRef{Name: r.value},
	}
	if r.metricName != "" {
		refs.MetricName = &chplan.ColumnRef{Name: r.metricName}
	}
	if r.timestamp != "" {
		refs.Timestamp = &chplan.ColumnRef{Name: r.timestamp}
	}
	if r.anchor != "" {
		refs.Anchor = &chplan.ColumnRef{Name: r.anchor}
	}
	return refs
}

// resolveSampleTemporalLayout validates the physical roles that describe one
// sample's identity and temporal envelope. It deliberately says nothing about
// whether a caller preserves MetricName or rewrites Value: those are wrapper
// policies, while this helper answers only which live columns carry the roles.
func resolveSampleTemporalLayout(row chplan.Schema) (sampleTemporalRoles, sampleProjectionLayout, error) {
	if row.Open {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, fmt.Errorf("promql: temporal sample layout requires a closed schema")
	}
	if row.SampleKind() == chplan.SampleKindInvalid {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, fmt.Errorf("promql: temporal sample layout has an invalid sample schema")
	}

	attributes, _, err := resolveOptionalSampleRoleName(row, chplan.RoleAttributes)
	if err != nil {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, err
	}
	if attributes == "" {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, fmt.Errorf("promql: temporal sample layout is missing the attributes role")
	}
	value, _, err := resolveOptionalSampleRoleName(row, chplan.RoleValue)
	if err != nil {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, err
	}
	if value == "" {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, fmt.Errorf("promql: temporal sample layout is missing the value role")
	}
	metricName, hasMetricName, err := resolveOptionalSampleRoleName(row, chplan.RoleMetricName)
	if err != nil {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, err
	}
	timestamp, hasTimestamp, err := resolveOptionalSampleRoleName(row, chplan.RoleTimestamp)
	if err != nil {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, err
	}
	anchor, hasAnchor, err := resolveOptionalSampleRoleName(row, chplan.RoleAnchor)
	if err != nil {
		return sampleTemporalRoles{}, sampleProjectionLayout{}, err
	}

	roles := sampleTemporalRoles{
		metricName: metricName,
		attributes: attributes,
		timestamp:  timestamp,
		anchor:     anchor,
		value:      value,
	}
	switch {
	case hasAnchor && hasTimestamp:
		return roles, sampleProjectionLayout{canonical: hasMetricName, anchored: true}, nil
	case hasAnchor:
		return sampleTemporalRoles{}, sampleProjectionLayout{}, fmt.Errorf("promql: temporal sample layout anchor role requires a timestamp role")
	case hasTimestamp:
		return roles, sampleProjectionLayout{canonical: true}, nil
	case hasMetricName:
		return sampleTemporalRoles{}, sampleProjectionLayout{}, fmt.Errorf("promql: temporal sample layout metric-name role requires a timestamp role")
	default:
		return roles, sampleProjectionLayout{}, nil
	}
}

// sourceMetrics adapts existing expression builders that take configured names.
// The copy is input-only: public output aliases still use the caller's schema.
func (r sampleRoleRefs) sourceMetrics(s schema.Metrics) schema.Metrics {
	if r.MetricName != nil {
		s.MetricNameColumn = r.MetricName.Name
	}
	if r.Attributes != nil {
		s.AttributesColumn = r.Attributes.Name
	}
	if r.Timestamp != nil {
		s.TimestampColumn = r.Timestamp.Name
	}
	if r.Value != nil {
		s.ValueColumn = r.Value.Name
	}
	return s
}

// projectSampleRoles resolves the actual input names before constructing either
// replacement expression. Output aliases and order remain the public sample
// contract; no expression is rewritten after it has already captured a name.
func projectSampleRoles(
	inner chplan.Node,
	s schema.Metrics,
	policy sampleProjectionPolicy,
	layout sampleProjectionLayout,
	rewrite func(sampleRoleRefs) sampleRoleRewrite,
) *chplan.Project {
	if policy.name != dropSampleName && policy.name != preserveSampleName {
		panic("promql: sample forwarder requires an explicit name policy")
	}
	if policy.payload != floatSamplePayload && policy.payload != preserveMixedSamplePayload {
		panic("promql: sample forwarder requires an explicit payload policy")
	}
	row := inner.RowType()
	completeHistogram, discriminated := validateSamplePayload(row)
	floatOnly := !completeHistogram && !discriminated || mixedFloatRowsProven(inner)
	liveMixed := completeHistogram && discriminated && !floatOnly
	if !floatOnly && (policy.payload != preserveMixedSamplePayload || !liveMixed) {
		panic("promql: sample forwarder received histogram-shaped input without a supported payload policy; use the histogram recognizer")
	}
	if liveMixed && !layout.canonical {
		panic("promql: mixed sample forwarding requires a canonical envelope")
	}
	refs := resolveSampleRoleRefs(row, s, policy, layout)
	var histogram []chplan.Projection
	var discriminator *chplan.ColumnRef
	if liveMixed {
		for _, want := range chplan.HistogramPayloadColumns() {
			requireUniqueNamedColumn(row, want)
			histogram = append(histogram, chplan.Projection{Expr: &chplan.ColumnRef{Name: want.Name}})
		}
		discriminator = requireSampleRole(row, chplan.RoleDiscriminator)
	}
	changed := rewrite(refs)
	if liveMixed && changed.value != nil {
		panic("promql: mixed payload preservation cannot rewrite the float placeholder; use an explicit payload-transform lowering")
	}
	var projections []chplan.Projection
	if layout.canonical || refs.MetricName != nil {
		if policy.name == dropSampleName || refs.MetricName == nil {
			projections = append(projections, chplan.Projection{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn})
		} else {
			projections = append(projections, sampleForwardColumn(refs.MetricName, s.MetricNameColumn, false))
		}
	}
	if changed.attributes != nil {
		projections = append(projections, chplan.Projection{Expr: changed.attributes, Alias: s.AttributesColumn})
	} else {
		projections = append(projections, sampleForwardColumn(refs.Attributes, s.AttributesColumn, layout.materializeAliases))
	}
	if layout.anchored {
		projections = append(projections, sampleForwardColumn(refs.Anchor, chplan.RangeWindowAnchorColumn, false))
	}
	if refs.Timestamp != nil {
		projections = append(projections, sampleForwardColumn(refs.Timestamp, s.TimestampColumn, layout.anchored || layout.materializeAliases))
	}
	if changed.value != nil {
		projections = append(projections, chplan.Projection{Expr: changed.value, Alias: s.ValueColumn})
	} else {
		projections = append(projections, sampleForwardColumn(refs.Value, s.ValueColumn, layout.materializeAliases))
	}
	projections = append(projections, histogram...)
	if discriminator != nil {
		projections = append(projections, sampleForwardColumn(discriminator, mixedDiscriminatorColumn, false))
	}
	return &chplan.Project{Input: inner, Roles: metricRoles(s), Projections: projections}
}

// validateSamplePayload checks the wire contract before a float-row proof may
// discard its columns. Private histogram working columns may accompany a real
// float Value; public histogram-role fields or a discriminator claim the payload.
func validateSamplePayload(row chplan.Schema) (bool, bool) {
	if row.SampleKind() == chplan.SampleKindInvalid {
		panic("promql: sample forwarder received an invalid public sample schema")
	}
	completeHistogram := row.HasHistogramPayload()
	discriminated := row.Has(chplan.RoleDiscriminator)
	publicHistogram := false
	for _, field := range chplan.HistogramPayloadColumns() {
		for _, column := range row.Columns {
			if column.Name == field.Name && column.Role == chplan.RoleHistogramField {
				publicHistogram = true
			}
		}
	}
	if publicHistogram || discriminated {
		if !completeHistogram {
			panic("promql: sample forwarder received incomplete public histogram payload")
		}
		for _, field := range chplan.HistogramPayloadColumns() {
			requireUniqueNamedColumn(row, field)
		}
		if discriminated {
			requireSampleRole(row, chplan.RoleDiscriminator)
		}
	}
	return completeHistogram, discriminated
}

func resolveSampleRoleRefs(row chplan.Schema, s schema.Metrics, policy sampleProjectionPolicy, layout sampleProjectionLayout) sampleRoleRefs {
	refs := sampleRoleRefs{
		Attributes: requireSampleRole(row, chplan.RoleAttributes),
		Value:      requireSampleRole(row, chplan.RoleValue),
	}
	validateConfiguredSampleRole(row, s.AttributesColumn, chplan.RoleAttributes, false)
	validateConfiguredSampleRole(row, s.TimestampColumn, chplan.RoleTimestamp, false)
	validateConfiguredSampleRole(row, s.ValueColumn, chplan.RoleValue, false)
	if policy.name == preserveSampleName {
		validateConfiguredSampleRole(row, s.MetricNameColumn, chplan.RoleMetricName, true)
	}
	if layout.canonical || layout.anchored {
		refs.Timestamp = requireSampleRole(row, chplan.RoleTimestamp)
	}
	if layout.anchored {
		refs.Anchor = requireSampleRole(row, chplan.RoleAnchor)
	}
	if policy.name == preserveSampleName {
		if row.Has(chplan.RoleMetricName) {
			refs.MetricName = requireSampleRole(row, chplan.RoleMetricName)
		} else if column, exists := row.ByName(s.MetricNameColumn); exists {
			if column.Role != chplan.RoleOpaque {
				panic("promql: configured metric-name column carries a conflicting role")
			}
			// Compatibility boundary pinned by the missing-name repair: an
			// existing configured name is not a missing name, even when its
			// producer has not attached a role. Never synthesize over it.
			requireUniqueNamedColumn(row, column)
			refs.MetricName = &chplan.ColumnRef{Name: column.Name}
		} else if layout.canonical && (row.Open || row.Has(chplan.RoleHistogramField) || row.Has(chplan.RoleDiscriminator)) {
			// The missing-name float repair permits canonical synthesis only
			// for a closed float output, never an unknown or payload envelope.
			panic("promql: sample forwarder cannot synthesize a missing metric name for an unknown or histogram output")
		}
	}
	return refs
}

func validateConfiguredSampleRole(row chplan.Schema, name string, role chplan.ColumnRole, allowOpaqueFallback bool) {
	column, exists := row.ByName(name)
	if !exists {
		return
	}
	requireUniqueNamedColumn(row, column)
	if column.Role == role || allowOpaqueFallback && column.Role == chplan.RoleOpaque {
		return
	}
	panic(fmt.Sprintf("promql: configured sample column %q carries role %d, want role %d", name, column.Role, role))
}

func requireSampleRole(row chplan.Schema, role chplan.ColumnRole) *chplan.ColumnRef {
	ref, ok, err := resolveOptionalSampleRole(row, role)
	if err != nil {
		panic(err.Error())
	}
	if !ok {
		panic(fmt.Sprintf("promql: sample forwarder is missing required role %d", role))
	}
	return ref
}

func resolveOptionalSampleRole(row chplan.Schema, role chplan.ColumnRole) (*chplan.ColumnRef, bool, error) {
	name, ok, err := resolveOptionalSampleRoleName(row, role)
	if err != nil || !ok {
		return nil, ok, err
	}
	return &chplan.ColumnRef{Name: name}, true, nil
}

func resolveOptionalSampleRoleName(row chplan.Schema, role chplan.ColumnRole) (string, bool, error) {
	var name string
	for _, column := range row.Columns {
		if column.Role != role {
			continue
		}
		if name != "" || column.Name == "" {
			return "", false, fmt.Errorf("promql: temporal sample layout requires one named column for role %d", role)
		}
		name = column.Name
	}
	if name == "" {
		return "", false, nil
	}
	for _, column := range row.Columns {
		if column.Name == name && column.Role != role {
			return "", false, fmt.Errorf("promql: temporal sample layout role %d has ambiguous output name %q", role, name)
		}
	}
	return name, true, nil
}

func requireUniqueNamedColumn(row chplan.Schema, want chplan.Column) {
	count := 0
	for _, column := range row.Columns {
		if column.Name == want.Name {
			if column.Role != want.Role {
				panic("promql: sample forwarding requires unambiguous named roles")
			}
			count++
		}
	}
	if count != 1 {
		panic("promql: sample forwarding requires a unique named column")
	}
}

func sampleForwardColumn(ref *chplan.ColumnRef, output string, materialize bool) chplan.Projection {
	projection := chplan.Projection{Expr: ref}
	if materialize || ref.Name != output {
		projection.Alias = output
	}
	return projection
}

// Additional filters and ordering preserve an already-proven float-only subset.
// Neither can introduce histogram rows or change their payload. Other nodes
// remain proof barriers; an OR predicate is never itself a narrowing proof.
func mixedFloatRowsProven(inner chplan.Node) bool {
	for {
		if chplan.IsMixedFloatNarrowing(inner) {
			return true
		}
		switch node := inner.(type) {
		case *chplan.Filter:
			inner = node.Input
		case *chplan.OrderBy:
			inner = node.Input
		default:
			return false
		}
	}
}
