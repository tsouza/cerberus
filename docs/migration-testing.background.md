# Migration-scenario end-to-end testing — background

This document collects the design rationale, rejected alternatives, incident
history, and measurements behind
[`migration-testing.md`](migration-testing.md). It answers "why is the lane
built this way" rather than "what does it assert" — nothing here is required to
run the lane, read a PASS assertion or add a scenario correctly;
`migration-testing.md` is self-sufficient for that.

## Why the lane is scheduled rather than a PR gate

The heavier tiers stand up multi-container stacks (Prometheus + OTel collector +
ClickHouse + cerberus, plus a shadow ruler + Alertmanager) and seed minutes of
rolling telemetry with target restarts. That is far too heavy and too slow to
sit on the required PR path.

## Why counting scenarios was rejected as evidence

`MODE=verify` walks feature FILES, so a scenario that never ran counts exactly
the same as one that passed. That is the shape that let a branch report "30/30
across 26/26 stories; 0 violations" while its five Tier-2 scenarios had never
executed once, their job skipped by a `needs:` cascade. `MODE=attest` — reading
each tier's run report back and holding every counted scenario to "appeared in
a report, with every step passed" — exists to close exactly that gap, which is
why enumerated and attested coverage are reported as two different numbers
rather than one.

## What the PASS-assertion pin is compensating for

The ratchet derives its anchors *live* from `migration-testing.md`, which means
the anchor is editable in the same commit as the code it anchors. It checks a
scenario's tier tag against the **Tier(s)** column but never looked at the PASS
assertion's *text*, so narrowing a PASS cell was always a valid route to "full
coverage": weaken what the document demands, implement the weaker thing, stay
green. That happened twice in one session — MIG-23's "the old backend is kept
read-only as a historical tier" clause was deleted, and MIG-18's PASS assertion
was narrowed from an incumbent-vs-shadow notification-stream diff to a
single-ruler lifecycle in the same commit that implemented the narrower thing.

MIG-23's split-router gap is the one that remains. MIG-18's dual-ruler gap was
the other; it closed when the Tier-2 substrate grew its incumbent leg — a second
ruler, its own Alertmanager and its own receiver — and the pin moved in the same
diff as the scenarios that earned it.

## Why MIG-18's timing skew is bounded rather than asserted zero

Observed live, the two rulers fire the same correct alert about three seconds
apart, which straddles a 10s quantization boundary roughly as often as not — so
demanding zero quantized skew would fail on a healthy substrate.

## What MIG-19's oracle, rounding and fixture were fixed against

Recovering the incumbent's own recording instants rounds to the nearest
millisecond. Skipping that rounding made a re-evaluation land 77ns off the
ruler's own instant and, over the ramp, showed up as a 2.7e-06 divergence
against a 1e-09 epsilon.

The source series is seeded with a ramped counter rate because the earlier
fixture accrued exactly one idle-second per wall-clock second, so
`1 - rate(...)` was 0.0 everywhere and "landed equals re-evaluated" held whether
or not the write-back path preserved anything.

The per-scenario `seed_scope` label exists because MIG-09's seed and
MIG-13/MIG-19's seed otherwise interleave into a single non-monotonic series.
That was observed on the first live Tier-2 run as a landed sample of `0.05`
against a re-evaluation of `-27.8`, a negative CPU utilisation.

## The phased build order the lane landed in

The harness code, compose files, seeders and workflow landed in follow-up build
PRs in the phase order below.

Cheapest-first, so value lands before the heavy infra, and each phase's
assertions become the trust anchor the next depends on.

**Phase 1 — Tier-0 offline (build first).** The `godog` runner + the step
library + `test/e2e/migration/cmd/scenarios/` + the coverage ratchet +
`migration-e2e.mjs` + the
`migration-e2e.yml` skeleton running Tier-0 only. Scenarios MIG-01, MIG-03,
MIG-04, MIG-05, MIG-10 (render half), MIG-14 (lookback compute), MIG-26 (gate
compute), plus the `gate` fold. Lands the eight archetype `rules/` +
`dashboards/` + `expected/` fixtures (the seed telemetry generators come in
Phase 2). Dependencies: none beyond the merged CLI — no Docker, seconds to run,
so it ships and starts catching regressions immediately.

These seven scenarios are entirely predicate kinds 1 and 2, so Phase 1 also
fixes the scenario language cheaply: it proves the tag vocabulary, the
strict-mode + `.feature` lint discipline, and the "relation named in prose,
arithmetic in Go" split against real scenarios before the heavier tiers commit
to them. `tolerances/` stays empty until Phase 2 — the first ε is derived from a
measured margin on a live backend, never guessed ahead of one.

**Phase 2 — Tier-1 dual-backend.** `docker-compose.dual.yml` (Prometheus,
Loki, Tempo, OTel collector, ClickHouse, cerberus) + collector config + the
per-archetype `seed/` declarations (incl. the pod-restart counter-reset +
`container_id`-churn shape). Scenarios MIG-02, MIG-06, MIG-07, MIG-08, MIG-10
(diff half), MIG-11, MIG-12, MIG-13 (read-back half), MIG-14 (live TTL),
MIG-15, MIG-16, MIG-17, MIG-20, MIG-21, MIG-22, MIG-23, MIG-25, MIG-26 (live
TTL). Dependencies: Phase-1 corpora
feed `verify --corpus`; reuses e2e.yml's free-disk-space + docker-hub-login +
log-dump patterns. MIG-08's faults are `docker compose kill/pause/stop` on the
compose stack — the Layer-13 `chaos-run.mjs` primitives are k3d/NetworkPolicy
and do not apply to a compose substrate.

**Phase 3 — Tier-2 ruler.** `docker-compose.ruler.yml` extending the dual stack
with a query-only external ruler → cerberus and a dead-end Alertmanager.
Scenarios MIG-09, MIG-13 (write-back half), MIG-18, MIG-19 (write-back timing),
MIG-24. Dependencies: the recording-rule landing zone from Phase 2, and —
critically — firing parity cannot be proven before query parity: MIG-18/24 gate
on MIG-16/17 being green (`migration-tier2 needs migration-tier1`), the same
"ruler-first only after result parity" ordering the stories themselves demand.
