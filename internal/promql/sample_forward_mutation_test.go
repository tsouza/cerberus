package promql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Tests in this file kill LIVED gremlins mutants reported on the
// phase4-promql-other leg (cerberus issue #3467) against sample_forward.go.
// See gremlins_kill_test.go for the shared file-header convention this
// file follows.
//
// Two of the leg's reported survivors against this file —
// sample_forward.go:resolveSampleTemporalLayout:`hasAnchor && hasTimestamp`
// and sample_forward.go:sourceMetrics:`r.Attributes != nil` — are already
// killed by TestLegacySampleProjectionLayoutRejectsInvalidRoles's "anchor
// without timestamp" subtest and by TestSampleForwardRolePolicyBoundaries
// (both in sample_forward_test.go); hand-verified by mutating each
// construct and confirming those existing tests go red, rather than
// duplicated here.

// TestResolveSampleRoleRefs_DropPolicySkipsMetricNameRoleValidation kills
// the CONDITIONALS_NEGATION mutant on resolveSampleRoleRefs's FIRST
// `policy.name == preserveSampleName` guard — the one immediately
// followed by the validateConfiguredSampleRole(…MetricNameColumn…) call,
// not the later metric-name-resolution block sharing the identical
// condition text further down the same function — rewritten to `!=`.
// Under [dropSampleName], the caller has already said
// it does not care what occupies the configured metric-name column, so
// this validation must NOT run — a column at that name carrying some
// unrelated role is legal input to drop. Under the mutant, the check
// runs anyway and panics on exactly that legal input.
func TestResolveSampleRoleRefs_DropPolicySkipsMetricNameRoleValidation(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	row := chplan.Schema{Columns: []chplan.Column{
		{Name: "attrs", Role: chplan.RoleAttributes},
		{Name: "value", Role: chplan.RoleValue},
		// Occupies the configured metric-name column under a role that is
		// neither RoleMetricName nor RoleOpaque — validateConfiguredSampleRole
		// would reject this if it ran, which under dropSampleName it must not.
		{Name: s.MetricNameColumn, Role: chplan.RoleAnchor},
	}}
	policy := sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload}
	refs := resolveSampleRoleRefs(row, s, policy, sampleProjectionLayout{})
	if refs.MetricName != nil {
		t.Fatalf("resolveSampleRoleRefs(drop) refs.MetricName = %#v, want nil", refs.MetricName)
	}
}

// TestValidateConfiguredSampleRole_OpaqueFallbackRequiresBothConditions
// kills the INVERT_LOGICAL mutant on validateConfiguredSampleRole's
//
//	if column.Role == role || allowOpaqueFallback && column.Role == chplan.RoleOpaque {
//
// rewritten (the `&&`, which binds tighter than the surrounding `||`) to
// `||`. That turns the guard into `column.Role == role || allowOpaqueFallback
// || column.Role == chplan.RoleOpaque` — accepting ANY role at all the
// instant a caller merely PERMITS an opaque fallback, whether or not the
// column is actually opaque. A configured column whose role is neither
// the wanted one nor opaque must still be rejected when opaque fallback
// is allowed.
func TestValidateConfiguredSampleRole_OpaqueFallbackRequiresBothConditions(t *testing.T) {
	t.Parallel()

	row := chplan.Schema{Columns: []chplan.Column{{Name: "metric", Role: chplan.RoleValue}}}
	msg := capturePanic(t, func() {
		validateConfiguredSampleRole(row, "metric", chplan.RoleMetricName, true)
	})
	if !strings.Contains(msg, "want role") {
		t.Fatalf("panic = %q, want it to name the wanted role (validateConfiguredSampleRole's own message)", msg)
	}
}

