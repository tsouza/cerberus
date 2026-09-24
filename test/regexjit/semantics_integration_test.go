//go:build integration

package regexjit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chclient"
)

// semanticBuilds are the builds the semantic corpus runs against: the first
// patch releases of the two lines that carry the regular-expression compiler
// in this repository's pinned set, and the supported floor, which has no
// compiler and pins that the emitted patterns answer like the reference
// engines there too.
var semanticBuilds = []string{
	"clickhouse/clickhouse-server:26.7.13.12-alpine",
	"clickhouse/clickhouse-server:26.8.10.6-alpine",
	"clickhouse/clickhouse-server:24.8-alpine",
}

// Seed identity. Every probe row sits inside one five-minute window ending
// at probeEnd, so an instant query at probeEnd and a log query over the
// window see all of them.
const (
	probeMetric      = "rjit_probe"
	bytesMetric      = "rjit_bytes"
	probeService     = "rjit"
	bytesService     = "rjitbytes"
	unwrapService    = "rjitunwrap"
	probeLabel       = "v"
	indexLabel       = "i"
	dstLabel         = "dst"
	probeSampleAge   = 30 * time.Second
	probeWindow      = 5 * time.Minute
	probeLineLimit   = 1000
	probeRangeStep   = time.Minute
	probeMetricValue = 1
)

var probeEnd = time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

// probeValues are the label values and log lines the corpus matches
// against: anchors' edge cases, newlines on either side, carriage returns
// and vertical tabs, multi-byte UTF-8 (including the two non-ASCII code
// points RE2 case-folds onto ASCII letters, U+212A KELVIN SIGN and U+017F
// LATIN SMALL LETTER LONG S), and the empty string. Every value is valid
// UTF-8: OTLP carries strings as UTF-8, which is the input domain the
// compiled and interpreted engines are required to agree on.
var probeValues = []string{
	"", "a", "api", "api-7", "api\n", "\napi", "api\nx", "x\napi", "API-12",
	"café", "été", "ÉTÉ", "日本-9", "😀x", "host-", "host-1", "host-\n2",
	"ab\r", "  lead", "123", "a-b", "a_b", "ab", "xyz", "-7", "web-3", "svc-1",
	"SVC-1", "\u212a", "\u212aelvin", "kelvin", "\u017fvc", "\v", "a\vb",
	"\u0085", "\u00a0", "a.b", "a\nb", "err: timeout after 5ms",
	"ERROR x", "status=503 path=/api", "GET /api/v1 200",
}

// invalidUTF8Values are byte strings no OTLP pipeline produces but a direct
// INSERT can. The compiled engine matches bytes; RE2 decodes UTF-8 and never
// matches an invalid byte with `.` or a negated class. Go's regexp — the
// reference engines' — reads an invalid byte as U+FFFD, which both match.
var invalidUTF8Values = []string{"\xff", "a\xffb", "api\xff", "\x85", "caf\xe9"}

// matcherCase is a pattern the corpus lowers as a label matcher (anchored)
// and as a line filter (unanchored). The two flags record whether ClickHouse
// compiles the emitted form; they are observations of the pinned builds.
type matcherCase struct {
	pattern       string
	anchoredJIT   bool
	lineFilterJIT bool
}

var matcherCases = []matcherCase{
	{pattern: `api.*`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `api-[0-9]+`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `.*`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `.+`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: ``, anchoredJIT: true, lineFilterJIT: false},
	{pattern: `(?i)api.*`, anchoredJIT: false, lineFilterJIT: true},
	{pattern: `(?i)kelvin`, anchoredJIT: false, lineFilterJIT: false},
	{pattern: `api|web.*`, anchoredJIT: false, lineFilterJIT: false},
	{pattern: `a.b`, anchoredJIT: false, lineFilterJIT: false},
	{pattern: `[^-]+`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `\w+-\d+`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `host-(.*)`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `(\w+)(-(\d+))?`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `caf.`, anchoredJIT: false, lineFilterJIT: false},
	{pattern: `\S+`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `\s*lead`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `\s`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `(?:x(y))?api`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `.*\n.*`, anchoredJIT: false, lineFilterJIT: false},
	{pattern: `(?s:.*)`, anchoredJIT: false, lineFilterJIT: false},
	{pattern: `^api`, anchoredJIT: false, lineFilterJIT: true},
	{pattern: `api$`, anchoredJIT: false, lineFilterJIT: true},
	{pattern: `timeout after \d+ms`, anchoredJIT: true, lineFilterJIT: true},
	{pattern: `status=5..`, anchoredJIT: false, lineFilterJIT: false},
	{pattern: `api`, anchoredJIT: true, lineFilterJIT: false},
	{pattern: `\d+`, anchoredJIT: true, lineFilterJIT: true},
}

