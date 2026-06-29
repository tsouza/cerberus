package logql

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestParserShape_* tests pin specific properties of the parsed Loki
// AST so we catch silent breakage when the upstream parser bumps. The
// asserted fields mirror what cerberus's lowering layer reads in
// internal/logql/{lower,range_aggregation,vector_aggregation}.go.
//
// Pure acceptance / rejection is already covered by TestParserSmoke
// / TestParserSmoke_Rejected.

func mustParseLogQL(t *testing.T, q string) syntax.Expr {
	t.Helper()
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	if expr == nil {
		t.Fatalf("ParseExpr(%q) returned nil", q)
	}
	return expr
}

// TestParserShape_StreamSelector pins the *MatchersExpr root + matcher
// shape for `{app="api"}`.
func TestParserShape_StreamSelector(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"}`)
	me, ok := expr.(*syntax.MatchersExpr)
	if !ok {
		t.Fatalf("expected *MatchersExpr, got %T", expr)
	}
	if len(me.Mts) != 1 {
		t.Fatalf("len(Mts) = %d; want 1", len(me.Mts))
	}
	m := me.Mts[0]
	if m.Name != "app" {
		t.Errorf("matcher Name = %q; want %q", m.Name, "app")
	}
	if m.Value != "api" {
		t.Errorf("matcher Value = %q; want %q", m.Value, "api")
	}
	if m.Type != labels.MatchEqual {
		t.Errorf("matcher Type = %v; want MatchEqual", m.Type)
	}
}

// TestParserShape_LineFilter pins the *PipelineExpr / *LineFilterExpr
// shape for `{app="api"} |= "error"`.
func TestParserShape_LineFilter(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} |= "error"`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	if pe.Left == nil {
		t.Fatal("PipelineExpr.Left is nil; expected stream selector")
	}
	if len(pe.MultiStages) != 1 {
		t.Fatalf("len(MultiStages) = %d; want 1", len(pe.MultiStages))
	}
	lf, ok := pe.MultiStages[0].(*syntax.LineFilterExpr)
	if !ok {
		t.Fatalf("stage = %T; want *LineFilterExpr", pe.MultiStages[0])
	}
	if lf.Match != "error" {
		t.Errorf("LineFilter.Match = %q; want %q", lf.Match, "error")
	}
	if lf.Ty != syntax.LineMatchEqual {
		t.Errorf("LineFilter.Ty = %v; want LineMatchEqual", lf.Ty)
	}
}

// TestParserShape_JSONAndLabelFilter pins the `*LineParserExpr` (json)
// plus following `*LabelFilterExpr` shape.
func TestParserShape_JSONAndLabelFilter(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} | json | level="error"`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	if len(pe.MultiStages) != 2 {
		t.Fatalf("len(MultiStages) = %d; want 2 (json + label filter)", len(pe.MultiStages))
	}
	js, ok := pe.MultiStages[0].(*syntax.LineParserExpr)
	if !ok {
		t.Fatalf("stage[0] = %T; want *LineParserExpr", pe.MultiStages[0])
	}
	if js.Op != syntax.OpParserTypeJSON {
		t.Errorf("LineParserExpr.Op = %q; want %q", js.Op, syntax.OpParserTypeJSON)
	}
	if _, ok := pe.MultiStages[1].(*syntax.LabelFilterExpr); !ok {
		t.Errorf("stage[1] = %T; want *LabelFilterExpr", pe.MultiStages[1])
	}
}

