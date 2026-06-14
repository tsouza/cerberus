package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

func parseExperimental(t *testing.T, q string) parser.Expr {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	return expr
}

// TestLower_Info_DefaultTargetInfo pins the bare `info(v)` lowering: the
// info side defaults to the target_info metric, the join keys on the
// hard-coded identifying labels (instance, job), and the output Attributes
// merge the info data labels UNDER the base labels (mapConcat with base
// last → base wins).
func TestLower_Info_DefaultTargetInfo(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	node, err := promql.Lower(context.Background(), parseExperimental(t, `info(up)`), s)
	if err != nil {
		t.Fatalf("Lower(info(up)): %v", err)
	}
	sql, args, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	for _, want := range []string{
		"LEFT JOIN",
		"mapConcat(R.`_info_data`, B.`Attributes`)",
		"mapFilter((k, v) -> k IN (?, ?), `Attributes`) AS `_info_sig`",
		"GROUP BY _info_sig",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("info(up) SQL missing %q\nSQL: %s", want, sql)
		}
	}
	// The default info metric is target_info (+ its dotted alias).
	if !argsContain(args, "target_info") {
		t.Errorf("info(up) args missing target_info default: %v", args)
	}
	// instance + job are the identifying labels (appear on both the
	// signature build and the join key).
	if !argsContain(args, "instance") || !argsContain(args, "job") {
		t.Errorf("info(up) args missing identifying labels: %v", args)
	}
}

// TestLower_Info_DataLabelSelector pins the optional `{…}` selector path:
// a non-__name__ matcher both constrains which info series participate
// (it lands as a Filter on the info-side scan) and restricts which data
// labels are copied (it adds a `k IN (…)` conjunct to the info data-label
// mapFilter).
func TestLower_Info_DataLabelSelector(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	node, err := promql.Lower(context.Background(), parseExperimental(t, `info(up, {version=~".+"})`), s)
	if err != nil {
		t.Fatalf("Lower(info data selector): %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// data-label filter conjunct on the info side
	if !strings.Contains(sql, "AND k IN (?)") {
		t.Errorf("data-label selector did not restrict copied labels by name\nSQL: %s", sql)
	}
	// the matcher also filters the info-side scan
	if !strings.Contains(sql, "match(`Attributes`[?], ?)") {
		t.Errorf("data-label matcher did not filter the info-side scan\nSQL: %s", sql)
	}
}

// TestLower_Info_ExplicitNameMatcher pins that a `{__name__="X"}` selector
// overrides the default target_info metric without becoming a data-label
// filter (a __name__ matcher selects the info metric, it never copies a
// label).
func TestLower_Info_ExplicitNameMatcher(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	node, err := promql.Lower(context.Background(), parseExperimental(t, `info(up, {__name__="build_info"})`), s)
	if err != nil {
		t.Fatalf("Lower(info explicit name): %v", err)
	}
	_, args, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !argsContain(args, "build_info") {
		t.Errorf("explicit __name__ matcher did not select build_info: %v", args)
	}
	if argsContain(args, "target_info") {
		t.Errorf("explicit __name__ matcher should override the target_info default: %v", args)
	}
}

// TestLower_Info_RejectsRegexNameMatcher pins the residual-risk boundary:
// the reference's effectiveInfoNameMatchers synthesis (regex / negated /
// multi-metric info-name selection) is outside this lowering's parity
// envelope, so a non-equality __name__ matcher is rejected with a clear
// error rather than silently mis-answering.
func TestLower_Info_RejectsRegexNameMatcher(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	_, err := promql.Lower(context.Background(), parseExperimental(t, `info(up, {__name__=~".+_info"})`), s)
	if err == nil {
		t.Fatal("expected regex __name__ info selector to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "equality match") {
		t.Errorf("unexpected rejection message: %v", err)
	}
}

func argsContain(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}