// labelReplaceCase is one label_replace(rjit_probe, "dst", repl, "v", regex).
type labelReplaceCase struct {
	regex string
	repl  string
	jit   bool
}

var labelReplaceCases = []labelReplaceCase{
	{regex: `host-(.*)`, repl: `svc-$1`, jit: true},
	{regex: `(.*)`, repl: `$1`, jit: true},
	{regex: `(.*)`, repl: `value-$1`, jit: true},
	{regex: `(\w+)(-(\d+))?`, repl: `[$1|$2|$3]`, jit: true},
	{regex: `([a-z]*)-?([0-9]*)`, repl: `$2$1`, jit: true},
	{regex: `.*`, repl: `all`, jit: true},
	{regex: `(?P<name>\w+)-\d+`, repl: `${name}!`, jit: true},
	{regex: `(.*)-[0-9]+`, repl: `$1`, jit: true},
	{regex: ``, repl: `none`, jit: true},
	{regex: `api|web-(\d)`, repl: `x$1`, jit: false},
	{regex: `caf(.)`, repl: `$1`, jit: false},
	{regex: `(?i)(k)elvin`, repl: `$1`, jit: false},
	{regex: `a)|(b`, repl: `[$1]`, jit: false},
}

// Unwrap probes: logfmt lines whose duration and byte fields exercise the
// regular expressions cerberus's duration() and bytes() conversions emit.
var (
	unwrapDurations = []string{"12ms", "1.5s", "3h2m", "-7s", ".5s", "1.s", "0", "-0", "bogus", "5", "+2m", "1h1h"}
	unwrapBytes     = []string{"10KiB", "7 MB", "1.5GB", "42", "bogus", "3 kb", "1e3", "12 B", ""}
)

// logQLOnlyEqualityCases are LogQL stages whose regular expressions have no
// reference engine to diff against here; they are held to compiled/
// interpreted equality alone.
var logQLOnlyEqualityCases = []struct {
	name  string
	query string
}{
	{"regexp-parser", `{service_name="` + probeService + `"} | regexp "(?P<word>\\w+)-(?P<num>\\d+)?"`},
	{"label-format-regexReplaceAll", `{service_name="` + probeService + `"} | label_format out="{{ regexReplaceAll \"(\\\\w+)-(\\\\d+)\" .v \"${2}:${1}\" }}"`},
	{"line-format-regexReplaceAllLiteral", `{service_name="` + probeService + `"} | line_format "{{ regexReplaceAllLiteral \"[0-9]+\" __line__ \"#\" }}"`},
}

