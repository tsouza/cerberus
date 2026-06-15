package config

import (
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// TestFromEnv_CHConnection_Defaults pins that with none of the full-surface
// connection knobs set, the resolved chclient.Config is byte-identical to the
// pre-knob connection: native protocol, in-order strategy, no TLS, no
// compression, single addr, zero buffers.
func TestFromEnv_CHConnection_Defaults(t *testing.T) {
	clearAllEnv(t)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	cc := cfg.ClickHouse
	if cc.Addr != "localhost:9000" {
		t.Errorf("Addr = %q; want localhost:9000", cc.Addr)
	}
	if cc.Addrs != nil {
		t.Errorf("Addrs = %v; want nil (single-host path)", cc.Addrs)
	}
	if cc.Protocol != clickhouse.Native {
		t.Errorf("Protocol = %v; want Native", cc.Protocol)
	}
	if cc.ConnOpenStrategy != clickhouse.ConnOpenInOrder {
		t.Errorf("ConnOpenStrategy = %v; want InOrder", cc.ConnOpenStrategy)
	}
	if cc.TLS != nil {
		t.Errorf("TLS = %v; want nil (plaintext)", cc.TLS)
	}
	if cc.Compression != nil {
		t.Errorf("Compression = %v; want nil (uncompressed)", cc.Compression)
	}
	if cc.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v; want 0 (derived from QueryTimeout)", cc.ReadTimeout)
	}
	if cc.BlockBufferSize != 0 || cc.MaxCompressionBuffer != 0 {
		t.Errorf("buffers = (%d,%d); want (0,0)", cc.BlockBufferSize, cc.MaxCompressionBuffer)
	}
	if cc.FreeBufOnConnRelease || cc.Debug {
		t.Errorf("FreeBufOnConnRelease/Debug = (%v,%v); want both false", cc.FreeBufOnConnRelease, cc.Debug)
	}
}

// TestFromEnv_CHMultiHost confirms a comma-separated CERBERUS_CH_ADDR resolves
// to a multi-element Addrs slice (trimmed) with Addr = the first host.
func TestFromEnv_CHMultiHost(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envCHAddr, " ch-1:9000 , ch-2:9000 ,ch-3:9000")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	want := []string{"ch-1:9000", "ch-2:9000", "ch-3:9000"}
	if cfg.ClickHouse.Addr != "ch-1:9000" {
		t.Errorf("Addr = %q; want ch-1:9000 (first host)", cfg.ClickHouse.Addr)
	}
	if len(cfg.ClickHouse.Addrs) != len(want) {
		t.Fatalf("Addrs = %v; want %v", cfg.ClickHouse.Addrs, want)
	}
	for i := range want {
		if cfg.ClickHouse.Addrs[i] != want[i] {
			t.Errorf("Addrs[%d] = %q; want %q", i, cfg.ClickHouse.Addrs[i], want[i])
		}
	}
}

