package promql

import "github.com/tsouza/cerberus/internal/chplan"

// mixedDiscriminatorColumn names the shared mixed-row wire discriminator.
const mixedDiscriminatorColumn = chplan.MixedDiscriminatorColumn

// mixedRowsFloatOnly applies the established float-wrapper admission policy.
// Family dispatch remains unchanged here; projectSampleRoles independently
// verifies a complete payload and an actual float-narrowing predicate before a
// value rewrite may discard histogram columns. Physical mixed columns remain
// present after narrowing, so an existing float-row proof makes this idempotent.
func mixedRowsFloatOnly(inner chplan.Node) chplan.Node {
	if !mixedRowsNeedPreparation(inner) {
		return inner
	}
	discriminator := requireSampleRole(inner.RowType(), chplan.RoleDiscriminator)
	return &chplan.Filter{
		Input: inner,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  discriminator,
			Right: &chplan.LitInt{V: mixedDiscriminatorFloat},
		},
	}
}

// mixedRowsNeedPreparation separates the physical mixed payload from the live
// sample kinds a consumer must still handle. Payload validation runs before a
// proof can bypass preparation, so malformed public roles fail closed.
func mixedRowsNeedPreparation(inner chplan.Node) bool {
	completeHistogram, discriminated := validateSamplePayload(inner.RowType())
	return completeHistogram && discriminated && !mixedFloatRowsProven(inner)
}
