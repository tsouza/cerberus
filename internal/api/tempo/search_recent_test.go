package tempo_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearchRecent_DefaultLimit — `GET /api/search/recent` with no
// ?limit returns the standard search response shape; the emitted SQL
// carries `LIMIT 20` (the default) so the CH side doesn't over-fetch.
func TestSearchRecent_DefaultLimit(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: oneSpanSample()}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	assertSQLContains(t, q.lastSQL, "LIMIT 20")
	assertSQLContains(t, q.lastSQL, "ORDER BY")
	assertSQLContains(t, q.lastSQL, "DESC")
}

// TestSearchRecent_HonoursLimit — `?limit=N` for N in (0, 200] sets the
// SQL LIMIT to exactly N. The Grafana Search UI passes a small limit
// for the first page; this pins the parameter wiring.
func TestSearchRecent_HonoursLimit(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 5, 50, 200} {
		n := n
		t.Run(fmt.Sprintf("limit=%d", n), func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(fmt.Sprintf("%s/api/search/recent?limit=%d", srv.URL, n))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			assertSQLContains(t, q.lastSQL, fmt.Sprintf("LIMIT %d", n))
		})
	}
}

// TestSearchRecent_CapsAtMax — `?limit=N` for N > 200 gets capped at
// 200. Prevents a runaway query against a large traces table when
// Grafana's UI (or a bad actor) asks for everything.
func TestSearchRecent_CapsAtMax(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent?limit=9999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	assertSQLContains(t, q.lastSQL, "LIMIT 200")
	// Guard against the user-supplied value leaking in unmodified.
	if strings.Contains(q.lastSQL, "LIMIT 9999") {
		t.Errorf("limit not capped: %s", q.lastSQL)
	}
}

// TestSearchRecent_IgnoresBadLimit — non-numeric or non-positive
// `?limit=` values fall back to the default (20). Loose parser
// matches Loki/Prom's tolerance for ill-formed parameters; Grafana
// occasionally sends `limit=` empty when the user clears the field.
func TestSearchRecent_IgnoresBadLimit(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"abc", "0", "-5", "", "1.5"} {
		raw := raw
		t.Run("limit="+raw, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/search/recent?limit=" + raw)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			assertSQLContains(t, q.lastSQL, "LIMIT 20")
		})
	}
}

// TestSearchRecent_EmptyResult — CH returns zero rows. The handler
// must still emit the canonical `{traces: []}` shape (not omit the
// field) so Grafana's UI doesn't crash on a nil array.
func TestSearchRecent_EmptyResult(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	// Decode into the public response type — confirms the JSON shape
	// is `{traces: [], metrics: {...}}` not `{}` or `{traces: null}`.
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr.Traces == nil {
		t.Errorf("expected non-nil empty Traces slice, got nil (JSON null)")
	}
	if len(sr.Traces) != 0 {
		t.Errorf("expected 0 traces, got %d", len(sr.Traces))
	}
}

// TestSearchRecent_CHFailure — when CH errors out, the handler returns
// 502 + the Tempo error envelope shape Grafana renders specifically
// (vs the generic JSON error). The envelope must include `error:true`
// so the UI knows it's an error and not a partial success.
func TestSearchRecent_CHFailure(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q, want application/json", ct)
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error {
		t.Errorf("expected error=true, got %+v", er)
	}
	if !strings.Contains(er.Message, "connection refused") {
		t.Errorf("expected upstream error to surface in Message; got %q", er.Message)
	}
}

// oneSpanSample is the per-test seeded result row used to assert the
// summary shape. One span = one trace summary; the Recent endpoint
// pivots the row into TraceSummary via toTraceSummaries.
func oneSpanSample() []chclient.Sample {
	return []chclient.Sample{{
		MetricName: "GET /api/users",
		Labels:     map[string]string{"service.name": "frontend"},
		Timestamp:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		Value:      150_000_000, // 150ms in nanoseconds
	}}
}

// assertSQLContains is a small helper so each test's failure points
// at the substring it cared about (vs printing the full SQL each
// time, which makes the failure noisy).
func assertSQLContains(t *testing.T, sql, want string) {
	t.Helper()
	if !strings.Contains(sql, want) {
		t.Errorf("SQL missing %q\n  got: %s", want, sql)
	}
}
