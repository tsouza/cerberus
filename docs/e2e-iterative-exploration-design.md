# Iterative dashboard exploration for `compose_grafana_smoke.spec.ts`

Status: design / RFC. No implementation in this PR — the deliverable is
the structure, the helper inventory, and the gap analysis a reviewer
needs to approve before code lands.

## 1. Why this design exists

The existing `test/e2e/playwright/compose_grafana_smoke.spec.ts`
(~790 LoC, one big test fn) was grown organically: every time a manual
"corner sweep" of the dev stack uncovered a regression (Grafana
tunneling per-target errors, Tempo proto wire-format drift, dotted
OTel attributes collapsing partitions, …) the maintainer added a
bespoke check at the bottom of the spec.

Two recent rounds of manual gesture-driven sweeps surfaced **18 distinct
bugs** (henceforth `N1` … `N18` in this doc — the maintainer's internal
sweep IDs; each corresponds to one or more shipped PRs in the
`#617` … `#663` band). The shape of every Nx fix is the same:

> "When I clicked / typed / drilled / opened panel `P` on dashboard `D`,
> Grafana fired request `R` against datasource `DS`, the response came
> back `wrong-in-some-way`, and the panel rendered `degraded-but-not-red`."

The current spec is good at catching the **specific** failure that
already happened — every Nx fix landed a targeted assertion. It is
**bad** at catching the **next** Nx of the same class on a new panel
or a new variable substitution, because:

- The fixed-surfaces list (`fixedSurfaces` at L110) hand-encodes one
  hard-coded query per language. A new dashboard panel firing the same
  query shape gets zero coverage until someone notices and hand-codes
  another entry.
- The dashboard-surfaces loop only asserts `2xx + no tunneled error +
  no stuck loading + no red banner`. It does **not** read the
  dashboard's panels and assert that the **shape of the returned data
  matches what the panel asked for** — `sum by (X)` returning a single
  anonymous bucket is "200, no error, no spinner, no red banner" (it's
  N2 / N14, fixed in `#214` / `#657` / `#663`).
- Variable substitution, single-panel kiosk views, drilldown-app deep
  iteration, histogram completeness, and the no-fabricated-value rule
  are all "I'll add an assertion when it breaks" territory.

This doc proposes turning the gesture script into a **structural
iteration**: walk every dashboard, every panel, every target; classify
the query; apply the class-specific assertion; iterate filters /
variables / time ranges.

## 2. Current coverage map

What the spec checks today, by surface kind:

| Surface kind                  | Source                                                                                                | What's asserted                                                                                                                |
| ----------------------------- | ----------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `home`                        | `${baseURL}/`                                                                                         | HTTP 2xx on captured ds/query + dashboards + datasources/proxy + datasources/uid/.../resources. No tunneled per-target error.  |
| `app:lokiexplore`             | `/a/grafana-lokiexplore-app/explore?var-ds=cerberus-loki`                                             | Same wire-level assertions; covers `/detected_labels` + friends.                                                               |
| `explore:prom` (×2)           | hand-encoded Explore URLs — `up`, `cerberus_queries_total`                                            | Wire-level assertions only.                                                                                                    |
| `explore:loki`                | hand-encoded `{service_name=~".+"}`                                                                   | Wire-level assertions only.                                                                                                    |
| `explore:tempo`               | hand-encoded `{}`                                                                                     | Wire-level assertions only.                                                                                                    |
| `dash:<uid>` (each)           | enumerated via `/api/search?type=dash-db`                                                             | Wire-level assertions; stuck-loading sweep; panel-error banner sweep.                                                          |
| `health:cerberus-{prom,loki}` | `/api/datasources/uid/<uid>/health`                                                                   | 2xx + `status != ERROR`.                                                                                                       |
| `trace-click`                 | Click first `<a href*="/explore">` in "Slow cerberus traces" panel.                                   | Wire 2xx; v2 trace URL hit; tunneled-error sweep; `illegal wireType` / `plugin.downstreamError` DOM alert sweep.               |
| `partition:cerberus_ql`       | Locate "Query rate by language" panel; intercept its `/api/ds/query` body.                            | At least 2 grouped frames in the ds/query response (N2 / N14).                                                                 |

What the spec **does not** check today:

