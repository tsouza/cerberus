//go:build agpl_oracle

// Package lsyntax's A/B oracle test. It parses a LogQL corpus with BOTH
// the in-house clean-room parser and the upstream AGPL grafana/loki
// reference parser, then asserts the two produce structurally identical
// ASTs (and agree on accept/reject).
//
// This file is the ONLY place that imports the AGPL parser, and it is
// gated behind the `agpl_oracle` build tag so neither the production
// build nor the default test run pulls it in. Run it explicitly with:
//
//	CGO_ENABLED=1 go test -tags agpl_oracle ./internal/logql/lsyntax/
//
// The structural comparison normalizes each AST to a canonical string by
// reflecting over the exported field names (which the two ASTs share by
// design) plus a handful of accessor-backed nodes whose payload lives in
// unexported fields. The label-filter / extraction / format leaf values
// are now in-house lsyntax types (no longer shared with upstream's
// `pkg/logql/log`); they have byte-identical String()/field shapes, so
// they normalise to the same text. The one cosmetic difference —
// upstream's NewStringLabelFilter returns a LineFilterLabelFilter that
// renders regex matchers with backticks, while the in-house parser keeps
// a StringLabelFilter (matcher promotion, double quotes) — is normalised
// by rendering the string/line-filter leaves via their embedded matcher.
package lsyntax

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	lokisyntax "github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"
)

// corpus covers every LogQL construct the in-house parser implements.
var corpus = []string{
	// bare selectors + matcher operators
	`{app="api"}`,
	`{app="api", env=~"prod|stg", region!="us", tier!~"gold"}`,
	`{app="api"} | json`,
	// line filters
	`{app="api"} |= "error"`,
	`{app="api"} != "debug" |~ "5.." !~ "health"`,
	`{app="api"} |= "a" or "b" or "c"`,
	`{app="api"} != "a" or "b"`,
	`{app="api"} |> "<_> error <_>"`,
	`{app="api"} !> "<_> ok"`,
	`{app="api"} |= ip("192.168.0.0/16")`,
	`{app="api"} != ip("10.0.0.1")`,
	// label filters
	`{app="api"} | level="error"`,
	`{app="api"} | level="error" and status=~"5.."`,
	`{app="api"} | level="error" or status="500"`,
	`{app="api"} | duration > 5s`,
	`{app="api"} | duration >= 1m30s`,
	`{app="api"} | size > 1KB`,
	`{app="api"} | size <= 1.5MiB`,
	`{app="api"} | code >= 400`,
	`{app="api"} | code == 200`,
	`{app="api"} | code != -1`,
	`{app="api"} | addr = ip("1.2.3.4")`,
	`{app="api"} | (a="1" or b="2") and c="3"`,
	`{app="api"} | a="1" b="2"`,
	// parsers
	`{app="api"} | logfmt`,
	`{app="api"} | logfmt --strict --keep-empty`,
	`{app="api"} | logfmt foo, bar="baz"`,
	`{app="api"} | json foo="response.code", bar="status"`,
	`{app="api"} | regexp "(?P<method>\\w+) (?P<path>\\S+)"`,
	`{app="api"} | pattern "<ip> <_> <method> <path>"`,
	`{app="api"} | unpack`,
	// format / decolorize / drop / keep
	`{app="api"} | line_format "[{{.level}}] {{__line__}}"`,
	`{app="api"} | label_format svc=job, lvl="{{.severity}}"`,
	`{app="api"} | decolorize`,
	`{app="api"} | drop env, pod`,
	`{app="api"} | drop env="prod", region`,
	`{app="api"} | keep job, env`,
	// range aggregations
	`count_over_time({app="api"}[5m])`,
	`rate({app="api"} |= "error" [1m])`,
	`bytes_over_time({app="api"}[5m]) offset 1h`,
	`sum_over_time({app="api"} | unwrap latency [5m])`,
	`quantile_over_time(0.99, {app="api"} | unwrap latency [5m])`,
	`avg_over_time({app="api"} | logfmt | unwrap duration(latency) [5m])`,
	`max_over_time({app="api"} | unwrap bytes(size) [5m]) by (job)`,
	`count_over_time(({app="api"})[5m])`,
	`absent_over_time({app="api"}[1m])`,
	`sum_over_time({app="api"} | unwrap latency | __error__="" [5m])`,
	// vector aggregations
	`sum(rate({app="api"}[1m]))`,
	`sum by (job) (rate({app="api"}[1m]))`,
	`sum without (pod) (count_over_time({app="api"}[5m]))`,
	`topk(5, sum by (job) (rate({app="api"}[1m])))`,
	`bottomk(3, rate({app="api"}[1m]))`,
	`avg(max_over_time({app="api"} | unwrap latency [5m]))`,
	`sort_desc(sum by (job) (rate({app="api"}[1m])))`,
	// binary ops + modifiers
	`rate({app="api"}[1m]) + rate({app="db"}[1m])`,
	`sum(rate({app="api"}[1m])) / sum(rate({app="db"}[1m]))`,
	`rate({app="api"}[1m]) > bool 0.5`,
	`rate({app="api"}[1m]) and on(job) rate({app="db"}[1m])`,
	`rate({app="api"}[1m]) / ignoring(pod) group_left(team) rate({app="db"}[1m])`,
	`2 * rate({app="api"}[1m])`,
	`1 + 1`,
	`10 / 2 + 3`,
	`-5 * count_over_time({app="api"}[5m])`,
	// literal / vector / label_replace
	`5`,
	`vector(1)`,
	`label_replace(rate({app="api"}[1m]), "dst", "$1", "src", "(.*)")`,
	// variants
	`variants(count_over_time({app="api"}[1m]), bytes_over_time({app="api"}[1m])) of ({app="api"}[1m])`,
	// nesting / parens
	`(rate({app="api"}[1m]))`,
	`sum(rate({app="api"}[1m]) + rate({app="db"}[1m]))`,
}

