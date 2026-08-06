package reqctx_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/reqctx"
)

const (
	defBudget   = 5 * time.Second
	smallBudget = time.Second
	deltaSlack  = 2 * time.Second
)

// deadlineWithin reports whether ctx carries a deadline landing roughly
// `want` from now (within deltaSlack), so the assertions don't flake on
// scheduling jitter.
func deadlineWithin(t *testing.T, want time.Duration, dl time.Time, ok bool) {
	t.Helper()
	if !ok {
		t.Fatalf("expected a context deadline for budget %s, got none", want)
	}
	got := time.Until(dl)
	if got < want-deltaSlack || got > want+deltaSlack {
		t.Fatalf("deadline %s off target %s (slack %s)", got, want, deltaSlack)
	}
}

func TestApplyQueryTimeout_NoParamNoDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/query", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline when neither default nor ?timeout= caps")
	}
}

func TestApplyQueryTimeout_DefaultApplies(t *testing.T) {
	r := httptest.NewRequest("GET", "/query", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	dl, ok := ctx.Deadline()
	deadlineWithin(t, defBudget, dl, ok)
}

func TestApplyQueryTimeout_RequestSmallerWins(t *testing.T) {
	r := httptest.NewRequest("GET", "/query?timeout=1s", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	dl, ok := ctx.Deadline()
	deadlineWithin(t, smallBudget, dl, ok)
}

func TestApplyQueryTimeout_DefaultSmallerWins(t *testing.T) {
	r := httptest.NewRequest("GET", "/query?timeout=10s", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	dl, ok := ctx.Deadline()
	deadlineWithin(t, defBudget, dl, ok)
}

func TestApplyQueryTimeout_Malformed(t *testing.T) {
	for _, raw := range []string{"nonsense", "-1s"} {
		r := httptest.NewRequest("GET", "/query?timeout="+raw, nil)
		ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
		cancel()
		if err == nil {
			t.Fatalf("timeout=%q: expected error, got none", raw)
		}
		if _, ok := ctx.Deadline(); ok {
			t.Fatalf("timeout=%q: expected bare request context on error", raw)
		}
	}
}
