package chsql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// IsHistogramMergeBudgetError reports whether err is the deliberate query
// abort the native-histogram merge budget guard plants (see
// [chplan.HistogramMergeBudgetMessage]) rather than a backend failure.
//
// It is the sibling of [IsDuplicateLabelsetError] / [IsInfoConflictingLabelError]
// and exists for the same reason: the HTTP layer must classify the abort as a
// deliberate, query-shape-fault rejection — a 422 execution error — rather
// than the 502 bucket every other ClickHouse execute failure falls into. The
// exception prefix is matched alongside the message so a query that merely
// mentions the phrase in a label value cannot be mistaken for one.
func IsHistogramMergeBudgetError(err error) bool {
	return isThrowIfError(err, chplan.HistogramMergeBudgetMessage)
}
