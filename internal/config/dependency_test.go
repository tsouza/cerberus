package config

import (
	"strings"
	"testing"
)

// TestFromEnv_DependencyRules exercises the cross-setting dependency matrix:
// each case is a COMBINATION of individually-valid values that is incoherent
// together and must be rejected at startup with an error naming the knobs.
func TestFromEnv_DependencyRules(t *testing.T) {
	caPEM, certPEM, keyPEM := genTestPKI(t)
	dir := t.TempDir()
	caFile := writeFile(t, dir, "ca.pem", caPEM)
	certFile := writeFile(t, dir, "cert.pem", certPEM)
	keyFile := writeFile(t, dir, "key.pem", keyPEM)

	cases := []struct {
		name    string
		env     map[string]string
		wantSub []string // substrings the error must contain (knob names)
	}{
		{
			name:    "tls_cert_without_key",
			env:     map[string]string{envCHTLSEnabled: "true", envCHTLSCertFile: certFile},
			wantSub: []string{envCHTLSCertFile, envCHTLSKeyFile},
		},
		{
			name:    "tls_key_without_cert",
			env:     map[string]string{envCHTLSEnabled: "true", envCHTLSKeyFile: keyFile},
			wantSub: []string{envCHTLSCertFile, envCHTLSKeyFile},
		},
		{
			name:    "tls_subknob_without_enable",
			env:     map[string]string{envCHTLSCAFile: caFile},
			wantSub: []string{envCHTLSEnabled, envCHTLSCAFile},
		},
		{
			name:    "tls_servername_without_enable",
			env:     map[string]string{envCHTLSServerName: "ch.internal"},
			wantSub: []string{envCHTLSEnabled},
		},
		{
			name:    "skipverify_with_ca",
			env:     map[string]string{envCHTLSEnabled: "true", envCHTLSSkipVerify: "true", envCHTLSCAFile: caFile},
			wantSub: []string{envCHTLSSkipVerify, envCHTLSCAFile},
		},
		{
			name:    "skipverify_with_servername",
			env:     map[string]string{envCHTLSEnabled: "true", envCHTLSSkipVerify: "true", envCHTLSServerName: "ch.internal"},
			wantSub: []string{envCHTLSSkipVerify, envCHTLSServerName},
		},
		{
			name:    "http_headers_under_native",
			env:     map[string]string{envCHProtocol: "native", envCHHTTPHeaders: "k=v"},
			wantSub: []string{envCHHTTPHeaders, envCHProtocol},
		},
		{
			name:    "http_urlpath_under_native",
			env:     map[string]string{envCHHTTPURLPath: "/q"},
			wantSub: []string{envCHHTTPURLPath, envCHProtocol},
		},
		{
			name:    "http_maxconns_under_native",
			env:     map[string]string{envCHHTTPMaxConns: "8"},
			wantSub: []string{envCHHTTPMaxConns, envCHProtocol},
		},
		{
			name:    "http_proxy_under_native",
			env:     map[string]string{envCHHTTPProxyURL: "http://p:3128"},
			wantSub: []string{envCHHTTPProxyURL, envCHProtocol},
		},
		{
			name:    "compression_level_without_method",
			env:     map[string]string{envCHCompression: "none", envCHCompressionLevel: "5"},
			wantSub: []string{envCHCompressionLevel, envCHCompression},
		},
		{
			name:    "compression_level_lz4_out_of_range",
			env:     map[string]string{envCHCompression: "lz4", envCHCompressionLevel: "13"},
			wantSub: []string{envCHCompressionLevel, envCHCompression},
		},
		{
			name:    "compression_level_zstd_out_of_range",
			env:     map[string]string{envCHCompression: "zstd", envCHCompressionLevel: "23"},
			wantSub: []string{envCHCompressionLevel, envCHCompression},
		},
		{
			name:    "read_timeout_below_query_timeout",
			env:     map[string]string{envCHReadTimeout: "10s", envQueryTimeout: "30s"},
			wantSub: []string{envCHReadTimeout, envQueryTimeout},
		},
		{
			name:    "idle_exceeds_open",
			env:     map[string]string{envCHMaxIdleConns: "20", envCHMaxOpenConns: "10"},
			wantSub: []string{envCHMaxIdleConns, envCHMaxOpenConns},
		},
		{
			name:    "http_header_timeout_exceeds_read_timeout",
			env:     map[string]string{envHTTPReadTimeout: "5s", envHTTPReadHdrTimeout: "10s"},
			wantSub: []string{envHTTPReadHdrTimeout, envHTTPReadTimeout},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAllEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted incoherent combo %v; want error", tc.env)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not name %q", err, sub)
				}
			}
		})
	}
}

// TestFromEnv_DependencyRules_CoherentCombosAccepted confirms the matrix does
// NOT over-reject: combos that ARE coherent must pass.
func TestFromEnv_DependencyRules_CoherentCombosAccepted(t *testing.T) {
	t.Run("read_timeout_equal_query_timeout", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHReadTimeout, "30s")
		t.Setenv(envQueryTimeout, "30s")
		if _, err := FromEnv(); err != nil {
			t.Fatalf("FromEnv rejected read==query timeout: %v", err)
		}
	})
	t.Run("idle_equal_open", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHMaxIdleConns, "10")
		t.Setenv(envCHMaxOpenConns, "10")
		if _, err := FromEnv(); err != nil {
			t.Fatalf("FromEnv rejected idle==open: %v", err)
		}
	})
	t.Run("lower_open_only_keeps_default_idle", func(t *testing.T) {
		// The chaos overlay lowers MAX_OPEN_CONNS to 4 and leaves idle at its
		// default 5. That is coherent (the driver clamps idle to open) and must
		// be accepted — the idle<=open rule only fires on an EXPLICIT idle.
		clearAllEnv(t)
		t.Setenv(envCHMaxOpenConns, "4")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv rejected lowered open with default idle: %v", err)
		}
		if cfg.ClickHouse.MaxOpenConns != 4 || cfg.ClickHouse.MaxIdleConns != 5 {
			t.Errorf("pool = (open %d, idle %d); want (4, 5)", cfg.ClickHouse.MaxOpenConns, cfg.ClickHouse.MaxIdleConns)
		}
	})
	t.Run("http_knobs_under_http", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHProtocol, "http")
		t.Setenv(envCHHTTPURLPath, "/query")
		if _, err := FromEnv(); err != nil {
			t.Fatalf("FromEnv rejected http knobs under http protocol: %v", err)
		}
	})
	t.Run("compression_level_in_range", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envCHCompression, "lz4")
		t.Setenv(envCHCompressionLevel, "9")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv rejected in-range lz4 level: %v", err)
		}
		if cfg.ClickHouse.Compression.Level != 9 {
			t.Errorf("Compression.Level = %d; want 9", cfg.ClickHouse.Compression.Level)
		}
	})
	t.Run("keepalive_subknobs_inert_when_disabled", func(t *testing.T) {
		// Sub-knobs that are inert when keepalive is OFF must not be rejected
		// (they have no runtime effect) — a degenerate idle/interval/count is
		// only validated when keepalive is enabled.
		clearAllEnv(t)
		t.Setenv(envCHKeepAliveEnabled, "false")
		t.Setenv(envCHKeepAliveIdle, "0s")
		t.Setenv(envCHKeepAliveInterval, "0s")
		t.Setenv(envCHKeepAliveCount, "0")
		if _, err := FromEnv(); err != nil {
			t.Fatalf("FromEnv rejected inert keepalive sub-knobs when disabled: %v", err)
		}
	})
}
