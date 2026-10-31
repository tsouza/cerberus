package httperr_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/httperr"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestUnsafeMixedFleetMessage pins that a wrapped refusal is recognised and
// rendered with its reason, and that nothing else is.
func TestUnsafeMixedFleetMessage(t *testing.T) {
	t.Parallel()

	refused := fmt.Errorf("engine: execute: chclient: query: %w", &chclient.UnsafeMixedFleetError{Reason: "upgrade every node"})
	msg, ok := httperr.UnsafeMixedFleetMessage(refused)
	if !ok || !strings.Contains(msg, "upgrade every node") || strings.HasPrefix(msg, "engine:") {
		t.Errorf("UnsafeMixedFleetMessage(refusal) = %q, %v; want the refusal's own text", msg, ok)
	}
	if msg, ok := httperr.UnsafeMixedFleetMessage(fmt.Errorf("x: %w", chclient.ErrUnsafeMixedFleet)); !ok || msg != chclient.ErrUnsafeMixedFleet.Error() {
		t.Errorf("UnsafeMixedFleetMessage(bare sentinel) = %q, %v; want the sentinel's text", msg, ok)
	}
	for _, err := range []error{errors.New("other"), chclient.ErrShardUnavailable} {
		if msg, ok := httperr.UnsafeMixedFleetMessage(err); ok {
			t.Errorf("UnsafeMixedFleetMessage(%v) = %q, true; want false", err, msg)
		}
	}
}
