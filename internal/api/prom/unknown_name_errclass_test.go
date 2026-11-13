package prom

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestClassifySentinelError_UnknownNameIs502: a ClickHouse node too old for a function
// cerberus sent is a server-side failure, 502 errorType=internal, answered
// with the error that names the function and the node's build instead of the
// redacted exception.
func TestClassifySentinelError_UnknownNameIs502(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("execute: %w", &chclient.UnknownNameError{
		Kind:        "function",
		Name:        "argAndMax",
		NodeVersion: "25.10.7.6",
		Cause:       &clickhouse.Exception{Code: 46, Message: "Function with name `argAndMax` does not exist"},
	})
	got := classifySentinelError(err)
	if got == nil {
		t.Fatal("classifySentinelError returned nil; want the unknown-name rejection classified")
	}
	if got.Status != http.StatusBadGateway || got.Kind != ErrInternal {
		t.Errorf("classified %d %s; want %d %s", got.Status, got.Kind, http.StatusBadGateway, ErrInternal)
	}
	for _, want := range []string{"argAndMax", "25.10.7.6"} {
		if !strings.Contains(got.Err.Error(), want) {
			t.Errorf("message %q does not name %q", got.Err.Error(), want)
		}
	}
}
