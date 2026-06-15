package chclient

import (
	"crypto/tls"
	"net/url"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// TestBuildOptions_MultiHost confirms a multi-element Addrs is the
// authoritative Addr slice handed to the driver, and a single scalar Addr is
// used when Addrs is empty.
func TestBuildOptions_MultiHost(t *testing.T) {
	t.Parallel()
	t.Run("multi", func(t *testing.T) {
		opts := buildOptions(Config{Addr: "a:9000", Addrs: []string{"a:9000", "b:9000", "c:9000"}})
		if len(opts.Addr) != 3 {
			t.Fatalf("Addr = %v; want 3 hosts", opts.Addr)
		}
	})
	t.Run("single", func(t *testing.T) {
		opts := buildOptions(Config{Addr: "a:9000"})
		if len(opts.Addr) != 1 || opts.Addr[0] != "a:9000" {
			t.Fatalf("Addr = %v; want [a:9000]", opts.Addr)
		}
	})
}

// TestBuildOptions_ProtocolStrategyCompression maps the enum + compression
// fields onto the driver options.
func TestBuildOptions_ProtocolStrategyCompression(t *testing.T) {
	t.Parallel()
	compr := &clickhouse.Compression{Method: clickhouse.CompressionZSTD, Level: 5}
	opts := buildOptions(Config{
		Addr:             "a:9000",
		Protocol:         clickhouse.HTTP,
		ConnOpenStrategy: clickhouse.ConnOpenRoundRobin,
		Compression:      compr,
	})
	if opts.Protocol != clickhouse.HTTP {
		t.Errorf("Protocol = %v; want HTTP", opts.Protocol)
	}
	if opts.ConnOpenStrategy != clickhouse.ConnOpenRoundRobin {
		t.Errorf("ConnOpenStrategy = %v; want RoundRobin", opts.ConnOpenStrategy)
	}
	if opts.Compression != compr {
		t.Errorf("Compression = %v; want the configured pointer", opts.Compression)
	}
}

// TestBuildOptions_TLSSetIffEnabled confirms a non-nil Config.TLS is wired and
// a nil one leaves opts.TLS nil (plaintext) — the "TLS set iff enabled" rule.
func TestBuildOptions_TLSSetIffEnabled(t *testing.T) {
	t.Parallel()
	t.Run("enabled", func(t *testing.T) {
		tlsCfg := &tls.Config{ServerName: "ch.internal", MinVersion: tls.VersionTLS12}
		opts := buildOptions(Config{Addr: "a:9000", TLS: tlsCfg})
		if opts.TLS != tlsCfg {
			t.Fatalf("TLS = %v; want the configured pointer", opts.TLS)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		opts := buildOptions(Config{Addr: "a:9000"})
		if opts.TLS != nil {
			t.Fatalf("TLS = %v; want nil (plaintext)", opts.TLS)
		}
	})
}

// TestBuildOptions_Buffers maps the buffer + free-buf + debug knobs.
func TestBuildOptions_Buffers(t *testing.T) {
	t.Parallel()
	opts := buildOptions(Config{
		Addr:                 "a:9000",
		BlockBufferSize:      8,
		MaxCompressionBuffer: 20 << 20,
		FreeBufOnConnRelease: true,
		Debug:                true,
	})
	if opts.BlockBufferSize != 8 {
		t.Errorf("BlockBufferSize = %d; want 8", opts.BlockBufferSize)
	}
	if opts.MaxCompressionBuffer != 20<<20 {
		t.Errorf("MaxCompressionBuffer = %d; want %d", opts.MaxCompressionBuffer, 20<<20)
	}
	if !opts.FreeBufOnConnRelease {
		t.Error("FreeBufOnConnRelease = false; want true")
	}
	if !opts.Debug { //nolint:staticcheck // SA1019: asserting the exposed CERBERUS_CH_DEBUG knob
		t.Error("Debug = false; want true")
	}
}

// TestBuildOptions_HTTPKnobs maps the HTTP-protocol-only fields.
func TestBuildOptions_HTTPKnobs(t *testing.T) {
	t.Parallel()
	proxy, _ := url.Parse("http://proxy:3128")
	opts := buildOptions(Config{
		Addr:                "a:8123",
		Protocol:            clickhouse.HTTP,
		HTTPHeaders:         map[string]string{"X-Scope-OrgID": "t"},
		HTTPURLPath:         "/query",
		HTTPMaxConnsPerHost: 16,
		HTTPProxyURL:        proxy,
	})
	if opts.HttpHeaders["X-Scope-OrgID"] != "t" {
		t.Errorf("HttpHeaders = %v; want X-Scope-OrgID=t", opts.HttpHeaders)
	}
	if opts.HttpUrlPath != "/query" {
		t.Errorf("HttpUrlPath = %q; want /query", opts.HttpUrlPath)
	}
	if opts.HttpMaxConnsPerHost != 16 {
		t.Errorf("HttpMaxConnsPerHost = %d; want 16", opts.HttpMaxConnsPerHost)
	}
	if opts.HTTPProxyURL != proxy {
		t.Errorf("HTTPProxyURL = %v; want the configured pointer", opts.HTTPProxyURL)
	}
}

// TestBuildOptions_ReadTimeoutKnobBeatsDerived confirms an explicit
// Config.ReadTimeout overrides the QueryTimeout-derived ceiling.
func TestBuildOptions_ReadTimeoutKnobBeatsDerived(t *testing.T) {
	t.Parallel()
	opts := buildOptions(Config{
		Addr:         "a:9000",
		ReadTimeout:  90 * time.Second,
		QueryTimeout: 30 * time.Second,
	})
	if opts.ReadTimeout != 90*time.Second {
		t.Fatalf("ReadTimeout = %v; want 90s (explicit knob beats derived 30s)", opts.ReadTimeout)
	}
}

// TestBuildOptions_ReadTimeoutDerivedWhenKnobZero confirms a zero
// Config.ReadTimeout falls back to the QueryTimeout derivation.
func TestBuildOptions_ReadTimeoutDerivedWhenKnobZero(t *testing.T) {
	t.Parallel()
	opts := buildOptions(Config{Addr: "a:9000", QueryTimeout: 30 * time.Second})
	if opts.ReadTimeout != 30*time.Second {
		t.Fatalf("ReadTimeout = %v; want 30s (derived from QueryTimeout)", opts.ReadTimeout)
	}
}
