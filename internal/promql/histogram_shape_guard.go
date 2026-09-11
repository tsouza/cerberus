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

// rowsMayContainHistograms answers which payload path a subquery must take.
// A discriminator-zero proof makes a physically mixed relation safe for the
// float path without changing its columns. Unknown or malformed schemas stay
// on the conservative path: callers must never erase a possible histogram by
// projecting the ordinary float envelope over a relation they cannot prove.
func rowsMayContainHistograms(inner chplan.Node) bool {
	row := inner.RowType()
	if row.Open {
		return true
	}
	completeHistogram, discriminated, valid := inspectLiveSamplePayload(row)
	if !valid {
		return true
	}
	if !liveSampleRolesAreUnambiguous(row) {
		return true
	}
	if completeHistogram {
		if !discriminated {
			return true
		}
		return !mixedFloatRowsProven(inner)
	}
	return !hasUnambiguousNamedRole(row, chplan.RoleValue)
}

func liveSampleRolesAreUnambiguous(row chplan.Schema) bool {
	for _, role := range [...]chplan.ColumnRole{
		chplan.RoleMetricName,
		chplan.RoleAttributes,
		chplan.RoleTimestamp,
		chplan.RoleAnchor,
		chplan.RoleValue,
		chplan.RoleDiscriminator,
	} {
		if !roleIsUnambiguousWhenPresent(row, role) {
			return false
		}
	}
	return true
}

// inspectLiveSamplePayload validates the public histogram/discriminator
// envelope without panicking. The lowering pipeline can then conservatively
// retain an unrecognised payload and let its histogram-aware continuation
// report the established error, rather than silently selecting the float path.
func inspectLiveSamplePayload(row chplan.Schema) (completeHistogram, discriminated, valid bool) {
	valid = true
	histogramFields := chplan.HistogramPayloadColumns()
	sawHistogramContract := false
	completeHistogram = true
	for _, field := range histogramFields {
		count := 0
		for _, column := range row.Columns {
			if column.Name != field.Name {
				continue
			}
			sawHistogramContract = true
			if column.Role != chplan.RoleHistogramField {
				valid = false
				continue
			}
			count++
			if count != 1 {
				valid = false
			}
		}
		if count != 1 {
			completeHistogram = false
		}
	}
	for _, column := range row.Columns {
		if column.Role != chplan.RoleHistogramField {
			continue
		}
		sawHistogramContract = true
		canonicalName := false
		for _, field := range histogramFields {
			if column.Name == field.Name {
				canonicalName = true
				break
			}
		}
		if !canonicalName {
			valid = false
		}
	}
	if sawHistogramContract && !completeHistogram {
		valid = false
	}

	discriminated = row.Has(chplan.RoleDiscriminator)
	if !roleIsUnambiguousWhenPresent(row, chplan.RoleDiscriminator) {
		valid = false
	}
	if discriminated && !completeHistogram {
		valid = false
	}
	if completeHistogram && discriminated && !hasUnambiguousNamedRole(row, chplan.RoleValue) {
		valid = false
	}
	return completeHistogram, discriminated, valid
}

func hasUnambiguousNamedRole(row chplan.Schema, role chplan.ColumnRole) bool {
	found := false
	name := ""
	for _, column := range row.Columns {
		if column.Role != role {
			continue
		}
		if found || column.Name == "" {
			return false
		}
		found = true
		name = column.Name
	}
	if !found {
		return false
	}
	for _, column := range row.Columns {
		if column.Name == name && column.Role != role {
			return false
		}
	}
	return true
}

func roleIsUnambiguousWhenPresent(row chplan.Schema, role chplan.ColumnRole) bool {
	if !row.Has(role) {
		return true
	}
	return hasUnambiguousNamedRole(row, role)
}
