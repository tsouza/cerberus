//go:build chdb

// Symptom-first regression test for cerberus issue #2829's M2 milestone
// exit criterion ("M2: No knowingly-wrong answers on the default path").
// #2829 itself is closed — internal/logql/lower.go's maybePushLogLineLimit
// pushes Loki's `limit` into a real SQL `ORDER BY ... LIMIT N`, and
// limit_pushdown_test.go / limit_pushdown_chdb_test.go already characterize
// the plan-shape and correctness side of that change. What none of those
// tests cover is the PRODUCTION SYMPTOM the issue names: a wide-window Loki
// range query tripping the per-query sample budget
// (CERBERUS_QUERY_MAX_SAMPLES / chclient.TooManySamplesError) and
// surfacing as HTTP 400, because without a SQL-side LIMIT the query
// decodes the entire matching window before anything in cerberus ever
// gets to clamp it.
//
// This file starts from that symptom (Phase 1 proves the pre-#2829 plan
// shape really does trip the budget against a genuine chDB-decoded row
// count, not an injected error), then proves the shipped, unmodified
// production handler resolves it for the IDENTICAL query and window
// (Phase 2), and finally confirms the emitted plan carries the
// ORDER BY + LIMIT shape and is eligible for lazy materialization
// (Phase 3).
package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// sampleBudgetedQuerier wraps chclienttest.Client and reproduces the
// row-count-over-budget rejection the REAL production *chclient.Client's
// cursor enforces (Config.MaxQuerySamples, checked in
// internal/chclient/client.go's drainBudgetExceeded / cursor.go's
// rowsCursor.maxSamples abort). chclienttest.Client — the chDB test double
// every other chdb-tagged test in this package uses — never consults a
// budget at all: it is a thin decode-everything double, not a
// budget-aware cursor. Without this wrapper a chDB-backed test could never
// observe a genuine *chclient.TooManySamplesError produced from a REAL
// decoded row count; it could only inject one synthetically (as
// handler_sample_budget_test.go's stubQuerier does for the generic
// error-mapping proof).
//
// The boundary matches production exactly: a positive maxSamples aborts
// once the decoded count exceeds it (">", not ">="), and maxSamples<=0
// disables the check.
type sampleBudgetedQuerier struct {
	*chclienttest.Client
	maxSamples int64
}

func (q *sampleBudgetedQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	samples, err := q.Client.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if q.maxSamples > 0 && int64(len(samples)) > q.maxSamples {
		return nil, &chclient.TooManySamplesError{Limit: q.maxSamples}
	}
	return samples, nil
}

// streamsRangeResponse decodes a /loki/api/v1/query_range "streams" body
// with Result typed directly as []Stream, sidestepping the any-typed
// QueryData.Result field so the test can read entry counts without a
// re-marshal round trip.
type streamsRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string   `json:"resultType"`
		Result     []Stream `json:"result"`
	} `json:"data"`
}

