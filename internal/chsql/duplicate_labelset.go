package chsql

import (
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// IsDuplicateLabelsetError reports whether err is the deliberate query abort
// the name-drop collision guard plants (see [chplan.DuplicateLabelsetMessage])
// rather than a backend failure.
//
// It is the sibling of [IsInfoConflictingLabelError] and exists for the same
// reason: the HTTP layer must classify the abort the way the reference engine
// classifies its own — a 422 execution error naming the query as at fault, not
// a 5xx blaming ClickHouse. The exception prefix is matched alongside the
// message so a query that merely mentions the phrase in a label value cannot be
// mistaken for one.
func IsDuplicateLabelsetError(err error) bool {
	return err != nil && strings.Contains(err.Error(), chExceptionPrefix+chplan.DuplicateLabelsetMessage)
}