// TestRegexJIT_EmittedShapesMatchReference drives the corpus through the
// production handlers on every semantic build.
func TestRegexJIT_EmittedShapesMatchReference(t *testing.T) {
	for _, image := range semanticBuilds {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			s := startServer(ctx, t, image)
			seedSemanticProbe(ctx, t, s)
			h := newHandlers(s.client(t))

			modes := []jitMode{jitDefault}
			if s.regexpJIT {
				modes = []jitMode{jitOff, jitNow, jitDefault}
			}
			r := &runner{s: s, h: h, modes: modes, minCount: s.defaultMinCount(ctx, t)}

			for _, c := range matcherCases {
				c := c
				q := fmt.Sprintf(`%s{%s=~%s}`, probeMetric, probeLabel, strconv.Quote(c.pattern))
				r.check(ctx, t, "prom-match/"+c.pattern, c.anchoredJIT, func(ctx context.Context) []byte {
					return h.promInstant(ctx, t, q, probeEnd)
				}, func(body []byte) { assertSelected(t, body, probeValues, c.pattern, labels.MatchRegexp) })

				nq := fmt.Sprintf(`%s{%s!~%s}`, probeMetric, probeLabel, strconv.Quote(c.pattern))
				r.check(ctx, t, "prom-notmatch/"+c.pattern, c.anchoredJIT, func(ctx context.Context) []byte {
					return h.promInstant(ctx, t, nq, probeEnd)
				}, func(body []byte) { assertSelected(t, body, probeValues, c.pattern, labels.MatchNotRegexp) })

				sq := fmt.Sprintf(`{service_name=%q, %s=~%s}`, probeService, probeLabel, strconv.Quote(c.pattern))
				r.check(ctx, t, "loki-stream-match/"+c.pattern, c.anchoredJIT, func(ctx context.Context) []byte {
					return h.lokiRange(ctx, t, sq, probeEnd.Add(-probeWindow), probeEnd, probeRangeStep, probeLineLimit)
				}, func(body []byte) { assertStreamSelected(t, body, c.pattern) })

				lq := fmt.Sprintf(`{service_name=%q} |~ %s`, probeService, strconv.Quote(c.pattern))
				r.check(ctx, t, "loki-line-filter/"+c.pattern, c.lineFilterJIT, func(ctx context.Context) []byte {
					return h.lokiRange(ctx, t, lq, probeEnd.Add(-probeWindow), probeEnd, probeRangeStep, probeLineLimit)
				}, func(body []byte) { assertLines(t, body, probeValues, c.pattern, false) })

				nlq := fmt.Sprintf(`{service_name=%q} !~ %s`, probeService, strconv.Quote(c.pattern))
				r.check(ctx, t, "loki-line-notfilter/"+c.pattern, c.lineFilterJIT, func(ctx context.Context) []byte {
					return h.lokiRange(ctx, t, nlq, probeEnd.Add(-probeWindow), probeEnd, probeRangeStep, probeLineLimit)
				}, func(body []byte) { assertLines(t, body, probeValues, c.pattern, true) })
			}

			for _, c := range labelReplaceCases {
				c := c
				q := fmt.Sprintf(`label_replace(%s, %q, %s, %q, %s)`, probeMetric, dstLabel, strconv.Quote(c.repl), probeLabel, strconv.Quote(c.regex))
				r.check(ctx, t, "label_replace/"+c.regex+"/"+c.repl, c.jit, func(ctx context.Context) []byte {
					return h.promInstant(ctx, t, q, probeEnd)
				}, func(body []byte) { assertLabelReplace(t, body, probeValues, c.regex, c.repl) })

				rq := fmt.Sprintf(`label_replace(rate(%s[%s]), %q, %s, %q, %s)`, probeMetric, probeWindow, dstLabel, strconv.Quote(c.repl), probeLabel, strconv.Quote(c.regex))
				r.check(ctx, t, "label_replace-range/"+c.regex+"/"+c.repl, c.jit, func(ctx context.Context) []byte {
					return h.promRange(ctx, t, rq, probeEnd.Add(-probeWindow), probeEnd, probeRangeStep)
				}, nil)
			}

			durQ := fmt.Sprintf(`sum by (%s) (sum_over_time({service_name=%q} | logfmt | unwrap duration(d) | __error__="" [%s]))`, indexLabel, unwrapService, probeWindow)
			r.check(ctx, t, "unwrap-duration", true, func(ctx context.Context) []byte {
				return h.lokiRange(ctx, t, durQ, probeEnd, probeEnd, probeRangeStep, probeLineLimit)
			}, func(body []byte) { assertUnwrap(t, body, unwrapDurations, parseGoDuration) })

			bytesQ := fmt.Sprintf(`sum by (%s) (sum_over_time({service_name=%q} | logfmt | unwrap bytes(b) | __error__="" [%s]))`, indexLabel, unwrapService, probeWindow)
			r.check(ctx, t, "unwrap-bytes", true, func(ctx context.Context) []byte {
				return h.lokiRange(ctx, t, bytesQ, probeEnd, probeEnd, probeRangeStep, probeLineLimit)
			}, func(body []byte) { assertUnwrap(t, body, unwrapBytes, parseHumanBytes) })

			for _, c := range logQLOnlyEqualityCases {
				c := c
				r.check(ctx, t, "loki-stage/"+c.name, false, func(ctx context.Context) []byte {
					return h.lokiRange(ctx, t, c.query, probeEnd.Add(-probeWindow), probeEnd, probeRangeStep, probeLineLimit)
				}, nil)
			}

			if s.regexpJIT {
				t.Run("invalid-utf8", func(t *testing.T) { checkInvalidUTF8(ctx, t, s, h) })
			}
		})
	}
}

