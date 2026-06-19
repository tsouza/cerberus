package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/api/resource"
)

// parseByteSize parses a byte-size config value, accepting BOTH the historical
// raw-integer-of-bytes form AND a humanized, Kubernetes-style suffixed form so
// the byte-cap knobs read the same way operators already type memory in a Helm
// values file or a Pod resource request.
//
// Backward compatibility is exact and comes FIRST: a bare base-10 integer (e.g.
// `1073741824`) is parsed with strconv.ParseInt and returned verbatim, so every
// previously-valid value resolves to the identical int64 it always did — no
// float round-trip, no precision loss even past 2^53. Only when the value is
// NOT a bare integer does it fall through to the humanized parser.
//
// The humanized form is the Kubernetes resource.Quantity (BinarySI) grammar,
// which matches the Helm/Pod world byte-for-byte:
//
//   - binary suffixes  Ki / Mi / Gi / Ti / Pi  = 2^10 .. 2^50  (`2Gi` = 2147483648)
//   - decimal suffixes k / K / M / G / T / P    = 10^3 .. 10^15 (`1G`  = 1000000000,
//     `1k` = 1000)
//   - no suffix                                  = bytes (handled by the BWC path)
//
// `0` is preserved with its unset/unlimited semantics — it parses to 0 like any
// other value, and each caller keeps its own "0 = don't set / driver default"
// handling. Negative values and unparseable garbage are rejected with an error
// naming the offending key, preserving the fail-fast contract.
//
// A fractional quantity that does not resolve to a whole number of bytes (e.g.
// `1.5Gi` happens to be whole, but `0.5Ki` = 512 is whole too; truly fractional
// byte counts like `1.0001Ki`) is rejected rather than silently truncated.
func parseByteSize(key, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s: missing value", key)
	}

	// BWC path first: a bare base-10 integer resolves to the exact same int64
	// it always did. ParseInt rejects suffixes, so a humanized value falls
	// through to the Quantity parser below.
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("%s: must be >= 0, got %d", key, n)
		}
		return n, nil
	}

	// Humanized path: Kubernetes resource.Quantity (BinarySI) — the same
	// grammar Helm values and Pod resource requests use. Require at least one
	// digit first: resource.ParseQuantity treats a bare suffix like "Gi" as a
	// zero mantissa (-> 0 bytes), which would silently swallow a typo; demand a
	// number so "Gi" / "Ki" are rejected as the misconfigurations they are.
	if !strings.ContainsAny(raw, "0123456789") {
		return 0, fmt.Errorf("%s: invalid byte size %q: want a non-negative integer (bytes) or a humanized size like 2Gi, 500Mi, 1G, 1k", key, raw)
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid byte size %q: want a non-negative integer (bytes) or a humanized size like 2Gi, 500Mi, 1G, 1k", key, raw)
	}
	bytes, ok := q.AsInt64()
	if !ok {
		return 0, fmt.Errorf("%s: byte size %q does not resolve to a whole number of bytes", key, raw)
	}
	if bytes < 0 {
		return 0, fmt.Errorf("%s: must be >= 0, got %d", key, bytes)
	}
	return bytes, nil
}

// getByteSize resolves key from the viper loader and parses it as a byte size
// via parseByteSize, accepting both the raw-integer (BWC) and humanized
// (`2Gi`, `500Mi`, `1G`) forms. It mirrors getInt64 / getDuration: an empty
// resolved value is rejected (every byte-size key carries a non-empty
// SetDefault), and a malformed value fails fast with an error naming the key.
func getByteSize(v *viper.Viper, key string) (int64, error) {
	return parseByteSize(key, getString(v, key))
}
