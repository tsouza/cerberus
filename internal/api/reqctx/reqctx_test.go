package reqctx_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/reqctx"
	"github.com/tsouza/cerberus/internal/chclient"
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

// budgetInstalled asserts BOTH halves of what ApplyQueryTimeout owes for a
// positive budget: the context deadline that unblocks the handler, and the
// chclient carrier that narrows ClickHouse's max_execution_time so the
// server stops working on a query nobody awaits.
//
// The carrier half needs its own assertion because it is invisible from
// the outside: drop the chclient.WithQueryTimeout call and the deadline
// still fires, the handler still returns on time, and only ClickHouse's
// CPU knows the difference.
func budgetInstalled(t *testing.T, ctx context.Context, want time.Duration) {
	t.Helper()
	dl, ok := ctx.Deadline()
	deadlineWithin(t, want, dl, ok)
	got, ok := chclient.QueryTimeoutFromContext(ctx)
	if !ok {
		t.Fatalf("no chclient query-timeout carrier on the context for budget %s", want)
	}
	if got != want {
		t.Fatalf("carrier holds %s, want %s", got, want)
	}
}

// budgetAbsent is the negative control: with no cap from either side,
// neither half may be installed. Without it, a version that unconditionally
// installed the carrier would satisfy every other case here.
func budgetAbsent(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline when neither default nor ?timeout= caps")
	}
	if d, ok := chclient.QueryTimeoutFromContext(ctx); ok {
		t.Fatalf("expected no query-timeout carrier when nothing caps, got %s", d)
	}
}

func TestApplyQueryTimeout_NoParamNoDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/query", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	budgetAbsent(t, ctx)
}

func TestApplyQueryTimeout_DefaultApplies(t *testing.T) {
	r := httptest.NewRequest("GET", "/query", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	budgetInstalled(t, ctx, defBudget)
}

func TestApplyQueryTimeout_RequestSmallerWins(t *testing.T) {
	r := httptest.NewRequest("GET", "/query?timeout=1s", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	budgetInstalled(t, ctx, smallBudget)
}

func TestApplyQueryTimeout_DefaultSmallerWins(t *testing.T) {
	r := httptest.NewRequest("GET", "/query?timeout=10s", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	budgetInstalled(t, ctx, defBudget)
}

// TestApplyQueryTimeout_RequestCapsUncappedDefault covers the deployment
// with no configured default: the request's own ?timeout= is then the only
// bound, and both halves must still carry it.
func TestApplyQueryTimeout_RequestCapsUncappedDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/query?timeout=1s", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	budgetInstalled(t, ctx, smallBudget)
}

// TestApplyQueryTimeout_ZeroRequestKeepsDefault pins the upstream reading
// of `?timeout=0`: zero means "no cap from this source", so the configured
// default survives rather than being min'd away to nothing.
func TestApplyQueryTimeout_ZeroRequestKeepsDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/query?timeout=0s", nil)
	ctx, cancel, err := reqctx.ApplyQueryTimeout(r, defBudget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cancel()
	budgetInstalled(t, ctx, defBudget)
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
