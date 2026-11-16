package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestClassicBucketWindowGuard_ClassifiesAs422 pins that the classic
// per-series window guard's throwIf abort reaches the client as a 422
// errorType=execution carrying the guard's own message, never as the generic
// 502.
func TestClassicBucketWindowGuard_ClassifiesAs422(t *testing.T) {
	t.Parallel()
	const (
		throwIfCode = 395
		trailer     = ": while executing 'FUNCTION throwIf(...)'"
	)
	msg := chplan.ClassicBucketWindowSeriesBudgetMessage
	wrapped := fmt.Errorf("chclienttest: query: %w", &clickhouse.Exception{Code: throwIfCode, Message: msg + trailer})
	querier := &gridNativeGuardQuerier{guardErr: wrapped}
	querier.err = wrapped
	srv := newServer(querier)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/v1/query?query=up&time=1767225600")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || body.ErrorType != "execution" || body.Error != msg {
		t.Fatalf("status=%d kind=%q message=%q; want 422 execution %q", resp.StatusCode, body.ErrorType, body.Error, msg)
	}
}