- Panel-level query introspection (it never reads `dashboard.panels[].targets[].expr`).
- Label-shape of `sum by (X)` / `count by (X)` responses for arbitrary panels.
- Filter drill-down: re-query with `{key="value"}` and assert the result is a non-empty strict subset.
- Variable substitution across multiple time ranges and steps.
- Single-panel `?viewPanel=N` kiosk rendering per panel.
- Drilldown-app deep iteration (Explore Metrics / Explore Logs / Explore Traces — only the boot is touched).
- `histogram_quantile` underlying `_bucket` series presence.
- No-fabricated-value rule for `histogram_quantile` over name patterns with no `_bucket` series.

## 3. Gap table — Nx → would the new iteration have caught it?

The Nx labels below match the maintainer's two-round sweep notes;
the PR column is the shipped fix.

| Nx   | Gesture that exposed it                                                                                | Fixed by        | Caught today?                                                                         | Caught by proposed iteration?                                    |
| ---- | ------------------------------------------------------------------------------------------------------ | --------------- | ------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| N1   | Click "Update branch" on stale stack → panels still spin                                               | #621            | YES (stuck-loading sweep)                                                             | YES (preserved)                                                  |
| N2   | Open cerberus-self → "Query rate by language" → legend is one anonymous "Value" bucket                 | #214 / #663     | YES (partition probe)                                                                 | YES — *generalised* via label-shape rule                         |
| N3   | Open Explore-Logs → click a stream label value → result is empty                                       | #623            | NO  (panel exists, no drill assertion)                                                | YES — filter-drill rule                                          |
| N4   | Open Explore-Traces → click any trace ID → "Query error: illegal wireType"                             | #650 / #208     | YES (trace-click flow)                                                                | YES (preserved)                                                  |
| N5   | Open cerberus-self → "P95 latency by language" → all series flat at 0                                  | #645 / #637     | NO  (panel 200s, no `_bucket` presence assertion)                                     | YES — histogram-completeness rule                                |
| N6   | Type `histogram_quantile(0.95, foo_total)` in Prom Explore → returns a non-empty synthetic float       | #644 / #642     | PARTIAL (a tunneled error sometimes surfaces, but a fabricated value does not)        | YES — no-fabricated-value rule                                   |
| N7   | Open any Prom Explore page → 502 on `/api/datasources/uid/.../resources/...`                           | #630 / #643     | YES (Explore prom surface in fixed list)                                              | YES (preserved)                                                  |
| N8   | Open Explore-Logs → CheckHealth probe `vector(1)+vector(1)` 500s                                       | #631            | YES (Loki ResourceAttributes scope; covered via Explore-Loki surface + /health probe) | YES (preserved)                                                  |
| N9   | Open Explore-Traces → buildinfo probe 404s                                                             | #633            | YES (Tempo buildinfo via Explore-Tempo surface)                                       | YES (preserved)                                                  |
| N10  | Open any Prom-backed dashboard → `/api/v1/rules` + `/api/v1/alerts` 404                                | #632            | YES (tolerated 404 list; tracked)                                                     | YES (preserved + tracked)                                        |
| N11  | Open cerberus-self → "Error rate by language" → single anonymous series (same class as N2 on a div)    | #214 / #657     | NO  (the partition probe only targets one panel)                                      | YES — label-shape rule applies to every `sum by`                 |
| N12  | Open Explore → switch time range from "Last 1h" to "Last 5m" → some panels go empty                    | (latent)        | NO  (no variable / time-range iteration)                                              | YES — variable substitution rule                                 |
| N13  | Hover-click panel title → "View" kiosk → panel renders but with a different layout that errors         | (latent)        | NO  (no single-panel kiosk pass)                                                      | YES — kiosk view rule                                            |
| N14  | Open cerberus-self → "Admission rejections" → `sum by (cerberus_ql, reason)` partitions by reason only | #214 / #663     | NO  (partition probe is single-panel)                                                 | YES — label-shape rule asserts EVERY `by ()` key is present      |
| N15  | Open Explore-Metrics → click "service_name" facet → drill empty                                        | (latent)        | NO  (drilldown-app boot only)                                                         | YES — drilldown-app iteration                                    |
| N16  | Open trace view → ms-vs-ns timestamp split flips                                                       | #643            | YES (Explore-Loki / Tempo surface)                                                    | YES (preserved)                                                  |
| N17  | Provision a new dashboard → it isn't picked up                                                         | (n/a — dynamic) | YES (`/api/search` enumeration)                                                       | YES (preserved)                                                  |
| N18  | Loki stuck-loading on `{}` matcher                                                                     | #649            | PARTIAL (stuck-loading sweep catches the spinner, not the wrong-matcher root cause)   | YES — empty-set-is-allowed-but-tunneled-error-is-not (preserved) |

