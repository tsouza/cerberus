package config

import (
	"strings"
	"testing"
)

// TestParseByteSize_RawIntegerBWC asserts the historical raw-integer-of-bytes
// form parses to the EXACT same int64 it always did — no float round-trip, no
// precision loss even past 2^53 — so every previously-valid value is unchanged.
func TestParseByteSize_RawIntegerBWC(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"0", 0},
		{"1", 1},
		{"1073741824", 1_073_741_824},
		{"536870912", 536_870_912},
		// Past 2^53: the bare-integer fast path must stay byte-exact (a float
		// detour would round this).
		{"9007199254740993", 9_007_199_254_740_993},
		{"  42  ", 42}, // surrounding whitespace trimmed like the rest of the loader
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseByteSize("CERBERUS_TEST", tc.raw)
			if err != nil {
				t.Fatalf("parseByteSize(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseByteSize(%q) = %d; want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseByteSize_Humanized asserts the Kubernetes BinarySI grammar: binary
// suffixes Ki/Mi/Gi/Ti are powers of 1024 and decimal suffixes k/K/M/G/T are
// powers of 1000, matching the Helm / Pod-resource world byte-for-byte.
func TestParseByteSize_Humanized(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"2Gi", 2_147_483_648},
		{"500Mi", 524_288_000},
		{"512Mi", 536_870_912},
		{"1Ki", 1_024},
		{"1Mi", 1_048_576},
		{"1Gi", 1_073_741_824},
		{"1G", 1_000_000_000},
		{"1M", 1_000_000},
		{"1k", 1_000}, // k8s uses lowercase k for decimal-1000 (uppercase K is not an SI suffix)
		{"0Gi", 0},    // humanized zero keeps the unset/unlimited semantics
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseByteSize("CERBERUS_TEST", tc.raw)
			if err != nil {
				t.Fatalf("parseByteSize(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseByteSize(%q) = %d; want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseByteSize_Rejected asserts negatives, fractional byte counts, empty,
// and unparseable garbage all fail fast with an error naming the key.
func TestParseByteSize_Rejected(t *testing.T) {
	for _, raw := range []string{
		"",         // empty
		"-1",       // negative raw integer
		"-1Gi",     // negative humanized
		"1GiB",     // not the k8s grammar (trailing B)
		"1.5",      // fractional bytes
		"1.0001Ki", // does not resolve to a whole byte count
		"garbage",  // unparseable
		"2 Gi",     // internal space
		"Gi",       // no number
		"0x10",     // not base-10
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseByteSize("CERBERUS_TEST_KEY", raw)
			if err == nil {
				t.Fatalf("parseByteSize(%q) accepted; want error", raw)
			}
			if !strings.Contains(err.Error(), "CERBERUS_TEST_KEY") {
				t.Errorf("error %q does not name the key", err)
			}
		})
	}
}

// TestFromEnv_ByteSizeKeys_RoundTrip exercises every byte-size CONFIG key
// end-to-end through FromEnv with both the raw-integer (BWC) and humanized
// forms, confirming each threads to its downstream field.
func TestFromEnv_ByteSizeKeys_RoundTrip(t *testing.T) {
	t.Run("CH_QUERY_MAX_MEMORY", func(t *testing.T) {
		t.Setenv("CERBERUS_CH_QUERY_MAX_MEMORY", "2Gi")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ClickHouse.MaxQueryMemoryBytes != 2_147_483_648 {
			t.Errorf("MaxQueryMemoryBytes = %d; want 2147483648", cfg.ClickHouse.MaxQueryMemoryBytes)
		}
	})

	t.Run("CH_MAX_COMPRESSION_BUFFER", func(t *testing.T) {
		t.Setenv("CERBERUS_CH_MAX_COMPRESSION_BUFFER", "16Mi")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ClickHouse.MaxCompressionBuffer != 16_777_216 {
			t.Errorf("MaxCompressionBuffer = %d; want 16777216", cfg.ClickHouse.MaxCompressionBuffer)
		}
	})

	t.Run("HTTP_MAX_HEADER_BYTES", func(t *testing.T) {
		t.Setenv("CERBERUS_HTTP_MAX_HEADER_BYTES", "1Mi")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.HTTPServer.MaxHeaderBytes != 1_048_576 {
			t.Errorf("MaxHeaderBytes = %d; want 1048576", cfg.HTTPServer.MaxHeaderBytes)
		}
	})

	t.Run("raw-integer BWC still exact for each key", func(t *testing.T) {
		t.Setenv("CERBERUS_CH_QUERY_MAX_MEMORY", "1073741824")
		t.Setenv("CERBERUS_CH_MAX_COMPRESSION_BUFFER", "10485760")
		t.Setenv("CERBERUS_HTTP_MAX_HEADER_BYTES", "1048576")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.ClickHouse.MaxQueryMemoryBytes != 1_073_741_824 {
			t.Errorf("MaxQueryMemoryBytes = %d; want 1073741824", cfg.ClickHouse.MaxQueryMemoryBytes)
		}
		if cfg.ClickHouse.MaxCompressionBuffer != 10_485_760 {
			t.Errorf("MaxCompressionBuffer = %d; want 10485760", cfg.ClickHouse.MaxCompressionBuffer)
		}
		if cfg.HTTPServer.MaxHeaderBytes != 1_048_576 {
			t.Errorf("MaxHeaderBytes = %d; want 1048576", cfg.HTTPServer.MaxHeaderBytes)
		}
	})
}
