package tempo

import (
	"errors"
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

func TestTagsErrStatus(t *testing.T) {
	t.Parallel()
	if got := tagsErrStatus(&chclient.TooManySamplesError{Limit: 5}); got != http.StatusUnprocessableEntity {
		t.Fatalf("TooManySamples: got %d, want 422", got)
	}
	if got := tagsErrStatus(&chclient.MemoryLimitError{Limit: 1 << 30}); got != http.StatusUnprocessableEntity {
		t.Fatalf("MemoryLimit: got %d, want 422", got)
	}
	if got := tagsErrStatus(errors.New("boom")); got != http.StatusBadGateway {
		t.Fatalf("generic: got %d, want 502", got)
	}
}
