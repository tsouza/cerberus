package prom

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclient"
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

// TestLang_Parse_ResponseShape — Meta.ResponseShape is chclient.ResponseShapeMatrix
// only when Step is set (the /api/v1/query_range shape, the only PromQL path
// that reaches engine.QueryCursor / chclient's columnar decode), and left
// empty for the instant-query shape (Step == 0, executeInstant's Query path,
// which never reads ResponseShape). Pins the #1429 defense-in-depth signal
// at its source so a future edit can't silently stop declaring it.
func TestLang_Parse_ResponseShape(t *testing.T) {
	t.Parallel()

	t.Run("range query sets the matrix shape", func(t *testing.T) {
		l := langForTest()
		l.Step = 15 * time.Second
		_, meta, err := l.Parse(context.Background(), "up")
		if err != nil {
			t.Fatalf("Parse(`up`): unexpected err: %v", err)
		}
		if meta.ResponseShape != chclient.ResponseShapeMatrix {
			t.Errorf("Meta.ResponseShape: got %q, want %q", meta.ResponseShape, chclient.ResponseShapeMatrix)
		}
	})

	t.Run("instant query leaves the shape unset", func(t *testing.T) {
		l := langForTest()
		_, meta, err := l.Parse(context.Background(), "up")
		if err != nil {
			t.Fatalf("Parse(`up`): unexpected err: %v", err)
		}
		if meta.ResponseShape != "" {
			t.Errorf("Meta.ResponseShape: got %q, want empty (instant queries never reach QueryCursor)", meta.ResponseShape)
		}
	})
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

// TestLang_Parse_LowerError — a parseable but lower-stage-rejected PromQL
// form surfaces as a parseStageError tagged "lower". Verifies the parse →
// lower split is preserved through the adapter.
//
// The example is `topk(NaN, up)`: the parser type-checks it happily (NaN
// is a scalar literal), and the lowering rejects it because reference
// Prometheus itself errors on a NaN K ("Parameter value is NaN",
// promql/engine.go::rangeEvalAgg). That makes it a stable
// parse→lower-split example — it is a rejection PARITY case, so it can
// never be "implemented away" the way a merely-unsupported shape can.
//
// Earlier revisions keyed this on `first_over_time` /
// `double_exponential_smoothing`, and then on a computed-K `limitk`; all
// three now lower cleanly, so none of them can exercise the lower-error
// path any more.
func TestLang_Parse_LowerError(t *testing.T) {
	t.Parallel()

	l := langForTest()
	const q = `topk(NaN, up)`
	_, _, err := l.Parse(context.Background(), q)
	if err == nil {
		t.Fatalf("Parse(%q): expected lower failure, got nil", q)
	}
	var ps *parseStageError
	if !errors.As(err, &ps) {
		t.Fatalf("Parse(%q): err type = %T, want *parseStageError; err=%v", q, err, err)
	}
	if ps.stage != "lower" {
		t.Errorf("parseStageError.stage: got %q, want %q (got err=%v)", ps.stage, "lower", err)
	}
	if !strings.Contains(err.Error(), "K must not be NaN") {
		t.Errorf("err message: got %q, want it to mention the NaN-K rejection", err.Error())
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
