# e2e Playwright helpers

Phase-0 foundation for the dashboard-iteration sweep described in
`~/.claude/plans/e2e-enhance.md`. This directory contains pure helpers;
no spec lives here. The phase-1+ specs that consume these helpers
land in subsequent PRs.

## Modules

| Module           | Responsibility                                                                                                                                                                                        |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `dashboard.ts`   | Enumerate provisioned dashboards via `/api/search` + `/api/dashboards/uid/<uid>`, flatten rows, expose `Dashboard` + `Panel` types.                                                                   |
| `query-shape.ts` | Regex-based target classification + rewriting: `extractByKeys`, `extractWithoutKeys`, `expectedByKeys`, `isHistogramQuantile`, `extractHistogramName`, `addLabelFilter`, `expressionHasMatcherFor`.   |
| `assertions.ts`  | Per-shape assertions over the Grafana `/api/ds/query` envelope (`assertLabelShape` / `assertLabelAbsent` / histogram pair / `assertSubsetByCount`) + the zero-404 gate (`assertNon200ResponseClass`). |
| `sweep.ts`       | `generateSelfTraffic` — pre-step that fires self-traffic against cerberus so the cerberus dashboards have data to render.                                                                             |
| `drilldown.ts`   | Drilldown-app catalogue (4 built-in apps) + `drillTwoLevels` gesture driver + `isAppInstalled` (`/api/plugins/<id>/settings` probe so the iteration handles apps that aren't provisioned).            |
| `dom.ts`         | Browser-side helpers: console-error capture, `role="alert"` banner read, kiosk repaint-flicker tolerance.                                                                                             |
| `probes.ts`      | `fetchAndAssert200` (the zero-404 gate on direct HTTP probes) + `extractDataSourceProxyURL` (panel → datasource proxy path).                                                                          |

Re-exported in lockstep via `helpers/index.ts`.

## Phase specs that will consume these

Phase 0 (this PR) ships helpers only. Each later phase lands one new
spec under `test/e2e/playwright/` that wires the helpers into a
concrete iteration:

| Phase | Spec file                            | Helpers it consumes                                                                                                                                 |
| ----- | ------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1     | `iterate-panel-shape.spec.ts`        | `dashboard`, `query-shape`, `assertions`, `sweep`, `probes`                                                                                         |
| 2     | (extends `compose_panel_shape`)      | `assertions.assertHistogramComplete`, `assertions.assertNoFabricatedValue`, `probes`                                                                |
| 3     | `iterate-filter-drill.spec.ts`       | `dashboard`, `query-shape` (`addLabelFilter`, `expressionHasMatcherFor`, `expectedByKeys`), `assertions` (`assertSubsetByCount`), `sweep`, `probes` |
| 4     | `compose_panel_kiosk.spec.ts`        | `dashboard`, `dom`, `assertions`                                                                                                                    |
| 5     | `iterate-time-ranges.spec.ts`        | `dashboard`, `query-shape`, `assertions`, `sweep`, `probes` — nightly-only (e2e.yml `dashboard` job), NOT compose-smoke (Q2)                        |
| 6     | `iterate-drilldown-apps.spec.ts`     | `drilldown` (4-app catalogue + `isAppInstalled`), `dom`, `probes`                                                                                   |

The existing `compose_grafana_smoke.spec.ts` is untouched in this PR;
phase 1 will retire its bespoke `driveCerberusQLPartition` /
`driveSeverityPartition` helpers in favour of the generic
`assertLabelShape` rule.

## Pinned Grafana version

The compose stack pins `grafana/grafana:11.4.0` (see
`docker-compose.yml`). The drilldown-app catalogue in `drilldown.ts`
and the panel-schema flattening in `dashboard.ts` both assume the
Grafana 11.x dashboard JSON shape:

- Rows nest their contents under `panel.panels[]`.
- Panel headers expose `data-testid="data-testid Panel header <title>"`.
- Drilldown-app affordances expose stable `data-testid` prefixes
  (`data-testid metric-select`, `data-testid detected-label`, …).
- Grafana 11.4.0 ships `grafana-metricsdrilldown-app`,
  `grafana-lokiexplore-app`, and `grafana-exploretraces-app`
  preinstalled+enabled out of the box. `grafana-pyroscope-app` is
  NOT preinstalled on the cerberus compose stack — the phase-6 spec
  uses `isAppInstalled` to detect availability and annotates a
  cleanly missing app instead of failing.

**Bumping Grafana requires updating the phase specs in the same PR**
(resolved decision Q4, `~/.claude/plans/e2e-enhance.md` §9). The
maintenance tax is the price of full UI coverage.

When bumping:

1. Update `docker-compose.yml` (and any k3d/manifests that pin the
   tag).
2. Re-audit `dashboard.ts` for panel-schema drift (`/api/dashboards/uid`).
3. Re-audit `drilldown.ts`'s testid selectors against the new
   Grafana's @grafana/e2e-selectors. The literal `data-testid`
   prefix on every testid value is a Grafana convention, not a
   typo — preserve it.
4. Re-run every phase spec locally (`just e2e-up && just e2e-run`).

## Hard policies

### Q5 — zero 404 toleration

`assertNon200ResponseClass` does NOT carry a tolerated-status allow
list. The legacy `isKnownTolerated404` in
`compose_grafana_smoke.spec.ts` was retired in task #230 (PR
`test/e2e-phase-7-retire-404-allow-list`) once its last two entries
(`/api/v1/rules` + `/api/v1/alerts`) began returning 200 after PR #632
merged. Every non-2xx during a sweep is a failure; the fix is either
to implement the endpoint or to remove that surface from the
iteration, not to extend the allow-list.

### Q3 — filter-drill subset rule is ≤ baseline count

The phase-3 filter-drill (`{label="value"}` re-query) asserts the
filtered result is non-empty AND its series count is `≤ baseline`.
Element-wise strict-subset is order-dependent and flaked under
reorderings — the count comparator is the load-bearing gate.

## Smoke-validation

`helpers.spec.ts` (sibling) exercises each public function on a
happy path. Run it via `just e2e-up && cd test/e2e/playwright && npx
playwright test helpers.spec.ts`. The smoke gates that the helper
contracts compile + work against a live compose stack before any
phase spec consumes them.
