package health

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// Layer 11 — per-head readiness. Each query head fronts ClickHouse through its
// own circuit breaker, so "can cerberus serve" is a per-head question. The probe
// answers it two ways: it NAMES every served head's phase in the body, and it
// takes the pod out of its Service once every head it serves is OPEN.

// readyzHeads decodes the /readyz body's `heads` object and the status code.
func readyzHeads(t *testing.T, h *Handler) (map[string]any, int) {
	t.Helper()
	rec := serveReadyz(t, h)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal /readyz body: %v (body=%q)", err, rec.Body.String())
	}
	heads, _ := body["heads"].(map[string]any)
	return heads, rec.Code
}

// staticHeads adapts a fixed phase map into a HeadBreakersFunc.
func staticHeads(states map[string]string) HeadBreakersFunc {
	return func() map[string]string { return states }
}

// TestReadyz_NamesEveryServedHeadsPhase is the observability half of the
// contract: a tripped head has to be visible ON the probe, not only in metrics.
// A probe body that reports an aggregate ClickHouse "ok" while one head is
// fast-failing every query tells an operator the opposite of what is happening.
func TestReadyz_NamesEveryServedHeadsPhase(t *testing.T) {
	h := New(Options{
		Pinger:      &stubPinger{},
		SchemaReady: func() bool { return true },
		HeadBreakers: staticHeads(map[string]string{
			"prom":  "open",
			"loki":  "closed",
			"tempo": "half-open",
		}),
		CacheTTL: -1,
	})

	heads, code := readyzHeads(t, h)
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — one tripped head must not evict a pod still serving two", code)
	}
	want := map[string]string{"prom": "open", "loki": "closed", "tempo": "half-open"}
	if len(heads) != len(want) {
		t.Fatalf("heads = %v; want %v", heads, want)
	}
	for name, phase := range want {
		if heads[name] != phase {
			t.Errorf("heads[%s] = %v; want %q", name, heads[name], phase)
		}
	}
}

// TestReadyz_NotReadyWhenEveryServedHeadIsOpen is the eviction half: with every
// head fast-failing, the pod can answer nothing and belongs out of its Service.
// Under the split deployment mode (one head per Deployment) this IS "this head's
// breaker is OPEN", which is the case the per-head gate exists for.
func TestReadyz_NotReadyWhenEveryServedHeadIsOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		heads map[string]string
	}{
		{"split mode, the one served head is open", map[string]string{"loki": "open"}},
		{"combined mode, all three open", map[string]string{"prom": "open", "loki": "open", "tempo": "open"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(Options{
				Pinger:       &stubPinger{},
				SchemaReady:  func() bool { return true },
				HeadBreakers: staticHeads(tc.heads),
				CacheTTL:     -1,
			})

			heads, code := readyzHeads(t, h)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d; want 503 with every served head open", code)
			}
			// The body still has to say WHICH heads, or the operator learns only
			// that the pod is unready and not why.
			if len(heads) != len(tc.heads) {
				t.Errorf("heads on the 503 = %v; want %v", heads, tc.heads)
			}
		})
	}
}

// TestReadyz_HalfOpenHeadIsNotExhaustion — a HALF-OPEN head admits a recovery
// probe, so it is recovering rather than failing. Counting it as exhaustion
// would evict a pod at the exact moment it is coming back.
func TestReadyz_HalfOpenHeadIsNotExhaustion(t *testing.T) {
	h := New(Options{
		Pinger:       &stubPinger{},
		SchemaReady:  func() bool { return true },
		HeadBreakers: staticHeads(map[string]string{"prom": "half-open"}),
		CacheTTL:     -1,
	})

	if _, code := readyzHeads(t, h); code != http.StatusOK {
		t.Fatalf("status = %d; want 200 for a single recovering head", code)
	}
}

// TestReadyz_HeadsOmittedWhenUnwired — with no HeadBreakersFunc the body carries
// no `heads` key at all, and an empty report is never read as exhaustion. A
// process that reports no heads has nothing to exhaust; evicting on the strength
// of an unwired probe would take down every pod at once.
func TestReadyz_HeadsOmittedWhenUnwired(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   HeadBreakersFunc
	}{
		{"nil func", nil},
		{"empty map", staticHeads(map[string]string{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(Options{
				Pinger:       &stubPinger{},
				SchemaReady:  func() bool { return true },
				HeadBreakers: tc.fn,
				CacheTTL:     -1,
			})

			rec := serveReadyz(t, h)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200", rec.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, present := body["heads"]; present {
				t.Errorf("body carries a heads key with no breakers wired: %v", body)
			}
		})
	}
}

// TestReadyz_HeadsReportedOnEveryFailureShape — an operator debugging a red
// probe needs the per-head phases whichever condition tipped it, so the `heads`
// object is stamped on the CH-failure and schema-gated responses too.
func TestReadyz_HeadsReportedOnEveryFailureShape(t *testing.T) {
	heads := map[string]string{"prom": "open", "loki": "closed"}
	pingFailed := errors.New("dial tcp 10.0.0.7:9000: connect: connection refused")

	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"ch ping fails", Options{
			Pinger:       &stubPinger{err: pingFailed},
			HeadBreakers: staticHeads(heads),
			CacheTTL:     -1,
		}},
		{"schema absent", Options{
			Pinger:        &stubPinger{},
			SchemaPresent: func() (bool, string) { return false, "table otel_logs absent" },
			HeadBreakers:  staticHeads(heads),
			CacheTTL:      -1,
		}},
		{"schema pending", Options{
			Pinger:       &stubPinger{},
			SchemaReady:  func() bool { return false },
			HeadBreakers: staticHeads(heads),
			CacheTTL:     -1,
		}},
		{"no pinger configured", Options{
			HeadBreakers: staticHeads(heads),
			CacheTTL:     -1,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, code := readyzHeads(t, New(tc.opts))
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d; want 503", code)
			}
			if len(got) != len(heads) || got["prom"] != "open" || got["loki"] != "closed" {
				t.Errorf("heads = %v; want %v", got, heads)
			}
		})
	}
}

// TestReadyz_HeadsAreReReadPerProbe — the phases must be read on each
// uncached probe, so a head that trips (or recovers) after boot moves the body
// and the status without a restart.
func TestReadyz_HeadsAreReReadPerProbe(t *testing.T) {
	states := map[string]string{"prom": "closed"}
	h := New(Options{
		Pinger:       &stubPinger{},
		SchemaReady:  func() bool { return true },
		HeadBreakers: func() map[string]string { return states },
		CacheTTL:     -1,
	})

	if heads, code := readyzHeads(t, h); code != http.StatusOK || heads["prom"] != "closed" {
		t.Fatalf("before trip: code=%d heads=%v; want 200 / prom closed", code, heads)
	}

	states = map[string]string{"prom": "open"}
	if heads, code := readyzHeads(t, h); code != http.StatusServiceUnavailable || heads["prom"] != "open" {
		t.Fatalf("after trip: code=%d heads=%v; want 503 / prom open", code, heads)
	}

	states = map[string]string{"prom": "closed"}
	if heads, code := readyzHeads(t, h); code != http.StatusOK || heads["prom"] != "closed" {
		t.Fatalf("after recovery: code=%d heads=%v; want 200 / prom closed", code, heads)
	}
}
