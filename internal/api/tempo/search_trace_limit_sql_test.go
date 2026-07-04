package tempo_test

// Handler-level SQL-shape pins for the /api/search trace-limit pushdown
// (the summaries-drain OOM fix). The spec/traceql golden lane threads no
// search limit, so the SearchTraceLimit wrap is a no-op there and the
// emitted top-N subquery never shows up in a txtar golden — these tests
// drive the real handler (which threads `limit` + window into lowering)
// and assert against the SQL the engine handed the querier
// (stubQuerier.lastSQL).

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearch_NegatedTwoPhase_SQLShape pins (non-chdb lane) that a negated
// recursive search `!>>` routes two-phase: the phase-A ranking narrows the same
// LEFT ANTI JOIN shape to the top-N per trace (min(Timestamp) over GROUP BY
// TraceId). The R-side top-N restriction that keeps phase B correct is proven
// end-to-end by TestSearch_NegatedTwoPhase_Parity_ChDB.
func TestSearch_NegatedTwoPhase_SQLShape(t *testing.T) {
	t.Parallel()
	q := url.QueryEscape(`{ resource.service.name = "root-svc" } !>> { resource.service.name = "leaf-svc" }`)
	sql := searchSQL(t, "/api/search?q="+q+"&limit=3")

	for _, want := range []string{
		"LEFT ANTI JOIN",
		"GROUP BY `TraceId`",
		"min(`Timestamp`)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("negated phase-A SQL missing %q (two-phase did not engage?):\n%s", want, sql)
		}
	}
}

// TestSearch_UnionTwoPhase_SQLShape pins (non-chdb lane) that a union recursive
// search `&>>` routes two-phase: the phase-A ranking narrows the two-arm UNION to
// the top-N per trace (min(Timestamp) over GROUP BY TraceId). Byte-identical
// parity + the memory bound are proven end-to-end by
// TestSearch_UnionTwoPhase_Parity_ChDB.
func TestSearch_UnionTwoPhase_SQLShape(t *testing.T) {
	t.Parallel()
	q := url.QueryEscape(`{ resource.service.name = "root-svc" } &>> { resource.service.name = "leaf-svc" }`)
	sql := searchSQL(t, "/api/search?q="+q+"&limit=3")

	for _, want := range []string{
		"UNION",
		"GROUP BY `TraceId`",
		"min(`Timestamp`)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("union phase-A SQL missing %q (two-phase did not engage?):\n%s", want, sql)
		}
	}
}

// TestSearch_WrappedSelectTwoPhase_SQLShape pins (in the non-chdb lane) that a
// `>> | select(...)` query routes two-phase: the phase-A ranking query the
// engine emits carries the min(Timestamp) top-N over GROUP BY TraceId. A
// single-query fallback (had the Project not been unwrapped) would emit the wide
// closure with no such ranking, so these assertions prove the wrapped shape
// engaged the seam.
func TestSearch_WrappedSelectTwoPhase_SQLShape(t *testing.T) {
	t.Parallel()
	q := url.QueryEscape(`{ resource.service.name = "root-svc" } >> { resource.service.name = "leaf-svc" } | select(resource.service.name)`)
	sql := searchSQL(t, "/api/search?q="+q+"&limit=3")

	// The structural phase-A ranks per trace by min(Timestamp) aliased to rankTs
	// (buildStructuralPhaseAPlan), then ORDER BY that alias — so assert the
	// aggregate + group-by that only the two-phase ranking emits, not the inline
	// ORDER BY form the plain-search pushdown uses.
	for _, want := range []string{
		"GROUP BY `TraceId`",
		"min(`Timestamp`)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("wrapped-select phase-A SQL missing %q (two-phase did not engage?):\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "max(`Timestamp`)") {
		t.Errorf("phase-A must rank by min(Timestamp), not max:\n%s", sql)
	}
}

// searchSQL drives one /api/search request through the full handler
// pipeline and returns the SQL the engine emitted (captured by the stub
// querier). An empty sample set is fine — we assert on SQL shape, not
// rows.
func searchSQL(t *testing.T, query string) string {
	t.Helper()
	q := &stubQuerier{samples: []chclient.Sample{}}
	srv := newServer(q, "v-test")
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + query)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if q.lastSQL == "" {
		t.Fatalf("no SQL captured for %q", query)
	}
	return q.lastSQL
}

