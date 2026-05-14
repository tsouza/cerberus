//go:build chdb

// chDB-backed regression coverage for the prom handler's
// subquery-bare-vector wire-wrap. PR #310 collapsed the rename Project
// layer that swallowed the matrix RangeWindow's per-row anchor column,
// and `internal/api/prom/handler.go::wrapWithSampleProjection`'s matrix
// branch was the one callsite that still wired its outer Project to the
// pre-#310 alias (`s.TimestampColumn`). The fix points the ColumnRef at
// `anchor_ts` directly (matching the emitter's outer SELECT) and lets
// the projection's own Alias rename it back to s.TimestampColumn on the
// way out.
//
// This file's test pins the regression end-to-end (handler → engine →
// chDB) so a future refactor that re-flips the alias gets caught before
// the e2e `TestPromQuerySubqueryBareVector` failure surfaces against
// `/api/v1/query` with `query=up[1m:30s]`.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestQueryRange_SubqueryBareVector_ChDB exercises `up[1m:30s]` on
// `/api/v1/query` end-to-end against an in-process chDB session. Pins
// the Regression B failure (`wrapWithSampleProjection`'s matrix branch
// ColumnRef'd `s.TimestampColumn` — a column the emitWindowedArrayMatrix
// outer SELECT does NOT expose; the fix points it at `anchor_ts`).
//
// Mirrors `TestQueryRange_Matrix_ChDB`'s shape but uses the
// subquery-bare-vector instant-query route the e2e harness exercises.
// The assertion set is intentionally minimal — HTTP 200 + status
// success + non-empty result — because the per-step values depend on
// `now64()` (no `@` modifier) and the synthesised seed window has to
// straddle CH-now to surface any data. Pinning a wire-shape regression
// rather than a value regression keeps the test stable across time.
func TestQueryRange_SubqueryBareVector_ChDB(t *testing.T) {
	// Seed five `up` gauge samples spaced 30s apart, ending close to
	// "now" so the matrix RangeWindow's `End = now64(9)` (no `@`
	// modifier on the subquery) catches them in its [end-1m, end]
	// window across at least one anchor.
	//
	// The exact-timestamp granularity isn't load-bearing — the
	// regression we're pinning is wire-format (which column the outer
	// Project ColumnRefs), not arithmetic.
	now := time.Now().UTC()
	seedRows := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		// Anchor each sample at (now - i*30s); five samples, 30s apart,
		// span 2 minutes — wide enough that the matrix's 1m window
		// catches at least one per anchor.
		ts := now.Add(-time.Duration(i) * 30 * time.Second).Format("2006-01-02 15:04:05.000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('up', map('job', 'api'), toDateTime64('%s', 9), 1.0)`, ts,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"

	srv, _ := newChDBServer(t, seed)

	// `up[1m:30s]` is a SubqueryExpr — Prom's range-vector form usable
	// in an instant query. The handler emits a vector envelope around
	// the matrix RangeWindow rows (cerberus's pivot rule for
	// /api/v1/query). The wire test we want to pin runs entirely on
	// the underlying matrix RangeWindow's `anchor_ts` projection — if
	// the wire-wrap ColumnRef'd the wrong column, chDB would 502 with
	// `UNKNOWN_IDENTIFIER` long before any data shape mismatch.
	url := srv.URL + "/api/v1/query?query=" + escape("up[1m:30s]")
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: got %q (errorType=%q error=%q), want success "+
			"(pre-fix Regression B path: 502 from chDB "+
			"UNKNOWN_IDENTIFIER: 'TimeUnix' on the outer matrix Project)",
			parsed.Status, parsed.ErrorType, parsed.Error)
	}

	// The result is wrapped as a JSON array (cerberus pivots subquery
	// matrix output to vector samples via toVector). Decode raw and
	// assert non-empty — anything in the result array proves the
	// matrix RangeWindow SQL executed without hitting an
	// UNKNOWN_IDENTIFIER on the wire-wrap.
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var result []any
	if err := json.Unmarshal(rawResult, &result); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, rawResult)
	}
	if len(result) == 0 {
		t.Fatalf("expected non-empty result (matrix RangeWindow over 5 "+
			"seeded samples must surface at least one anchor); got: %s",
			rawResult)
	}
}

// escape is `url.QueryEscape` minus the import — kept local to this
// file so the small test stays focused. Mirrors what the test runner
// would emit for `?query=<x>` with `<x>` containing `[`, `]`, and `:`.
func escape(q string) string {
	var b strings.Builder
	for _, r := range q {
		switch {
		case r == ' ':
			b.WriteByte('+')
		case r == '[':
			b.WriteString("%5B")
		case r == ']':
			b.WriteString("%5D")
		case r == ':':
			b.WriteString("%3A")
		case r == '{':
			b.WriteString("%7B")
		case r == '}':
			b.WriteString("%7D")
		case r == '"':
			b.WriteString("%22")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
