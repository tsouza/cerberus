package tempo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// The three tests in this file pin the wire contract for a TraceQL
// comparison whose operands have mismatched types — issue #2033, where all
// three of these answered 502 with ClickHouse's own NO_COMMON_TYPE
// exception (`code: 386, message: There is no supertype for types ...`)
// as the message.

// searchStatusAndBody issues /api/search for q and returns the status plus
// the decoded Tempo error envelope's message.
func searchStatusAndBody(t *testing.T, srv, q string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv + "/api/search?q=" + url.QueryEscape(q) + "&limit=1")
	if err != nil {
		t.Fatalf("GET %s: %v", q, err)
	}
	defer resp.Body.Close()
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		// A 200 carries the search envelope, not the error envelope.
		return resp.StatusCode, ""
	}
	return resp.StatusCode, er.Message
}

// TestSearch_StaticTypeMismatchIs400 pins that a comparison between a
// statically-typed intrinsic and an incompatible literal is a CLIENT
// error. 502 claimed an upstream failure for what is a typo, letting any
// user drive 5xx — and the breaker/alerting logic keyed on 5xx with it.
func TestSearch_StaticTypeMismatchIs400(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	status, msg := searchStatusAndBody(t, srv.URL, `{ name > 3 }`)

	if status != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", status)
	}
	if !strings.Contains(msg, "binary operations must operate on the same type") {
		t.Errorf("message: got %q, want the reference backend's wording", msg)
	}
	for _, leak := range []string{"code:", "supertype", "ClickHouse"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message leaked %q: %q", leak, msg)
		}
	}
}

// TestSearch_DynamicAttributeMismatchIs200 pins the other half: an
// attribute's type is not known until a span is read, so a comparison
// against one is answered, not rejected. Spans whose value is not
// comparable simply do not match — the reference backend's semantics, and
// the reason this must not become a 400 alongside the case above.
func TestSearch_DynamicAttributeMismatchIs200(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	for _, q := range []string{
		`{ duration > span.foo }`,
		`{ duration > resource.service.name }`,
	} {
		status, msg := searchStatusAndBody(t, srv.URL, q)
		if status != http.StatusOK {
			t.Errorf("%s: status got %d (%s), want 200", q, status, msg)
		}
	}
}

// TestSearch_ClickHouseExceptionTextIsNotOnTheWire pins the information
// leak independently of the two fixes above: whatever else goes wrong
// downstream, ClickHouse's error code, type names and SQL fragment do not
// belong in a Tempo-shaped response body. The status class is unchanged —
// an unclassified execute-stage failure is still a 502.
func TestSearch_ClickHouseExceptionTextIsNotOnTheWire(t *testing.T) {
	t.Parallel()

	ex := &clickhouse.Exception{
		Code:    386,
		Message: "There is no supertype for types UInt64, String because some of them are String/FixedString/Enum and some of them are not: while executing 'FUNCTION greater(Duration, SpanAttributes)'",
	}
	q := &stubQuerier{err: fmt.Errorf("chclient: query: %w", ex)}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	status, msg := searchStatusAndBody(t, srv.URL, `{ resource.service.name = "api" }`)

	if status != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", status)
	}
	for _, leak := range []string{"386", "supertype", "UInt64", "FUNCTION greater", "code:"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message leaked %q: %q", leak, msg)
		}
	}
	if msg == "" {
		t.Error("message: empty (Grafana renders this)")
	}
}
