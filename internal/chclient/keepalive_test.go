package chclient

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestNew_KeepAliveEnabled_BuildsDialer asserts New succeeds with TCP keepalive
// configured (the production default) and that the resulting dialer dials a TCP
// socket. clickhouse.Open is lazy (no dial), so we exercise dialContext
// directly against a local listener to prove the dialer actually connects with
// keepalive armed — the root-cause restart-recovery fix.
func TestNew_KeepAliveEnabled_BuildsDialer(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Addr:              "localhost:9000",
		KeepAliveEnabled:  true,
		KeepAliveIdle:     10 * time.Second,
		KeepAliveInterval: 5 * time.Second,
		KeepAliveProbes:   3,
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New (keepalive enabled): %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	assertDialerConnects(t, dialContext(5*time.Second, cfg))
}

// TestNew_KeepAliveDisabled_StillDials asserts that with keepalive OFF New
// still succeeds and the dialer still dials (Enable:false) — disabling
// keepalive must not drop DialContext behaviour, only the keepalive policy.
func TestNew_KeepAliveDisabled_StillDials(t *testing.T) {
	t.Parallel()
	cfg := Config{Addr: "localhost:9000", KeepAliveEnabled: false}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New (keepalive disabled): %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	assertDialerConnects(t, dialContext(5*time.Second, cfg))
}

// TestNew_ZeroConfig_Succeeds pins the bare-Config zero-value convention: a
// Config with no fields set (as many tests build) opens cleanly — keepalive is
// simply not armed (Enable:false), matching the documented zero-value sanity.
func TestNew_ZeroConfig_Succeeds(t *testing.T) {
	t.Parallel()
	client, err := New(Config{Addr: "localhost:9000"})
	if err != nil {
		t.Fatalf("New (zero config): %v", err)
	}
	_ = client.Close()
}

// assertDialerConnects spins up a throwaway TCP listener and proves the
// DialContext func actually opens a connection to it (independent of keepalive
// being enabled or not).
func assertDialerConnects(t *testing.T, dial func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	conn, err := dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if conn == nil {
		t.Fatal("dial returned nil conn")
	}
	_ = conn.Close()
}
