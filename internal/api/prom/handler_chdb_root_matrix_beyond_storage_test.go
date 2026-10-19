//go:build chdb

package prom_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestQuery_RootMatrixBeyondStorage_ChDB pins an instant query whose root
// subquery answers grid instants past the last one a DateTime64(9) holds:
// the query reports them as Unix milliseconds, and the handler encodes
// them as the instants Prometheus answers.
func TestQuery_RootMatrixBeyondStorage_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, gaugeDDL)
	url := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s",
		srv.URL, escape(`vector(1)[3m:1m] @ 10000000000`), escape("2026-01-01T00:00:01Z"))
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	const want = `"values":[[9999999840,"1"],[9999999900,"1"],[9999999960,"1"]]`
	if !strings.Contains(body, want) {
		t.Fatalf("body lacks %s:\n%s", want, body)
	}
}
