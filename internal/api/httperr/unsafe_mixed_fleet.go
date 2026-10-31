package httperr

import (
	"errors"

	"github.com/tsouza/cerberus/internal/chclient"
)

// UnsafeMixedFleetMessage reports whether err is a dispatch chclient refused
// because the ClickHouse fleet mixes a build that needs a defect-gate setting
// with a build too old to accept it (chclient.UnsafeMixedFleetError), and
// returns its client-facing text: the defect, the setting and the upgrade
// that lifts the refusal. Every head answers it 503 unavailable — ClickHouse
// is healthy, the fleet cannot answer this query correctly until the upgrade
// finishes — in its own envelope.
func UnsafeMixedFleetMessage(err error) (string, bool) {
	var refused *chclient.UnsafeMixedFleetError
	switch {
	case errors.As(err, &refused):
		return refused.Error(), true
	case errors.Is(err, chclient.ErrUnsafeMixedFleet):
		return chclient.ErrUnsafeMixedFleet.Error(), true
	}
	return "", false
}
