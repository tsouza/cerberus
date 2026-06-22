//go:build chdb

// chDB-backed repro for the Tempo /api/search summaries-drain OOM
// (sparkling-brewing-conway.md). Before the SearchTraceLimit pushdown,
// `GET /api/search?q={}&limit=N` emitted `SELECT … FROM otel_traces`
// with no window and no LIMIT: the handler drained EVERY matching span
// into a []Sample slice + per-trace map and only then truncated to N in
// Go. For a wide window the full match set is buffered first, OOMing the
// process before the limit ever bites.
//
// The fix wraps the plain-search row source in chplan.SearchTraceLimit,
// which the emitter renders as a `TraceId IN (SELECT … GROUP BY TraceId
// ORDER BY min(Timestamp) DESC, TraceId LIMIT N)` restriction. The SQL
// then returns only the kept traces' spans, so the drain — and the
// InspectedTraces count the response reports (len(res.Samples)) — is
// bounded to N traces, not the whole table.
//
// The load-bearing assertion is InspectedTraces: it equals the kept
// traces' span count, NOT the full seed. Temporarily reverting
// stampSearchTraceLimit's wrap (so the plan stays a bare Scan / Filter)
// makes the count jump to the full seed — that before/after is reported
// in the PR description; this test pins the fixed behaviour.
package tempo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// Each of these seeded traces carries spansPerTrace spans (one root +
// two children), so the full table holds seedTraceCount*spansPerTrace
// rows. With searchLimit < seedTraceCount the pushdown must bound the
// drain to searchLimit*spansPerTrace rows.
const (
	seedTraceCount = 8
	spansPerTrace  = 3
	searchLimit    = 3
)

// manyTracesSeed builds seedTraceCount traces, each with a root span and
// two children. Trace i's root starts at 10:0i:00; the children follow a
// few nanoseconds later (so the trace's min(Timestamp) is the root's
// start). Trace start times strictly increase with i, so the newest
// searchLimit traces by min(start) are the highest-i traces, and the
// TraceId tie-break never has to fire (all min-starts are distinct) — the
// dedicated ranking-parity test below exercises ties + out-of-order
// children.
func manyTracesSeed() string {
	rows := make([]string, 0, seedTraceCount*spansPerTrace)
	for i := 1; i <= seedTraceCount; i++ {
		traceID := fmt.Sprintf("c%031x", i)
		// Root + two children. Minute = i keeps each trace's window
		// disjoint; the +1ns / +2ns children sit after the root so the
		// trace start (min Timestamp) is the root's timestamp.
		base := fmt.Sprintf("2026-05-01 10:%02d:00.000000001", i)
		c1 := fmt.Sprintf("2026-05-01 10:%02d:00.000000002", i)
		c2 := fmt.Sprintf("2026-05-01 10:%02d:00.000000003", i)
		root := fmt.Sprintf("%016x", i*10+1)
		child1 := fmt.Sprintf("%016x", i*10+2)
		child2 := fmt.Sprintf("%016x", i*10+3)
		rows = append(
			rows,
			fmt.Sprintf("('%s', '%s', '', 'GET /root', 'Server', 1000, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'))", traceID, root, base),
			fmt.Sprintf("('%s', '%s', '%s', 'child-a', 'Internal', 500, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'svc-a'))", traceID, child1, root, c1),
			fmt.Sprintf("('%s', '%s', '%s', 'child-b', 'Client', 300, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'svc-b'))", traceID, child2, root, c2),
		)
	}
	insert := "INSERT INTO otel_traces VALUES\n"
	for i, r := range rows {
		if i > 0 {
			insert += ",\n"
		}
		insert += "    " + r
	}
	return insert + ";"
}

func newManyTracesChDBServer(t *testing.T, seed string) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, tracesDDL)
	c.Seed(t, seed)
	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func doSearch(t *testing.T, srv *httptest.Server, path string) tempo.SearchResponse {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	return sr
}

// TestSearch_TraceLimitPushdown_BoundsDrain_ChDB is the genuinely-failing
// repro: it seeds many traces, drives a plain `{}` search with a small
// limit, and asserts the kept set is the newest-by-min-start prefix AND
// that the drain (InspectedTraces == len(res.Samples)) is bounded to the
// kept traces' spans, not the full seed. The InspectedTraces bound is the
// assertion that fails without stampSearchTraceLimit's wrap.
func TestSearch_TraceLimitPushdown_BoundsDrain_ChDB(t *testing.T) {
	srv := newManyTracesChDBServer(t, manyTracesSeed())

	sr := doSearch(t, srv, fmt.Sprintf("/api/search?q=%%7B%%7D&limit=%d&spss=20", searchLimit))

	// Exactly `searchLimit` traces returned.
	if len(sr.Traces) != searchLimit {
		t.Fatalf("got %d traces, want %d", len(sr.Traces), searchLimit)
	}

	// The kept set is the newest `searchLimit` traces by min(start):
	// trace i's start increases with i, so the kept TraceIDs are the
	// highest-i ones (8, 7, 6 for limit=3), in start-desc order.
	for rank := 0; rank < searchLimit; rank++ {
		wantTrace := seedTraceCount - rank
		wantID := fmt.Sprintf("c%031x", wantTrace)
		if sr.Traces[rank].TraceID != wantID {
			t.Errorf("trace[%d] = %q, want %q (newest-by-min-start desc)", rank, sr.Traces[rank].TraceID, wantID)
		}
	}

	// Start-desc ordering invariant across the whole returned list.
	for i := 1; i < len(sr.Traces); i++ {
		prev, _ := strconv.ParseUint(sr.Traces[i-1].StartTimeUnixNano, 10, 64)
		cur, _ := strconv.ParseUint(sr.Traces[i].StartTimeUnixNano, 10, 64)
		if prev < cur {
			t.Errorf("traces not start-desc at %d: %d < %d", i, prev, cur)
		}
	}

	// ★ Load-bearing: the drain is bounded to the kept traces' spans.
	// kept = searchLimit traces * spansPerTrace spans. The full seed is
	// seedTraceCount*spansPerTrace; without the pushdown InspectedTraces
	// would equal the full seed (the OOM-prone unbounded drain).
	wantInspected := searchLimit * spansPerTrace
	fullSeed := seedTraceCount * spansPerTrace
	if sr.Metrics.InspectedTraces != wantInspected {
		t.Errorf("InspectedTraces = %d, want %d (kept %d traces * %d spans); full seed is %d — a value of %d means the drain was NOT bounded (the OOM bug)",
			sr.Metrics.InspectedTraces, wantInspected, searchLimit, spansPerTrace, fullSeed, fullSeed)
	}
	// Guard the assertion against a degenerate seed: the bound must be a
	// real reduction, otherwise the test proves nothing.
	if wantInspected >= fullSeed {
		t.Fatalf("seed misconfigured: kept-span count %d not < full seed %d", wantInspected, fullSeed)
	}
}

