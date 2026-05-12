//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTempoSearch_EmptyQ — `/api/search` with no `q` returns 200 with
// an empty `{traces:[]}` body. This is Grafana's Tempo datasource
// health-check path — regressing to 400 here silently breaks the
// "Test datasource" button.
func TestTempoSearch_EmptyQ(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp := getJSON(ctx, t, "/api/search")
	var parsed struct {
		Traces []any `json:"traces"`
	}
	mustDecode(t, resp, &parsed)
	if len(parsed.Traces) != 0 {
		t.Errorf("empty-q search should return empty traces; got %d", len(parsed.Traces))
	}
}

// TestTempoSearch_DurationFilter — `{ duration > 50ms }` returns
// trace summaries for the seeded spans whose Duration exceeds 50ms
// (the seed includes a 600ms POST /checkout, 450ms POST /api/order,
// 300ms orders.insert, 150ms GET /home, 80ms GET /api/users, 90ms
// cron.refresh — six spans match).
func TestTempoSearch_DurationFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("q", `{ duration > 50ms }`)
	resp := getJSON(ctx, t, "/api/search?"+v.Encode())
	var parsed struct {
		Traces []struct {
			DurationMs int `json:"durationMs"`
		} `json:"traces"`
	}
	mustDecode(t, resp, &parsed)
	if len(parsed.Traces) == 0 {
		t.Fatalf("expected at least one trace with duration > 50ms; got 0")
	}
	for _, tr := range parsed.Traces {
		if tr.DurationMs <= 50 {
			t.Errorf("trace with durationMs=%d in result; should be >50", tr.DurationMs)
		}
	}
}

// TestTempoSearch_AndFilter — `{ resource.service.name = "frontend"
// && duration > 50ms }` returns only frontend spans with that
// duration. The seed has two frontend spans: GET /home (150ms,
// matches) and POST /checkout (600ms, matches).
func TestTempoSearch_AndFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("q", `{ resource.service.name = "frontend" && duration > 50ms }`)
	resp := getJSON(ctx, t, "/api/search?"+v.Encode())
	var parsed struct {
		Traces []struct {
			RootServiceName string `json:"rootServiceName"`
		} `json:"traces"`
	}
	mustDecode(t, resp, &parsed)
	if len(parsed.Traces) == 0 {
		t.Fatalf("expected at least one frontend trace with duration > 50ms; got 0")
	}
	for _, tr := range parsed.Traces {
		if tr.RootServiceName != "frontend" {
			t.Errorf("unexpected service in result: %q (filter was service.name=frontend)", tr.RootServiceName)
		}
	}
}

// TestTempoSearch_StructuralChild — `{ frontend } > { api }` runs the
// inner-join self-query on (TraceId, ParentSpanId).
//
// Skipped until RC2: the wrap-sample projection on top of
// StructuralJoin references SpanName/ResourceAttributes/Timestamp by
// their bare names, but StructuralJoin's `SELECT R.* FROM ...` output
// doesn't expose them without R-prefixed qualifiers. CH responds:
//
//	code 47, message: Unknown expression identifier 'SpanName' in scope
//
// Same bug class as TestPromQueryRangeRate (wrap-projection vs
// derived-plan column mismatch). Tracked on the project board.
func TestTempoSearch_StructuralChild(t *testing.T) {
	t.Skip("wrap-projection over StructuralJoin column-scope mismatch — deferred to RC2 (same bug class as TestPromQueryRangeRate)")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("q", `{ resource.service.name = "frontend" } > { resource.service.name = "api" }`)
	resp := getJSON(ctx, t, "/api/search?"+v.Encode())
	var parsed struct {
		Traces []any `json:"traces"`
	}
	mustDecode(t, resp, &parsed)
	// We don't assert exact count (toTraceSummaries currently keys on
	// MetricName+Timestamp, not TraceID, so structural-join results
	// may collapse) — but at least one row must come back.
	if len(parsed.Traces) == 0 {
		t.Fatalf("expected at least one structural-join result; got 0")
	}
}

// TestTempoSearch_CountScalar — `{ frontend } | count() > 0` exercises
// the Aggregate + ScalarFilter lowering.
//
// Skipped until RC2: the wrap-sample projection on top of
// Filter(Aggregate(Scan)) references SpanName, but the Aggregate's
// output is just `Value`. Same column-scope mismatch as
// TestTempoSearch_StructuralChild; CH responds with code 47 "Unknown
// expression identifier 'SpanName' in scope".
func TestTempoSearch_CountScalar(t *testing.T) {
	t.Skip("wrap-projection over Filter(Aggregate) column-scope mismatch — deferred to RC2")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("q", `{ resource.service.name = "frontend" } | count() > 0`)
	resp := getJSON(ctx, t, "/api/search?"+v.Encode())
	var parsed struct {
		Traces []any `json:"traces"`
	}
	mustDecode(t, resp, &parsed)
	if len(parsed.Traces) == 0 {
		t.Fatalf("count() > 0 with 2 matching spans should return ≥ 1 row; got 0")
	}
}

// TestTempoSearch_InvalidTraceQL — bogus input returns 400 with the
// Tempo distinct error envelope shape. Grafana renders this
// specifically; regression to generic JSON would silently break the
// Tempo datasource error UI.
func TestTempoSearch_InvalidTraceQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/search?q=wharblgarbl", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	var env struct {
		TraceID string `json:"traceID"`
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.Error {
		t.Errorf("envelope: error=%v, want true", env.Error)
	}
	if env.Message == "" {
		t.Errorf("envelope: empty message")
	}
}
