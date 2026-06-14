package chclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// chTimeoutException builds the typed driver exception ClickHouse raises
// for TIMEOUT_EXCEEDED — the shape a query that crosses max_execution_time
// with timeout_overflow_mode=throw surfaces ("Timeout exceeded: elapsed
// 120.0 seconds, maximum: 120 seconds").
func chTimeoutException() *clickhouse.Exception {
	return &clickhouse.Exception{
		Code:    159,
		Name:    "TIMEOUT_EXCEEDED",
		Message: "Timeout exceeded: elapsed 120.4 seconds, maximum: 120 seconds",
	}
}

// TestWrapQueryTimeout_Code159 — a CH exception with code 159 becomes a
// *QueryTimeoutError carrying the configured budget, matchable both via
// errors.Is (sentinel) and errors.As (typed exception still reachable).
func TestWrapQueryTimeout_Code159(t *testing.T) {
	t.Parallel()

	const budget = 2 * time.Minute
	got := wrapQueryTimeout(chTimeoutException(), budget)

	var qt *QueryTimeoutError
	if !errors.As(got, &qt) {
		t.Fatalf("wrapQueryTimeout returned %T (%v); want *QueryTimeoutError", got, got)
	}
	if qt.Timeout != budget {
		t.Errorf("Timeout = %s; want %s", qt.Timeout, budget)
	}
	if !errors.Is(got, ErrQueryTimeout) {
		t.Error("errors.Is(got, ErrQueryTimeout) = false; want true")
	}
	var ex *clickhouse.Exception
	if !errors.As(got, &ex) || ex.Code != 159 {
		t.Errorf("underlying *clickhouse.Exception not reachable via errors.As (ex=%v)", ex)
	}
	if !strings.Contains(qt.Error(), "2m0s") {
		t.Errorf("Error() = %q; want it to name the configured budget", qt.Error())
	}
}

// TestWrapQueryTimeout_PassThrough — nil, non-exception errors, and
// exceptions with other codes pass through unchanged: classification is
// typed-code-159-only, never string matching.
func TestWrapQueryTimeout_PassThrough(t *testing.T) {
	t.Parallel()

	if got := wrapQueryTimeout(nil, time.Minute); got != nil {
		t.Errorf("wrapQueryTimeout(nil) = %v; want nil", got)
	}

	plain := errors.New("read: connection reset by peer mentioning Timeout exceeded")
	if got := wrapQueryTimeout(plain, time.Minute); got != plain {
		t.Errorf("plain error was rewritten to %v; want pass-through (no string matching)", got)
	}

	otherCode := &clickhouse.Exception{Code: 241, Name: "MEMORY_LIMIT_EXCEEDED", Message: "Memory limit exceeded"}
	if errors.Is(wrapQueryTimeout(otherCode, time.Minute), ErrQueryTimeout) {
		t.Error("code-241 exception classified as a timeout; want only code 159")
	}
}

// TestQueryTimeoutError_NoBudget — Timeout 0 (no per-query cap; the
// rejection came from a CH server-side limit) renders an honest message
// that does not invent a budget value.
func TestQueryTimeoutError_NoBudget(t *testing.T) {
	t.Parallel()

	got := wrapQueryTimeout(chTimeoutException(), 0)
	var qt *QueryTimeoutError
	if !errors.As(got, &qt) {
		t.Fatalf("wrapQueryTimeout returned %T; want *QueryTimeoutError", got)
	}
	if qt.Timeout != 0 {
		t.Errorf("Timeout = %s; want 0", qt.Timeout)
	}
	if strings.Contains(qt.Error(), "(0s)") {
		t.Errorf("Error() = %q; must not invent a 0s budget", qt.Error())
	}
}

// TestEffectiveQueryTimeout — the per-request WithQueryTimeout override
// min's with the Client's configured default (Prometheus's smaller-wins
// rule); a 0 on either side means "no cap from that source".
func TestEffectiveQueryTimeout(t *testing.T) {
	t.Parallel()

	const def = 2 * time.Minute
	c := &Client{queryTimeout: def}

	// No override → the configured default applies.
	if got := c.effectiveQueryTimeout(context.Background()); got != def {
		t.Errorf("no override: effective = %s; want %s", got, def)
	}

	// Smaller override wins.
	small := 30 * time.Second
	if got := c.effectiveQueryTimeout(WithQueryTimeout(context.Background(), small)); got != small {
		t.Errorf("smaller override: effective = %s; want %s", got, small)
	}

	// Larger override does NOT widen past the default (min, not max).
	large := 10 * time.Minute
	if got := c.effectiveQueryTimeout(WithQueryTimeout(context.Background(), large)); got != def {
		t.Errorf("larger override: effective = %s; want %s (default is the ceiling)", got, def)
	}

	// Disabled default + an override → the override applies (no cap to min against).
	bare := &Client{}
	if got := bare.effectiveQueryTimeout(WithQueryTimeout(context.Background(), small)); got != small {
		t.Errorf("disabled default + override: effective = %s; want %s", got, small)
	}

	// Disabled default + no override → no cap at all.
	if got := bare.effectiveQueryTimeout(context.Background()); got != 0 {
		t.Errorf("disabled default, no override: effective = %s; want 0", got)
	}
}