// TestLimitPushdown_SampleBudget400ThenRows_ChDB is the M2 exit-criterion
// proof: it starts from the symptom (a wide-window query tripping the
// sample budget), proves that symptom is real, and then proves the
// shipped fix resolves it for the identical query.
//
// Arithmetic this test pins: seedRows (2000) > maxSamples (300) — decoding
// the full matching window WOULD exceed the budget — while reqLimit (50)
// stays under maxSamples (300), so SQL-side truncation to reqLimit keeps
// the decoded count comfortably under budget. A real deployment's window
// and CERBERUS_QUERY_MAX_SAMPLES=5,000,000 budget have the identical
// shape at production scale; the magnitudes here are only shrunk to keep
// chDB seeding fast.
func TestLimitPushdown_SampleBudget400ThenRows_ChDB(t *testing.T) {
	const (
		seedRows   = 2000
		rowSpacing = 5 * time.Second
		maxSamples = 300
		reqLimit   = 50
	)
	if seedRows <= maxSamples {
		t.Fatalf("test misconfigured: seedRows (%d) must exceed maxSamples (%d), or the pre-pushdown decode would not actually trip the budget", seedRows, maxSamples)
	}
	if reqLimit >= maxSamples {
		t.Fatalf("test misconfigured: reqLimit (%d) must stay under maxSamples (%d), or SQL-side truncation would not actually land under budget", reqLimit, maxSamples)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(seedRows * rowSpacing)
	s := schema.DefaultOTelLogs()

	ddl := insertRows(
		seedRows,
		start,
		func(i int) map[string]string { return map[string]string{"app": "x"} },
		func(i int) string { return fmt.Sprintf("line %d", i) },
	)

	c := chclienttest.NewChDB(t)
	c.Seed(t, ddl)
	q := &sampleBudgetedQuerier{Client: c, maxSamples: maxSamples}
	h := New(q, s, nil)

	const query = `{app="x"}`
	expr, err := logql.ParseExprPermissive(query)
	if err != nil {
		t.Fatalf("ParseExprPermissive(%q): %v", query, err)
	}

	// --- Phase 1: the pre-#2829 plan genuinely trips the sample budget. ---
	//
	// logql.LowerAt (no LowerOpts) is the exact plan shape every log-line
	// query got before #2829 — no Limit(OrderBy(...)) wrap, full-window
	// decode. It is not a hypothetical or a deleted code path:
	// TestLogLineLimitPushdown_ShapeAndGating (internal/logql) pins it as
	// the base plan maybePushLogLineLimit wraps additively, and it is
	// exercised here exactly as that test builds it.
	basePlan, err := logql.LowerAt(context.Background(), expr, s, start, end)
	if err != nil {
		t.Fatalf("LowerAt (pre-pushdown plan): %v", err)
	}
	langOld := &logql.Lang{Schema: s, Start: start, End: end, LogLineLimit: int64(reqLimit), LogLineBackward: true}
	meta := engine.Meta{IsMetric: false, ResponseShape: "loki-streams", Extra: map[string]any{"expr": expr}}

	_, errOld := h.Engine.QueryPlan(context.Background(), langOld, basePlan, meta)
	if errOld == nil {
		t.Fatalf("pre-pushdown plan against %d seeded rows (budget %d) succeeded; want a genuine sample-budget rejection — the repro did not reproduce the bug", seedRows, maxSamples)
	}
	var tooMany *chclient.TooManySamplesError
	if !errors.As(errOld, &tooMany) {
		t.Fatalf("pre-pushdown plan error = %v (%T), want errors.As(_, *chclient.TooManySamplesError) — the repro must fail the SAME way the issue claims, not some other error", errOld, errOld)
	}
	if tooMany.Limit != maxSamples {
		t.Errorf("TooManySamplesError.Limit = %d, want %d", tooMany.Limit, maxSamples)
	}

	// classifyEngineErr is the SAME function handleQueryRange calls on
	// every h.Engine.Query/QueryPlan error (see classifyEngineErr's
	// sample-budget branch) — TestQueryRange_SampleBudget400
	// (handler_sample_budget_test.go) pins its wire-level output against
	// an INJECTED error; this call feeds it the REAL error Phase 1 just
	// produced from a genuine over-budget chDB decode.
	mapped := classifyEngineErr(errOld)
	apiErr, ok := mapped.(*apiError)
	if !ok {
		t.Fatalf("classifyEngineErr(%v) = %T, want *apiError", errOld, mapped)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("mapped Status = %d, want %d", apiErr.Status, http.StatusBadRequest)
	}
	if apiErr.Kind != ErrBadData {
		t.Errorf("mapped Kind = %q, want %q", apiErr.Kind, ErrBadData)
	}
	wantMsg := fmt.Sprintf(
		"maximum number of samples (%d) reached for a single query; consider reducing the query range or resolution",
		maxSamples,
	)
	if apiErr.Err.Error() != wantMsg {
		t.Errorf("mapped message = %q, want %q", apiErr.Err.Error(), wantMsg)
	}

	// --- Phase 2: the SAME query, through the CURRENT, unmodified,
	// real HTTP handler, returns 200 with rows. ---
	//
	// handleQueryRange threads the request's own `limit` unconditionally
	// (langForRangeRequest) and internal/logql/lower.go's
	// maybePushLogLineLimit pushes it into SQL for this safe bare-selector
	// shape — this is not a bypass or a reconstruction, it is the exact
	// code path a real /query_range request runs today.
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var inspected int64
	h.SetOnQueryRangeDrain(func(n int64) { inspected = n })

	reqURL := fmt.Sprintf(
		"%s/loki/api/v1/query_range?query=%s&start=%d&end=%d&limit=%d&direction=backward",
		srv.URL,
		url.QueryEscape(query),
		start.UnixNano(),
		end.UnixNano(),
		reqLimit,
	)
	resp, err := http.Get(reqURL) //nolint:noctx // test-local httptest.Server, no context to thread
	if err != nil {
		t.Fatalf("GET %s: %v", reqURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (the SAME query that tripped the sample budget in Phase 1 must now succeed) — body: %s", resp.StatusCode, rawBody)
	}

	var body streamsRangeResponse
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatalf("decode body: %v (body: %s)", err, rawBody)
	}
	if body.Status != "success" {
		t.Fatalf("status field: got %q, want \"success\"", body.Status)
	}
	entries := 0
	for _, stream := range body.Data.Result {
		entries += len(stream.Values)
	}
	if entries == 0 {
		t.Fatal("response carries zero log entries; want rows")
	}

	// Anti-vacuous: the drain genuinely happened under budget, and
	// truncated to the request's own limit rather than the full
	// seedRows-row window — proving SQL-side LIMIT, not luck, kept
	// decode under budget.
	if inspected == 0 {
		t.Fatal("Inspected drain count never fired (onQueryRangeDrain hook not observed)")
	}
	if inspected > maxSamples {
		t.Errorf("Inspected = %d, exceeds maxSamples %d — the same query that 400ed in Phase 1 would 400 again", inspected, maxSamples)
	}
	if inspected != reqLimit {
		t.Errorf("Inspected = %d, want exactly %d (SQL must have truncated to the request limit; %d seeded rows were available to decode without it)", inspected, reqLimit, seedRows)
	}

	// --- Phase 3: the emitted plan carries ORDER BY + LIMIT, and
	// qualifies for lazy materialization. ---
	pushedPlan, err := logql.LowerAtRangeOpts(context.Background(), expr, s, start, end, 0, logql.LowerOpts{
		LogLineLimit:    int64(reqLimit),
		LogLineBackward: true,
	})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts (pushed plan): %v", err)
	}
	lim, ok := pushedPlan.(*chplan.Limit)
	if !ok {
		t.Fatalf("pushed plan top node = %T, want *chplan.Limit", pushedPlan)
	}
	if lim.Count != int64(reqLimit) {
		t.Errorf("Limit.Count = %d, want %d", lim.Count, reqLimit)
	}
	if _, ok := lim.Input.(*chplan.OrderBy); !ok {
		t.Fatalf("Limit.Input = %T, want *chplan.OrderBy — the ORDER BY + LIMIT shape the milestone's exit criterion requires", lim.Input)
	}

	elLimit, elOK := engine.EligibleForLazyMaterialization(pushedPlan)
	if !elOK {
		t.Fatal("EligibleForLazyMaterialization(pushedPlan) ok = false, want true — the pushed Loki plan must qualify for query_plan_optimize_lazy_materialization")
	}
	if elLimit != int64(reqLimit) {
		t.Errorf("EligibleForLazyMaterialization limit = %d, want %d", elLimit, reqLimit)
	}
}
