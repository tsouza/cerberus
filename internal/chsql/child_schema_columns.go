package chsql

import (
	"errors"
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file is the emitters' one door to chplan's role resolution
// (chplan.Schema.UniqueNamedRole / UniqueNamedHistogramField). Every emitter
// that needs "the child column carrying role R" goes through a helper here;
// none re-derives the lookup (role_resolution_class_test.go pins that). The
// helpers add what the resolver deliberately leaves to the operator: the nil
// and open-child checks, and the ErrUnsupported wrapping that names the owner.

// closedChildSchema returns child's row type for owner, rejecting a nil child
// and an open schema.
func closedChildSchema(owner string, child chplan.Node) (chplan.Schema, error) {
	if child == nil {
		return chplan.Schema{}, fmt.Errorf("%w: %s.Input is nil", ErrUnsupported, owner)
	}
	row := child.RowType()
	if row.Open {
		return chplan.Schema{}, fmt.Errorf("%w: %s requires a closed child schema", ErrUnsupported, owner)
	}
	return row, nil
}

// roleColumnName resolves role in row and reports a failure as owner's
// unsupported-plan error.
func roleColumnName(owner string, row chplan.Schema, role chplan.ColumnRole) (string, error) {
	column, err := row.UniqueNamedRole(role)
	if err != nil {
		return "", fmt.Errorf("%w: %s child schema: %w", ErrUnsupported, owner, err)
	}
	return column.Name, nil
}

// histogramFieldColumnName resolves field in row and reports a failure as
// owner's unsupported-plan error. With optional set, an absent field resolves
// to the empty name rather than an error; every other failure still rejects.
func histogramFieldColumnName(owner string, row chplan.Schema, field chplan.HistogramField, optional bool) (string, error) {
	column, err := row.UniqueNamedHistogramField(field)
	if err != nil {
		if optional && errors.Is(err, chplan.ErrRoleMissing) {
			return "", nil
		}
		return "", fmt.Errorf("%w: %s child schema: %w", ErrUnsupported, owner, err)
	}
	return column.Name, nil
}

// roleChildColumn resolves role in owner's closed child.
func roleChildColumn(owner string, child chplan.Node, role chplan.ColumnRole) (string, error) {
	row, err := closedChildSchema(owner, child)
	if err != nil {
		return "", err
	}
	return roleColumnName(owner, row, role)
}

// timestampChildColumn resolves the timestamp role in owner's closed child.
func timestampChildColumn(owner string, child chplan.Node) (string, error) {
	return roleChildColumn(owner, child, chplan.RoleTimestamp)
}

// histogramFieldChildColumn resolves field in owner's closed child; see
// histogramFieldColumnName for optional.
func histogramFieldChildColumn(owner string, child chplan.Node, field chplan.HistogramField, optional bool) (string, error) {
	row, err := closedChildSchema(owner, child)
	if err != nil {
		return "", err
	}
	return histogramFieldColumnName(owner, row, field, optional)
}
