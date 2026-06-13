package chclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// chMemLimitException builds the typed driver exception ClickHouse
// raises for MEMORY_LIMIT_EXCEEDED — the same shape the k3d dashboard
// run 27277793810 surfaced mid-stream ("Memory limit (total) exceeded:
// would use 2.12 GiB … maximum: 1.80 GiB").
func chMemLimitException() *clickhouse.Exception {
	return &clickhouse.Exception{
		Code:    241,
		Name:    "MEMORY_LIMIT_EXCEEDED",
		Message: "Memory limit (total) exceeded: would use 2.12 GiB, maximum: 1.80 GiB. OvercommitTracker decision: Query was selected to stop by OvercommitTracker",
	}
}

// TestWrapMemoryLimit_Code241 — a CH exception with code 241 becomes a
// *MemoryLimitError carrying the configured cap, matchable both via
// errors.Is (sentinel) and errors.As (typed exception still reachable).
func TestWrapMemoryLimit_Code241(t *testing.T) {
	t.Parallel()

	const limit = int64(1 << 30)
	got := wrapMemoryLimit(chMemLimitException(), limit)

	var memLimit *MemoryLimitError
	if !errors.As(got, &memLimit) {
		t.Fatalf("wrapMemoryLimit returned %T (%v); want *MemoryLimitError", got, got)
	}
	if memLimit.Limit != limit {
		t.Errorf("Limit = %d; want %d", memLimit.Limit, limit)
	}
	if !errors.Is(got, ErrMemoryLimitExceeded) {
		t.Error("errors.Is(got, ErrMemoryLimitExceeded) = false; want true")
	}
	var ex *clickhouse.Exception
	if !errors.As(got, &ex) || ex.Code != 241 {
		t.Errorf("underlying *clickhouse.Exception not reachable via errors.As (ex=%v)", ex)
	}
	if !strings.Contains(memLimit.Error(), "1073741824 bytes") {
		t.Errorf("Error() = %q; want it to name the configured cap in bytes", memLimit.Error())
	}
}

// TestWrapMemoryLimit_PassThrough — nil, non-exception errors, and
// exceptions with other codes pass through unchanged: classification
// is typed-code-241-only, never string matching.
func TestWrapMemoryLimit_PassThrough(t *testing.T) {
	t.Parallel()

	if got := wrapMemoryLimit(nil, 1); got != nil {
		t.Errorf("wrapMemoryLimit(nil) = %v; want nil", got)
	}

	plain := errors.New("read: connection reset by peer mentioning Memory limit (total) exceeded")
	if got := wrapMemoryLimit(plain, 1); got != plain {
		t.Errorf("plain error was rewritten to %v; want pass-through (no string matching)", got)
	}

	otherCode := &clickhouse.Exception{Code: 60, Name: "UNKNOWN_TABLE", Message: "Table otel.missing does not exist"}
	if got := wrapMemoryLimit(otherCode, 1); !errors.Is(got, error(otherCode)) {
		t.Errorf("code-60 exception was rewritten to %v; want pass-through", got)
	}
	if errors.Is(wrapMemoryLimit(otherCode, 1), ErrMemoryLimitExceeded) {
		t.Error("code-60 exception classified as memory-limit; want only code 241")
	}
}

// TestMemoryLimitError_NoCapConfigured — Limit 0 (no per-query cap;
// the rejection came from a CH server-side limit) renders an honest
// message that does not invent a cap value.
func TestMemoryLimitError_NoCapConfigured(t *testing.T) {
	t.Parallel()

	got := wrapMemoryLimit(chMemLimitException(), 0)
	var memLimit *MemoryLimitError
	if !errors.As(got, &memLimit) {
		t.Fatalf("wrapMemoryLimit returned %T; want *MemoryLimitError", got)
	}
	if memLimit.Limit != 0 {
		t.Errorf("Limit = %d; want 0", memLimit.Limit)
	}
	if strings.Contains(memLimit.Error(), "bytes)") {
		t.Errorf("Error() = %q; must not name a per-query cap when none is configured", memLimit.Error())
	}
	if !strings.Contains(memLimit.Error(), "server-side memory limit") {
		t.Errorf("Error() = %q; want it to name the server-side limit", memLimit.Error())
	}
}

// TestQuerySettings_MaxMemoryUsage — the per-query settings map carries
// max_memory_usage with the configured byte value, and is nil (setting
// not sent at all) when the cap is 0/unset.
func TestQuerySettings_MaxMemoryUsage(t *testing.T) {
	t.Parallel()

	c := &Client{maxMemory: 1 << 30}
	settings := c.querySettings(context.Background())
	if settings == nil {
		t.Fatal("querySettings() = nil with a configured cap; want max_memory_usage set")
	}
	got, ok := settings["max_memory_usage"]
	if !ok {
		t.Fatalf("settings %v missing max_memory_usage", settings)
	}
	if got != int64(1<<30) {
		t.Errorf("max_memory_usage = %v (%T); want %d (int64)", got, got, int64(1<<30))
	}
	if len(settings) != 1 {
		t.Errorf("settings carries %d entries (%v); want exactly max_memory_usage", len(settings), settings)
	}

	unset := &Client{}
	if s := unset.querySettings(context.Background()); s != nil {
		t.Errorf("querySettings() with cap 0 = %v; want nil (setting not sent)", s)
	}
}

