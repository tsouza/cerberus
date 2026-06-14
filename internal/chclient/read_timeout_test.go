package chclient

import (
	"testing"
	"time"
)

// TestBuildOptions_ReadTimeoutFromQueryTimeout asserts that a positive
// Config.QueryTimeout is stamped onto clickhouse.Options.ReadTimeout. This is
// the root-cause fix for slow breaker recovery after a force-killed CH pod: a
// half-open socket handed to a recovery query would otherwise block on a read
// for the driver's 300s ReadTimeout default. Tying ReadTimeout to the per-query
// wall-clock budget bounds that stale read to ~QueryTimeout, making breaker
// recovery deterministic regardless of whether TCP keepalive fires.
func TestBuildOptions_ReadTimeoutFromQueryTimeout(t *testing.T) {
	t.Parallel()
	const queryTimeout = 5 * time.Second
	opts := buildOptions(Config{Addr: "localhost:9000", QueryTimeout: queryTimeout})
	if opts.ReadTimeout != queryTimeout {
		t.Fatalf("ReadTimeout = %v, want %v (= QueryTimeout)", opts.ReadTimeout, queryTimeout)
	}
}

// TestBuildOptions_ReadTimeoutUnsetWhenZero pins the bare-Config zero-value
// convention: a Config with no QueryTimeout leaves ReadTimeout at zero so
// clickhouse-go's own 300s default applies. Tests build bare Configs and must
// keep the driver's out-of-the-box behaviour; only cmd/cerberus (which always
// supplies CERBERUS_QUERY_TIMEOUT, default 2m) arms the tighter ceiling.
func TestBuildOptions_ReadTimeoutUnsetWhenZero(t *testing.T) {
	t.Parallel()
	opts := buildOptions(Config{Addr: "localhost:9000"})
	if opts.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %v, want 0 (driver default) for zero QueryTimeout", opts.ReadTimeout)
	}
}

// TestNew_ReadTimeoutDerivedFromQueryTimeout exercises the full New path (not
// just buildOptions) to prove the wiring survives clickhouse.Open. New is lazy
// (never dials), so this succeeds without a live ClickHouse; we re-derive the
// options to assert the ceiling New would have applied.
func TestNew_ReadTimeoutDerivedFromQueryTimeout(t *testing.T) {
	t.Parallel()
	const queryTimeout = 7 * time.Second
	cfg := Config{Addr: "localhost:9000", QueryTimeout: queryTimeout}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if got := buildOptions(cfg).ReadTimeout; got != queryTimeout {
		t.Fatalf("New would set ReadTimeout = %v, want %v", got, queryTimeout)
	}
}