// TestFromEnv_CHEnums exercises the protocol / strategy / compression enum
// overrides and rejection of unknown values.
func TestFromEnv_CHEnums(t *testing.T) {
	t.Run("protocol_http", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHProtocol, "http")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ClickHouse.Protocol != clickhouse.HTTP {
			t.Errorf("Protocol = %v; want HTTP", cfg.ClickHouse.Protocol)
		}
	})
	t.Run("strategy_round_robin", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHConnOpenStrategy, "round_robin")
		t.Setenv(envCHAddr, "a:9000,b:9000")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ClickHouse.ConnOpenStrategy != clickhouse.ConnOpenRoundRobin {
			t.Errorf("ConnOpenStrategy = %v; want RoundRobin", cfg.ClickHouse.ConnOpenStrategy)
		}
	})
	t.Run("compression_lz4", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHCompression, "lz4")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ClickHouse.Compression == nil || cfg.ClickHouse.Compression.Method != clickhouse.CompressionLZ4 {
			t.Errorf("Compression = %v; want lz4", cfg.ClickHouse.Compression)
		}
	})
	invalid := []struct{ env, val string }{
		{envCHProtocol, "grpc"},
		{envCHConnOpenStrategy, "random_pick"},
		{envCHCompression, "snappy"},
	}
	for _, tc := range invalid {
		t.Run("invalid_"+tc.env+"="+tc.val, func(t *testing.T) {
			clearAllEnv(t)
			t.Setenv(tc.env, tc.val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %s=%q; want error", tc.env, tc.val)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

// TestFromEnv_CHBuffers covers the block buffer / max-compression-buffer
// override + range validation.
func TestFromEnv_CHBuffers(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHBlockBufferSize, "8")
		t.Setenv(envCHMaxComprBuffer, "20971520")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ClickHouse.BlockBufferSize != 8 {
			t.Errorf("BlockBufferSize = %d; want 8", cfg.ClickHouse.BlockBufferSize)
		}
		if cfg.ClickHouse.MaxCompressionBuffer != 20971520 {
			t.Errorf("MaxCompressionBuffer = %d; want 20971520", cfg.ClickHouse.MaxCompressionBuffer)
		}
	})
	invalid := []struct{ env, val string }{
		{envCHBlockBufferSize, "256"}, // > 255
		{envCHBlockBufferSize, "-1"},
		{envCHBlockBufferSize, "nope"},
		{envCHMaxComprBuffer, "-1"},
		{envCHMaxComprBuffer, "huge"},
	}
	for _, tc := range invalid {
		t.Run("invalid_"+tc.env+"="+tc.val, func(t *testing.T) {
			clearAllEnv(t)
			t.Setenv(tc.env, tc.val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %s=%q; want error", tc.env, tc.val)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

// TestFromEnv_CHReadTimeout confirms the first-class read-timeout knob
// overrides the QueryTimeout derivation and is validated as non-negative.
func TestFromEnv_CHReadTimeout(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envCHReadTimeout, "90s")
	t.Setenv(envQueryTimeout, "30s")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.ReadTimeout != 90*time.Second {
		t.Errorf("ReadTimeout = %v; want 90s (first-class knob)", cfg.ClickHouse.ReadTimeout)
	}
}

// TestFromEnv_CHTLS_mTLS builds a real cert/key/CA on disk and confirms the
// *tls.Config carries the client certificate, the CA pool, and ServerName.
func TestFromEnv_CHTLS_mTLS(t *testing.T) {
	clearAllEnv(t)
	caPEM, certPEM, keyPEM := genTestPKI(t)
	dir := t.TempDir()
	caFile := writeFile(t, dir, "ca.pem", caPEM)
	certFile := writeFile(t, dir, "cert.pem", certPEM)
	keyFile := writeFile(t, dir, "key.pem", keyPEM)

	t.Setenv(envCHTLSEnabled, "true")
	t.Setenv(envCHTLSCAFile, caFile)
	t.Setenv(envCHTLSCertFile, certFile)
	t.Setenv(envCHTLSKeyFile, keyFile)
	t.Setenv(envCHTLSServerName, "clickhouse.internal")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	tlsCfg := cfg.ClickHouse.TLS
	if tlsCfg == nil {
		t.Fatal("TLS = nil; want non-nil when enabled")
	}
	if tlsCfg.ServerName != "clickhouse.internal" {
		t.Errorf("ServerName = %q; want clickhouse.internal", tlsCfg.ServerName)
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("Certificates = %d; want 1 (mTLS client cert)", len(tlsCfg.Certificates))
	}
	if tlsCfg.RootCAs == nil {
		t.Error("RootCAs = nil; want the configured CA pool")
	}
	if tlsCfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true; want false")
	}
}

// TestFromEnv_CHTLS_SkipVerify confirms a bare enable + skip-verify yields a
// permissive *tls.Config (no CA / serverName).
func TestFromEnv_CHTLS_SkipVerify(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envCHTLSEnabled, "true")
	t.Setenv(envCHTLSSkipVerify, "true")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.TLS == nil || !cfg.ClickHouse.TLS.InsecureSkipVerify {
		t.Fatalf("TLS = %v; want InsecureSkipVerify=true", cfg.ClickHouse.TLS)
	}
}

// TestFromEnv_CHTLS_BadCAFile confirms a missing CA path fails fast naming the
// env var.
func TestFromEnv_CHTLS_BadCAFile(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envCHTLSEnabled, "true")
	t.Setenv(envCHTLSCAFile, "/nonexistent/ca.pem")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv accepted a missing CA file; want error")
	}
	if !strings.Contains(err.Error(), envCHTLSCAFile) {
		t.Errorf("error %q does not name %s", err, envCHTLSCAFile)
	}
}

// TestFromEnv_CHHTTPKnobs confirms the HTTP-protocol knobs thread through when
// Protocol=http.
func TestFromEnv_CHHTTPKnobs(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envCHProtocol, "http")
	t.Setenv(envCHHTTPHeaders, "X-Scope-OrgID=tenant-a,X-Trace=1")
	t.Setenv(envCHHTTPURLPath, "/query")
	t.Setenv(envCHHTTPMaxConns, "16")
	t.Setenv(envCHHTTPProxyURL, "http://proxy.internal:3128")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	cc := cfg.ClickHouse
	if cc.HTTPHeaders["X-Scope-OrgID"] != "tenant-a" {
		t.Errorf("HTTPHeaders = %v; want X-Scope-OrgID=tenant-a", cc.HTTPHeaders)
	}
	if cc.HTTPURLPath != "/query" {
		t.Errorf("HTTPURLPath = %q; want /query", cc.HTTPURLPath)
	}
	if cc.HTTPMaxConnsPerHost != 16 {
		t.Errorf("HTTPMaxConnsPerHost = %d; want 16", cc.HTTPMaxConnsPerHost)
	}
	if cc.HTTPProxyURL == nil || cc.HTTPProxyURL.Host != "proxy.internal:3128" {
		t.Errorf("HTTPProxyURL = %v; want proxy.internal:3128", cc.HTTPProxyURL)
	}
}

// TestFromEnv_CHHTTPProxy_Malformed rejects a bad proxy URL.
func TestFromEnv_CHHTTPProxy_Malformed(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envCHProtocol, "http")
	t.Setenv(envCHHTTPProxyURL, "://nohost")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv accepted a malformed proxy URL; want error")
	}
	if !strings.Contains(err.Error(), envCHHTTPProxyURL) {
		t.Errorf("error %q does not name %s", err, envCHHTTPProxyURL)
	}
}
