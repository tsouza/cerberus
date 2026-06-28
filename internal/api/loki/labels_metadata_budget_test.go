package loki_test

import (
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestLabels_MetadataDrainBudget422 — GAP-A end-to-end. A metadata drain that
// busts the per-query sample budget (chclient.drainBudgetExceeded, now wired
// into QueryStrings/QueryLabelSets/...) used to be the path to a hard process
// OOM with no net. It now aborts with ErrTooManySamples, and the labels
// handler maps it to Loki's limit-vocabulary 400 (via classifyMetadataErr)
// rather than a misleading 502 — exactly like the query path.
func TestLabels_MetadataDrainBudget422(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{stringsErr: &chclient.TooManySamplesError{Limit: 5}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("metadata drain budget: got %d, want 400 (Loki limit rejection)", resp.StatusCode)
	}
}
