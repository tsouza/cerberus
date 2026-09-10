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
	if layout.canonical && layout.anchored {
		panic("promql: sample forwarder cannot combine canonical and direct-grid envelopes")
	}
	row := inner.RowType()
	completeHistogram := row.HasHistogramPayload()
	discriminated := row.Has(chplan.RoleDiscriminator)
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
	if layout.canonical {
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

func resolveSampleRoleRefs(row chplan.Schema, s schema.Metrics, policy sampleProjectionPolicy, layout sampleProjectionLayout) sampleRoleRefs {
	refs := sampleRoleRefs{
		Attributes: requireSampleRole(row, chplan.RoleAttributes),
		Value:      requireSampleRole(row, chplan.RoleValue),
	}
	if layout.canonical || layout.anchored {
		refs.Timestamp = requireSampleRole(row, chplan.RoleTimestamp)
	}
	if layout.anchored {
		refs.Anchor = requireSampleRole(row, chplan.RoleAnchor)
	}
	if layout.canonical && policy.name == preserveSampleName {
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
		} else if row.Open || row.Has(chplan.RoleHistogramField) || row.Has(chplan.RoleDiscriminator) {
			// The missing-name float repair permits canonical synthesis only
			// for a closed float output, never an unknown or payload envelope.
			panic("promql: sample forwarder cannot synthesize a missing metric name for an unknown or histogram output")
		}
	}
	return refs
}

func requireSampleRole(row chplan.Schema, role chplan.ColumnRole) *chplan.ColumnRef {
	var name string
	for _, column := range row.Columns {
		if column.Role != role {
			continue
		}
		if name != "" || column.Name == "" {
			panic(fmt.Sprintf("promql: sample forwarder requires one named column for role %d", role))
		}
		name = column.Name
	}
	if name == "" {
		panic(fmt.Sprintf("promql: sample forwarder is missing required role %d", role))
	}
	for _, column := range row.Columns {
		if column.Name == name && column.Role != role {
			panic(fmt.Sprintf("promql: sample forwarder role %d has ambiguous output name %q", role, name))
		}
	}
	return &chplan.ColumnRef{Name: name}
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
