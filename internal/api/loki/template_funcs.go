package loki

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	"github.com/dustin/go-humanize"
)

// templateFuncs returns the function map cerberus exposes inside
// `| line_format` and `| label_format` templates, with `__line__` /
// `__timestamp__` bound to the per-row capture closures.
//
// This is a clean-room reimplementation of Loki's
// pkg/logql/log/fmt.go AddLineAndTimestampFunctions (AGPLv3) so the
// binary stays Apache-only. The surface is identical: the Loki-native
// functions below, plus the sprig allow-list (sprig itself is
// Apache/MIT and imported directly), plus the two magic row funcs.
//
// currLine and currTs are per-execution capture closures the caller
// updates before each Execute so the two magic funcs read the right
// row. `__timestamp__` returns a time.Time (time.Unix(0, ns)) so
// `{{ __timestamp__ | date "..." }}` stays at parity.
func templateFuncs(currLine func() string, currTs func() int64) template.FuncMap {
	out := make(template.FuncMap, len(logqlFunctionMap)+2)
	for k, v := range logqlFunctionMap {
		out[k] = v
	}
	out[functionLineName] = func() string { return currLine() }
	out[functionTimestampName] = func() time.Time { return time.Unix(0, currTs()) }
	return out
}

const (
	functionLineName      = "__line__"
	functionTimestampName = "__timestamp__"
)

// sprigAllowList is the exact subset of sprig's generic function map
// Loki exposes in line_format / label_format templates.
var sprigAllowList = []string{
	"b64enc", "b64dec",
	"lower", "upper", "title", "trunc", "substr",
	"contains", "hasPrefix", "hasSuffix",
	"indent", "nindent", "replace", "repeat",
	"trim", "trimAll", "trimSuffix", "trimPrefix",
	"int", "float64",
	"add", "sub", "mul", "div", "mod",
	"addf", "subf", "mulf", "divf",
	"max", "min", "maxf", "minf",
	"ceil", "floor", "round",
	"fromJson", "date", "toDate", "now", "unixEpoch", "default",
}

// logqlFunctionMap is the shared (per-process) function map: the
// Loki-native helpers plus the allow-listed sprig functions. The two
// row-specific funcs (`__line__` / `__timestamp__`) are layered on per
// call in templateFuncs.
var logqlFunctionMap = buildLogqlFunctionMap()

func buildLogqlFunctionMap() template.FuncMap {
	fm := template.FuncMap{
		// Deprecated capitalised aliases (kept for parity).
		"ToLower":    strings.ToLower,
		"ToUpper":    strings.ToUpper,
		"Replace":    strings.Replace,
		"Trim":       strings.Trim,
		"TrimLeft":   strings.TrimLeft,
		"TrimRight":  strings.TrimRight,
		"TrimPrefix": strings.TrimPrefix,
		"TrimSuffix": strings.TrimSuffix,
		"TrimSpace":  strings.TrimSpace,
		// Regex helpers.
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
		"count": func(regexsubstr, s string) (int, error) {
			r, err := regexp.Compile(regexsubstr)
			if err != nil {
				return 0, err
			}
			return len(r.FindAllStringIndex(s, -1)), nil
		},
		// URL helpers.
		"urldecode": url.QueryUnescape,
		"urlencode": url.QueryEscape,
		// Loki-native conversions.
		"bytes":            convertBytes,
		"duration":         convertDuration,
		"duration_seconds": convertDuration,
		"unixEpochMillis":  unixEpochMillis,
		"unixEpochNanos":   unixEpochNanos,
		"toDateInZone":     toDateInZone,
		"unixToTime":       unixToTime,
		"alignLeft":        alignLeft,
		"alignRight":       alignRight,
	}
	sprigFuncs := sprig.GenericFuncMap()
	for _, name := range sprigAllowList {
		if fn, ok := sprigFuncs[name]; ok {
			fm[name] = fn
		}
	}
	return fm
}

func convertBytes(v string) (float64, error) {
	b, err := humanize.ParseBytes(v)
	if err != nil {
		return 0, err
	}
	return float64(b), nil
}

func convertDuration(v string) (float64, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	return d.Seconds(), nil
}

func unixEpochMillis(date time.Time) string {
	return strconv.FormatInt(date.UnixMilli(), 10)
}

func unixEpochNanos(date time.Time) string {
	return strconv.FormatInt(date.UnixNano(), 10)
}

func toDateInZone(format, zone, str string) time.Time {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc, _ = time.LoadLocation("UTC")
	}
	t, _ := time.ParseInLocation(format, str, loc)
	return t
}

// unixToTime parses an integer epoch string, inferring the unit from its
// digit count (days / seconds / millis / micros / nanos), matching Loki.
func unixToTime(epoch string) (time.Time, error) {
	var ct time.Time
	i, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return ct, fmt.Errorf("unable to parse time '%v': %w", epoch, err)
	}
	switch len(epoch) {
	case epochDigitsDays:
		return time.Unix(i*secondsPerDay, 0), nil
	case epochDigitsSeconds:
		return time.Unix(i, 0), nil
	case epochDigitsMillis:
		return time.Unix(0, i*int64(time.Millisecond)), nil
	case epochDigitsMicros:
		return time.Unix(0, i*int64(time.Microsecond)), nil
	case epochDigitsNanos:
		return time.Unix(0, i), nil
	default:
		return ct, fmt.Errorf("unable to parse time '%v': %w", epoch, err)
	}
}

// Digit counts unixToTime uses to infer the epoch unit, plus the
// seconds-per-day multiplier for the day form.
const (
	epochDigitsDays    = 5
	epochDigitsSeconds = 10
	epochDigitsMillis  = 13
	epochDigitsMicros  = 16
	epochDigitsNanos   = 19
	secondsPerDay      = 86400
)

func alignLeft(count int, src string) string {
	runes := []rune(src)
	l := len(runes)
	if count < 0 || count == l {
		return src
	}
	if pad := count - l; pad > 0 {
		return src + strings.Repeat(" ", pad)
	}
	return string(runes[:count])
}

func alignRight(count int, src string) string {
	runes := []rune(src)
	l := len(runes)
	if count < 0 || count == l {
		return src
	}
	if pad := count - l; pad > 0 {
		return strings.Repeat(" ", pad) + src
	}
	return string(runes[l-count:])
}
