package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

type nativeMatrixInputColumns struct {
	timestamp string
	value     string
}

func resolveNativeMatrixInputColumns(r *chplan.RangeWindowGridNative) (nativeMatrixInputColumns, error) {
	if r.Input == nil {
		return nativeMatrixInputColumns{}, fmt.Errorf("%w: RangeWindowGridNative.Input is nil", ErrUnsupported)
	}
	row := r.Input.RowType()
	if row.Open {
		return nativeMatrixInputColumns{}, fmt.Errorf("%w: RangeWindowGridNative requires a closed child schema", ErrUnsupported)
	}
	timestamp, err := uniqueNativeMatrixRole(row, chplan.RoleTimestamp, "timestamp")
	if err != nil {
		return nativeMatrixInputColumns{}, err
	}
	value, err := uniqueNativeMatrixRole(row, chplan.RoleValue, "value")
	if err != nil {
		return nativeMatrixInputColumns{}, err
	}
	if timestamp == value {
		return nativeMatrixInputColumns{}, fmt.Errorf("%w: RangeWindowGridNative child timestamp and value are ambiguous", ErrUnsupported)
	}
	return nativeMatrixInputColumns{timestamp: timestamp, value: value}, nil
}

func uniqueNativeMatrixRole(row chplan.Schema, role chplan.ColumnRole, label string) (string, error) {
	var found chplan.Column
	count := 0
	for _, column := range row.Columns {
		if column.Role == role {
			found = column
			count++
		}
	}
	if count != 1 || found.Name == "" {
		return "", fmt.Errorf("%w: RangeWindowGridNative requires one named %s in its child schema", ErrUnsupported, label)
	}
	for _, column := range row.Columns {
		if column.Name == found.Name && column.Role != role {
			return "", fmt.Errorf("%w: RangeWindowGridNative child %s is ambiguous", ErrUnsupported, label)
		}
	}
	return found.Name, nil
}
