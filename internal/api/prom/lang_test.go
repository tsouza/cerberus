package prom

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// langForTest is shared scaffolding: a Lang built with the same
// experimental-functions parser options as the real Handler so the
// test surfaces match prod, plus a fixed eval window so `@ start() /
// @ end()` modifiers resolve deterministically when they appear in
// fixtures.
func langForTest() *lang {
	return &lang{
		Parser: promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true}),
		Schema: schema.DefaultOTelMetrics(),
		Start:  time.Unix(1_000, 0).UTC(),
		End:    time.Unix(2_000, 0).UTC(),
	}
}

func TestLang_Name(t *testing.T) {
	t.Parallel()
	if got := (&lang{}).Name(); got != "promql" {
		t.Errorf("Name(): got %q, want %q", got, "promql")
	}
}

// TestLang_Parse_SimpleSelector — a bare `up` query lowers to a Scan
// (no LabelMatchers) wrapped by a Filter (__name__ = "up"). The exact
// shape isn't asserted to keep the test resilient to lowering tweaks;
// only "got a plan + IsMetric flag set" is checked.
func TestLang_Parse_SimpleSelector(t *testing.T) {
	t.Parallel()

	l := langForTest()
	plan, meta, err := l.Parse(context.Background(), "up")
	if err != nil {
		t.Fatalf("Parse(`up`): unexpected err: %v", err)
	}
	if plan == nil {
		t.Fatalf("Parse(`up`): nil plan")
	}
	if !meta.IsMetric {
		t.Errorf("Meta.IsMetric: got false, want true (every PromQL query is metric-shaped)")
	}
}

// TestLang_Parse_ParseError — invalid syntax surfaces as a
// parseStageError tagged "parse". The handler-side classifier reads
// that tag to map to 400 bad_data; this test pins the tag itself so
// the upstream contract is observable.
func TestLang_Parse_ParseError(t *testing.T) {
	t.Parallel()

	l := langForTest()
	_, _, err := l.Parse(context.Background(), "up +")
	if err == nil {
		t.Fatalf("Parse(`up +`): expected parser failure, got nil")
	}
	var ps *parseStageError
	if !errors.As(err, &ps) {
		t.Fatalf("Parse(`up +`): err type = %T, want *parseStageError; err=%v", err, err)
	}
	if ps.stage != "parse" {
		t.Errorf("parseStageError.stage: got %q, want %q", ps.stage, "parse")
	}
}

// TestLang_Parse_LowerError — a parseable but unsupported PromQL form
// (`label_replace`, currently not lowered) surfaces as a
// parseStageError tagged "lower". Verifies the parse → lower split is
// preserved through the adapter.
func TestLang_Parse_LowerError(t *testing.T) {
	t.Parallel()

	l := langForTest()
	_, _, err := l.Parse(context.Background(), `label_replace(up, "x", "$1", "instance", "(.*)")`)
	if err == nil {
		t.Fatalf("Parse(label_replace): expected lower failure, got nil")
	}
	var ps *parseStageError
	if !errors.As(err, &ps) {
		t.Fatalf("Parse(label_replace): err type = %T, want *parseStageError; err=%v", err, err)
	}
	if ps.stage != "lower" {
		t.Errorf("parseStageError.stage: got %q, want %q (got err=%v)", ps.stage, "lower", err)
	}
	if !strings.Contains(err.Error(), "label_replace") {
		t.Errorf("err message: got %q, want it to mention label_replace", err.Error())
	}
}

// TestLang_ProjectSamples_WrapsCanonicalShape — Scan-rooted plans get
// the canonical (MetricName / Attributes / TimeUnix / Value) Project
// wrap. The check is structural: the result must be a *chplan.Project
// whose Projections slice has four entries.
func TestLang_ProjectSamples_WrapsCanonicalShape(t *testing.T) {
	t.Parallel()

	l := langForTest()
	plan := &chplan.Scan{Table: l.Schema.GaugeTable}
	wrapped := l.ProjectSamples(plan, engine.Meta{IsMetric: true})

	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples: got %T, want *chplan.Project", wrapped)
	}
	if got := len(proj.Projections); got != 4 {
		t.Errorf("Projections len: got %d, want 4 (MetricName/Attributes/TimeUnix/Value)", got)
	}
	if proj.Input != plan {
		t.Errorf("Project.Input: should reference the original plan unchanged")
	}
}