// TestQuerySettings_TSGridSetting — the experimental
// timeSeriesRateToGrid setting is added ONLY when ctx is marked by
// WithTSGridSetting, and when it is, it MERGES with max_memory_usage on
// the same map (never clobbers it). Unmarked ctx never carries the knob.
func TestQuerySettings_TSGridSetting(t *testing.T) {
	t.Parallel()

	// Unmarked ctx → no experimental knob even on a configured client.
	c := &Client{maxMemory: 1 << 30}
	plain := c.querySettings(context.Background())
	if _, ok := plain[SettingExperimentalTSGridAggregate]; ok {
		t.Errorf("plain ctx carries %s; want it absent", SettingExperimentalTSGridAggregate)
	}

	// Marked ctx → both knobs present on the one map (the merge, not a
	// clobbering second WithSettings wrap).
	marked := c.querySettings(WithTSGridSetting(context.Background()))
	if marked[SettingExperimentalTSGridAggregate] != 1 {
		t.Errorf("%s = %v; want 1", SettingExperimentalTSGridAggregate, marked[SettingExperimentalTSGridAggregate])
	}
	if marked["max_memory_usage"] != int64(1<<30) {
		t.Errorf("max_memory_usage = %v; want %d (the merge must not drop the cap)", marked["max_memory_usage"], int64(1<<30))
	}
	if len(marked) != 2 {
		t.Errorf("marked settings carries %d entries (%v); want exactly the two knobs", len(marked), marked)
	}

	// Marked ctx with NO memory cap → only the experimental knob, no
	// spurious max_memory_usage=0.
	bare := (&Client{}).querySettings(WithTSGridSetting(context.Background()))
	if bare[SettingExperimentalTSGridAggregate] != 1 {
		t.Errorf("bare client %s = %v; want 1", SettingExperimentalTSGridAggregate, bare[SettingExperimentalTSGridAggregate])
	}
	if _, ok := bare["max_memory_usage"]; ok {
		t.Errorf("bare client carries max_memory_usage; want it absent (cap is 0)")
	}
	if len(bare) != 1 {
		t.Errorf("bare settings carries %d entries (%v); want exactly the experimental knob", len(bare), bare)
	}
}

// TestSettingExperimentalTSGridAggregate_ExactName pins the exact
// ClickHouse setting spelling so a future server-side rename surfaces
// loudly here (chDB does not enforce the gate, so the chdb parity lane
// cannot catch a mis-spelled or omitted setting).
func TestSettingExperimentalTSGridAggregate_ExactName(t *testing.T) {
	t.Parallel()
	// The CANONICAL name (the rename target), not the deprecated
	// `allow_experimental_ts_to_grid_aggregate_function` alias. See the
	// constant's doc comment for the ClickHouse PR #80590 rename history:
	// the canonical name is what every released build that has the function
	// recognises, and what the server's experimental-gate error hint names.
	const want = "allow_experimental_time_series_aggregate_functions"
	if SettingExperimentalTSGridAggregate != want {
		t.Errorf("SettingExperimentalTSGridAggregate = %q; want %q", SettingExperimentalTSGridAggregate, want)
	}
}

// TestQueryContext_Derivation — queryContext derives a new context only
// when a cap is configured; with cap 0 the caller's ctx is returned
// verbatim so the unconfigured path stays allocation-free.
func TestQueryContext_Derivation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	unset := &Client{}
	if got := unset.queryContext(ctx); got != ctx {
		t.Error("queryContext with cap 0 derived a new ctx; want pass-through")
	}
	capped := &Client{maxMemory: 1 << 30}
	if got := capped.queryContext(ctx); got == ctx {
		t.Error("queryContext with a configured cap returned ctx verbatim; want a derived ctx carrying the settings")
	}
}

