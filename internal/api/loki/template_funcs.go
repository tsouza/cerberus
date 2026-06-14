package loki

import (
	"text/template"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
)

// templateFuncs returns the function map cerberus exposes inside
// `| line_format` and `| label_format` templates.
//
// Cerberus consumes upstream Loki's funcmap VERBATIM via
// loglib.AddLineAndTimestampFunctions (pkg/logql/log/fmt.go) so the
// surface is identical by construction — there is no hand-maintained
// subset to drift out of sync. The set is the union of:
//
//   - ~40 sprig funcs (sprig.GenericFuncMap filtered to Loki's
//     allow-list): b64enc / b64dec, lower / upper / title, trunc /
//     substr, contains / hasPrefix / hasSuffix, indent / nindent,
//     replace / repeat, trim / trimAll / trimPrefix / trimSuffix,
//     int / float64, add / sub / mul / div / mod (+ the `*f` float
//     variants), max / min / maxf / minf, ceil / floor / round,
//     fromJson, date / toDate / now / unixEpoch, and the variadic
//     `default`.
//   - The Loki-native funcs: bytes, duration, duration_seconds,
//     unixEpochMillis, unixEpochNanos, toDateInZone, unixToTime,
//     alignLeft, alignRight, plus the deprecated capitalised aliases
//     (ToLower / ToUpper / Replace / Trim* ) and the regex helpers
//     (regexReplaceAll / regexReplaceAllLiteral / count) and URL
//     helpers (urldecode / urlencode).
//   - `__line__` — the current line, supplied by the caller's closure.
//   - `__timestamp__` — the current sample timestamp as a time.Time
//     (time.Unix(0, ns)), supplied by the caller's closure. Returning a
//     time.Time (not a string) keeps `{{ __timestamp__ | date "..." }}`
//     at Loki parity.
//
// currLine and currTs are per-execution capture closures the caller
// updates before each Execute so the two magic funcs read the right
// row. They mirror upstream's AddLineAndTimestampFunctions(currLine,
// currTimestamp) contract exactly.
func templateFuncs(currLine func() string, currTs func() int64) template.FuncMap {
	return template.FuncMap(loglib.AddLineAndTimestampFunctions(currLine, currTs))
}
