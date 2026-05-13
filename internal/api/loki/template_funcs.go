package loki

import (
	"net/url"
	"regexp"
	"strings"
	"text/template"
)

// templateFuncs returns the function map cerberus exposes inside
// `| line_format` and `| label_format` templates.
//
// Cerberus mirrors a subset of Loki's full surface — the funcs that
// surface in real Grafana dashboards. Each name matches Loki's
// signature exactly so a query that works against Loki also works
// against cerberus.
//
// What's exposed:
//
//   - Casing: `lower`, `upper`, `title` (sprig-compatible names) plus
//     the older capitalised aliases `ToLower`, `ToUpper`.
//   - Trim family: `trim`, `trimSpace`, `trimPrefix`, `trimSuffix`,
//     `trimAll`, `trimLeft`, `trimRight`, plus capitalised `Trim*`
//     aliases.
//   - Replace: `replace`, `Replace`.
//   - Regex: `regexReplaceAll`, `regexReplaceAllLiteral`, `count`.
//   - URL: `urldecode`, `urlencode`.
//   - String predicates: `contains`, `hasPrefix`, `hasSuffix`.
//   - Substring: `substr`, `trunc`.
//   - Defaults: `default` (returns its second arg when first is empty).
//   - Concat: `repeat`.
//
// What's NOT exposed (yet) — referenced in upstream Loki docs but the
// real-world use is rare. Template parse fails with a clear
// "function ... not defined" error so the user knows the gap:
//
//   - Time / date: `now`, `unixEpoch`, `unixEpochMillis`, `date`,
//     `toDate`, `toDateInZone`, `unixToTime`.
//   - Bytes / duration conversions: `bytes`, `duration`,
//     `duration_seconds`, `unixEpochNanos`.
//   - Encoding: `b64enc`, `b64dec`, `fromJson`.
//   - Math: `add`, `sub`, `mul`, `div`, `mod`, plus `*f` float variants
//     and `min`/`max`/`ceil`/`floor`/`round`.
//   - Indenting: `indent`, `nindent`.
//   - Numeric conversion: `int`, `float64`.
//   - Alignment: `alignLeft`, `alignRight`.
//
// Add cases on demand; each addition needs a unit test pinning Loki
// parity.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// Casing.
		"lower":   strings.ToLower,
		"upper":   strings.ToUpper,
		"title":   strings.Title, //nolint:staticcheck // Loki ships strings.Title; mirror its behaviour even though Go deprecated it
		"ToLower": strings.ToLower,
		"ToUpper": strings.ToUpper,

		// Trim family. The "trimAll" name is sprig's — same as Loki's
		// signature (cutset string, src string). Note Go stdlib's
		// strings.Trim takes (s, cutset) — different arg order.
		"trim":       strings.TrimSpace,
		"trimSpace":  strings.TrimSpace,
		"trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) },
		"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
		"trimAll":    func(cutset, s string) string { return strings.Trim(s, cutset) },
		"trimLeft":   func(cutset, s string) string { return strings.TrimLeft(s, cutset) },
		"trimRight":  func(cutset, s string) string { return strings.TrimRight(s, cutset) },
		"Trim":       strings.Trim,
		"TrimSpace":  strings.TrimSpace,
		"TrimLeft":   strings.TrimLeft,
		"TrimRight":  strings.TrimRight,
		"TrimPrefix": strings.TrimPrefix,
		"TrimSuffix": strings.TrimSuffix,

		// Replace. Sprig's `replace` is (old, new, src); Loki ships
		// both that and the older capitalised `Replace` which takes
		// (s, old, new, n) — strings.Replace's signature.
		"replace": func(old, replacement, src string) string {
			return strings.ReplaceAll(src, old, replacement)
		},
		"Replace": strings.Replace,

		// Regex. Compile errors surface up as template execution
		// errors — Loki returns the same shape.
		"regexReplaceAll": func(regex, s, repl string) (string, error) {
			r, err := regexp.Compile(regex)
			if err != nil {
				return "", err
			}
			return r.ReplaceAllString(s, repl), nil
		},
		"regexReplaceAllLiteral": func(regex, s, repl string) (string, error) {
			r, err := regexp.Compile(regex)
			if err != nil {
				return "", err
			}
			return r.ReplaceAllLiteralString(s, repl), nil
		},
		"count": func(regex, s string) (int, error) {
			r, err := regexp.Compile(regex)
			if err != nil {
				return 0, err
			}
			return len(r.FindAllStringIndex(s, -1)), nil
		},

		// URL.
		"urldecode": url.QueryUnescape,
		"urlencode": url.QueryEscape,

		// String predicates.
		"contains":  func(substr, s string) bool { return strings.Contains(s, substr) },
		"hasPrefix": func(prefix, s string) bool { return strings.HasPrefix(s, prefix) },
		"hasSuffix": func(suffix, s string) bool { return strings.HasSuffix(s, suffix) },

		// Substring.
		"substr": substr,
		"trunc":  trunc,

		// Defaults.
		"default": func(defaultVal, val string) string {
			if val == "" {
				return defaultVal
			}
			return val
		},

		// Repetition.
		"repeat": func(n int, s string) string { return strings.Repeat(s, n) },
	}
}

// substr matches sprig's signature: substr(start, end, s) returns the
// substring [start, end). Negative indices are clamped to 0. End > len
// is clamped to len. start > end returns the empty string.
func substr(start, end int, s string) string {
	if start < 0 {
		start = 0
	}
	if end > len(s) {
		end = len(s)
	}
	if start >= end {
		return ""
	}
	return s[start:end]
}

// trunc returns the first n bytes of s (or all of s if shorter). If n
// is negative, returns the last |n| bytes — matches sprig's signature.
func trunc(n int, s string) string {
	if n < 0 {
		if -n > len(s) {
			return s
		}
		return s[len(s)+n:]
	}
	if n > len(s) {
		return s
	}
	return s[:n]
}
