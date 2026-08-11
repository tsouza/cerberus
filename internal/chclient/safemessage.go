package chclient

import (
	"errors"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// serverExceptionPlaceholder is what replaces a ClickHouse server
// exception's own rendering in a client-facing message. It says the same
// thing the exception said — the storage engine refused this query — in
// cerberus's vocabulary rather than ClickHouse's.
const serverExceptionPlaceholder = "ClickHouse rejected the query"

// SafeMessage renders err as the text an HTTP response body may carry.
//
// The three API heads speak Prometheus / Loki / Tempo wire formats, and
// none of those contracts has a place for the STORAGE engine's internals.
// A raw ClickHouse server exception — `code: 386, message: There is no
// supertype for types UInt64, String ... : while executing ...` — names
// ClickHouse error codes, ClickHouse type names, and a fragment of the
// generated SQL. Passing it through tells a caller what cerberus is built
// on and how it phrased their query, which is neither useful to them nor
// cerberus's to disclose (issue #2033).
//
// What survives is everything cerberus wrote itself: parse rejections,
// lowering rejections, budget and timeout messages, the breaker's
// vocabulary. Those already read as cerberus errors and are what a caller
// acts on. Only the exception's own rendering is substituted, in place, so
// the surrounding stage markers (`engine: execute:`, `chclient: query:`)
// still say WHERE the failure happened.
//
// The full chain, exception text included, is unaffected on the logging
// and tracing side: this function shapes one string for one response body
// and is not an error-handling step.
func SafeMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) {
		return msg
	}
	// A classified failure (memory limit, execution timeout) carries the
	// exception for errors.As but renders its own cerberus-authored
	// message, in which the exception text does not appear — ReplaceAll
	// then correctly changes nothing.
	return strings.ReplaceAll(msg, ex.Error(), serverExceptionPlaceholder)
}
