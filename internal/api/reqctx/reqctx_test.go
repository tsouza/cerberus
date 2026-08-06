package reqctx_test

import (
	"net/http/httptest"
	"strings"
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

// rejectTimeout drives the rejection path for one `?timeout=` value and
// returns the client-visible message, asserting the shared shape both
// branches owe: a non-nil error, a bare (deadline-free) context, and a
// message free of any fmt bad-verb placeholder. The placeholder check is
// the load-bearing one — a `%w` verb applied to a nil error still
// produces a non-nil error, so an `err != nil` assertion alone passes
// while the body reads `invalid parameter 'timeout': %!w(<nil>)`.
func rejectTimeout(t *testing.T, raw string) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/query?timeout="+raw, nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	cancel()
	if err == nil {
		t.Fatalf("timeout=%q: expected error, got none", raw)
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatalf("timeout=%q: expected bare request context on error", raw)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "invalid parameter 'timeout': ") {
		t.Fatalf("timeout=%q: message %q lacks the upstream parameter prefix", raw, msg)
	}
	if strings.Contains(msg, "%!") {
		t.Fatalf("timeout=%q: message %q contains an fmt bad-verb placeholder", raw, msg)
	}
	return msg
}

func TestApplyQueryTimeout_Unparseable(t *testing.T) {
	msg := rejectTimeout(t, "nonsense")
	if !strings.Contains(msg, "nonsense") {
		t.Fatalf("message %q does not name the offending value", msg)
	}
}

// TestApplyQueryTimeout_Negative pins the branch a table lumping both
// rejection reasons together cannot see: format.ParseDuration accepts
// "-1s" and returns a nil error, so the negative value is rejected on
// its own merits and must carry its own message.
func TestApplyQueryTimeout_Negative(t *testing.T) {
	msg := rejectTimeout(t, "-1s")
	if !strings.Contains(msg, "-1s") {
		t.Fatalf("message %q does not name the offending value", msg)
	}
}