// TestParserShape_RegexLineFilter pins LineMatchRegexp on `|~`.
func TestParserShape_RegexLineFilter(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} |~ "(?i)error"`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	lf, ok := pe.MultiStages[0].(*syntax.LineFilterExpr)
	if !ok {
		t.Fatalf("stage[0] = %T; want *LineFilterExpr", pe.MultiStages[0])
	}
	if lf.Ty != syntax.LineMatchRegexp {
		t.Errorf("LineFilter.Ty = %v; want LineMatchRegexp", lf.Ty)
	}
	if lf.Match != "(?i)error" {
		t.Errorf("LineFilter.Match = %q; want %q", lf.Match, "(?i)error")
	}
}

// TestParserShape_RateMetric pins the *RangeAggregationExpr root for
// `rate({app="api"}[5m])`.
func TestParserShape_RateMetric(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `rate({app="api"}[5m])`)
	rae, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("expected *RangeAggregationExpr, got %T", expr)
	}
	if rae.Operation != syntax.OpRangeTypeRate {
		t.Errorf("Operation = %q; want %q", rae.Operation, syntax.OpRangeTypeRate)
	}
	if rae.Left == nil {
		t.Fatal("Left (LogRangeExpr) is nil")
	}
	if rae.Left.Interval.Minutes() != 5 {
		t.Errorf("Interval = %v; want 5m", rae.Left.Interval)
	}
	if rae.Left.Unwrap != nil {
		t.Errorf("Unwrap = %v; want nil for line-counter rate", rae.Left.Unwrap)
	}
}

// TestParserShape_SumByCountOverTime pins the
// *VectorAggregationExpr(*RangeAggregationExpr) chain for the canonical
// `sum by(level)(count_over_time({app="api"}[1m]))` query.
func TestParserShape_SumByCountOverTime(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `sum by(level)(count_over_time({app="api"}[1m]))`)
	va, ok := expr.(*syntax.VectorAggregationExpr)
	if !ok {
		t.Fatalf("expected *VectorAggregationExpr, got %T", expr)
	}
	if va.Operation != syntax.OpTypeSum {
		t.Errorf("Operation = %q; want %q", va.Operation, syntax.OpTypeSum)
	}
	if va.Grouping == nil {
		t.Fatal("Grouping is nil; want non-nil with [level]")
	}
	if va.Grouping.Without {
		t.Error("Grouping.Without = true; want false")
	}
	if got, want := va.Grouping.Groups, []string{"level"}; !equalStrings(got, want) {
		t.Errorf("Grouping.Groups = %v; want %v", got, want)
	}
	inner, ok := va.Left.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("Left = %T; want *RangeAggregationExpr", va.Left)
	}
	if inner.Operation != syntax.OpRangeTypeCount {
		t.Errorf("inner Operation = %q; want %q", inner.Operation, syntax.OpRangeTypeCount)
	}
}

// TestParserShape_LogfmtLineFormat pins the `*LogfmtParserExpr` +
// `*LineFmtExpr` stage chain.
func TestParserShape_LogfmtLineFormat(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} | logfmt | line_format "{{.msg}}"`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	if len(pe.MultiStages) != 2 {
		t.Fatalf("len(MultiStages) = %d; want 2", len(pe.MultiStages))
	}
	if _, ok := pe.MultiStages[0].(*syntax.LogfmtParserExpr); !ok {
		t.Errorf("stage[0] = %T; want *LogfmtParserExpr", pe.MultiStages[0])
	}
	lfm, ok := pe.MultiStages[1].(*syntax.LineFmtExpr)
	if !ok {
		t.Fatalf("stage[1] = %T; want *LineFmtExpr", pe.MultiStages[1])
	}
	if lfm.Value != "{{.msg}}" {
		t.Errorf("LineFmtExpr.Value = %q; want %q", lfm.Value, "{{.msg}}")
	}
}