**Summary**:

- Today's spec covers N1, N2 (one panel), N4, N7-N10, N16, N17 outright; partial on N6, N18.
- The proposed iteration covers all of the above **plus** N3, N5, N11, N12, N13, N14, N15 — the "next gesture" class.

## 4. Proposed structure

### 4.1 Top-level flow

```text
1. Enumerate dashboards via /api/search?type=dash-db.
2. For each dashboard:
     a. Fetch /api/dashboards/uid/<uid> to get the full JSON
        (panels[] + templating.list — `/api/search` only returns titles).
     b. Build a Variable matrix from templating.list — keep two
        canonical instantiations per variable (e.g. first option +
        wildcard) plus the dashboard's default.
     c. For each (variable-binding, time-range, step) tuple in a small
        explicit matrix (≤ 4 combinations: default, [5m, 30s], [1h,
        15s], [24h, 1m]):
          i.   Navigate to /d/<uid>?var-X=<v>&from=<from>&to=<to>.
          ii.  Capture every /api/ds/query + /api/datasources/proxy/* +
               /api/datasources/uid/*/resources/* response (existing
               sweep, kept).
          iii. For each panel:
                 - Read panel.targets[]; route each target to its
                   classified shape (sum-by / count-by / histogram /
                   plain rate / log range / log instant / traceql).
                 - Apply the per-shape assertion (§4.2).
          iv.  Run the existing stuck-loading + panel-error sweeps.
     d. For each panel (single time-range pass — kiosk doesn't need
        variable cross-product):
          i.   Navigate to /d/<uid>?viewPanel=<panelID>.
          ii.  Re-run wire + stuck-loading + panel-error sweeps.
          iii. ESC / back; assert the back-nav is clean.
3. For each Grafana drilldown app
   (grafana-metricsdrilldown-app, grafana-lokiexplore-app,
   grafana-exploretraces-app — exact path catalogue in §6):
     a. Open the app root.
     b. Pick the first selectable facet / dimension / service.
     c. Drill one level (click).
     d. Drill a second level (click).
     e. Run wire + DOM sweeps after each click.
4. Existing post-passes — datasource-health probes, trace-click flow.
5. Aggregate failures, emit single `expect.soft` + throw (preserved).
```

### 4.2 Per-shape assertion rules

Targets are classified by a small regex pass on `target.expr` /
`target.query`. The classifier dispatches on these shapes (regex
shown in pseudo-form for readability — the spec lives in
`helpers/query-shape.ts`):

- **Label-shape** — `(sum|count|avg|min|max) by (K1, K2, ...)`.
  *Assertion*: for every `by (K1, K2, …)` key list in the expr, the
  ds/query response's frame schemas must carry labels `K1`, `K2`, … on
  at least one frame; no frame may be the bare anonymous `"Value"`
  frame with zero labels. (Catches N2 / N11 / N14.)
- **Histogram** — `histogram_quantile(...)` over a name whose
  `<name>_bucket` series exists. *Assertion*: (a) the metric-name
  argument's `<name>_bucket` series MUST be present in the dataset
  (probe `/api/v1/series?match[]={__name__="<name>_bucket"}` returns
  ≥ 1); (b) the histogram_quantile response MUST be non-empty.
  (Catches N5.)
- **Histogram-without-buckets** — as above, but `_bucket` series
  absent. *Assertion*: the histogram_quantile response MUST be empty
  (no frames, no synthetic float). (Catches N6.)
- **Plain rate** — `rate(...)` with no aggregator wrapper.
  *Assertion*: response is non-empty for windows ≥ 1m and step ≤
  window.
- **Log range / instant** — LogQL stream selector
  (`{k="v"}`). *Assertion*: stream-selector matchers normalise
  OTel-dotted keys to underscored grammar (N3 / N18 root cause);
  response is well-formed.
- **TraceQL** — starts with `{` and contains `.` or known TraceQL
  ops. *Assertion*: response is well-formed; `traces[].traceID` and
  `spans[].spanID` are 32 / 16 hex chars when present. (Catches N4 /
  N16 surface bits.)
