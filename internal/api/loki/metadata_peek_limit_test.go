package loki_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// The metadata peek endpoints (/detected_fields, /patterns) buffer their
// WHOLE result into a Go slice with no streaming, so the `line_limit` param
// (which becomes the SQL `LIMIT`) is the only thing standing between a single
// request and an unbounded heap. Before the clamp, line_limit accepted any
// value up to 2^31-1, so `line_limit=2000000000` emitted `LIMIT 2000000000`
// and the drain tried to buffer billions of rows — a no-net process OOM that
// crash-loops the pod (max_memory_usage caps ClickHouse, not the cerberus
// heap). These tests pin the clamp: an over-large line_limit is silently
// capped to maxLogPeekLineLimit (10000), and a normal request is untouched.

const absurdLineLimit = "2000000000"

// peekSQL issues a metadata-peek GET, asserts 200, and returns the SQL the
// stub captured so the test can inspect the emitted LIMIT.
func peekSQL(t *testing.T, path string) (string, *stubQuerier) {
	t.Helper()
	q := &stubQuerier{
		// One canned row per drain so the heuristics have something to chew
		// on; the assertion is about the emitted SQL, not the body.
		detectedRows: []chclient.DetectedFieldRow{{Line: `{"a":1}`}},
		tsLines:      []chclient.TimestampedLine{{Body: "GET /a 200 ok"}},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	return q.LastSQL(), q
}

func TestDetectedFields_LineLimitClampedToCeiling(t *testing.T) {
	t.Parallel()
	sql, _ := peekSQL(t,
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200&line_limit=`+absurdLineLimit)
	if strings.Contains(sql, absurdLineLimit) {
		t.Fatalf("line_limit not clamped — SQL still carries the absurd LIMIT:\n%s", sql)
	}
	if !strings.Contains(sql, "LIMIT 10000") {
		t.Fatalf("expected the clamped `LIMIT 10000`, got:\n%s", sql)
	}
}

func TestPatterns_LineLimitClampedToCeiling(t *testing.T) {
	t.Parallel()
	sql, _ := peekSQL(t,
		`/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200&line_limit=`+absurdLineLimit)
	if strings.Contains(sql, absurdLineLimit) {
		t.Fatalf("line_limit not clamped — SQL still carries the absurd LIMIT:\n%s", sql)
	}
	if !strings.Contains(sql, "LIMIT 10000") {
		t.Fatalf("expected the clamped `LIMIT 10000`, got:\n%s", sql)
	}
}

// A request within the ceiling is untouched — the clamp only caps DOWN, it
// never raises a small/default limit.
func TestDetectedFields_LineLimitWithinCeilingUnchanged(t *testing.T) {
	t.Parallel()
	sql, _ := peekSQL(t,
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200&line_limit=500`)
	if !strings.Contains(sql, "LIMIT 500") {
		t.Fatalf("a within-ceiling line_limit must pass through unchanged, got:\n%s", sql)
	}
}