// TestParserShape_UnpackStage pins `*LineParserExpr{Op:
// OpParserTypeUnpack}` for `| unpack`.
func TestParserShape_UnpackStage(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} | unpack`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	if len(pe.MultiStages) != 1 {
		t.Fatalf("len(MultiStages) = %d; want 1", len(pe.MultiStages))
	}
	lp, ok := pe.MultiStages[0].(*syntax.LineParserExpr)
	if !ok {
		t.Fatalf("stage[0] = %T; want *LineParserExpr", pe.MultiStages[0])
	}
	if lp.Op != syntax.OpParserTypeUnpack {
		t.Errorf("LineParserExpr.Op = %q; want %q", lp.Op, syntax.OpParserTypeUnpack)
	}
	if lp.Param != "" {
		t.Errorf("LineParserExpr.Param = %q; want \"\" (unpack takes no param)", lp.Param)
	}
}

// TestParserShape_PatternStage pins `*LineParserExpr{Op: pattern, Param:
// "<...>"}`.
func TestParserShape_PatternStage(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} | pattern "<_> <method> <_>"`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	lp, ok := pe.MultiStages[0].(*syntax.LineParserExpr)
	if !ok {
		t.Fatalf("stage[0] = %T; want *LineParserExpr", pe.MultiStages[0])
	}
	if lp.Op != syntax.OpParserTypePattern {
		t.Errorf("LineParserExpr.Op = %q; want %q", lp.Op, syntax.OpParserTypePattern)
	}
	if lp.Param != "<_> <method> <_>" {
		t.Errorf("LineParserExpr.Param = %q; want %q", lp.Param, "<_> <method> <_>")
	}
}

// TestParserShape_DropStage pins `*DropLabelsExpr` (with a bare-name
// labels list reachable via the exported Names() accessor) as the
// post-fetch label-projection stage shape cerberus's lowering relies
// on. If upstream relaxes the parser to accept additional matcher
// shapes or renames the accessor, this test surfaces the breakage.
func TestParserShape_DropStage(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} | drop env, pod`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	if len(pe.MultiStages) != 1 {
		t.Fatalf("len(MultiStages) = %d; want 1", len(pe.MultiStages))
	}
	dl, ok := pe.MultiStages[0].(*syntax.DropLabelsExpr)
	if !ok {
		t.Fatalf("stage[0] = %T; want *DropLabelsExpr", pe.MultiStages[0])
	}
	names := dl.Names()
	if len(names) != 2 || names[0] != "env" || names[1] != "pod" {
		t.Errorf("Names() = %v; want [env pod]", names)
	}
	if dl.HasNamedMatchers() {
		t.Errorf("HasNamedMatchers() = true; want false (bare-name list)")
	}
}

// TestParserShape_KeepStage pins `*KeepLabelsExpr` as the
// post-fetch projection sibling of DropLabelsExpr. The
// KeepLabelsExpr exposes its keep entries via Matchers(); cerberus's
// post-process applies them as map operations. The shape check confirms
// the parser surfaces the two bare keep names (catching regressions
// where the parser emits a malformed KeepLabelsExpr).
func TestParserShape_KeepStage(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `{app="api"} | keep job, env`)
	pe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("expected *PipelineExpr, got %T", expr)
	}
	if len(pe.MultiStages) != 1 {
		t.Fatalf("len(MultiStages) = %d; want 1", len(pe.MultiStages))
	}
	kl, ok := pe.MultiStages[0].(*syntax.KeepLabelsExpr)
	if !ok {
		t.Fatalf("stage[0] = %T; want *KeepLabelsExpr", pe.MultiStages[0])
	}
	matchers := kl.Matchers()
	if len(matchers) != 2 {
		t.Fatalf("Matchers() len = %d; want 2", len(matchers))
	}
	for _, m := range matchers {
		if m.Matcher != nil || (m.Name != "job" && m.Name != "env") {
			t.Errorf("unexpected keep entry %+v; want bare job/env", m)
		}
	}
}

// TestParserShape_BytesRate pins `*RangeAggregationExpr{Operation:
// OpRangeTypeBytesRate}` so the byte-counting code path stays
// observable.
func TestParserShape_BytesRate(t *testing.T) {
	t.Parallel()
	expr := mustParseLogQL(t, `bytes_rate({app="api"}[5m])`)
	rae, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("expected *RangeAggregationExpr, got %T", expr)
	}
	if rae.Operation != syntax.OpRangeTypeBytesRate {
		t.Errorf("Operation = %q; want %q", rae.Operation, syntax.OpRangeTypeBytesRate)
	}
	if rae.Left.Interval.Minutes() != 5 {
		t.Errorf("Interval = %v; want 5m", rae.Left.Interval)
	}
}