// rejectCorpus collects queries both parsers must reject.
var rejectCorpus = []string{
	`{app="api"`,                     // unterminated selector
	`rate({app="api"}[`,              // unterminated range
	`{="api"}`,                       // missing matcher name
	`avg_over_time({app="api"}[5m])`, // typed agg without unwrap
	`{app=~".*"}`,                    // empty-compatible (strict ParseExpr rejects)
	`sum by (job) (5)`,               // grouping over literal? both should agree
}

func TestAGPLOracle_Accept(t *testing.T) {
	for _, q := range corpus {
		q := q
		t.Run(q, func(t *testing.T) {
			mine, myErr := ParseExpr(q)
			ref, refErr := lokisyntax.ParseExpr(q)
			if (myErr == nil) != (refErr == nil) {
				t.Fatalf("accept/reject disagreement:\n  in-house err=%v\n  loki err=%v", myErr, refErr)
			}
			if myErr != nil {
				return // both rejected; acceptable for this corpus entry
			}
			got := normalize(reflect.ValueOf(mine))
			want := normalize(reflect.ValueOf(ref))
			if got != want {
				// Full dumps can be long; ORACLE_DUMP_DIR captures them to
				// disk for inspection when a mismatch surfaces.
				if dir := os.Getenv("ORACLE_DUMP_DIR"); dir != "" {
					_ = os.WriteFile(dir+"/got.txt", []byte(got), 0o644)
					_ = os.WriteFile(dir+"/want.txt", []byte(want), 0o644)
				}
				t.Fatalf("AST mismatch for %q\n in-house: %s\n     loki: %s", q, got, want)
			}
		})
	}
}

func TestAGPLOracle_Reject(t *testing.T) {
	for _, q := range rejectCorpus {
		q := q
		t.Run(q, func(t *testing.T) {
			_, myErr := ParseExpr(q)
			_, refErr := lokisyntax.ParseExpr(q)
			if (myErr == nil) != (refErr == nil) {
				t.Fatalf("reject disagreement for %q:\n  in-house err=%v\n  loki err=%v", q, myErr, refErr)
			}
		})
	}
}

// normalize renders an AST value to a canonical, package-agnostic string
// by walking exported fields. Two structurally equivalent trees from the
// two different parser packages render identically.
func normalize(v reflect.Value) string {
	if !v.IsValid() {
		return "nil"
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return "nil"
		}
		// Leaf value types (labels.Matcher, upstream log.* filterers, and
		// their in-house lsyntax equivalents) are compared by their
		// normalised String(). BinaryLabelFilter is excluded so it is
		// walked structurally on both sides — that way its Left/Right
		// leaves get normalised individually (avoiding upstream's
		// LineFilterLabelFilter backtick rendering leaking into the parent).
		et := v.Elem().Type()
		if isStringifiedLeaf(et) {
			if v.CanInterface() {
				return leafString(v.Interface())
			}
		}
		return normalize(v.Elem())
	case reflect.Interface:
		if v.IsNil() {
			return "nil"
		}
		return normalize(v.Elem())
	case reflect.Slice, reflect.Array:
		parts := make([]string, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			parts = append(parts, normalize(v.Index(i)))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case reflect.Struct:
		return normalizeStruct(v)
	default:
		if v.CanInterface() {
			return strings.TrimSpace(stringify(v.Interface()))
		}
		return v.String()
	}
}

