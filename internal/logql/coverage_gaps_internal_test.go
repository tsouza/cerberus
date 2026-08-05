package logql

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/httperr"
	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// =================================================================
// Lang — the engine adapter surface
// =================================================================

func TestLang_Name(t *testing.T) {
	l := &Lang{Schema: schema.DefaultOTelLogs()}
	if got := l.Name(); got != telemetry.QLLogQL {
		t.Fatalf("Name() = %q, want %q", got, telemetry.QLLogQL)
	}
}

func TestLang_LateMatShape_ReportsTheResolvedTableNotTheDefault(t *testing.T) {
	s := schema.DefaultOTelLogs()
	s.LogsTable = "custom_logs"
	l := &Lang{Schema: s}

	table, wide, rowKey := l.LateMatShape()
	if table != "custom_logs" {
		t.Fatalf("LateMatShape table = %q, want the overridden table", table)
	}
	if len(wide) != len(s.WideColumns) {
		t.Fatalf("wide columns = %v, want %v", wide, s.WideColumns)
	}
	for i := range wide {
		if wide[i] != s.WideColumns[i] {
			t.Fatalf("wide[%d] = %q, want %q", i, wide[i], s.WideColumns[i])
		}
	}
	if len(rowKey) != len(s.RowKey) {
		t.Fatalf("row key = %v, want %v", rowKey, s.RowKey)
	}
}