// TestResolveSampleRoleRefs_MissingNameSynthesisGuardDisjuncts kills the
// two INVERT_LOGICAL mutants on resolveSampleRoleRefs's
//
//	} else if layout.canonical && (row.Open || row.Has(chplan.RoleHistogramField) || row.Has(chplan.RoleDiscriminator)) {
//
// each `||` rewritten to `&&` independently. This guard is reached only
// when policy preserves the metric name, no RoleMetricName column
// exists, and no column occupies the configured metric-name column
// either — the "genuinely missing name" case — and it must panic
// whenever the schema is Open, carries a histogram-field column, or
// carries a discriminator, because the "missing-name float repair" this
// guard protects is sound only for an unambiguous closed float output.
// Each subtest sets exactly ONE of the three disjuncts true and the
// other two false: under the original `||` every subtest panics, but
// under either mutated `&&` the disjunct made third-priority folds away
// against the other two false operands and the guard is silently
// skipped instead.
func TestResolveSampleRoleRefs_MissingNameSynthesisGuardDisjuncts(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	base := []chplan.Column{
		{Name: "attrs", Role: chplan.RoleAttributes},
		{Name: "ts", Role: chplan.RoleTimestamp},
		{Name: "value", Role: chplan.RoleValue},
	}
	policy := sampleProjectionPolicy{name: preserveSampleName, payload: floatSamplePayload}
	layout := sampleProjectionLayout{canonical: true}

	for _, tc := range []struct {
		name string
		open bool
		row  chplan.Schema
	}{
		{name: "open", open: true, row: chplan.Schema{Columns: append([]chplan.Column(nil), base...)}},
		{name: "histogram_field", row: chplan.Schema{Columns: append(append([]chplan.Column(nil), base...), chplan.Column{Name: "helper", Role: chplan.RoleHistogramField})}},
		{name: "discriminator", row: chplan.Schema{Columns: append(append([]chplan.Column(nil), base...), chplan.Column{Name: "kind", Role: chplan.RoleDiscriminator})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := tc.row
			row.Open = tc.open
			msg := capturePanic(t, func() { resolveSampleRoleRefs(row, s, policy, layout) })
			if !strings.Contains(msg, "cannot synthesize a missing metric name") {
				t.Fatalf("panic = %q, want the missing-name synthesis guard's own message", msg)
			}
		})
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// Two more of the leg's reported survivors against this file sit inside
// validateSamplePayload, and both are unobservable for the same reason:
// the function's own FIRST statement —
// `if row.SampleKind() == chplan.SampleKindInvalid { panic(...) }` —
// already enforces, for any row that reaches the mutated line, exactly
// the fact each mutant tries to falsify.
//
//  1. `column.Name == field.Name` (CONDITIONALS_NEGATION, rewritten to
//     `!=`) inside the `publicHistogram` double loop. [chplan.Schema.
//     SampleKind] rejects a RoleHistogramField column whose (name,
//     HistogramField) pair does not match one [chplan.
//     HistogramPayloadColumns] entry exactly, and separately rejects two
//     columns sharing a name. So on any row that reaches this loop,
//     every actual RoleHistogramField column's name equals exactly one
//     canonical field's name and no other — flipping `==` to `!=`
//     relabels WHICH of the nine outer iterations sets `publicHistogram`
//     true (the one exact match, under `==`, versus the other eight
//     under `!=`), never WHETHER one does: with nine distinct canonical
//     names, a present histogram-role column satisfies `!=` on eight of
//     them regardless of which one it actually matches.
//
//  2. `if publicHistogram || discriminated {` (INVERT_LOGICAL, rewritten
//     to `&&`). [chplan.Schema.SampleKind] additionally enforces that a
//     RoleHistogramField column present at all means ALL NINE canonical
//     fields are present (a partial set is itself SampleKindInvalid),
//     and that a RoleDiscriminator column present without a complete
//     histogram payload is SampleKindInvalid too. Given bullet 1 above,
//     `publicHistogram` and `completeHistogram` (this function's own
//     `row.HasHistogramPayload()`) always agree on any row that reaches
//     this guard, and `discriminated` true always implies
//     `publicHistogram` true. The one shape `||` and `&&` disagree on —
//     exactly one of the two operands true — cannot occur, so neither
//     the guard's own entry nor the `!completeHistogram` panic inside it
//     is reachable on any input this function has not already rejected
//     one line earlier.
//
// Both are reachable only by handing validateSamplePayload a Schema that
// violates its own SampleKind precondition, which is not a shape any
// real chplan.Node.RowType() can produce — [chplan.Schema.SampleKind] is
// exactly the contract every one of them commits to.