func normalizeStruct(v reflect.Value) string {
	t := v.Type()
	name := t.Name()

	// Leaf value types (upstream log.* types, prometheus labels, and their
	// in-house lsyntax equivalents) compare by value/String(): both parsers
	// build semantically identical leaves that render the same text.
	if isStringifiedLeaf(t) {
		if v.CanInterface() {
			return leafString(v.Interface())
		}
	}

	// Accessor-backed nodes whose payload is unexported in both ASTs.
	switch name {
	case "DropLabelsExpr", "KeepLabelsExpr":
		return name + "{" + callStringSlice(v, "Names") + "}"
	case "MultiVariantExpr":
		return name + "{variants=" + callNormalize(v, "Variants") +
			",logRange=" + callNormalize(v, "LogRange") + "}"
	}

	var fields []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		fv := v.Field(i)
		fields = append(fields, f.Name+"="+normalize(fv))
	}
	sort.Strings(fields)
	return name + "{" + strings.Join(fields, ",") + "}"
}

// isLeafLabelType reports whether a type name is one of the label-filter /
// extraction / format leaf value types (in-house lsyntax or upstream
// log.*). These are compared by their normalised String()/value rather
// than walked structurally. BinaryLabelFilter is intentionally absent: it
// is walked so its Left/Right leaves get normalised individually.
// isStringifiedLeaf reports whether a type should be rendered by its
// (normalised) String() rather than walked structurally. The upstream
// log.* leaf types, prometheus labels, and the in-house leaf equivalents
// qualify — except BinaryLabelFilter, which is always walked so its
// children get normalised one by one.
func isStringifiedLeaf(t reflect.Type) bool {
	if t.Name() == "BinaryLabelFilter" {
		return false
	}
	return strings.Contains(t.PkgPath(), "logql/log") ||
		strings.Contains(t.PkgPath(), "prometheus/model/labels") ||
		isLeafLabelType(t.Name())
}

func isLeafLabelType(name string) bool {
	switch name {
	case "StringLabelFilter", "LineFilterLabelFilter", "NumericLabelFilter",
		"DurationLabelFilter", "BytesLabelFilter", "IPLabelFilter",
		"LabelExtractionExpr", "LabelFmt":
		return true
	}
	return false
}

// leafString renders a label-filter leaf to a canonical string. For the
// string/line-filter family it renders via the embedded *labels.Matcher
// (double-quoted) rather than the type's own String(): upstream's
// NewStringLabelFilter returns a LineFilterLabelFilter whose String()
// renders regex matchers with backticks, while the in-house parser keeps
// a StringLabelFilter (matcher promotion). Both are valid, equivalent
// LogQL; normalising via the matcher makes the structural comparison
// ignore that purely cosmetic rendering choice.
func leafString(i interface{}) string {
	rv := reflect.ValueOf(i)
	name := rv.Type().Name()
	if rv.Kind() == reflect.Ptr && rv.Elem().Kind() == reflect.Struct {
		name = rv.Elem().Type().Name()
	}
	if name == "StringLabelFilter" || name == "LineFilterLabelFilter" {
		if m := embeddedMatcher(rv); m != nil {
			return m.String()
		}
	}
	return stringify(i)
}

// embeddedMatcher returns the *labels.Matcher field embedded in a leaf
// label-filter value, or nil.
func embeddedMatcher(rv reflect.Value) *labels.Matcher {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	matcherPtrType := reflect.TypeOf((*labels.Matcher)(nil))
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if f.Type() == matcherPtrType && f.CanInterface() {
			if m, ok := f.Interface().(*labels.Matcher); ok {
				return m
			}
		}
	}
	return nil
}

func stringify(i interface{}) string {
	if s, ok := i.(interface{ String() string }); ok {
		return s.String()
	}
	return strings.TrimSpace(fmt.Sprintf("%v", i))
}

// callStringSlice invokes a no-arg method returning []string (addressable
// pointer receiver is taken automatically).
func callStringSlice(v reflect.Value, method string) string {
	m := methodValue(v, method)
	if !m.IsValid() {
		return ""
	}
	out := m.Call(nil)
	if len(out) == 0 {
		return ""
	}
	return normalize(out[0])
}

func callNormalize(v reflect.Value, method string) string {
	m := methodValue(v, method)
	if !m.IsValid() {
		return "nil"
	}
	out := m.Call(nil)
	if len(out) == 0 {
		return "nil"
	}
	return normalize(out[0])
}

func methodValue(v reflect.Value, method string) reflect.Value {
	if m := v.MethodByName(method); m.IsValid() {
		return m
	}
	if v.CanAddr() {
		if m := v.Addr().MethodByName(method); m.IsValid() {
			return m
		}
	}
	// Build an addressable copy.
	pv := reflect.New(v.Type())
	pv.Elem().Set(v)
	return pv.MethodByName(method)
}
