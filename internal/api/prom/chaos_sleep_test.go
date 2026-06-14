//go:build chaos_sleep

package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestApplyChaosSleep_HeaderThreadsSleepIntoEmit asserts that, in the
// chaos_sleep build, a request carrying the undocumented chaos header makes
// applyChaosSleep stamp a ctx that chsql.Emit reads to splice a server-side
// ClickHouse sleep — the end-to-end trigger path the scenario relies on.
func TestApplyChaosSleep_HeaderThreadsSleepIntoEmit(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)
	r.Header.Set(chaosSleepHeader, "8")

	ctx := h.applyChaosSleep(context.Background(), r)

	sql, _, err := chsql.Emit(ctx, &chplan.OneRow{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "sleepEachRow") || !strings.Contains(sql, "numbers(8)") {
		t.Fatalf("chaos header did not thread a sleep into Emit; got: %q", sql)
	}
}

// TestApplyChaosSleep_NoHeaderIsInert asserts that a request WITHOUT the
// chaos header leaves the ctx untouched, so the same plan emits bare SQL —
// the "a normal query is never slowed" guarantee, proven inside the
// chaos_sleep build itself.
func TestApplyChaosSleep_NoHeaderIsInert(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)

	ctx := h.applyChaosSleep(context.Background(), r)

	sql, _, err := chsql.Emit(ctx, &chplan.OneRow{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "sleepEachRow") {
		t.Fatalf("no-header request still spliced a sleep; got: %q", sql)
	}
}

// TestApplyChaosSleep_NonPositiveHeaderIsInert asserts a malformed or
// non-positive header value is inert (no sleep), matching the handler guard.
func TestApplyChaosSleep_NonPositiveHeaderIsInert(t *testing.T) {
	for _, v := range []string{"0", "-2", "abc", ""} {
		h := &Handler{}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)
		if v != "" {
			r.Header.Set(chaosSleepHeader, v)
		}
		ctx := h.applyChaosSleep(context.Background(), r)
		sql, _, err := chsql.Emit(ctx, &chplan.OneRow{})
		if err != nil {
			t.Fatalf("Emit(%q): %v", v, err)
		}
		if strings.Contains(sql, "sleepEachRow") {
			t.Fatalf("header %q spliced a sleep; got: %q", v, sql)
		}
	}
}
