package promql

import "github.com/tsouza/cerberus/internal/chplan"

// mixedDiscriminatorColumn names the shared mixed-row wire discriminator.
const mixedDiscriminatorColumn = chplan.MixedDiscriminatorColumn

// mixedRowsFloatOnly applies the established float-wrapper admission policy.
// Family dispatch remains unchanged here; projectSampleRoles independently
// verifies a complete payload and an actual float-narrowing predicate before a
// value rewrite may discard histogram columns. The legacy shape classification
// remains until all runtime consumers are reconciled.
func mixedRowsFloatOnly(inner chplan.Node) chplan.Node {
	if chplan.RowShapeOf(inner) != chplan.MixedRowShape {
		return inner
	}
	return mixedDiscriminatorFilter(inner, mixedDiscriminatorFloat)
}
