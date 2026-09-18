package chsql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

type nativeMatrixInputColumns struct {
	timestamp string
	value     string
}

func resolveNativeMatrixInputColumns(r *chplan.RangeWindowGridNative) (nativeMatrixInputColumns, error) {
	const owner = "RangeWindowGridNative"
	row, err := closedChildSchema(owner, r.Input)
	if err != nil {
		return nativeMatrixInputColumns{}, err
	}
	timestamp, err := roleColumnName(owner, row, chplan.RoleTimestamp)
	if err != nil {
		return nativeMatrixInputColumns{}, err
	}
	value, err := roleColumnName(owner, row, chplan.RoleValue)
	if err != nil {
		return nativeMatrixInputColumns{}, err
	}
	return nativeMatrixInputColumns{timestamp: timestamp, value: value}, nil
}
