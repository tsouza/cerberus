package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

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
	return mixedDiscriminatorFilter(inner, mixedDiscriminatorFloat)
}

// provenFloatRowsSchema gives a physically mixed relation whose rows are
// proven float ([mixedFloatRowsProven]) the float schema its rows actually
// carry: the canonical sample columns, without the histogram payload or the
// discriminator. Every other relation passes through unchanged. A consumer
// that needs a float operand — not merely float rows — applies it, so a
// float-narrowed mixed relation (a comparison, sort, or empty clamp over a
// mixed `or`) is that operand wherever an ordinary float vector is.
func provenFloatRowsSchema(inner chplan.Node, s schema.Metrics) chplan.Node {
	if !inner.RowType().Has(chplan.RoleDiscriminator) || !mixedFloatRowsProven(inner) {
		return inner
	}
	return projectSampleRoles(inner, s,
		sampleProjectionPolicy{name: preserveSampleName, payload: floatSamplePayload},
		derivedSampleProjectionLayout(inner),
		func(sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{} })
}

// mixedRowsNeedPreparation separates the physical mixed payload from the live
// sample kinds a consumer must still handle. Payload validation runs before a
// proof can bypass preparation, so malformed public roles fail closed.
func mixedRowsNeedPreparation(inner chplan.Node) bool {
	completeHistogram, discriminated := validateSamplePayload(inner.RowType())
	return completeHistogram && discriminated && !mixedFloatRowsProven(inner)
}

// liveSampleKind refines the physical schema contract with the only
// value-domain proof represented in the plan: a discriminator-zero filter over
// a mixed payload contains float rows only while retaining all mixed columns.
func liveSampleKind(inner chplan.Node) chplan.SampleKind {
	return chplan.LiveSampleKind(inner)
}

// rowsMayContainHistograms answers which payload path a subquery must take.
// A discriminator-zero proof makes a physically mixed relation safe for the
// float path without changing its columns. Unknown or malformed schemas stay
// on the conservative path: callers must never erase a possible histogram by
// projecting the ordinary float envelope over a relation they cannot prove.
func rowsMayContainHistograms(inner chplan.Node) bool {
	return liveSampleKind(inner) != chplan.SampleKindFloat
}
