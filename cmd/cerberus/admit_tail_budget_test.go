package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// admitTestConfig is the minimal Config the limiter + Loki-handler
// bootstrap reads: the four admission caps plus the log schema the
// handler is built against.
func admitTestConfig(a config.AdmitConfig) config.Config {
	return config.Config{
		Admit: a,
		Logs:  schema.DefaultOTelLogs(),
	}
}

// TestNewAdmitLimiters_TailIsItsOwnBudget pins the process-level half of
// the fix for issue #1482: CERBERUS_ADMIT_TAIL must produce a limiter
// that is a DIFFERENT semaphore from the Loki request budget, not a
// second reference to it and not a nil that silently falls back.
//
// The whole starvation bug is one shared semaphore, so pointer identity
// is the property worth asserting here — a wiring that aliased the two
// would satisfy every cap-arithmetic assertion in the package while
// reproducing the bug exactly.
func TestNewAdmitLimiters_TailIsItsOwnBudget(t *testing.T) {
	cfg := admitTestConfig(config.AdmitConfig{
		Prom:  config.DefaultAdmitProm,
		Loki:  config.DefaultAdmitLoki,
		Tempo: config.DefaultAdmitTempo,
		Tail:  config.DefaultAdmitTail,
	})

	limiters := newAdmitLimiters(cfg, quietLogger())

	if limiters.lokiTail == nil {
		t.Fatal("lokiTail limiter is nil — CERBERUS_ADMIT_TAIL built no budget, leaving /tail unadmitted")
	}
	if limiters.loki == nil {
		t.Fatal("loki limiter is nil")
	}
	if limiters.lokiTail == limiters.loki {
		t.Fatal("lokiTail and loki are the same *admit.Limiter — one semaphore means tails still starve queries (#1482)")
	}
	if got := limiters.lokiTail.Budget(); got != admit.BudgetTail {
		t.Errorf("lokiTail.Budget() = %q, want %q", got, admit.BudgetTail)
	}
	if got := limiters.loki.Budget(); got != admit.BudgetRequest {
		t.Errorf("loki.Budget() = %q, want %q", got, admit.BudgetRequest)
	}
	// Both budgets belong to the Loki head, so the existing per-head
	// telemetry keeps resolving; only `budget` tells them apart.
	if got := limiters.lokiTail.Head(); got != "loki" {
		t.Errorf("lokiTail.Head() = %q, want \"loki\"", got)
	}
}

// TestNewAdmitLimiters_TailCapIsIndependentOfLokiCap pins that the two
// caps are read from their own config fields. A tail cap derived from
// (or clamped to) the Loki cap would make CERBERUS_ADMIT_TAIL cosmetic.
func TestNewAdmitLimiters_TailCapIsIndependentOfLokiCap(t *testing.T) {
	// Tail budget 1, Loki request budget 2. Drain the tail budget; the
	// request budget must still hand out both of its slots.
	limiters := newAdmitLimiters(admitTestConfig(config.AdmitConfig{
		Loki: 2,
		Tail: 1,
	}), quietLogger())

	rel, ok := limiters.lokiTail.Acquire(t.Context())
	if !ok {
		t.Fatal("tail acquire 1: want ok")
	}
	defer rel()
	if _, ok := limiters.lokiTail.Acquire(t.Context()); ok {
		t.Fatal("tail acquire 2: want reject — the tail cap of 1 is not being applied")
	}

	for i := range 2 {
		relQ, ok := limiters.loki.Acquire(t.Context())
		if !ok {
			t.Fatalf("loki acquire %d with the tail budget drained: want ok (#1482)", i)
		}
		defer relQ()
	}
}

// TestNewAdmitLimiters_DisabledNilsEveryBudget pins that the master
// switch reaches the new budget too. A CERBERUS_ADMIT_DISABLED=true
// process that still built a tail limiter would cap live-tailing on a
// deployment that asked for no admission control at all.
func TestNewAdmitLimiters_DisabledNilsEveryBudget(t *testing.T) {
	limiters := newAdmitLimiters(admitTestConfig(config.AdmitConfig{
		Disabled: true,
		Prom:     config.DefaultAdmitProm,
		Loki:     config.DefaultAdmitLoki,
		Tempo:    config.DefaultAdmitTempo,
		Tail:     config.DefaultAdmitTail,
	}), quietLogger())

	if limiters != (admitLimiters{}) {
		t.Fatalf("CERBERUS_ADMIT_DISABLED=true must nil every budget, got %+v", limiters)
	}
}

// TestNewAdmitLimiters_ZeroTailCapMeansUnadmitted pins the "0 ==
// unlimited" convention on the new knob, matching every other admission
// cap: CERBERUS_ADMIT_TAIL=0 (or `false`) builds no tail limiter, so
// /tail mounts pass-through rather than being capped at zero — which
// would reject every tail and break Live-tailing outright.
func TestNewAdmitLimiters_ZeroTailCapMeansUnadmitted(t *testing.T) {
	limiters := newAdmitLimiters(admitTestConfig(config.AdmitConfig{
		Loki: config.DefaultAdmitLoki,
		Tail: 0,
	}), quietLogger())

	if limiters.lokiTail != nil {
		t.Fatalf("tail cap 0 must build no limiter (unlimited), got %+v", limiters.lokiTail)
	}
	if limiters.loki == nil {
		t.Fatal("an unlimited tail budget must not disable the Loki request budget")
	}
}

// TestNewLokiHandler_WiresBothBudgets closes the loop from the limiters
// to the handler fields the Loki mux reads. newAdmitLimiters can build
// two perfectly distinct budgets and the bug still returns if the
// handler receives the request limiter in both slots.
func TestNewLokiHandler_WiresBothBudgets(t *testing.T) {
	cfg := admitTestConfig(config.AdmitConfig{
		Loki: config.DefaultAdmitLoki,
		Tail: config.DefaultAdmitTail,
	})
	limiters := newAdmitLimiters(cfg, quietLogger())

	h := newLokiHandler(lazyClient(t), cfg, chopt.EnabledSet{}, limiters.loki, limiters.lokiTail, quietLogger())

	if h.Limiter != limiters.loki {
		t.Errorf("Handler.Limiter = %p, want the loki request budget %p", h.Limiter, limiters.loki)
	}
	if h.TailLimiter != limiters.lokiTail {
		t.Errorf("Handler.TailLimiter = %p, want the tail budget %p", h.TailLimiter, limiters.lokiTail)
	}
	if h.TailLimiter == h.Limiter {
		t.Error("Handler.TailLimiter aliases Handler.Limiter — /tail would draw on the query budget again (#1482)")
	}
}
