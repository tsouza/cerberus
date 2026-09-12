package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

func quantileHistogramField(owner string, input chplan.Node, field chplan.HistogramField, optional bool) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%w: %s.Input is nil", ErrUnsupported, owner)
	}
	row := input.RowType()
	if row.Open {
		return "", fmt.Errorf("%w: %s requires a closed child schema", ErrUnsupported, owner)
	}
	column, ok := row.FindHistogramField(field)
	if ok {
		for _, candidate := range row.Columns {
			if candidate.Name == column.Name && candidate != column {
				return "", fmt.Errorf("%w: %s child column %q has ambiguous roles", ErrUnsupported, owner, column.Name)
			}
		}
		return column.Name, nil
	}
	for _, candidate := range row.Columns {
		if candidate.HistogramField == field {
			return "", fmt.Errorf("%w: %s child schema has an invalid histogram field %d", ErrUnsupported, owner, field)
		}
	}
	if optional {
		return "", nil
	}
	return "", fmt.Errorf("%w: %s child schema is missing histogram field %d", ErrUnsupported, owner, field)
}
