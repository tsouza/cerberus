package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

func rangeWindowInputTimestampColumn(r *chplan.RangeWindow) string {
	if r == nil || r.Input == nil {
		return ""
	}
	if _, nested := r.Input.(*chplan.RangeWindow); nested {
		return r.TimestampColumn
	}
	column, _ := r.Input.RowType().Find(chplan.RoleTimestamp)
	return column.Name
}

func validateRangeWindowInputTimestamp(r *chplan.RangeWindow) error {
	if r.Input == nil {
		return fmt.Errorf("%w: RangeWindow.Input is nil", ErrUnsupported)
	}
	if _, nested := r.Input.(*chplan.RangeWindow); nested {
		return nil
	}
	row := r.Input.RowType()
	var found chplan.Column
	count := 0
	for _, column := range row.Columns {
		if column.Role == chplan.RoleTimestamp {
			found = column
			count++
		}
	}
	if row.Open || count != 1 || found.Name == "" {
		return fmt.Errorf("%w: RangeWindow requires one named timestamp in a closed child schema", ErrUnsupported)
	}
	for _, column := range row.Columns {
		if column.Name == found.Name && column.Role != chplan.RoleTimestamp {
			return fmt.Errorf("%w: RangeWindow child timestamp is ambiguous", ErrUnsupported)
		}
	}
	return nil
}
