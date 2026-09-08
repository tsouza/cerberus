package chclient

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

// TestSafeMessage_ScrubsTheColumnarDialsException pins that BOTH server
// exception types are scrubbed.
//
// cerberus dials ClickHouse two ways. The row path yields a
// *clickhouse.Exception, which SafeMessage has always replaced. The
// columnar path yields ch-go's *proto.Exception, whose Error() renders the
// same server text — including the `while executing 'FUNCTION …'` trailer
// that names the emitted SQL's internal aliases — and which SafeMessage
// passed straight through. Every columnar-path failure therefore disclosed
// exactly what issue #2033 removed from the row path.
func TestSafeMessage_ScrubsTheColumnarDialsException(t *testing.T) {
	t.Parallel()

	const trailer = "while executing 'FUNCTION greater(__table1.Duration, __table1.SpanAttributes)'"
	chEx := &ch.Exception{
		Code:    proto.Error(386),
		Name:    "DB::Exception",
		Message: "There is no supertype for types UInt64, String: " + trailer,
	}
	wrapped := fmt.Errorf("chclient: query: %w", chEx)

	got := SafeMessage(wrapped)

	if strings.Contains(got, trailer) {
		t.Errorf(
			"the emitted SQL's internal aliases reached the response body:\n%s",
			got,
		)
	}
	if strings.Contains(got, "supertype") {
		t.Errorf("the ClickHouse exception text survived:\n%s", got)
	}
	if !strings.Contains(got, serverExceptionPlaceholder) {
		t.Errorf("the placeholder did not replace the exception:\n%s", got)
	}
	if !strings.HasPrefix(got, "chclient: query: ") {
		t.Errorf("the cerberus stage marker must survive, saying WHERE it failed:\n%s", got)
	}
}

// TestSafeMessage_LeavesCerberusAuthoredTextAlone is the other half: only
// the exception's own rendering is substituted.
func TestSafeMessage_LeavesCerberusAuthoredTextAlone(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("promql: rate() expects a range-vector selector")
	if got := SafeMessage(err); got != err.Error() {
		t.Errorf("SafeMessage rewrote a cerberus-authored message: %q", got)
	}
}