// TestQuerySettings_MaxExecutionTime — a configured query timeout stamps
// max_execution_time (seconds) + timeout_overflow_mode=throw, merging
// with the memory cap on the same map (never clobbering it). A
// per-request override narrows the value sent.
func TestQuerySettings_MaxExecutionTime(t *testing.T) {
	t.Parallel()

	c := &Client{queryTimeout: 2 * time.Minute}
	s := c.querySettings(context.Background())
	if s == nil {
		t.Fatal("querySettings() = nil with a configured timeout; want max_execution_time set")
	}
	if got := s[settingMaxExecutionTime]; got != float64(120) {
		t.Errorf("max_execution_time = %v (%T); want 120 (float64 seconds)", got, got)
	}
	if got := s[settingTimeoutOverflowMode]; got != timeoutOverflowModeThrow {
		t.Errorf("timeout_overflow_mode = %v; want %q", got, timeoutOverflowModeThrow)
	}

	// Override narrows the value actually sent.
	marked := c.querySettings(WithQueryTimeout(context.Background(), 30*time.Second))
	if got := marked[settingMaxExecutionTime]; got != float64(30) {
		t.Errorf("override max_execution_time = %v; want 30 (the smaller wins)", got)
	}

	// Merges with the memory cap rather than clobbering it.
	both := (&Client{queryTimeout: time.Minute, maxMemory: 1 << 30}).querySettings(context.Background())
	if both["max_memory_usage"] != int64(1<<30) {
		t.Errorf("max_memory_usage = %v; want the cap preserved alongside the timeout", both["max_memory_usage"])
	}
	if both[settingMaxExecutionTime] != float64(60) {
		t.Errorf("max_execution_time = %v; want 60", both[settingMaxExecutionTime])
	}

	// No timeout configured and no override → setting omitted entirely.
	none := (&Client{}).querySettings(context.Background())
	if none != nil {
		if _, ok := none[settingMaxExecutionTime]; ok {
			t.Errorf("querySettings with no timeout carries max_execution_time; want it absent")
		}
	}
}

// TestBreaker_QueryTimeoutNeutral_Closed — a stream of code-159
// rejections must never trip the breaker: CH answering with a typed
// exception is proof it is alive and enforcing a per-query wall-clock
// cap, not an outage.
func TestBreaker_QueryTimeoutNeutral_Closed(t *testing.T) {
	t.Parallel()

	var b breaker
	ctx := context.Background()
	for i := 0; i < breakerThreshold*3; i++ {
		if !b.allow() {
			t.Fatalf("allow() = false after %d timeout rejections; breaker must stay closed", i)
		}
		b.record(ctx, chTimeoutException())
	}
	if got := b.currentState(); got != "closed" {
		t.Errorf("breaker state = %q after %d timeout rejections; want closed", got, breakerThreshold*3)
	}

	// A 159 also RESETS the consecutive-failure count (it is a success):
	// real failures interleaved with timeout rejections never accumulate.
	outage := errors.New("dial tcp: connection refused")
	for i := 0; i < breakerThreshold*2; i++ {
		b.record(ctx, outage)
		b.record(ctx, chTimeoutException())
	}
	if got := b.currentState(); got != "closed" {
		t.Errorf("breaker state = %q with 159s interleaving failures; want closed (159 resets the streak)", got)
	}
}

// TestCursor_MidStreamQueryTimeout — a long-window query can cross
// max_execution_time mid-stream, surfacing through cursor.Err() the same
// way the memory cap does. The cursor must classify it as a typed
// *QueryTimeoutError carrying the budget, not a bare transport error.
func TestCursor_MidStreamQueryTimeout(t *testing.T) {
	t.Parallel()

	cursor := &rowsCursor{
		rows:         &errRows{err: chTimeoutException()},
		queryTimeout: 2 * time.Minute,
	}
	if cursor.Next() {
		t.Fatal("Next() = true; want immediate termination")
	}
	err := cursor.Err()
	if !errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("cursor.Err() = %v; want errors.Is ErrQueryTimeout", err)
	}
	var qt *QueryTimeoutError
	if !errors.As(err, &qt) {
		t.Fatalf("cursor.Err() = %T; want *QueryTimeoutError in the chain", err)
	}
	if qt.Timeout != 2*time.Minute {
		t.Errorf("Timeout = %s; want 2m0s", qt.Timeout)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestChCodeTimeoutExceeded_ExactCode pins the ClickHouse TIMEOUT_EXCEEDED
// error code so a future server-side renumber surfaces loudly here rather
// than silently mis-classifying a timeout as a generic transport failure.
func TestChCodeTimeoutExceeded_ExactCode(t *testing.T) {
	t.Parallel()
	const want = 159
	if chCodeTimeoutExceeded != want {
		t.Errorf("chCodeTimeoutExceeded = %d; want %d", chCodeTimeoutExceeded, want)
	}
}