// TestSearch_TraceLimitPushdown_SQLShape pins that a plain `{}` search
// carries the top-N trace subquery: the outer drain is restricted to
// `TraceId IN (SELECT TraceId FROM (...) GROUP BY TraceId ORDER BY
// min(Timestamp) DESC, TraceId LIMIT <limit>)`. This is the SQL that
// bounds the Go-side drain to the kept traces' spans instead of every
// matching row.
func TestSearch_TraceLimitPushdown_SQLShape(t *testing.T) {
	t.Parallel()
	sql := searchSQL(t, "/api/search?q=%7B%7D&limit=3")

	// The IN-subquery restriction over the trace-id column.
	if !strings.Contains(sql, "`TraceId` IN (SELECT `TraceId` FROM (") {
		t.Errorf("plain {} search SQL missing TraceId IN (subquery) restriction:\n%s", sql)
	}
	// Top-N ranking: newest-by-min-start, TraceId tie-break — NEVER max.
	if !strings.Contains(sql, "GROUP BY `TraceId`") {
		t.Errorf("SQL missing GROUP BY TraceId:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY min(`Timestamp`) DESC, `TraceId`") {
		t.Errorf("SQL missing min(Timestamp) DESC, TraceId ranking (parity key is min, not max):\n%s", sql)
	}
	if strings.Contains(sql, "max(`Timestamp`)") {
		t.Errorf("SQL ranks by max(Timestamp) — parity verdict is min(Timestamp):\n%s", sql)
	}
	// The request limit is pushed into the subquery LIMIT.
	if !strings.Contains(sql, "LIMIT 3") {
		t.Errorf("SQL missing LIMIT 3 (the request limit):\n%s", sql)
	}
}

// TestSearch_TraceLimitPushdown_DefaultLimit pins that the subquery
// LIMIT defaults to DefaultSearchLimit when the request omits `limit`,
// matching TruncateSummaries' Go-side default.
func TestSearch_TraceLimitPushdown_DefaultLimit(t *testing.T) {
	t.Parallel()
	sql := searchSQL(t, "/api/search?q=%7B%7D")
	if !strings.Contains(sql, "`TraceId` IN (SELECT `TraceId` FROM (") {
		t.Fatalf("default-limit search SQL missing IN (subquery):\n%s", sql)
	}
	if !strings.Contains(sql, "LIMIT 20") {
		t.Errorf("SQL missing LIMIT 20 (DefaultSearchLimit):\n%s", sql)
	}
}

// TestSearch_TraceLimitPushdown_LimitClamped pins the P3 hardening: a
// client-supplied `limit` above MaxSearchLimit is clamped, so the pushed
// subquery LIMIT is the ceiling (1000), not the requested value. Without the
// clamp an unauthenticated caller could make the summary shaper buffer the
// spans of an unbounded number of traces.
func TestSearch_TraceLimitPushdown_LimitClamped(t *testing.T) {
	t.Parallel()
	sql := searchSQL(t, "/api/search?q=%7B%7D&limit=999999")
	if !strings.Contains(sql, "LIMIT 1000") {
		t.Errorf("oversized limit must clamp to MaxSearchLimit (LIMIT 1000):\n%s", sql)
	}
	if strings.Contains(sql, "LIMIT 999999") {
		t.Errorf("SQL carries the un-clamped requested limit:\n%s", sql)
	}
}

// TestSearch_TraceLimitPushdown_WindowFolded pins that an explicit
// start/end window folds a Timestamp range predicate into BOTH the inner
// ranking subquery and the outer drain, so each scan is bounded to the
// window rather than the whole table.
func TestSearch_TraceLimitPushdown_WindowFolded(t *testing.T) {
	t.Parallel()
	sql := searchSQL(t, "/api/search?q=%7B%7D&limit=5&start=1000&end=2000")

	// The window must appear twice — once on the outer drain scan and
	// once inside the top-N ranking subquery — so neither scans the
	// whole table.
	const ge = "`Timestamp` >= fromUnixTimestamp64Nano(?)"
	const le = "`Timestamp` <= fromUnixTimestamp64Nano(?)"
	if got := strings.Count(sql, ge); got != 2 {
		t.Errorf("expected the start bound on both scans (2x %q), got %d:\n%s", ge, got, sql)
	}
	if got := strings.Count(sql, le); got != 2 {
		t.Errorf("expected the end bound on both scans (2x %q), got %d:\n%s", le, got, sql)
	}
	if !strings.Contains(sql, "ORDER BY min(`Timestamp`) DESC, `TraceId`") {
		t.Errorf("windowed search SQL missing min-ranking subquery:\n%s", sql)
	}
}

// TestSearch_WindowlessDefaultsLookback_SQLShape pins the L2 fix: a plain
// `{}` search with NO start/end params is clamped by the handler to a
// recent-lookback window (DefaultSearchLookback), so the emitted SQL now
// carries a `Timestamp >=` / `<=` bound on BOTH the inner top-N ranking
// subquery and the outer drain — the inner `GROUP BY TraceId` is therefore
// windowed and never aggregates over the whole table. The clock instant is
// not pinned (the handler uses time.Now); the test asserts SQL SHAPE — the
// presence of the bounds — not the literal nanos.
func TestSearch_WindowlessDefaultsLookback_SQLShape(t *testing.T) {
	t.Parallel()
	sql := searchSQL(t, "/api/search?q=%7B%7D")

	// The window must appear twice — once on the outer drain scan and once
	// inside the top-N ranking subquery — so neither scans the whole table.
	const ge = "`Timestamp` >= fromUnixTimestamp64Nano(?)"
	const le = "`Timestamp` <= fromUnixTimestamp64Nano(?)"
	if got := strings.Count(sql, ge); got != 2 {
		t.Errorf("windowless search must clamp to a lookback: expected the start bound on both scans (2x %q), got %d:\n%s", ge, got, sql)
	}
	if got := strings.Count(sql, le); got != 2 {
		t.Errorf("windowless search must clamp to a lookback: expected the end bound on both scans (2x %q), got %d:\n%s", le, got, sql)
	}
	if !strings.Contains(sql, "GROUP BY `TraceId`") {
		t.Errorf("windowless search SQL missing the windowed top-N GROUP BY TraceId:\n%s", sql)
	}
}

// TestSearch_ExplicitWindow_HonoredVerbatim contrasts with the windowless
// clamp above: an explicit start/end is honored as supplied — the default
// lookback never overrides a window the caller provided. Together the two
// tests pin that the L2 clamp fires ONLY on the both-bounds-absent path.
func TestSearch_ExplicitWindow_HonoredVerbatim(t *testing.T) {
	t.Parallel()
	// start=1000s, end=2000s — explicit bounds. The args carry the exact
	// nanos; assert the supplied window reaches the SQL (both scans) rather
	// than being replaced by a now-relative lookback.
	q := &stubQuerier{}
	srv := newServer(q, "v-test")
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/search?q=%7B%7D&start=1000&end=2000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	sql := q.lastSQL
	const ge = "`Timestamp` >= fromUnixTimestamp64Nano(?)"
	const le = "`Timestamp` <= fromUnixTimestamp64Nano(?)"
	if got := strings.Count(sql, ge); got != 2 {
		t.Errorf("explicit window must fold on both scans (2x %q), got %d:\n%s", ge, got, sql)
	}
	if got := strings.Count(sql, le); got != 2 {
		t.Errorf("explicit window must fold on both scans (2x %q), got %d:\n%s", le, got, sql)
	}
	// The explicit start (1000s = 1e12 ns) and end (2000s = 2e12 ns) must
	// be the args, proving the supplied window — not a now-relative
	// lookback — reached the SQL.
	const startNanos = int64(1000) * 1_000_000_000
	const endNanos = int64(2000) * 1_000_000_000
	var sawStart, sawEnd bool
	for _, a := range q.lastArgs {
		if v, ok := a.(int64); ok {
			if v == startNanos {
				sawStart = true
			}
			if v == endNanos {
				sawEnd = true
			}
		}
	}
	if !sawStart || !sawEnd {
		t.Errorf("explicit window args not honored verbatim: sawStart=%v sawEnd=%v args=%v", sawStart, sawEnd, q.lastArgs)
	}
}