// TestBreaker_MemoryLimitNeutral_Closed — a stream of code-241
// rejections must never trip the breaker: CH answering with a typed
// exception is proof it is alive and enforcing a per-query cap, not an
// outage. A dashboard refresh firing many over-broad panels at once
// must not 503 unrelated traffic.
func TestBreaker_MemoryLimitNeutral_Closed(t *testing.T) {
	t.Parallel()

	var b breaker
	ctx := context.Background()
	for i := 0; i < breakerThreshold*3; i++ {
		if !b.allow() {
			t.Fatalf("allow() = false after %d memory-limit rejections; breaker must stay closed", i)
		}
		b.record(ctx, chMemLimitException())
	}
	if got := b.currentState(); got != "closed" {
		t.Errorf("breaker state = %q after %d memory-limit rejections; want closed", got, breakerThreshold*3)
	}

	// A 241 also RESETS the consecutive-failure count (it is a
	// success, not merely ignored): real failures interleaved with
	// memory-limit rejections never accumulate to the threshold.
	outage := errors.New("dial tcp: connection refused")
	for i := 0; i < breakerThreshold*2; i++ {
		b.record(ctx, outage)
		b.record(ctx, chMemLimitException())
	}
	if got := b.currentState(); got != "closed" {
		t.Errorf("breaker state = %q with 241s interleaving failures; want closed (241 resets the streak)", got)
	}
}

// TestBreaker_MemoryLimitClosesHalfOpen — a HALF-OPEN probe answered
// with a code-241 rejection closes the circuit: the probe reached a
// live ClickHouse, which is exactly what the probe exists to verify.
func TestBreaker_MemoryLimitClosesHalfOpen(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	b := breaker{now: func() time.Time { return now }}
	ctx := context.Background()

	outage := errors.New("dial tcp: connection refused")
	for i := 0; i < breakerThreshold; i++ {
		b.record(ctx, outage)
	}
	if got := b.currentState(); got != "open" {
		t.Fatalf("breaker state = %q after %d failures; want open", got, breakerThreshold)
	}

	now = now.Add(breakerOpenInterval + time.Second)
	if !b.allow() {
		t.Fatal("allow() = false after the open interval elapsed; want the half-open probe admitted")
	}
	b.record(ctx, chMemLimitException())
	if got := b.currentState(); got != "closed" {
		t.Errorf("breaker state = %q after a memory-limit probe answer; want closed (CH is alive)", got)
	}
}

// errRows is a driver.Rows whose stream terminates immediately with
// the supplied error — the mid-stream abort shape: ClickHouse killed
// the query after streaming began, so rows.Next() returns false and
// rows.Err() carries the exception.
type errRows struct {
	err    error
	closed bool
}

func (r *errRows) Next() bool { return false }
func (r *errRows) Scan(...any) error {
	return errors.New("test mock: Scan unreachable, Next never yields")
}

func (r *errRows) ScanStruct(any) error {
	return errors.New("test mock: ScanStruct unused")
}
func (r *errRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *errRows) Totals(...any) error              { return nil }
func (r *errRows) Columns() []string                { return nil }
func (r *errRows) Err() error                       { return r.err }
func (r *errRows) HasData() bool                    { return false }
func (r *errRows) Close() error {
	r.closed = true
	return nil
}

// TestCursor_MidStreamMemoryLimit — the exact failure shape of k3d run
// 27277793810: ClickHouse aborts a matrix query mid-stream with code
// 241, surfacing through cursor.Err(). The cursor must classify it as
// a *MemoryLimitError carrying the configured cap, not a bare
// transport error.
func TestCursor_MidStreamMemoryLimit(t *testing.T) {
	t.Parallel()

	cursor := &rowsCursor{
		rows:           &errRows{err: chMemLimitException()},
		maxMemoryBytes: 1 << 30,
	}
	if cursor.Next() {
		t.Fatal("Next() = true; want immediate termination")
	}
	err := cursor.Err()
	if !errors.Is(err, ErrMemoryLimitExceeded) {
		t.Fatalf("cursor.Err() = %v; want errors.Is ErrMemoryLimitExceeded", err)
	}
	var memLimit *MemoryLimitError
	if !errors.As(err, &memLimit) {
		t.Fatalf("cursor.Err() = %T; want *MemoryLimitError in the chain", err)
	}
	if memLimit.Limit != 1<<30 {
		t.Errorf("Limit = %d; want %d", memLimit.Limit, int64(1<<30))
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestQueryCursor_OpenTimeMemoryLimit — a 241 raised at query open
// (before any block streams) must come back as the same classified
// rejection AND leave the breaker closed.
func TestQueryCursor_OpenTimeMemoryLimit(t *testing.T) {
	t.Parallel()

	conn := newFlakyConn(chMemLimitException())
	conn.setFail(true)
	c := newWithConn(conn)

	for i := 0; i < breakerThreshold*2; i++ {
		_, err := c.QueryCursor(context.Background(), "SELECT 1")
		if err == nil {
			t.Fatal("QueryCursor: want the memory-limit rejection, got nil")
		}
		if !errors.Is(err, ErrMemoryLimitExceeded) {
			t.Fatalf("QueryCursor err = %v; want errors.Is ErrMemoryLimitExceeded", err)
		}
	}
	if got := c.br.currentState(); got != "closed" {
		t.Errorf("breaker state = %q after repeated open-time 241s; want closed", got)
	}
}