- **Filter-drill** (label-shape derivative) — for each label observed
  in a label-shape response. *Assertion*: re-query with
  `{label="<observed-value>"}` appended; result MUST be (a)
  non-empty, (b) ≤ the unfiltered series count. (Catches N3 / N15.)

### 4.3 Variable substitution

`templating.list[]` from the dashboard JSON gives us name, type, query
(for query-vars), and current default. The matrix is intentionally
shallow — the goal is to cover the "5m vs 1h" timing class
(N12-shaped), not exhaust every variable cross-product:

- Time range: `[from=now-5m, to=now, step=15s]`, `[from=now-1h,
  to=now, step=30s]`, `[from=now-24h, to=now, step=1m]`, plus the
  dashboard's own default.
- Query-variables: first option + wildcard if `multi: true`.
- Interval-variables: `__rate_interval` resolves implicitly via the
  time-range matrix above; no extra binding needed.

## 5. Helper inventory

New utilities, all in `test/e2e/playwright/helpers/`:

```ts
// helpers/dashboard.ts

export type DashboardEntry = { uid: string; title: string; type: string };
export type Panel = {
  id: number;
  title: string;
  type: string;
  datasource: { type: string; uid: string };
  targets: PanelTarget[];
  gridPos: { x: number; y: number; w: number; h: number };
};
export type PanelTarget = {
  refId: string;
  expr?: string;   // Prom / Loki
  query?: string;  // Tempo
  datasource?: { type: string; uid: string };
  legendFormat?: string;
  queryType?: string;
};
export type DashboardModel = {
  uid: string;
  title: string;
  templating: { list: Array<{ name: string; type: string; current?: { value: string | string[] } }> };
  panels: Panel[];
};

export async function listDashboards(req: APIRequestContext, baseURL: string): Promise<DashboardEntry[]>;
export async function fetchDashboard(req: APIRequestContext, baseURL: string, uid: string): Promise<DashboardModel>;
export function flattenPanels(dash: DashboardModel): Panel[]; // unwraps rows[] panels too
```

```ts
// helpers/query-shape.ts

export type ShapeKind =
  | 'label-shape'           // sum/count/avg/min/max by (k...)
  | 'histogram'             // histogram_quantile(...)
  | 'plain-rate'            // rate(...)
  | 'log-stream'            // {k="v"} ... (LogQL)
  | 'traceql'               // TraceQL
  | 'opaque';               // none of the above; fall back to wire-only

export type Shape = {
  kind: ShapeKind;
  byKeys?: string[];        // for label-shape
  metricRoot?: string;      // for histogram → "<name>" without _bucket
};

export function classifyTarget(t: PanelTarget): Shape;
```

```ts
// helpers/assertions.ts

export type Failure = { surface: string; kind: string; detail: string };

export async function assertLabelShape(
  resp: DsQueryResponse,
  shape: Shape,
  surface: string,
): Promise<Failure[]>;

export async function assertHistogramCompleteness(
  req: APIRequestContext,
  baseURL: string,
  shape: Shape,
  surface: string,
): Promise<Failure[]>;

export async function assertNoFabricatedValue(
  req: APIRequestContext,
  baseURL: string,
  shape: Shape,
  resp: DsQueryResponse,
  surface: string,
): Promise<Failure[]>;

export async function drillFilter(
  req: APIRequestContext,
  baseURL: string,
  shape: Shape,
  resp: DsQueryResponse,
  surface: string,
): Promise<Failure[]>;
```

```ts
// helpers/sweep.ts

export type VariableBinding = Record<string, string>;
export type TimeRange = { from: string; to: string };

export async function iterateDashboards(
  ctx: SweepContext,
): AsyncGenerator<DashboardModel>;

export async function* iterateBindings(
  dash: DashboardModel,
): AsyncGenerator<{ vars: VariableBinding; range: TimeRange }>;

export async function visitPanelKiosk(
  page: Page,
  baseURL: string,
  uid: string,
  panel: Panel,
): Promise<Failure[]>;
```

```ts
// helpers/drilldown.ts

export const DRILLDOWN_APPS = [
  { id: 'grafana-metricsdrilldown-app', root: '/a/grafana-metricsdrilldown-app/trail' },
  { id: 'grafana-lokiexplore-app',      root: '/a/grafana-lokiexplore-app/explore?var-ds=cerberus-loki' },
  { id: 'grafana-exploretraces-app',    root: '/a/grafana-exploretraces-app/explore' },
] as const;

export async function drillTwoLevels(
  page: Page,
  app: typeof DRILLDOWN_APPS[number],
): Promise<Failure[]>;
```

