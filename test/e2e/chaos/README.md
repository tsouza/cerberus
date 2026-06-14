# `test/e2e/chaos/` — live-stack chaos harness (Layer 12)

The robustness layer **above** Layer 10's deterministic stubbed-querier
unit chaos (in the required `check` lane): it fault-injects against the
**running k3d e2e stack** (cerberus + ClickHouse + Grafana + OTel
collector that `just e2e-up` stood up) and asserts the gateway's landed
resilience contracts hold under **real** faults.

This lane is **informational** — the `chaos` job in
`.github/workflows/e2e.yml` runs on push-to-main + nightly + manual
dispatch only, **never on a PR**, and is **not** a branch-protection
required check. k3d is heavy and chaos flakes; a regression is caught at
merge-to-main and reverted, not blocked pre-merge.

## Layout

- **`manifests/chaos-overlay.env`** — the resilience knobs patched onto
  the cerberus Deployment before fault injection (low breaker threshold,
  small `CERBERUS_QUERY_TIMEOUT`, small admit + pool caps) so faults trip
  fast and deterministically within budget. Applied by
  `just e2e-chaos-overlay` (`kubectl set env`).
- **`manifests/deny-egress-clickhouse.yaml`** — the deny-egress
  NetworkPolicy for the `ch-network-partition` scenario (blackhole
  cerberus → CH, enforced by k3s's bundled kube-router).
- **`manifests/netpol-enforcement-probe.yaml`** — the kube-router
  enforcement canary the partition scenario gates on, so it never passes
  vacuously when NetworkPolicy isn't enforced.
- The orchestration driver lives at **`.github/scripts/chaos-run.mjs`**
  (node ESM, `node:` builtins only — kubectl + `fetch`), per the repo's
  "non-trivial CI step logic lives in `.github/scripts/*.mjs`"
  convention. It owns fault inject/heal + bounded recovery polling + HTTP
  envelope assertions.

## Running locally

```sh
just e2e-up               # k3d cluster + cerberus:e2e + manifests
just e2e-seed-rolling     # deterministic rows + rolling reseed
just e2e-wait-otel        # wait for live OTel data
just e2e-chaos-overlay    # patch in the fast-trip resilience knobs
just e2e-chaos            # run the chaos lane (CHAOS_PHASE=phase-1 default)
just e2e-down             # tear down
```

Set `CHAOS_PHASE=all just e2e-chaos` to add the phase-2 scenarios
(`ch-network-partition`, `load-admit-saturation`), or
`CHAOS_SCENARIOS=ch-pod-kill just e2e-chaos` to run one in isolation.

## Scenarios + contracts

See [`docs/test-strategy.md`](../../../docs/test-strategy.md) Layer 12
for the full scenario list and the per-scenario contract map. In short:
`ch-pod-kill` (breaker trip + recovery, `/healthz`-green-on-CH-outage),
`ch-slow-query-timeout` (clean 503 `errorType=timeout`, breaker-neutral),
`cerberus-pod-kill` (replica resilience, ≥95 % aggregate success through
a single-replica kill), plus the phase-2 `ch-network-partition`
(enforcement-gated) and `load-admit-saturation` (clean shed, breaker
stays CLOSED). The handler-panic envelope (#885) is covered
deterministically by Layer 10; the live lane only corroborates a clean
end-of-run steady state.
