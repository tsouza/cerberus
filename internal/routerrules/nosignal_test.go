package routerrules

import (
	"context"
	"os"
	"testing"
)

// The empty-population fire-gate semantics. A corpus_percentile / corpus_agg
// param resolved over an EMPTY sub-population is NO SIGNAL, not a watermark of
// 0. For a >=/> fire-gate, normalizing empty to 0 would make the rule fire on
// everything (the inverse of safe), so a fire-gate with no signal must yield NO
// fire — the evaluator skips any rule that depends on it, and records why. A
// param used only in a finding MESSAGE (never as a condition operand) is not a
// fire-gate and keeps resolving an empty population to its 0/empty form.

// TestEmptyFireGateNoFire is the bug-fix proof: with zero route-B rows the
// route-B fanout floor (fanout_route_b_floor) has no signal, so the two rules
// that gate on it — route_a_high_fanout_should_shard (a >= gate that would
// otherwise fire on EVERY route-A row) and route_b_overshard_low_fanout (a <
// gate) — must NOT fire, and must be reported as skipped with a structured
// reason naming the no-signal param.
func TestEmptyFireGateNoFire(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	src := NewJSONLCorpusSource("testdata/effectiveness_no_route_b.jsonl", 0)
	rep, err := NewEvaluator(cat, effConfig(), src).
		Evaluate(context.Background(), EvalOptions{IncludeExperimental: true})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	gated := map[string]bool{
		"route_a_high_fanout_should_shard": true,
		"route_b_overshard_low_fanout":     true,
	}
	for _, f := range rep.Findings {
		if gated[f.RuleID] {
			t.Errorf("rule %q gates on the empty route-B fanout floor and must not fire, but it did on %q",
				f.RuleID, classOf(f.GroupKey))
		}
	}

	// Both fanout-floor rules must be reported as skipped for no signal, naming
	// the offending param — not silently dropped.
	skipped := map[string]SkippedRule{}
	for _, s := range rep.Skipped {
		skipped[s.RuleID] = s
	}
	for rule := range gated {
		s, ok := skipped[rule]
		if !ok {
			t.Errorf("rule %q must be reported as skipped (no fire-gate signal), skips=%+v", rule, rep.Skipped)
			continue
		}
		if len(s.Params) != 1 || s.Params[0] != "fanout_route_b_floor" {
			t.Errorf("rule %q skip should name fanout_route_b_floor, got %v", rule, s.Params)
		}
		if s.Reason == "" {
			t.Errorf("rule %q skip must carry a structured reason", rule)
		}
	}

	// Rules that don't depend on the empty floor still fire normally: the
	// route-A oom cluster is untouched by route-B emptiness.
	if firingClasses(rep, "oom_on_route_a")["language=promql,shape_id=prom:rate_sum_by_wide"] == 0 {
		t.Error("oom_on_route_a should still fire — it does not depend on the route-B fanout floor")
	}
}

// TestEmptyFireGateResolvesNoSignal pins the resolver half of the contract: with
// no route-B rows the scalar fire-gate watermark resolves to NoSignal (not 0),
// while the message-only count ratio still resolves to a finite scalar (0 is the
// correct "no rejections observed" value for message context).
func TestEmptyFireGateResolvesNoSignal(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	src := NewJSONLCorpusSource("testdata/effectiveness_no_route_b.jsonl", 0)
	env, err := NewParamResolver(effConfig(), src).Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	floor, ok := env["fanout_route_b_floor"]
	if !ok {
		t.Fatal("fanout_route_b_floor not resolved")
	}
	if !floor.NoSignal {
		t.Errorf("fanout_route_b_floor over an empty route-B population must be NoSignal, got Scalar=%v NoSignal=%v",
			floor.Scalar, floor.NoSignal)
	}

	// cerberus_reject_ratio: still resolves (message-only). Its numerator scope
	// (rejected rows) is non-empty here, so 0 < ratio; the point is it is never
	// marked NoSignal regardless of population.
	ratio, ok := env["cerberus_reject_ratio"]
	if !ok {
		t.Fatal("cerberus_reject_ratio not resolved")
	}
	if ratio.NoSignal {
		t.Error("message-only cerberus_reject_ratio must never be NoSignal")
	}
}

// TestMessageScalarEmptyResolvesZero proves the legitimate empty->0 path is
// preserved for a message scalar: a count ratio whose numerator population is
// empty resolves to a finite 0 (no rejections observed), never NoSignal. This is
// the case the fire-gate fix must NOT break.
func TestMessageScalarEmptyResolvesZero(t *testing.T) {
	// A corpus with route-A rows but zero rejected rows: the reject-ratio
	// numerator (exit_status=rejected) is empty, denominator (route A) is not.
	dir := t.TempDir()
	path := dir + "/no_rejected.jsonl"
	body := `{"event_time":1,"route":"A","exit_status":"ok"}
{"event_time":2,"route":"A","exit_status":"ok"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	src := NewJSONLCorpusSource(path, 0)
	as := AggSpec{
		CountRatio: true,
		NumScope:   Scope{"exit_status": "rejected"},
		DenScope:   Scope{"route": "A"},
	}
	v, err := src.Aggregate(context.Background(), as)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if v.NoSignal {
		t.Error("an empty-numerator count ratio is message context: it must resolve to 0, not NoSignal")
	}
	if v.Scalar != 0 {
		t.Errorf("empty-numerator count ratio = %v, want 0", v.Scalar)
	}
}