The existing in-spec helpers (`collectStuckLoadingPanels`,
`collectPanelErrors`, `truncate`, `stripBase`, `isKnownTolerated404`,
`driveTraceClick`, `driveCerberusQLPartition`) are preserved verbatim
and moved into `helpers/dom.ts` and `helpers/probes.ts`. The
partition-specific helper becomes redundant once the generic
label-shape rule lands — flag for removal in phase 3.

## 6. Proposed spec layout

Two options, evaluated:

### Option A — one big spec

Keep one `compose_grafana_smoke.spec.ts`, add the iteration loop and
the helpers. Pros: single CI artifact, single failure aggregation,
matches today's shape. Cons: runtime balloons; one failure forces a
single retry of the whole thing; triage requires scrolling a wall of
failures.

### Option B — split by concern (recommended)

```text
test/e2e/playwright/
  compose_grafana_smoke.spec.ts          # kept; thinned to the
                                          # wire-level dashboard sweep
                                          # + trace-click + partition.
  compose_panel_shape.spec.ts            # NEW. Iterates dashboards,
                                          # classifies targets, applies
                                          # label-shape + histogram
                                          # rules. (N2 / N5 / N6 / N11
                                          # / N14.)
  compose_panel_kiosk.spec.ts            # NEW. Iterates dashboards;
                                          # opens each panel via
                                          # ?viewPanel=<id>; wire +
                                          # DOM sweep per panel.
                                          # (N13.)
  compose_filter_drill.spec.ts           # NEW. Picks observed labels
                                          # from baseline responses;
                                          # re-queries with filter;
                                          # asserts non-empty strict
                                          # subset. (N3 / N15.)
  compose_variable_matrix.spec.ts        # NEW. Iterates the 3-4
                                          # canonical (varbind, range,
                                          # step) tuples per dashboard;
                                          # asserts every panel
                                          # responds + non-empty for
                                          # the windows the seed
                                          # supports. (N12.)
  compose_drilldown_apps.spec.ts         # NEW. Two-level click drill
                                          # through each of the three
                                          # built-in drilldown apps.
                                          # (N15 bis.)
  helpers/                               # new directory; signatures
                                          # in §5.
    dashboard.ts
    query-shape.ts
    assertions.ts
    sweep.ts
    drilldown.ts
    dom.ts
    probes.ts
```

Pros: per-spec triage (a failure in `compose_panel_shape.spec.ts`
immediately tells the maintainer "a sum-by label collapsed"),
parallelisable on CI runners that support it, helpers are reusable
across specs. Cons: more files; if the helpers' contracts drift,
multiple specs need touching.

The recommendation is **Option B**, run on the existing compose-smoke
CI job in a single sequential `npx playwright test` invocation
(Playwright handles per-file isolation natively).

## 7. Phased rollout

**Phase 0 — design** (this PR). Doc only.

**Phase 1 — helpers + label-shape rule**. Extract `helpers/dashboard.ts`,
`helpers/query-shape.ts`, `helpers/assertions.ts:assertLabelShape`,
`helpers/sweep.ts:iterateDashboards`. Land
`compose_panel_shape.spec.ts` with the label-shape rule wired against
every dashboard panel. Retire the bespoke
`driveCerberusQLPartition` helper from
`compose_grafana_smoke.spec.ts`. Highest leverage — directly closes
N2 / N11 / N14 across every current and future dashboard.

**Phase 2 — histogram completeness + no-fabricated-value**. Add
`assertHistogramCompleteness` and `assertNoFabricatedValue`; both wire
into `compose_panel_shape.spec.ts`. Closes N5 / N6.

**Phase 3 — filter drill-down**. Land `compose_filter_drill.spec.ts`
with the label → `{label="value"}` re-query. Closes N3 / N15.

**Phase 4 — single-panel kiosk**. Land
`compose_panel_kiosk.spec.ts`. Closes N13.

**Phase 5 — variable + time-range matrix**. Land
`compose_variable_matrix.spec.ts`. Closes N12. This one is highest
flake risk (variable resolution depends on the seed window) — gate
behind nightly initially, promote once flake rate < 1%.

**Phase 6 — drilldown apps**. Land `compose_drilldown_apps.spec.ts`.
Closes N15 bis. Drilldown apps are the highest UI-churn surface;
maintenance cost will dominate the gain.