func TestLang_Parse_LogStreamQuery(t *testing.T) {
	l := &Lang{Schema: schema.DefaultOTelLogs()}
	plan, meta, err := l.Parse(context.Background(), `{service_name="api"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if plan == nil {
		t.Fatal("Parse returned a nil plan")
	}
	if meta.IsMetric {
		t.Fatal("a stream selector must not classify as a metric query")
	}
	if meta.ResponseShape != "loki-streams" {
		t.Fatalf("ResponseShape = %q, want loki-streams", meta.ResponseShape)
	}
	if _, ok := meta.Extra["expr"]; !ok {
		t.Fatalf("Meta.Extra must carry the parsed expr, got %v", meta.Extra)
	}
}

func TestLang_Parse_MetricQuery(t *testing.T) {
	l := &Lang{Schema: schema.DefaultOTelLogs()}
	_, meta, err := l.Parse(context.Background(), `rate({service_name="api"}[5m])`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !meta.IsMetric {
		t.Fatal("rate(...) must classify as a metric query")
	}
	if meta.ResponseShape != "loki-matrix" {
		t.Fatalf("ResponseShape = %q, want loki-matrix", meta.ResponseShape)
	}
}

func TestLang_Parse_ParseFailureIsA400BadData(t *testing.T) {
	l := &Lang{Schema: schema.DefaultOTelLogs()}
	_, _, err := l.Parse(context.Background(), `{service_name=`)
	var he *httperr.Error
	if !errors.As(err, &he) {
		t.Fatalf("want *httperr.Error, got %#v", err)
	}
	if he.Status != http.StatusBadRequest || he.Kind != errBadData {
		t.Fatalf("parse failure mapped to %d/%s, want 400/%s", he.Status, he.Kind, errBadData)
	}
}

func TestLang_Parse_NormalisesDottedStreamSelectorKeys(t *testing.T) {
	// A Grafana Loki datasource sends `service.name`; the upstream grammar
	// rejects the dot, so Parse must rewrite it before parsing. Without the
	// normalisation this is a 400.
	l := &Lang{Schema: schema.DefaultOTelLogs()}
	if _, _, err := l.Parse(context.Background(), `{service.name="api"}`); err != nil {
		t.Fatalf("dotted selector must parse: %v", err)
	}
}

func TestNormalizeDottedLabels_RewritesOnlyTheSelectorKeys(t *testing.T) {
	got := NormalizeDottedLabels(`{service.name="a.b"}`)
	if !strings.Contains(got, `service_name=`) {
		t.Fatalf("key not normalised: %q", got)
	}
	if !strings.Contains(got, `"a.b"`) {
		t.Fatalf("the value must not be rewritten: %q", got)
	}
	// The package-private twin Lang.Parse routes through must agree.
	if other := normalizeLokiDottedLabels(`{service.name="a.b"}`); other != got {
		t.Fatalf("normalizeLokiDottedLabels = %q, want %q", other, got)
	}
}

// =================================================================
// NormalizeDetectedLevel
// =================================================================

func TestNormalizeDetectedLevel(t *testing.T) {
	cases := map[string]string{
		"":            "unknown",
		"INFO":        "info",
		"Inf":         "info",
		"information": "info",
		"WRN":         "warn",
		"warning":     "warn",
		"ERR":         "error",
		"Error":       "error",
		"TRC":         "trace",
		"dbg":         "debug",
		"CRITICAL":    "critical",
		"Fatal":       "fatal",
		// An unknown variant falls through as its lowercased self, matching
		// upstream normalizeLogLevel's default branch.
		"NOTICE": "notice",
	}
	for in, want := range cases {
		if got := NormalizeDetectedLevel(in); got != want {
			t.Fatalf("NormalizeDetectedLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

// =================================================================
// MatchesIPPattern — the Go-side twin of the SQL rendering
// =================================================================

func TestMatchesIPPattern(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		pattern string
		want    bool
	}{
		{"exact v4 hit", "from 10.0.0.1 ok", "10.0.0.1", true},
		{"exact v4 miss", "from 10.0.0.2 ok", "10.0.0.1", false},
		{"cidr contains", "client=192.168.4.7", "192.168.0.0/16", true},
		{"cidr excludes", "client=192.169.4.7", "192.168.0.0/16", false},
		{"cidr lower edge", "10.1.0.0", "10.1.0.0/24", true},
		{"cidr upper edge", "10.1.0.255", "10.1.0.0/24", true},
		{"cidr just past the edge", "10.1.1.0", "10.1.0.0/24", false},
		{"range contains", "ip 172.16.0.9", "172.16.0.1-172.16.0.10", true},
		{"range excludes", "ip 172.16.0.11", "172.16.0.1-172.16.0.10", false},
		{"no candidate at all", "no addresses here", "10.0.0.0/8", false},
		{"v6 hit", "peer=[2001:db8::5]", "2001:db8::/32", true},
		{"v6 miss", "peer=[2001:dc8::5]", "2001:db8::/32", false},
		// Family mismatch: a v4 candidate must never satisfy a v6 pattern.
		{"v4 candidate against a v6 pattern", "10.0.0.1", "2001:db8::/32", false},
		{"v6 candidate against a v4 pattern", "2001:db8::5", "10.0.0.0/8", false},
		// A candidate run that is not a parseable address is skipped, not
		// an error — the following real one still matches.
		{"unparseable candidate then a hit", "1.2.3.4.5 and 10.0.0.1", "10.0.0.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MatchesIPPattern(tc.subject, tc.pattern)
			if err != nil {
				t.Fatalf("MatchesIPPattern: %v", err)
			}
			if got != tc.want {
				t.Fatalf("MatchesIPPattern(%q, %q) = %v, want %v", tc.subject, tc.pattern, got, tc.want)
			}
		})
	}
}

func TestMatchesIPPattern_InvalidPatternIsAnError(t *testing.T) {
	got, err := MatchesIPPattern("10.0.0.1", "not-an-ip")
	if err == nil {
		t.Fatalf("invalid pattern must error, got match=%v", got)
	}
	if got {
		t.Fatal("an errored pattern must not report a match")
	}
	if !strings.Contains(err.Error(), "invalid pattern") {
		t.Fatalf("error text %q does not mirror the reference wording", err.Error())
	}
}

// =================================================================
// SelectorPredicate — the /index/stats + /index/volume entry point
// =================================================================

func TestSelectorPredicate(t *testing.T) {
	s := schema.DefaultOTelLogs()

	if got := SelectorPredicate(nil, s); got != nil {
		t.Fatalf("no matchers must yield a nil predicate, got %#v", got)
	}

	one := SelectorPredicate([]*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "app", "api"),
	}, s)
	b, ok := one.(*chplan.Binary)
	if !ok {
		t.Fatalf("single matcher lowered to %T, want *chplan.Binary", one)
	}
	if b.Op != chplan.OpEq {
		t.Fatalf("op = %v, want OpEq", b.Op)
	}
	if lit, ok := b.Right.(*chplan.LitString); !ok || lit.V != "api" {
		t.Fatalf("rhs = %#v, want LitString{api}", b.Right)
	}

	two := SelectorPredicate([]*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "app", "api"),
		labels.MustNewMatcher(labels.MatchNotEqual, "env", "dev"),
	}, s)
	and, ok := two.(*chplan.Binary)
	if !ok || and.Op != chplan.OpAnd {
		t.Fatalf("two matchers must AND-fold, got %#v", two)
	}
	if !and.Left.Equal(one) {
		t.Fatalf("the AND's left arm must be the first matcher's predicate, got %#v", and.Left)
	}
}

// =================================================================
// FiltersErrorLabel — the dynamic-label gate shared with the HTTP layer
// =================================================================

func TestFiltersErrorLabel(t *testing.T) {
	errStr := &syntax.StringLabelFilter{
		Matcher: labels.MustNewMatcher(labels.MatchEqual, syntax.ErrorLabel, ""),
	}
	detailsStr := &syntax.StringLabelFilter{
		Matcher: labels.MustNewMatcher(labels.MatchEqual, syntax.ErrorDetailsLabel, "x"),
	}
	plainStr := &syntax.StringLabelFilter{
		Matcher: labels.MustNewMatcher(labels.MatchEqual, "level", "info"),
	}

	cases := []struct {
		name string
		in   syntax.LabelFilterer
		want bool
	}{
		{"__error__ string filter", errStr, true},
		{"__error_details__ string filter", detailsStr, true},
		{"ordinary string filter", plainStr, false},
		{"numeric filter on __error__", &syntax.NumericLabelFilter{Name: syntax.ErrorLabel}, true},
		{"numeric filter on a plain label", &syntax.NumericLabelFilter{Name: "status"}, false},
		{"duration filter on __error__", &syntax.DurationLabelFilter{Name: syntax.ErrorLabel}, true},
		{"duration filter on a plain label", &syntax.DurationLabelFilter{Name: "dur"}, false},
		{"bytes filter on __error_details__", &syntax.BytesLabelFilter{Name: syntax.ErrorDetailsLabel}, true},
		{"bytes filter on a plain label", &syntax.BytesLabelFilter{Name: "size"}, false},
		{"ip filter on __error__", &syntax.IPLabelFilter{Label: syntax.ErrorLabel}, true},
		{"ip filter on a plain label", &syntax.IPLabelFilter{Label: "addr"}, false},
		{
			"binary composition finds it on the left",
			&syntax.BinaryLabelFilter{Left: errStr, Right: plainStr},
			true,
		},
		{
			"binary composition finds it on the right",
			&syntax.BinaryLabelFilter{Left: plainStr, Right: detailsStr},
			true,
		},
		{
			"binary composition of plain filters is clean",
			&syntax.BinaryLabelFilter{Left: plainStr, Right: plainStr},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FiltersErrorLabel(tc.in); got != tc.want {
				t.Fatalf("FiltersErrorLabel = %v, want %v", got, tc.want)
			}
		})
	}
}

// =================================================================
// Small mapping helpers
// =================================================================

func TestLabelFilterOp(t *testing.T) {
	want := map[syntax.LabelFilterType]chplan.BinaryOp{
		syntax.LabelFilterEqual:              chplan.OpEq,
		syntax.LabelFilterNotEqual:           chplan.OpNe,
		syntax.LabelFilterGreaterThan:        chplan.OpGt,
		syntax.LabelFilterGreaterThanOrEqual: chplan.OpGe,
		syntax.LabelFilterLesserThan:         chplan.OpLt,
		syntax.LabelFilterLesserThanOrEqual:  chplan.OpLe,
	}
	for in, exp := range want {
		if got := labelFilterOp(in); got != exp {
			t.Fatalf("labelFilterOp(%v) = %v, want %v", in, got, exp)
		}
	}
}

func TestLogqlBinaryOp(t *testing.T) {
	want := map[string]chplan.BinaryOp{
		syntax.OpTypeAdd:   chplan.OpAdd,
		syntax.OpTypeSub:   chplan.OpSub,
		syntax.OpTypeMul:   chplan.OpMul,
		syntax.OpTypeDiv:   chplan.OpDiv,
		syntax.OpTypeMod:   chplan.OpMod,
		syntax.OpTypePow:   chplan.OpPow,
		syntax.OpTypeCmpEQ: chplan.OpEq,
		syntax.OpTypeNEQ:   chplan.OpNe,
		syntax.OpTypeLT:    chplan.OpLt,
		syntax.OpTypeLTE:   chplan.OpLe,
		syntax.OpTypeGT:    chplan.OpGt,
		syntax.OpTypeGTE:   chplan.OpGe,
	}
	for in, exp := range want {
		got, err := logqlBinaryOp(in)
		if err != nil {
			t.Fatalf("logqlBinaryOp(%q): %v", in, err)
		}
		if got != exp {
			t.Fatalf("logqlBinaryOp(%q) = %v, want %v", in, got, exp)
		}
	}
	// The logical ops are routed to lowerVectorSetOp before this is called,
	// so reaching here with one is a genuine unknown-operator error.
	if _, err := logqlBinaryOp(syntax.OpTypeOr); err == nil {
		t.Fatal("a logical op must not resolve to a chplan binary op")
	}
}

func TestGateMark_AndsTheGateOnTheLeftAndKeepsTheDiagnostics(t *testing.T) {
	m := labelFilterMark{
		cond:    &chplan.ColumnRef{Name: "c"},
		kind:    "SampleExtractionErr",
		details: &chplan.LitString{V: "boom"},
	}
	gate := &chplan.ColumnRef{Name: "g"}

	out := gateMark(m, gate)
	b, ok := out.cond.(*chplan.Binary)
	if !ok || b.Op != chplan.OpAnd {
		t.Fatalf("gateMark cond = %#v, want an AND", out.cond)
	}
	if !b.Left.Equal(gate) {
		t.Fatalf("the gate must be the left arm, got %#v", b.Left)
	}
	if !b.Right.Equal(m.cond) {
		t.Fatalf("the original condition must be the right arm, got %#v", b.Right)
	}
	if out.kind != m.kind || !out.details.Equal(m.details) {
		t.Fatalf("gateMark must carry kind/details through, got %q/%#v", out.kind, out.details)
	}
	// The input mark is left untouched — the caller may reuse it.
	if _, mutated := m.cond.(*chplan.Binary); mutated {
		t.Fatal("gateMark mutated its input mark")
	}
}

func TestJPToken_Text(t *testing.T) {
	cases := []struct {
		tok  jpToken
		want string
	}{
		{jpToken{kind: jpEOF}, "<eof>"},
		{jpToken{kind: jpField, str: "user"}, "user"},
		{jpToken{kind: jpString, str: "a.b"}, "a.b"},
		{jpToken{kind: jpIndex, idx: 12}, "12"},
		{jpToken{kind: jpDot}, "."},
		{jpToken{kind: jpLSB}, "["},
		{jpToken{kind: jpRSB}, "]"},
		{jpToken{kind: jpTokenKind(99)}, "?"},
	}
	for _, tc := range cases {
		if got := tc.tok.text(); got != tc.want {
			t.Fatalf("jpToken{kind:%v}.text() = %q, want %q", tc.tok.kind, got, tc.want)
		}
	}
}