// rankingParitySeed builds two traces where:
//
//   - trace LOW has an out-of-order child whose timestamp is EARLIER than
//     its own root (so the trace's min(Timestamp) is the child's, not the
//     root's) — proving the ranking aggregates min over ALL matched
//     spans, not the root alone;
//   - trace HIGH's min(Timestamp) is later than LOW's min, but LOW's
//     MAX(Timestamp) is later than HIGH's max.
//
// min-ranking keeps {LOW, HIGH} ordered HIGH-first only if we rank by
// min DESC; a max-based ranking would order LOW first (its max is the
// latest in the table). With limit=1, min-ranking keeps HIGH; max-ranking
// would (wrongly) keep LOW. That divergence is what proves the fix uses
// min, not max.
const (
	// 32-hex trace IDs (Tempo wire IDs are hex). "lowTraceID" is the one
	// whose min-start is EARLIER but whose max-start is LATER; "highTraceID"
	// is the one min-ranking must keep at limit=1.
	lowTraceID  = "d00000000000000000000000000000aa"
	highTraceID = "e00000000000000000000000000000bb"
)

const rankingParitySeed = `INSERT INTO otel_traces VALUES
    ('d00000000000000000000000000000aa', '00000000000000a1', '', 'low root', 'Server', 1000, toDateTime64('2026-05-01 11:00:05.000000000', 9), 'Unset', '', '', '', map(), map('service.name', 'low')),
    ('d00000000000000000000000000000aa', '00000000000000a2', '00000000000000a1', 'low ooo child', 'Internal', 500, toDateTime64('2026-05-01 11:00:01.000000000', 9), 'Unset', '', '', '', map(), map('service.name', 'low')),
    ('d00000000000000000000000000000aa', '00000000000000a3', '00000000000000a1', 'low late child', 'Client', 300, toDateTime64('2026-05-01 11:00:59.000000000', 9), 'Unset', '', '', '', map(), map('service.name', 'low')),
    ('e00000000000000000000000000000bb', '00000000000000b1', '', 'high root', 'Server', 1000, toDateTime64('2026-05-01 11:00:10.000000000', 9), 'Unset', '', '', '', map(), map('service.name', 'high')),
    ('e00000000000000000000000000000bb', '00000000000000b2', '00000000000000b1', 'high child', 'Internal', 500, toDateTime64('2026-05-01 11:00:12.000000000', 9), 'Unset', '', '', '', map(), map('service.name', 'high'));`

// TestSearch_TraceLimitPushdown_MinRanking_ChDB proves the top-N ranking
// is min(Timestamp), not max. Trace LOW's min is 11:00:01 (its out-of-order
// child) and its max is 11:00:59; trace HIGH's min is 11:00:10 and max
// 11:00:12. With limit=1:
//
//   - min-DESC ranking keeps HIGH (11:00:10 > 11:00:01) — correct;
//   - max-DESC ranking would keep LOW (11:00:59 > 11:00:12) — wrong.
//
// HIGH is the only correct survivor, and a min ranking selects it while a
// max ranking would not.
//
// Scope: this fixture discriminates min from max. It does NOT, on its own,
// distinguish min-over-matched-spans from min-over-roots — both rank HIGH
// first here (HIGH's root 11:00:10 > LOW's root 11:00:05, same survivor as
// matched-min). That the ranking subquery folds the matcher predicate (so
// min runs over matched spans, not roots) is pinned by the SQL-shape tests
// in search_trace_limit_sql_test.go, which assert the predicate appears
// inside the ranking subquery and that ORDER BY uses min(`Timestamp`), not
// max(`Timestamp`).
func TestSearch_TraceLimitPushdown_MinRanking_ChDB(t *testing.T) {
	srv := newManyTracesChDBServer(t, rankingParitySeed)

	sr := doSearch(t, srv, "/api/search?q=%7B%7D&limit=1&spss=20")

	if len(sr.Traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(sr.Traces))
	}
	if sr.Traces[0].TraceID == lowTraceID {
		t.Fatalf("kept LOW trace — ranking used max(Timestamp) or root-only min, not min over matched spans")
	}
	if sr.Traces[0].TraceID != highTraceID {
		t.Fatalf("kept %q, want HIGH %q (min(Timestamp) DESC over matched spans)", sr.Traces[0].TraceID, highTraceID)
	}
}