// Helper: assert two []string slices are equal by element.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParserError_* tests pin the error message substrings cerberus's
// /loki HTTP handlers translate into LogQL error responses.

func errContainsAny(err error, wants ...string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, w := range wants {
		if strings.Contains(msg, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

func parseShouldFailLogQL(t *testing.T, q string) error {
	t.Helper()
	_, err := syntax.ParseExpr(q)
	if err == nil {
		t.Fatalf("ParseExpr(%q): expected error, got nil", q)
	}
	return err
}

func TestParserError_UnterminatedSelector(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `{`)
	if !errContainsAny(err, "parse", "unexpected", "EOF", "syntax", "}") {
		t.Errorf("err = %q; want substring indicating unterminated selector", err)
	}
}

func TestParserError_MissingMatcherValue(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `{app=}`)
	if !errContainsAny(err, "parse", "unexpected", "syntax", "expected", "value") {
		t.Errorf("err = %q; want substring rejecting missing matcher value", err)
	}
}

func TestParserError_LineFilterMissingPattern(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `{app="x"} |= `)
	if !errContainsAny(err, "parse", "unexpected", "syntax", "EOF", "expected") {
		t.Errorf("err = %q; want substring rejecting missing line filter pattern", err)
	}
}

func TestParserError_UnknownParserStage(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `{app="x"} | invalid_stage`)
	if !errContainsAny(err, "parse", "unexpected", "syntax", "invalid_stage", "expected") {
		t.Errorf("err = %q; want substring rejecting unknown parser stage", err)
	}
}

func TestParserError_RateMissingRange(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `rate({app="x"})`)
	if !errContainsAny(err, "parse", "unexpected", "range", "syntax", "expected", "log range") {
		t.Errorf("err = %q; want substring rejecting missing range vector", err)
	}
}

func TestParserError_EmptyMatcherSet(t *testing.T) {
	t.Parallel()
	// Loki rejects fully-empty matcher sets at parse / validation time.
	err := parseShouldFailLogQL(t, `{}`)
	if !errContainsAny(err, "matcher", "empty", "regexp", "equality", "matchers") {
		t.Errorf("err = %q; want substring rejecting empty matcher set", err)
	}
}

func TestParserError_UnterminatedString(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `{app="api`)
	if !errContainsAny(err, "parse", "unexpected", "syntax", "EOF", "string", "expected") {
		t.Errorf("err = %q; want substring rejecting unterminated string", err)
	}
}

func TestParserError_UnterminatedRange(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `rate({app="api"}[`)
	if !errContainsAny(err, "parse", "unexpected", "syntax", "EOF", "duration", "expected") {
		t.Errorf("err = %q; want substring rejecting unterminated range", err)
	}
}

func TestParserError_AggregationWithoutUnwrap(t *testing.T) {
	t.Parallel()
	// Loki rejects unwrap-required range aggregations (`avg_over_time`,
	// `quantile_over_time`, `sum_over_time`, …) when there is no
	// `| unwrap` stage. cerberus depends on this validation to keep
	// its lowering simple — if upstream loosens it, the typed-value path
	// could be reached with a nil unwrap.
	err := parseShouldFailLogQL(t, `avg_over_time({app="x"}[5m])`)
	if !errContainsAny(err, "unwrap", "invalid aggregation", "parse", "without") {
		t.Errorf("err = %q; want substring rejecting unwrap-less typed aggregation", err)
	}
}

func TestParserError_MissingMatcherName(t *testing.T) {
	t.Parallel()
	err := parseShouldFailLogQL(t, `{="api"}`)
	if !errContainsAny(err, "parse", "unexpected", "syntax", "expected", "matcher") {
		t.Errorf("err = %q; want substring rejecting missing matcher name", err)
	}
}