## 8. Tradeoffs

### 8.1 Runtime budget

The current `compose_grafana_smoke.spec.ts` runs in about 2-3 min on
the compose-smoke CI runner (compose up + Playwright). The proposed
iteration grows that by:

- Phase 1 (label-shape): +30s — one extra response-body parse per panel, no extra navigation.
- Phase 2 (histogram completeness): +30s — one `/api/v1/series` probe per histogram_quantile target, dedup'd by metric root.
- Phase 3 (filter drill): +60-90s — one extra ds/query per observed label per panel; deduped.
- Phase 4 (kiosk): +90-120s — one extra navigation per panel.
- Phase 5 (variable matrix): +3-4 min per non-default tuple; this is the runtime tax.
- Phase 6 (drilldown apps): +60s.

**Recommendation**: keep phases 1-4 in the `compose-smoke` PR-gated
job (total budget ~5-6 min, fits the existing 15-min `timeout-minutes`
cap). Phases 5-6 belong on the nightly `dashboard` job (E2E workflow)
where the time tax is irrelevant. Document the split in
`docs/test-strategy.md`.

### 8.2 Flake risk

- **Variable iteration (phase 5)**: a 5m time range against a seed
  window that drifted out can return empty. The seed already spans
  past + future by ±4-5 min (see `test/e2e/seed/cmd/seed/main.go` L141
  / the `insertGaugeSQL` window) — the matrix should pick ranges
  within that envelope. Document the seed-envelope contract in the
  helper so a future seed change forces the matrix to update.
- **Filter drill (phase 3)**: if the observed label value's series
  share other labels with the baseline, the strict-subset check is
  brittle. Relax to **non-empty + ≤ baseline** rather than strict
  subset by element.
- **Histogram completeness (phase 2)**: the `_bucket` series probe is
  a separate `/api/v1/series` call that races the panel's own
  query. Pin the time range explicitly on the probe to the panel's
  resolved range; don't rely on the page's `from`/`to`.
- **Kiosk (phase 4)**: `?viewPanel=N` repaints can race the
  network-idle wait. Treat a kiosk timeout as a stuck-loading hit;
  the existing handler already reports it.

### 8.3 False-positive triage

Each failure entry MUST carry `[<spec>:<dashboard>:<panel>] <rule>:
<detail>` (mirrors today's `[<kind>:<label>]` shape). The aggregator
must surface:

1. The exact panel JSON the failure came from (helps diff against the
   provisioned source).
2. The exact ds/query URL + body excerpt (current behaviour, kept).
3. The classified shape that triggered the rule (so a misclassified
   target is easy to spot).

When the rule itself is wrong (false positive), the fix lives in
`helpers/query-shape.ts` or `helpers/assertions.ts`, not in the
spec — the spec stays declarative.

## 9. Open questions for maintainer review

1. **Spec proliferation**: Option B adds 5 new spec files. Is the
   per-file triage win worth the file-count? An alternative is one
   `compose_iterative.spec.ts` with multiple `test()` blocks. (Author
   recommendation: Option B.)
2. **CI gate posture**: phases 1-4 in `compose-smoke` (PR-blocking).
   Phases 5-6 in nightly `dashboard` (informational). Acceptable, or
   should everything stay PR-blocking with a longer timeout?
3. **Filter-drill subset rule**: strict subset by element vs
   non-empty + ≤ baseline count? (Author recommendation: the latter,
   to avoid order-dependent flake.)
4. **Drilldown apps**: the three built-in apps churn UI on every
   Grafana 11.x bump. Is the maintenance burden worth N15-bis
   coverage, or should drilldown be left to manual sweeps for the
   v1.0 horizon?
5. **Tolerated 404 list**: `isKnownTolerated404` carries a narrow
   allow-list. As Nx fixes land (e.g. `#632` for `/api/v1/rules`),
   should the cleanup be a follow-up phase? (Author recommendation:
   yes — phase 7, a one-line PR per allowed entry that retires it.)
6. **Per-PR runtime cap**: today's compose-smoke runs in
   ~3 min. The phases 1-4 ceiling is ~6 min. Is 6 min on every PR
   acceptable, or should the iteration be gated behind a path-match
   so it only runs when `test/e2e/grafana/` / `test/e2e/seed/` /
   `internal/api/` change? (Author recommendation: no path-match —
   the bugs the iteration catches are cross-cutting; gating risks
   silently missing a fix.)