// runner evaluates one corpus entry under every mode a build offers.
type runner struct {
	s        *server
	h        handlers
	modes    []jitMode
	minCount int
}

// check runs request under each mode and requires, on a build with the
// regular-expression compiler:
//   - off, compile-on-first-use and default-threshold answers to be equal;
//   - a compile-on-first-use request, with every other compiler off, to
//     leave compiled code behind exactly when wantJIT;
//   - the default threshold to have compiled the pattern by the time a
//     repeated request crosses it, when wantJIT.
//
// reference, when non-nil, is applied to every mode's answer.
func (r *runner) check(ctx context.Context, t *testing.T, name string, wantJIT bool, request func(context.Context) []byte, reference func([]byte)) {
	t.Run(name, func(t *testing.T) {
		var baseline []byte
		for _, mode := range r.modes {
			runs := 1
			if mode == jitDefault && r.s.regexpJIT {
				r.s.dropCompiled(ctx, t)
				// One more request than the threshold, so the last one is
				// served by compiled code when the shape is eligible.
				runs = r.minCount + 1
			}
			if mode == jitNow {
				r.s.dropCompiled(ctx, t)
			}
			for run := 0; run < runs; run++ {
				got := canonical(t, request(withMode(ctx, mode)))
				if baseline == nil {
					baseline = got
				} else if !bytes.Equal(got, baseline) {
					t.Fatalf("mode %s (request %d) answered differently from mode %s:\n--- %s ---\n%s\n--- %s ---\n%s",
						mode, run+1, r.modes[0], r.modes[0], baseline, mode, got)
				}
				if reference != nil {
					reference(got)
				}
			}
			if mode == jitDefault && r.s.regexpJIT && wantJIT {
				if n := r.s.compiledEntries(ctx, t); n == 0 {
					t.Errorf("after %d requests at the server's default threshold (%d) no regular expression was compiled", runs, r.minCount)
				}
			}
		}
		if !r.s.regexpJIT {
			return
		}
		r.s.dropCompiled(ctx, t)
		tag := probeTag(name)
		request(chclient.WithQuerySetting(withOtherJITOff(withMode(ctx, jitNow)), settingLogComment, tag))
		if got := r.s.compiledEntries(ctx, t) > 0; got != wantJIT {
			t.Errorf("%s compiled a regular expression = %v; the corpus records %v. Emitted SQL:\n%s",
				r.s.version, got, wantJIT, strings.Join(r.s.loggedQueries(ctx, t, tag), "\n"))
		}
	})
}

// settingLogComment tags a request's queries in system.query_log.
const settingLogComment = "log_comment"

// probeTag is a unique log_comment for one eligibility probe.
func probeTag(name string) string {
	return fmt.Sprintf("regexjit-%d-%s", time.Now().UnixNano(), name)
}

