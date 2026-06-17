package chclient

// Head identifies the logical query head a HeadClient view fronts. Each Head
// owns its OWN circuit breaker instance in the per-Client registry, so one
// head's tripped breaker fast-fails (503) ONLY that head's queries and can
// never cascade to the others or to the readiness probe (#94). The value set
// is closed: the only Heads are the three data planes plus the readiness
// probe, declared as the consts below. An unknown Head is a wiring bug — the
// registry getter panics on a miss at New-time wiring rather than silently
// minting a garbage-keyed breaker at request time.
type Head string

const (
	// HeadProm fronts the Prometheus API (promql) data plane.
	HeadProm Head = "prom"
	// HeadLoki fronts the Loki API (logql) data plane.
	HeadLoki Head = "loki"
	// HeadTempo fronts the Tempo API (traceql) data plane.
	HeadTempo Head = "tempo"
	// HeadProbe fronts the readiness ping (/readyz). It is its OWN breaker,
	// driven ONLY by the low-rate, TTL-coalesced readiness pings — never by
	// data-plane traffic. That decouples "can cerberus reach ClickHouse at
	// all" (the only question readiness should ask) from "is one head's
	// workload melting ClickHouse": a prom-only query storm trips the prom
	// breaker, 503s prom queries, and leaves /readyz GREEN because the probe
	// breaker sees none of that traffic. A genuine total-CH outage still
	// fails the pings themselves and trips the probe breaker, so /readyz
	// still flips red and the pod is still evicted when it should be.
	HeadProbe Head = "probe"
)

// probeBreakerThreshold is the consecutive-failure budget for the HeadProbe
// breaker, tighter than the data-head default (breakerThreshold = 5). The
// readiness ping is low-rate: health.go coalesces concurrent k8s probes into
// one ping per ~2s TTL window, and the k8s readinessProbe in
// test/e2e/k3s/cerberus-values.yaml runs at periodSeconds=3 / failureThreshold=5
// (a 15s eviction budget). A total-CH outage now flips /readyz red ONLY via
// failed probe pings (decoupled from data-plane traffic, #94), so the probe
// breaker must trip well inside that 15s budget: at ~3s per ping a 3-failure
// budget trips in ~9s, leaving margin before k8s evicts. A data head keeps the
// looser 5-failure budget because its breaker is fed by high-rate query
// traffic, not the throttled probe stream. An explicit
// CERBERUS_CH_BREAKER_THRESHOLD override still wins for ALL heads (operators
// who tune it mean it); this default only applies when the knob is left zero.
const probeBreakerThreshold = 3

// allHeads is the closed set of heads the per-Client breaker registry is built
// over. The data heads come first so iteration order is stable for the
// zero-init telemetry pass (prom/loki/tempo/probe); ranging over a map would
// not be. Kept in one place so buildBreakers and any zero-init caller agree on
// the exact label set.
var allHeads = [...]Head{HeadProm, HeadLoki, HeadTempo, HeadProbe}

// String renders the head as its stable label value (the const string). It is
// the value stamped on the `head` attribute of cerberus_ch_breaker_state /
// _trips_total, so it must stay byte-stable across releases — dashboards pivot
// on it.
func (h Head) String() string { return string(h) }
