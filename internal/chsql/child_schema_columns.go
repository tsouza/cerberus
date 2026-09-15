package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

type childColumnRequirement struct {
	role           chplan.ColumnRole
	histogramField chplan.HistogramField
	name           string
	optional       bool
}

func resolveChildColumn(owner string, child chplan.Node, want childColumnRequirement) (string, error) {
	if child == nil {
		return "", fmt.Errorf("%w: %s.Input is nil", ErrUnsupported, owner)
	}
	row := child.RowType()
	if row.Open {
		return "", fmt.Errorf("%w: %s requires a closed child schema", ErrUnsupported, owner)
	}
	var resolved string
	for _, column := range row.Columns {
		matches := column.Role == want.role
		if want.role == chplan.RoleHistogramField {
			matches = matches && column.HistogramField == want.histogramField
			if column.HistogramField == want.histogramField && column.Role != chplan.RoleHistogramField {
				return "", fmt.Errorf("%w: %s child column %q has incompatible role for %s", ErrUnsupported, owner, column.Name, want.name)
			}
		}
		if !matches {
			continue
		}
		if column.Name == "" || resolved != "" {
			return "", fmt.Errorf("%w: %s requires one named child column for %s", ErrUnsupported, owner, want.name)
		}
		resolved = column.Name
	}
	if resolved == "" {
		if want.optional {
			return "", nil
		}
		return "", fmt.Errorf("%w: %s child schema is missing %s", ErrUnsupported, owner, want.name)
	}
	for _, column := range row.Columns {
		if column.Name == resolved && (column.Role != want.role ||
			(want.role == chplan.RoleHistogramField && column.HistogramField != want.histogramField)) {
			return "", fmt.Errorf("%w: %s child column %q is ambiguous for %s", ErrUnsupported, owner, resolved, want.name)
		}
	}
	return resolved, nil
}

func timestampChildColumn(owner string, child chplan.Node) (string, error) {
	return resolveChildColumn(owner, child, childColumnRequirement{role: chplan.RoleTimestamp, name: "timestamp role"})
}

func histogramChildColumn(child chplan.Node, field chplan.HistogramField, name string) (string, error) {
	return resolveChildColumn("HistogramProjection", child, childColumnRequirement{
		role: chplan.RoleHistogramField, histogramField: field, name: name,
	})
}

func optionalHistogramChildColumn(child chplan.Node, field chplan.HistogramField, name string) (string, error) {
	return resolveChildColumn("HistogramProjection", child, childColumnRequirement{
		role: chplan.RoleHistogramField, histogramField: field, name: name, optional: true,
	})
}