// loggedQueries returns the SQL of the finished queries tagged tag.
func (s *server) loggedQueries(ctx context.Context, t testing.TB, tag string) []string {
	t.Helper()
	s.exec(ctx, t, "SYSTEM FLUSH LOGS")
	rows, err := s.admin.Conn().Query(ctx, "SELECT query FROM system.query_log WHERE type = 'QueryFinish' AND log_comment = ? ORDER BY event_time_microseconds", tag)
	if err != nil {
		t.Fatalf("read query_log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatalf("scan query_log: %v", err)
		}
		out = append(out, q)
	}
	return out
}

// defaultMinCount reads the server's default compile threshold.
func (s *server) defaultMinCount(ctx context.Context, t testing.TB) int {
	t.Helper()
	if !s.regexpJIT {
		return 0
	}
	var v string
	if err := s.admin.Conn().QueryRow(ctx, "SELECT value FROM system.settings WHERE name = ?", settingMinCountRegexp).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", settingMinCountRegexp, err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s = %q: %v", settingMinCountRegexp, v, err)
	}
	return n
}

// seedSemanticProbe writes one gauge sample and one log line per probe
// value, one gauge sample per invalid UTF-8 value under its own metric, and
// the logfmt lines the unwrap probes read. Each row carries its index in the
// `i` attribute so a response row maps back to the value it was seeded from.
func seedSemanticProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	ts := probeEnd.Add(-probeSampleAge)
	insertGauge := func(metric string, values []string) {
		for i, v := range values {
			attrs := map[string]string{indexLabel: strconv.Itoa(i)}
			if v != "" {
				attrs[probeLabel] = v
			}
			s.exec(ctx, t, "INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value) VALUES (?, ?, ?, ?, ?)",
				probeService, metric, attrs, ts, float64(probeMetricValue))
		}
	}
	insertGauge(probeMetric, probeValues)
	insertGauge(bytesMetric, invalidUTF8Values)

	insertLog := func(service string, i int, body string, resource map[string]string) {
		res := map[string]string{"service.name": service}
		for k, v := range resource {
			res[k] = v
		}
		s.exec(ctx, t, "INSERT INTO otel_logs (Timestamp, ServiceName, Body, ResourceAttributes, LogAttributes) VALUES (?, ?, ?, ?, ?)",
			ts.Add(time.Duration(i)*time.Millisecond), service, body, res, map[string]string{indexLabel: strconv.Itoa(i)})
	}
	for i, v := range probeValues {
		res := map[string]string{}
		if v != "" {
			res[probeLabel] = v
		}
		insertLog(probeService, i, v, res)
	}
	for i, v := range invalidUTF8Values {
		insertLog(bytesService, i, v, nil)
	}
	for i := range max(len(unwrapDurations), len(unwrapBytes)) {
		d, b := "", ""
		if i < len(unwrapDurations) {
			d = unwrapDurations[i]
		}
		if i < len(unwrapBytes) {
			b = unwrapBytes[i]
		}
		insertLog(unwrapService, i, fmt.Sprintf("i=%d d=%s b=%s", i, strconv.Quote(d), strconv.Quote(b)), nil)
	}
}

// canonical re-encodes a JSON response with its statistics removed and its
// result rows in a stable order, so two answers compare by content.
func canonical(t testing.TB, body []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	if data, ok := doc["data"].(map[string]any); ok {
		delete(data, "stats")
		if rows, ok := data["result"].([]any); ok {
			sort.Slice(rows, func(i, j int) bool { return mustJSON(t, rows[i]) < mustJSON(t, rows[j]) })
		}
	}
	return []byte(mustJSON(t, doc))
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

// jsonString is s as it reads back after a JSON round trip: an invalid UTF-8
// byte becomes U+FFFD.
func jsonString(t testing.TB, s string) string {
	t.Helper()
	var out string
	if err := json.Unmarshal([]byte(mustJSON(t, s)), &out); err != nil {
		t.Fatalf("round-trip %q: %v", s, err)
	}
	return out
}

// promVector is the part of a Prometheus instant-vector response the
// reference checks read.
type promVector struct {
	Data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	} `json:"data"`
}

