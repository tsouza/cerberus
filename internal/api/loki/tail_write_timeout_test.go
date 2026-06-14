package loki

import (
	"testing"
	"time"
)

// TestHandler_TailWriteTimeout confirms the /tail per-write deadline resolves
// from the configured Handler.TailWriteTimeout, falling back to the package
// default when the field is zero (the bare-Handler convention tests rely on).
func TestHandler_TailWriteTimeout(t *testing.T) {
	t.Parallel()
	t.Run("default_when_unset", func(t *testing.T) {
		h := &Handler{}
		if got := h.tailWriteTimeout(); got != defaultTailWriteTimeout {
			t.Errorf("tailWriteTimeout() = %v; want default %v", got, defaultTailWriteTimeout)
		}
	})
	t.Run("configured_value_wins", func(t *testing.T) {
		h := &Handler{TailWriteTimeout: 25 * time.Second}
		if got := h.tailWriteTimeout(); got != 25*time.Second {
			t.Errorf("tailWriteTimeout() = %v; want 25s", got)
		}
	})
}
