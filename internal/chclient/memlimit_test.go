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

// TestApportionMemoryBytes pins the shared divide-and-floor formula both
// Client.querySettings (route A, divisor = DataShardCount alone) and
// internal/solver/executor.go (a K-shard fan-out, divisor = kEff x
// DataShardCount) apply to a configured max_memory_usage cap (cerberus
// issues #3081, #3122).
func TestApportionMemoryBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cap     int64
		divisor int64
		want    int64
	}{
		{"no-op divisor 1", 1 << 30, 1, 1 << 30},
		{"even split", 1 << 30, 2, (1 << 30) / 2},
		{"kEff x DataShardCount split", 1_073_741_824, 6, 178956970},
		{"divisor 0 treated as 1 (no-op)", 1 << 30, 0, 1 << 30},
		{"negative divisor treated as 1 (no-op)", 1 << 30, -3, 1 << 30},
		{"cap smaller than divisor floors to 1, never 0", 5, 10, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ApportionMemoryBytes(c.cap, c.divisor); got != c.want {
				t.Errorf("ApportionMemoryBytes(%d, %d) = %d; want %d", c.cap, c.divisor, got, c.want)
			}
		})
	}
}

// distributedPinCount is the number of settings distributed_query_settings.go
// pins UNCONDITIONALLY on every querySettings() call (cerberus issue #3078:
// skip_unavailable_shards + fallback_to_stale_replicas_for_distributed_queries;
// cerberus issue #3086: load_balancing + load_balancing_first_offset;
// cerberus issue #3118: distributed_product_mode).
// Every exact-entry-count assertion in this file adds this in, so a future
// sixth unconditional pin only needs updating here.
const distributedPinCount = 5

// TestQuerySettings_MaxMemoryUsage — the per-query settings map carries
// max_memory_usage with the configured byte value, and carries ONLY the
// unconditional distributed-query pins (never max_memory_usage) when
// the cap is 0/unset.
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
	if len(settings) != 1+distributedPinCount {
		t.Errorf("settings carries %d entries (%v); want exactly max_memory_usage plus the %d distributed pins",
			len(settings), settings, distributedPinCount)
	}

	unset := &Client{}
	s := unset.querySettings(context.Background())
	if _, ok := s["max_memory_usage"]; ok {
		t.Errorf("querySettings() with cap 0 = %v; want max_memory_usage absent", s)
	}
	if len(s) != distributedPinCount {
		t.Errorf("querySettings() with cap 0 carries %d entries (%v); want exactly the %d unconditional distributed pins",
			len(s), s, distributedPinCount)
	}
}

// TestQuerySettings_MaxMemoryUsage_DataShardApportionment — cerberus issue
// #3122: a Distributed-engine read fans a SINGLE statement this Client
// dispatches ("route A" — no solver K-shard split) out across every one of
// Config.DataShardCount's data-shard nodes, each enforcing max_memory_usage
// against its own local working set. Before this fix, querySettings stamped
// the RAW configured cap unconditionally regardless of DataShardCount — the
// exact gap e2e-datashard-verify.mjs's point-4 assertion caught on a real
// multi-data-shard cluster (dispatch run 34020019580: every route-A
// statement observed max_memory_usage == the unapportioned raw cap). This
// pins the fix: the stamped value must be the cap divided by DataShardCount
// alone (kEff=1 in the solver's own vocabulary, matching
// perShardMemoryBytes' formula for an unsplit statement).
func TestQuerySettings_MaxMemoryUsage_DataShardApportionment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		dataShardCount int64
		want           int64
	}{
		{"DataShardCount unset (0) is a no-op", 0, 1 << 30},
		{"DataShardCount 1 is a no-op", 1, 1 << 30},
		{"DataShardCount 2 halves the cap", 2, (1 << 30) / 2},
		{"DataShardCount 4 quarters the cap", 4, (1 << 30) / 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client := &Client{maxMemory: 1 << 30, dataShardCount: c.dataShardCount}
			settings := client.querySettings(context.Background())
			got, ok := settings["max_memory_usage"]
			if !ok {
				t.Fatalf("settings %v missing max_memory_usage", settings)
			}
			if got != c.want {
				t.Errorf("max_memory_usage = %v; want %d (cap=%d, DataShardCount=%d)",
					got, c.want, int64(1<<30), c.dataShardCount)
			}
		})
	}

	// The 0/unset case must carry EXACTLY max_memory_usage plus the
	// unconditional distributed pins (no accidental extra entries), mirroring
	// TestQuerySettings_MaxMemoryUsage's own exact-count assertion.
	bare := &Client{maxMemory: 1 << 30}
	if got := bare.querySettings(context.Background()); len(got) != 1+distributedPinCount {
		t.Errorf("settings carries %d entries (%v); want exactly max_memory_usage plus the %d distributed pins",
			len(got), got, distributedPinCount)
	}

	// A route-A statement with NO configured cap (MaxQueryMemoryBytes()==0)
	// must never have DataShardCount invent one — 0 stays 0/absent
	// regardless of DataShardCount, matching TestExecute_MemoryApportion_
	// UnconfiguredCapLeftUnapportioned's identical rule on the solver side.
	uncapped := &Client{dataShardCount: 4}
	if s := uncapped.querySettings(context.Background()); s["max_memory_usage"] != nil {
		t.Errorf("querySettings() with cap 0 = %v; want max_memory_usage absent regardless of DataShardCount", s)
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
	if len(marked) != 2+distributedPinCount {
		t.Errorf("marked settings carries %d entries (%v); want exactly the two knobs plus the %d distributed pins",
			len(marked), marked, distributedPinCount)
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
	if len(bare) != 1+distributedPinCount {
		t.Errorf("bare settings carries %d entries (%v); want exactly the experimental knob plus the %d distributed pins",
			len(bare), bare, distributedPinCount)
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

// TestQueryContext_Derivation — queryContext ALWAYS derives a new context,
// with or without a configured memory cap: cerberus issue #3078's two
// distributed-query pins (skip_unavailable_shards +
// fallback_to_stale_replicas_for_distributed_queries + load_balancing +
// load_balancing_first_offset) ride on every data-plane query
// unconditionally, so querySettings is never empty and there is no longer
// an "unconfigured, pass ctx through verbatim" path.
func TestQueryContext_Derivation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	unset := &Client{}
	if got := unset.queryContext(ctx); got == ctx {
		t.Error("queryContext with cap 0 returned ctx verbatim; want a derived ctx carrying the unconditional distributed pins")
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