// selectedIndexes returns the index labels of the series a PromQL response
// holds, and each series' dst label ("" when absent).
func selectedIndexes(t testing.TB, body []byte) map[int]string {
	t.Helper()
	var v promVector
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	out := map[int]string{}
	for _, r := range v.Data.Result {
		i, err := strconv.Atoi(r.Metric[indexLabel])
		if err != nil {
			t.Fatalf("series without an integer %q label: %v", indexLabel, r.Metric)
		}
		out[i] = r.Metric[dstLabel]
	}
	return out
}

// assertSelected requires a PromQL selector's series to be exactly those
// Prometheus's own matcher selects.
func assertSelected(t testing.TB, body []byte, values []string, pattern string, mt labels.MatchType) {
	t.Helper()
	m, err := labels.NewMatcher(mt, probeLabel, pattern)
	if err != nil {
		t.Fatalf("reference matcher %q: %v", pattern, err)
	}
	got := selectedIndexes(t, body)
	var want, have []int
	for i, v := range values {
		if m.Matches(v) {
			want = append(want, i)
		}
	}
	for i := range got {
		have = append(have, i)
	}
	slices.Sort(have)
	if !slices.Equal(have, want) {
		t.Errorf("selector %s%q selected %s; Prometheus selects %s", mt, pattern, describe(values, have), describe(values, want))
	}
}

// assertLabelReplace requires label_replace's dst label on every series to
// be what Prometheus's funcLabelReplace writes: the regex anchored as
// ^(?s:regex)$, the replacement expanded against the source value's
// submatches, and an empty result leaving the label unset.
func assertLabelReplace(t testing.TB, body []byte, values []string, regex, repl string) {
	t.Helper()
	re := regexp.MustCompile("^(?s:" + regex + ")$")
	got := selectedIndexes(t, body)
	for i, v := range values {
		want := ""
		if idx := re.FindStringSubmatchIndex(v); idx != nil {
			want = string(re.ExpandString(nil, repl, v, idx))
		}
		want = jsonString(t, want)
		have, ok := got[i]
		if !ok {
			t.Errorf("label_replace dropped series %d (%q)", i, v)
			continue
		}
		if have != want {
			t.Errorf("label_replace(%q, %q) on %q wrote dst=%q; Prometheus writes %q", regex, repl, v, have, want)
		}
	}
}

// lokiStreams is the part of a Loki streams response the reference checks
// read.
type lokiStreams struct {
	Data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func decodeLines(t testing.TB, body []byte) (lines []string, streams lokiStreams) {
	t.Helper()
	if err := json.Unmarshal(body, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if streams.Data.ResultType != "streams" {
		t.Fatalf("resultType %q; want streams", streams.Data.ResultType)
	}
	for _, r := range streams.Data.Result {
		for _, v := range r.Values {
			lines = append(lines, v[1])
		}
	}
	slices.Sort(lines)
	return lines, streams
}

// assertLines requires a line filter's lines to be exactly those Loki's
// unanchored Go-regexp line filter keeps (or drops, when negated).
func assertLines(t testing.TB, body []byte, values []string, pattern string, negated bool) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	have, _ := decodeLines(t, body)
	var want []string
	for _, v := range values {
		if re.MatchString(v) != negated {
			want = append(want, jsonString(t, v))
		}
	}
	slices.Sort(want)
	if !slices.Equal(have, want) {
		t.Errorf("line filter (negated=%v) %q kept %q; Loki keeps %q", negated, pattern, have, want)
	}
}

// assertStreamSelected requires a Loki stream selector to return the lines
// of exactly the streams Prometheus's matcher — which Loki's selectors use —
// selects.
func assertStreamSelected(t testing.TB, body []byte, pattern string) {
	t.Helper()
	m, err := labels.NewMatcher(labels.MatchRegexp, probeLabel, pattern)
	if err != nil {
		t.Fatalf("reference matcher %q: %v", pattern, err)
	}
	have, _ := decodeLines(t, body)
	var want []string
	for _, v := range probeValues {
		if m.Matches(v) {
			want = append(want, jsonString(t, v))
		}
	}
	slices.Sort(want)
	if !slices.Equal(have, want) {
		t.Errorf("stream selector %s=~%q returned lines %q; Loki returns %q", probeLabel, pattern, have, want)
	}
}

// assertUnwrap requires a `sum by (i)` over an unwrapped field to hold, for
// each seeded index, the value the reference conversion produces, and no
// series for a value it rejects.
func assertUnwrap(t testing.TB, body []byte, values []string, convert func(string) (float64, bool)) {
	t.Helper()
	var v struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode unwrap: %v", err)
	}
	got := map[int]float64{}
	for _, r := range v.Data.Result {
		i, err := strconv.Atoi(r.Metric[indexLabel])
		if err != nil {
			t.Fatalf("series without an integer %q label: %v", indexLabel, r.Metric)
		}
		point := r.Value
		if len(r.Values) > 0 {
			point = r.Values[len(r.Values)-1]
		}
		s, _ := point[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("series %d value %v: %v", i, point, err)
		}
		got[i] = f
	}
	for i, raw := range values {
		want, ok := convert(raw)
		have, present := got[i]
		switch {
		case !ok && present:
			t.Errorf("unwrap of %q produced %v; the reference rejects it", raw, have)
		case ok && !present:
			t.Errorf("unwrap of %q produced no sample; the reference converts it to %v", raw, want)
		case ok && have != want:
			t.Errorf("unwrap of %q produced %v; the reference converts it to %v", raw, have, want)
		}
	}
}

// parseGoDuration is Loki's duration() conversion.
func parseGoDuration(s string) (float64, bool) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d.Seconds(), true
}

// parseHumanBytes is Loki's bytes() conversion.
func parseHumanBytes(s string) (float64, bool) {
	b, err := humanize.ParseBytes(s)
	if err != nil {
		return 0, false
	}
	return float64(b), true
}

// checkInvalidUTF8 pins what the compiled engine does on bytes outside
// UTF-8: for every shape it compiles, a selector, a label_replace and a line
// filter answer as the reference engines do. The interpreted engine is not
// held to this — see the package documentation.
func checkInvalidUTF8(ctx context.Context, t *testing.T, s *server, h handlers) {
	jitCtx := withMode(ctx, jitNow)
	for _, c := range matcherCases {
		if c.anchoredJIT {
			q := fmt.Sprintf(`%s{%s=~%s}`, bytesMetric, probeLabel, strconv.Quote(c.pattern))
			s.dropCompiled(ctx, t)
			assertSelectedJSON(t, h.promInstant(jitCtx, t, q, probeEnd), c.pattern)
		}
		if c.lineFilterJIT {
			q := fmt.Sprintf(`{service_name=%q} |~ %s`, bytesService, strconv.Quote(c.pattern))
			s.dropCompiled(ctx, t)
			assertLines(t, h.lokiRange(jitCtx, t, q, probeEnd.Add(-probeWindow), probeEnd, probeRangeStep, probeLineLimit), invalidUTF8Values, c.pattern, false)
		}
	}
	for _, c := range labelReplaceCases {
		if !c.jit {
			continue
		}
		q := fmt.Sprintf(`label_replace(%s, %q, %s, %q, %s)`, bytesMetric, dstLabel, strconv.Quote(c.repl), probeLabel, strconv.Quote(c.regex))
		s.dropCompiled(ctx, t)
		assertLabelReplace(t, h.promInstant(jitCtx, t, q, probeEnd), invalidUTF8Values, c.regex, c.repl)
	}
}

// assertSelectedJSON is assertSelected over invalidUTF8Values.
func assertSelectedJSON(t testing.TB, body []byte, pattern string) {
	t.Helper()
	assertSelected(t, body, invalidUTF8Values, pattern, labels.MatchRegexp)
}

// describe renders selected indexes as their values, for failure messages.
func describe(values []string, idx []int) string {
	parts := make([]string, 0, len(idx))
	for _, i := range idx {
		parts = append(parts, strconv.Quote(values[i]))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
