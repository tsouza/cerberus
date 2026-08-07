/**
 * Grafana surface-crawler library — the stack-agnostic ENGINE.
 *
 * The crawler (crawl.spec.ts) BFS-walks the live Grafana UI from the
 * root page, harvesting same-origin links per page, and applies the
 * same universal oracle set on EVERY visited page. This module holds
 * the deterministic plumbing — URL canonicalization, scope
 * exclusions, the surface-inventory file format — so the spec stays a
 * thin driver and the rules are unit-pinnable.
 *
 * One engine, N stacks: nothing in this module (or the specs) may
 * branch on a stack name. Per-stack variation — base URL, scope
 * rules, inventory file, datasource UIDs, lint floors, lean seeds,
 * page caps — is declared as data in stacks.ts and threaded through
 * as parameters. The constants below (EXCLUDED_PATH_PATTERNS,
 * APP_BARE_PATH_ALIASES) are the shared Grafana-12.x defaults the
 * registered stack configs reference; the engine consumes them only
 * via the ScopeRules a config hands it.
 *
 * Why a crawler on top of the enumerated iterate-* specs: an off-CI
 * AI screenshot sweep (2026-06-09) found 34 unique error signatures
 * across 55 BFS-visited pages — several on surfaces NO enumerated
 * spec visits (drilldown-app tabs, logs-drilldown service pages,
 * traces-drilldown comparison view). Every find decomposed into a
 * deterministic signal once named; the crawler carries those signals
 * in CI forever, and the committed per-stack surface inventory
 * (grafana-surface-inventory.<stack>.json) makes coverage growth
 * explicit and shrink impossible.
 *
 * Determinism: the visited-set converges because canonicalization
 * strips volatile params (time ranges, session-state blobs) and
 * parameterizes dynamic path segments (service names, trace ids).
 * Two crawls of the same provisioned stack + pinned Grafana version
 * yield the same canonical set.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
// Type-only (erased at runtime) — stacks.ts value-imports the scope
// constants below, so a value import here would be a cycle.
import type { CrawlStackConfig } from './stacks.js';
// The shared init-race reconciler (dangling-operand TraceQL) + its request
// helpers now live in helpers/reconcile.ts so the drilldown-apps sweep shares
// one source of truth. Imported for the supersession reconcilers below
// (dsQuerySignature / isSupersededDsQueryFailure) and re-exported so crawl's
// existing consumers (crawl.spec.ts, reconcile.spec.ts) keep their import path.
import {
  type DsResponseView,
  isTransientInitRaceFailure,
  isTransientMalformedTraceQLFailure,
  isTransientMalformedTraceQLSearch,
  refIdToExpr,
} from '../helpers/reconcile.js';

export {
  type DsResponseView,
  isTransientInitRaceFailure,
  isTransientMalformedTraceQLFailure,
  isTransientMalformedTraceQLSearch,
  refIdToExpr,
};

/**
 * The scope slice of a stack config the canonicalizer consumes:
 * which route families are out of crawl scope, which bare app paths
 * alias to an entry route, and which query params are STRUCTURAL —
 * i.e. encode a consumption mode rather than session state.
 */
export type ScopeRules = {
  excludedPathPatterns: ReadonlyArray<RegExp>;
  appBarePathAliases: ReadonlyMap<string, string>;
  structuralParamRules: ReadonlyArray<StructuralParamRule>;
};

/**
 * A query param that is part of a surface's IDENTITY rather than its
 * session state. The default canonicalization doctrine strips every
 * query param ("a state of a surface is not a new surface") — but the
 * 2026-06-10 maintainer find proved the doctrine over-broad for a
 * narrow param class: the Traces Drilldown breakdown `var-groupBy`
 * param selects WHICH query family the page fires (`| rate()
 * by(kind)` vs `by(resource.service.name)`), and the groupBy=kind
 * state 422'd while the crawler — which only ever saw the default —
 * stayed green. Params that change the queries a page issues are
 * distinct consumption modes, hence distinct surfaces.
 *
 * Which retention mode applies is NOT a per-param taste call, and NOT
 * a list of names someone extends each time the ratchet goes red. It
 * is forced by one question with a checkable answer: **can the param's
 * complete option set be written down from the pinned app version
 * alone?**
 *
 *   - 'enumerate' — the option set is a fact about the APP: a literal
 *     array in the plugin bundle, fixed by the pinned plugin version
 *     (actionView tabs, metric type, primary signal). Every value keys
 *     its own surface, because each drives a structurally different
 *     query. The rule must DECLARE that set in `values`, and the
 *     canonicalizer rejects any value outside it.
 *   - 'parameterize' — the option set is a fact about the DATA: the
 *     app offers whichever attribute / metric / label names the
 *     ingested telemetry happens to carry, so no closed set exists.
 *     The value collapses to `{param}`, one representative surface
 *     family — the same doctrine as the `{service}` path segment.
 *
 * There is no third mode and no escape hatch: a param whose options
 * come from data cannot produce a `values` set, so the type itself
 * forces it to parameterize. That is what stops the doctrine decaying
 * into an enumeration of param names.
 *
 * `defaultValue` names the value the app writes on a cold boot; it is
 * DROPPED from the canonical in either mode, so the default state
 * stays keyed by the bare surface (the app rewriting its own defaults
 * into the URL must not re-key the surface).
 */
export type StructuralParamRule = {
  /** Canonical-path family the rule applies to (post path-rewrite). */
  pathPattern: RegExp;
  /** Exact query param name. */
  param: string;
  /** The app's cold-boot value, dropped from keys in either mode. */
  defaultValue?: string;
} & (
  | {
      mode: 'enumerate';
      /**
       * The COMPLETE option set the pinned app version offers, read
       * off the plugin bundle rather than off whichever values some
       * crawl happened to reach. `defaultValue` must be a member.
       */
      values: ReadonlyArray<string>;
    }
  | { mode: 'parameterize' }
);

/**
 * Grafana-12.x structural params, pinned to grafana/grafana:12.2.9.
 *
 * Every `values` set below is read off the shipped plugin bundle —
 * the literal option array the app itself iterates — NOT off whichever
 * values a crawl happened to reach. That distinction is the whole
 * point of the closed set. Cataloguing options by observation is how
 * this rule table went wrong before: `var-primarySignal` was recorded
 * as "Root spans | All spans" because those were the two states a
 * crawl had produced, while the bundle's array carries five. The crawl
 * later reached a third (`kind=server`) and the ratchet reported it as
 * new coverage, which it was — but one option at a time, each a fresh
 * red on `main`, with two more still queued behind it.
 *
 *   - Traces Drilldown (`grafana-exploretraces-app`): `actionView`
 *     (six tabs, boot `breakdown`), `var-metric` (rate | errors |
 *     duration, boot `rate`), `var-primarySignal` (five signals, boot
 *     `nestedSetParent<0`), `var-groupBy` (the breakdown ATTRIBUTE
 *     NAME, boot `resource.service.name` — data-derived, so it
 *     parameterizes; see below).
 *
 * `var-primarySignal` enumerates rather than parameterizes, and the
 * reason is the mirror image of `var-groupBy`'s. Its five options are
 * a literal array in the plugin bundle, each pairing a label with a
 * fixed TraceQL fragment the app splices into EVERY query the page
 * fires (`{nestedSetParent<0 && …}` vs `{kind=server && …}` vs
 * `{span.db.system.name!="" && …}`). Adding a span attribute to the
 * seeded stack cannot change that list. Collapsing it would key five
 * genuinely different query families as one surface and pin whichever
 * the crawl reached first — precisely the coverage #1889 (the Service
 * structure tab 500s on every primary signal but the default) needs
 * the crawl to keep reaching.
 *
 * `var-groupBy` parameterizes rather than enumerates because its
 * option list is a fact about the DATA, not about the surface: the
 * app offers whichever span and resource attributes the ingested
 * traces carry, capped at the combobox's first N. Pinning the values
 * pinned the dataset — adding one resource attribute to the compose
 * stack (#1822 added `service.namespace` / `deployment.environment`)
 * reshuffled the list, evicted `rootName` / `rootServiceName` past
 * the cap, and moved the sweep's representative from
 * `resource.service.version` to `resource.service.namespace`. The
 * ratchet then reported that pure data change as coverage growing AND
 * shrinking at once (#1825), which is noise, not a regression. The
 * surface is "the breakdown is grouped by SOME attribute"; the sweep
 * still DRIVES every option at full depth, exactly as before — only
 * the inventory key collapses. Same doctrine as the metrics-drilldown
 * `metric` name and the `{service}` path segment.
 *   - Metrics Drilldown (`grafana-metricsdrilldown-app`): `metric`
 *     (the selected metric NAME — one per series family in the
 *     stack, data-derived → parameterize), `actionView` (four tabs,
 *     written as `breakdown` on metric select), `var-groupby` (the
 *     breakdown LABEL NAME — a QueryVariable over
 *     `label_names(<metric>)`, so data-derived → parameterize; boot
 *     `$__all`, its includeAll sentinel).
 *   - Logs Drilldown service pages (`grafana-lokiexplore-app`):
 *     `visualizationType` (logs | table | json — JSON-string-quoted
 *     in the URL, boot `"logs"`), `sortOrder` (Descending |
 *     Ascending, likewise quoted, boot `"Descending"`).
 *
 * Time params (`from`/`to`), filters (`var-filters`), display state
 * (`displayedFields`, `urlColumns`, `patterns`, …) stay stripped —
 * they are session state, owned by the iterate-* sweeps.
 */
export const STRUCTURAL_PARAM_RULES: ReadonlyArray<StructuralParamRule> = [
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'actionView',
    mode: 'enumerate',
    // Bundle: the tab array the app maps over to build its TabsBar.
    // `exceptions` and `adaptiveTraces` mount conditionally (feature
    // flag / sibling plugin), so a crawl need not reach them — but
    // they are options of the pinned version, so they belong to the
    // declared set rather than becoming a future surprise red.
    values: [
      'breakdown',
      'structure',
      'comparison',
      'exceptions',
      'traceList',
      'adaptiveTraces',
    ],
    defaultValue: 'breakdown',
  },
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'var-metric',
    mode: 'enumerate',
    values: ['rate', 'errors', 'duration'],
    defaultValue: 'rate',
  },
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'var-groupBy',
    mode: 'parameterize',
    defaultValue: 'resource.service.name',
  },
  {
    pathPattern: /^\/a\/grafana-exploretraces-app\/explore$/,
    param: 'var-primarySignal',
    mode: 'enumerate',
    // Bundle: the literal `[{label, value, filter, description}, …]`
    // array, in its declared order. Each `value` is the TraceQL
    // fragment the app splices into every query the page fires.
    values: [
      'nestedSetParent<0',
      'true',
      'kind=server',
      'kind=consumer',
      'span.db.system.name!=""',
    ],
    defaultValue: 'nestedSetParent<0',
  },
  {
    pathPattern: /^\/a\/grafana-metricsdrilldown-app\/(drilldown|trail)$/,
    param: 'metric',
    mode: 'parameterize',
  },
  {
    pathPattern: /^\/a\/grafana-metricsdrilldown-app\/(drilldown|trail)$/,
    param: 'actionView',
    mode: 'enumerate',
    // Bundle: the tab-value enum. Note `relatedLogs` and
    // `queryResults` write `logs` / `results`, not their key names.
    values: ['breakdown', 'related', 'logs', 'results'],
    defaultValue: 'breakdown',
  },
  {
    pathPattern: /^\/a\/grafana-metricsdrilldown-app\/(drilldown|trail)$/,
    param: 'var-groupby',
    // Data-derived, so it cannot enumerate. The Group by control is a
    // QueryVariable whose options are `label_names(<selected metric>)`
    // resolved against the datasource — the LABEL NAMES the ingested
    // series carry. `$__all` is its includeAll sentinel, not an option
    // set. No closed set can be written down, which is exactly the
    // signal that the value is churn rather than coverage: pinning it
    // would pin the seeded label schema, the same defect #1825 hit
    // with the traces `var-groupBy`.
    mode: 'parameterize',
    defaultValue: '$__all',
  },
  {
    pathPattern: /^\/a\/grafana-lokiexplore-app\/explore\/service\/\{service\}\//,
    param: 'visualizationType',
    mode: 'enumerate',
    // JSON-string-quoted by the app, quotes included, as written.
    values: ['"logs"', '"table"', '"json"'],
    defaultValue: '"logs"',
  },
  {
    pathPattern: /^\/a\/grafana-lokiexplore-app\/explore\/service\/\{service\}\//,
    param: 'sortOrder',
    mode: 'enumerate',
    // Grafana core's LogsSortOrder enum, JSON-string-quoted as above.
    values: ['"Descending"', '"Ascending"'],
    defaultValue: '"Descending"',
  },
];

// ---------------------------------------------------------------------------
// Scope exclusions
// ---------------------------------------------------------------------------

/**
 * Canonical paths the crawler never navigates. The crawl is strictly
 * read-only — no mutations, no auth flows, no admin surfaces. Each
 * exclusion family is structural (the route class can never be a
 * cerberus consumption surface), NOT a tolerance for a failing page:
 *
 *   - auth flows (`/login`, `/logout`, `/signup`, `/verify`) — anonymous
 *     auth is on in the stacks; driving auth would mutate session state.
 *   - admin / org / user / connections / plugin-config — Grafana
 *     management UI, no datasource traffic, write affordances.
 *   - alerting — no cerberus alerting integration is provisioned; the
 *     surface is Grafana-internal.
 *   - create / edit / save routes — write affordances (`/dashboard/new`,
 *     folder creation, panel edit). Read-only crawl.
 *   - `/d-solo/` — the single-panel render path; per-panel render
 *     coverage is owned by iterate-panel-kiosk.spec.ts (`?viewPanel`),
 *     and one /d-solo link per panel would blow the page budget with
 *     duplicate states.
 *   - `/goto/` — short-link redirector (volatile token in the path).
 *   - `/api/`, `/apis/`, `/public/`, `/swagger`, `/metrics` — not UI
 *     pages; API behaviour is asserted by the response capture on the
 *     pages that call them.
 */
export const EXCLUDED_PATH_PATTERNS: ReadonlyArray<RegExp> = [
  /^\/login(\/|$)/,
  /^\/logout(\/|$)/,
  /^\/signup(\/|$)/,
  /^\/verify(\/|$)/,
  /^\/user(\/|$)/,
  /^\/profile(\/|$)/,
  /^\/admin(\/|$)/,
  /^\/org(\/|$)/,
  /^\/connections(\/|$)/,
  /^\/plugins(\/|$)/,
  /^\/alerting(\/|$)/,
  /^\/a\/grafana-easystart-app(\/|$)/, // "connect data" onboarding app — write affordances
  /^\/dashboard\/new(\/|$)/,
  /^\/dashboard\/import(\/|$)/,
  /^\/dashboards\/folder\/new(\/|$)/,
  /^\/d-solo\//,
  /^\/goto\//,
  /^\/api(s)?\//,
  /^\/public\//,
  /^\/swagger(\/|$)/,
  /^\/metrics(\/|$)/,
  /^\/playlists(\/|$)/, // playlist play mutates view state on a timer
  /\/edit(\/|$)/,
  /\/new$/,
];

/**
 * Bare `/a/<app-id>` paths are client-side REDIRECTORS into each
 * app's entry route — Grafana's app loader boots the plugin and
 * immediately replaces the URL (verified live: both paths land on
 * the identical entry URL). They alias to the entry route so the
 * crawl visits each app exactly once.
 *
 * Why this matters beyond dedupe: the grafana-exploretraces-app
 * RE-ENTRY boot (second mount in one browser context) restores its
 * persisted filter state with an empty primary-signal and fires the
 * malformed TraceQL `{ && true}` — which reference Tempo 2.7 rejects
 * with the byte-identical "syntax error: unexpected &&" 400 cerberus
 * returns (verified against grafana/tempo:2.7.1; the upstream
 * tempo parser rejects it at parse stage). An upstream app-state
 * bug, not a cerberus surface — collapsing the redirector onto the
 * entry route keeps the crawl to one deterministic boot per app, the
 * same posture iterate-drilldown-apps.spec.ts takes (one fresh-page
 * visit per catalogue entry).
 */
export const APP_BARE_PATH_ALIASES: ReadonlyMap<string, string> = new Map([
  ['/a/grafana-lokiexplore-app', '/a/grafana-lokiexplore-app/explore'],
  ['/a/grafana-exploretraces-app', '/a/grafana-exploretraces-app/explore'],
  ['/a/grafana-metricsdrilldown-app', '/a/grafana-metricsdrilldown-app/drilldown'],
  ['/a/grafana-pyroscope-app', '/a/grafana-pyroscope-app/explore'],
]);

// ---------------------------------------------------------------------------
// Canonicalization
// ---------------------------------------------------------------------------

/** 16+ hex chars — trace ids (32), span ids (16), short-url tokens. */
const HEX_SEGMENT = /^[0-9a-f]{16,64}$/;
/** UUID path segments (Grafana folder/library-panel uids are NOT UUIDs). */
const UUID_SEGMENT =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/**
 * Canonicalize a harvested href against the Grafana base URL,
 * returning both the canonical surface key and the concrete URL the
 * crawler should navigate for it. Returns null when the URL is out
 * of crawl scope (different origin, excluded route family, non-HTTP
 * scheme).
 *
 * The canonical key is the rewritten path plus the surface's
 * STRUCTURAL params only (see StructuralParamRule). Grafana routes
 * surfaces by path; query strings mostly carry volatile or session
 * state — time windows (`from`/`to`/`refresh`), org context,
 * kiosk / view-panel modes, Explore state blobs (`panes`/`left`/
 * `right`), filter selections, and the drilldown apps' serialized UI
 * state (`patterns`, `displayedFields`, `urlColumns`, `layout`, …).
 * The first full crawl demonstrated why a broad param-retention list
 * can't work: the logs-drilldown service page alone produced four
 * param-permutations of one surface. A state of a surface is not a
 * new surface — UNLESS the param changes which queries the page
 * fires: those params (the structural rules) join the canonical key,
 * either verbatim (`actionView=comparison`, low-cardinality) or
 * parameterized (`metric={metric}`, high-cardinality), with the
 * app's cold-boot default dropped so the default state keys the bare
 * surface.
 *
 * Path rules, in order:
 *   1. Same-origin only; hash dropped.
 *   2. Excluded route families → null (checked on the RAW path,
 *      before any rewriting, so `/d/<uid>/edit` can't slip past via
 *      the slug-drop; and re-checked after, so no rewrite can map
 *      into an excluded family).
 *   3. `/explore...` collapses to `/explore` — every Explore
 *      permutation is one surface.
 *   4. Bare `/a/<app-id>` redirector paths alias to the app's entry
 *      route (see APP_BARE_PATH_ALIASES) — the concrete URL becomes
 *      the entry route too.
 *   5. `/d/<uid>/<slug>` drops the cosmetic slug → `/d/<uid>`.
 *   6. Dynamic path segments parameterize (the canonical is a
 *      surface FAMILY; the concrete first-seen instance is what the
 *      crawler navigates):
 *        - `/dashboards/f/<folder-uid>` → `{folder}` (provisioned
 *          folder uids are minted at stack-boot time — pinning a
 *          concrete one would break the inventory on every rebuild);
 *        - the segment after `service` (logs-drilldown service
 *          pages) → `{service}`;
 *        - 16+-hex and UUID segments → `{hex}` (trace / span ids).
 */
export function canonicalTarget(
  href: string,
  baseURL: string,
  scope: ScopeRules,
): { canonical: string; concrete: string } | null {
  let url: URL;
  let base: URL;
  try {
    base = new URL(baseURL);
    url = new URL(href, baseURL);
  } catch {
    return null;
  }
  if (url.origin !== base.origin) return null;
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;

  let path = url.pathname.replace(/\/+$/, '');
  if (path === '') path = '/';

  // Exclusions on the raw path, before any rewriting.
  for (const re of scope.excludedPathPatterns) {
    if (re.test(path)) return null;
  }

  // Explore collapses wholesale — session state is not a surface.
  if (path === '/explore' || path.startsWith('/explore/')) {
    return { canonical: '/explore', concrete: '/explore' };
  }

  // Bare app redirector → entry route (canonical AND concrete).
  const alias = scope.appBarePathAliases.get(path);
  if (alias !== undefined) {
    return { canonical: alias, concrete: alias };
  }

  // /d/<uid>/<slug> → /d/<uid>.
  const dashMatch = /^\/d\/([^/]+)(?:\/.*)?$/.exec(path);
  if (dashMatch) {
    path = `/d/${dashMatch[1]}`;
  }

  // /dashboards/f/<uid>[/<slug>][/<tab>…] → /dashboards/f/{folder}[/<tab>…].
  // The folder uid is minted at provisioning time and the slug is the
  // cosmetic title; harvested hrefs carry the slugged and unslugged
  // forms interchangeably depending on which page rendered the link,
  // so both must key one surface family or the inventory flickers.
  const folderMatch = /^\/dashboards\/f\/([^/]+)(?:\/[^/]+)?(\/.*)?$/.exec(
    path,
  );
  if (folderMatch) {
    path = `/dashboards/f/{folder}${folderMatch[2] ?? ''}`;
  }

  // Parameterize dynamic segments.
  const segments = path.split('/');
  for (let i = 0; i < segments.length; i++) {
    const seg = segments[i] ?? '';
    if (HEX_SEGMENT.test(seg) || UUID_SEGMENT.test(seg)) {
      segments[i] = '{hex}';
      continue;
    }
    // Logs-drilldown service pages: the segment AFTER 'service' is the
    // service name harvested from live data; the segment after
    // 'label' is a label NAME from live data (the labels-tab drill); the
    // segment after 'field' is a log FIELD name from live data (the
    // fields-tab drill). All three parameterize to one representative per
    // route family, so the inventory pins the ROUTE the drill reaches rather
    // than whichever names happened to be in the seeded logs that day.
    if (i >= 1 && segments[i - 1] === 'service') {
      segments[i] = '{service}';
      continue;
    }
    if (i >= 1 && segments[i - 1] === 'label') {
      segments[i] = '{label}';
      continue;
    }
    if (i >= 1 && segments[i - 1] === 'field') {
      segments[i] = '{field}';
    }
  }
  path = segments.join('/');
  if (path === '') path = '/';

  // Re-check after rewriting — no rewrite may land in-scope a URL
  // whose canonical form is excluded.
  for (const re of scope.excludedPathPatterns) {
    if (re.test(path)) return null;
  }

  // Structural params join the canonical key (sorted by param name
  // so the key is order-independent of the app's URL writing). Every
  // other param is stripped from the canonical only.
  const retained: string[] = [];
  for (const rule of scope.structuralParamRules) {
    if (!rule.pathPattern.test(path)) continue;
    const value = url.searchParams.get(rule.param);
    if (value === null || value === '') continue;
    // The app's cold-boot value keys the BARE surface in both modes —
    // the default state is not a distinct surface.
    if (value === rule.defaultValue) continue;
    if (rule.mode === 'enumerate') {
      // An enumerated param's option set is a CLAIM about the pinned
      // app version, and this is where the claim gets checked. A value
      // outside it means exactly one of two things, and both need a
      // human: either the app version gained an option (real coverage
      // change, pin it deliberately) or the param's options actually
      // come from the data and the rule is miscategorised (it must
      // become 'parameterize'). Failing here names the param and the
      // value; letting it through instead surfaces as an anonymous
      // "coverage grew" row 40 minutes into the compose lane.
      if (!rule.values.includes(value)) {
        throw new Error(
          `canonical surface key: ${rule.param}=${JSON.stringify(value)} on ` +
            `${path} is outside the rule's declared option set ` +
            `[${rule.values.map((v) => JSON.stringify(v)).join(', ')}]. ` +
            `Either the pinned app version gained an option — add it to ` +
            `STRUCTURAL_PARAM_RULES and regenerate the inventory — or the ` +
            `option list comes from the seeded data, in which case the rule ` +
            `must be mode 'parameterize' so the value collapses to ` +
            `{${rule.param}}.`,
        );
      }
      retained.push(`${rule.param}=${value}`);
    } else {
      retained.push(`${rule.param}={${rule.param}}`);
    }
  }
  retained.sort();
  const canonical =
    retained.length > 0 ? `${path}?${retained.join('&')}` : path;

  // Concrete: the raw path + raw query (parameterized canonicals
  // aren't navigable, and drilldown-app hrefs carry var-* state the
  // page needs to boot into the linked view — navigating the
  // logs-drilldown service page without its var-filters leaves the
  // tab bar unmounted). Only the CANONICAL key reduces the query.
  return { canonical, concrete: `${url.pathname}${url.search}` };
}

/**
 * Number of structural params pinned in a canonical surface key —
 * the interaction sweep's pairwise depth bound (see
 * interactions.ts): 0 pinned → full single-control enumeration;
 * 1 pinned → representative combos only (each interaction there
 * forms a structural-param PAIR); ≥2 pinned → terminal, visited
 * with the universal oracles but not expanded further.
 */
export function pinnedStructuralParamCount(canonical: string): number {
  const q = canonical.split('?', 2)[1];
  if (q === undefined || q === '') return 0;
  return q.split('&').length;
}

/**
 * Known sibling-route families: when the crawl discovers one member,
 * it deterministically enqueues the rest — tab links inside scenes
 * apps mount only after the page's first query wave resolves, so
 * harvest-only discovery of them is timing-sensitive (run-to-run
 * inventory flicker), while the route family itself is fixed by the
 * pinned app version.
 *
 * Today: the logs-drilldown service page tabs (verified live against
 * grafana-lokiexplore-app on grafana/grafana:12.2.9).
 */
export function expandSiblingTabs(
  canonical: string,
  concrete: string,
): Array<{ canonical: string; concrete: string }> {
  const m = /^(\/a\/grafana-lokiexplore-app\/explore\/service\/\{service\})\/([a-z]+)$/.exec(
    canonical,
  );
  if (!m) return [];
  const tabs = ['logs', 'labels', 'fields', 'patterns'];
  const current = m[2] ?? '';
  const [concretePath, concreteQuery = ''] = concrete.split('?', 2);
  const concreteBase = (concretePath ?? '').replace(/\/[a-z]+$/, '');
  return tabs
    .filter((t) => t !== current)
    .map((t) => ({
      canonical: `${m[1]}/${t}`,
      concrete: `${concreteBase}/${t}${concreteQuery ? `?${concreteQuery}` : ''}`,
    }));
}

/** Canonical-only convenience wrapper (the pin tests use this). */
export function canonicalizeURL(
  href: string,
  baseURL: string,
  scope: ScopeRules,
): string | null {
  return canonicalTarget(href, baseURL, scope)?.canonical ?? null;
}

// ---------------------------------------------------------------------------
// Link harvesting
// ---------------------------------------------------------------------------

/**
 * Harvest every same-document `<a href>` currently in the DOM,
 * returning raw href strings (canonicalization is the caller's job).
 *
 * Before harvesting, the helper opens Grafana's mega menu if the
 * toggle is present — the nav tree only mounts its links while the
 * menu is open (Grafana 12.x renders the collapsed menu without
 * anchors). Opening the menu is a read-only UI gesture.
 */
export async function harvestLinks(page: Page): Promise<string[]> {
  // Open the mega menu so nav links mount. The toggle carries
  // aria-label "Open menu" when closed; if it's absent or already
  // open ("Close menu"), proceed with whatever the DOM has.
  const toggle = page.locator('[aria-label="Open menu"]').first();
  if ((await toggle.count()) > 0) {
    await toggle.click({ timeout: 5_000 }).catch(() => {
      // A nav overlay can obscure the toggle on narrow layouts; links
      // already in the DOM are still harvested below.
    });
    await page.waitForTimeout(300);
  }
  const hrefs = await page
    .locator('a[href]')
    .evaluateAll((nodes) =>
      nodes
        .map((n) => n.getAttribute('href') ?? '')
        .filter((h) => h.length > 0),
    );
  return hrefs;
}

// ---------------------------------------------------------------------------
// Page-crash banner signatures
// ---------------------------------------------------------------------------

/**
 * Substrings on a `role="alert"` banner that classify it as an
 * error-state surface. Mirrors ALERT_ERROR_PATTERNS in
 * iterate-panel-kiosk.spec.ts — kept in lockstep so a banner that
 * fails the kiosk sweep also fails the crawl.
 */
export const ALERT_ERROR_PATTERNS: ReadonlyArray<RegExp> = [
  /error/i,
  /failed/i,
  /illegal wiretype/i,
  /plugin\.downstream/i,
  /unable to/i,
];

/**
 * Read every VISIBLE `role="alert"` banner's text. The crawler can't
 * reuse helpers/dom.ts's captureRoleAlertBanners verbatim: Grafana
 * pre-mounts a hidden alert skeleton on some pages (`display:none;
 * opacity:0` — e.g. an "Unknown error" toast template on /explore,
 * found by the first crawl run) that never renders to the user.
 * Visibility-filtering is a correctness fix for the oracle (assert
 * what a user can see), not a tolerance: a banner that actually
 * displays still fails the crawl.
 */
export async function collectVisibleAlertBanners(
  page: Page,
): Promise<string[]> {
  return await page
    .locator('[role="alert"]')
    .evaluateAll((nodes) =>
      nodes
        .filter((n) =>
          (n as HTMLElement).checkVisibility
            ? (n as HTMLElement).checkVisibility({
                checkOpacity: true,
                checkVisibilityCSS: true,
              })
            : true,
        )
        .map((n) => (n.textContent ?? '').trim())
        .filter((s) => s.length > 0),
    );
}

/**
 * Page-level crash signatures: React error boundaries and Grafana's
 * top-level failure pages render one of these phrases in the body.
 * Any visited page whose text content carries one fails the crawl
 * loudly — these are whole-page failures, not per-panel states.
 */
export const PAGE_CRASH_PATTERNS: ReadonlyArray<RegExp> = [
  /an unexpected error happened/i,
  /application error/i,
  /something went wrong/i,
  /if you keep getting this error/i,
];

// ---------------------------------------------------------------------------
// Surface inventory (the ratchet)
// ---------------------------------------------------------------------------

/**
 * One pinned crawl surface. `url` is the canonical form produced by
 * canonicalizeURL. `lean: true` marks the surfaces the lean (per-PR)
 * crawl visits: the root page, the nav links harvested from it, and
 * one representative per drilldown app. Every surface is visited at
 * 'full' depth.
 */
export type InventorySurface = {
  url: string;
  lean: boolean;
};

export type SurfaceInventory = {
  /** Free-text header documenting the regen convention. */
  doc: string;
  /** Stack the inventory pins — must equal the stack config's name. */
  stack: string;
  surfaces: InventorySurface[];
};

export type SurfaceExclusion = {
  url: string;
  rationale: string;
};

export type SurfaceExclusions = {
  doc: string;
  exclusions: SurfaceExclusion[];
};

/** The slice of a stack config the inventory plumbing consumes. */
type StackFiles = Pick<
  CrawlStackConfig,
  'name' | 'inventoryFilename' | 'exclusionsFilename'
>;

export function inventoryPath(stack: StackFiles): string {
  return join(__dirname, stack.inventoryFilename);
}

export function exclusionsPath(stack: StackFiles): string {
  return join(__dirname, stack.exclusionsFilename);
}

export function loadInventory(stack: StackFiles): SurfaceInventory {
  const raw = readFileSync(inventoryPath(stack), 'utf8');
  const parsed = JSON.parse(raw) as SurfaceInventory;
  if (!Array.isArray(parsed.surfaces)) {
    throw new Error(
      `loadInventory: ${stack.inventoryFilename} has no surfaces[] array`,
    );
  }
  if (parsed.stack !== stack.name) {
    throw new Error(
      `loadInventory: ${stack.inventoryFilename} pins stack ${JSON.stringify(parsed.stack)} ` +
        `but the active config is ${JSON.stringify(stack.name)} — the config points at the wrong file`,
    );
  }
  return parsed;
}

export function loadExclusions(stack: StackFiles): SurfaceExclusions {
  const raw = readFileSync(exclusionsPath(stack), 'utf8');
  const parsed = JSON.parse(raw) as SurfaceExclusions;
  if (!Array.isArray(parsed.exclusions)) {
    throw new Error(
      `loadExclusions: ${stack.exclusionsFilename} has no exclusions[] array`,
    );
  }
  return parsed;
}

/**
 * Bootstrap guard. A stack registers with an EMPTY committed
 * inventory (the bootstrap convention: no inventory can exist before
 * the stack's first exhaustive crawl). Empty is ONLY legal on the
 * bootstrap run itself (CERBERUS_UPDATE_INVENTORY=1) — every other
 * run fails loudly here, so the bootstrap state can't silently
 * become permanent: the stack's CI lane stays red until the
 * regenerated inventory is committed.
 */
export function assertInventoryBootstrapped(
  inventory: SurfaceInventory,
  stack: StackFiles,
): void {
  if (inventory.surfaces.length > 0) return;
  if (process.env.CERBERUS_UPDATE_INVENTORY) return;
  throw new Error(
    `stack ${JSON.stringify(stack.name)} has an EMPTY surface inventory (${stack.inventoryFilename}) — ` +
      `it is registered but not yet bootstrapped. Bootstrap against a healthy ${stack.name} stack with:\n` +
      `  CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=${stack.name} npx playwright test crawl/crawl.spec.ts\n` +
      `then commit the regenerated ${stack.inventoryFilename}. This error fires on EVERY run until ` +
      `the inventory lands — the empty state is a bootstrap convention, never a steady state.`,
  );
}

/**
 * Order two strings by UTF-16 code unit — the ordering JS `<` gives —
 * for everything whose order the crawl's OUTPUT depends on.
 *
 * `localeCompare` is what this replaces, and it is not reproducible
 * across environments: its collation is chosen from the runtime's
 * default locale (so `LANG` decides it) and implemented by the
 * runtime's ICU data (so a Node upgrade can decide it). Collation also
 * treats punctuation as a lower-strength difference, which is exactly
 * what surface keys are made of — `#`, `?`, `=`, `{`, `"`, `-`. A
 * committed artefact ordered that way is a function of the machine
 * that regenerated it as much as of the application it describes.
 */
export function byCodepoint(a: string, b: string): number {
  if (a < b) return -1;
  return a > b ? 1 : 0;
}

/**
 * Render the canonical serialized form of an inventory — sorted by
 * URL, two-space indent, trailing newline — so regeneration is
 * byte-for-byte reproducible (mirrors test/inventory/'s
 * MarshalInventory convention).
 */
export function marshalInventory(inv: SurfaceInventory): string {
  const sorted = [...inv.surfaces].sort((a, b) => byCodepoint(a.url, b.url));
  return `${JSON.stringify({ doc: inv.doc, stack: inv.stack, surfaces: sorted }, null, 2)}\n`;
}

/**
 * Compare a crawl's visited set against the committed inventory for
 * the active depth. Returns human-readable violations (empty = the
 * ratchet holds):
 *
 *   - a visited surface missing from the inventory → coverage GREW
 *     (e.g. a Grafana bump added an app page); regenerate the
 *     inventory deliberately via CERBERUS_UPDATE_INVENTORY=1.
 *   - an inventory surface (for this depth) the crawl didn't visit →
 *     coverage SHRANK; that is a regression to fix, never a row to
 *     delete silently.
 *   - a surface listed in both the inventory and the exclusions file
 *     → stale exclusion.
 *   - an exclusion without a rationale → unsound exclusion.
 */
export function diffInventory(
  visited: ReadonlySet<string>,
  inventory: SurfaceInventory,
  exclusions: SurfaceExclusions,
  depth: 'lean' | 'full',
  stack: StackFiles,
): string[] {
  const out: string[] = [];

  const excluded = new Set<string>();
  for (const e of exclusions.exclusions) {
    if (!e.rationale || e.rationale.trim() === '') {
      out.push(
        `exclusion ${JSON.stringify(e.url)} has no rationale — every exclusion must document why the surface is genuinely uncrawlable`,
      );
    }
    if (excluded.has(e.url)) {
      out.push(`exclusion ${JSON.stringify(e.url)} is listed twice`);
    }
    excluded.add(e.url);
  }

  const expected = new Set<string>();
  for (const s of inventory.surfaces) {
    if (excluded.has(s.url)) {
      out.push(
        `surface ${JSON.stringify(s.url)} is in BOTH the inventory and the exclusions file — a crawled surface cannot be excluded (stale exclusion)`,
      );
    }
    if (depth === 'full' || s.lean) expected.add(s.url);
  }

  for (const url of [...visited].sort()) {
    if (!expected.has(url)) {
      out.push(
        `NEW surface ${JSON.stringify(url)} visited but not pinned in ${stack.inventoryFilename} — coverage grew; regenerate deliberately with CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=${stack.name}`,
      );
    }
  }
  for (const url of [...expected].sort()) {
    if (!visited.has(url)) {
      out.push(
        `pinned surface ${JSON.stringify(url)} (${depth} set) was NOT visited — coverage shrank; fix the crawl/stack regression, never delete the row to make this pass`,
      );
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// ds/query supersession reconciliation
// ---------------------------------------------------------------------------

/**
 * Stable signature of a ds/query request by its LOGICAL query payload
 * — the datasource-typed set of `refId=expr` pairs — with the
 * per-request `requestId`/`SQR…` nonce deliberately excluded. Two
 * ds/query requests carrying the same query set over the same
 * datasource type share a signature even though their unique
 * `requestId` differs.
 */
export function dsQuerySignature(resp: DsResponseView): string {
  const dsType =
    new URL(resp.url, 'http://x').searchParams.get('ds_type') ?? '';
  const pairs = [...refIdToExpr(resp.requestBody).entries()]
    .map(([refId, expr]) => `${refId}=${expr}`)
    .sort();
  return `${dsType} ${pairs.join(' ')}`;
}

/**
 * Set of ds/query signatures that ultimately SUCCEEDED (2xx) anywhere
 * in a capture window.
 *
 * Reconciles Grafana's last-write-wins query supersession: a Scenes
 * app (Explore Traces, Metrics Drilldown, …) re-fires a panel's query
 * whenever a variable resolves, and ABORTS the older in-flight request
 * the instant the newer one is issued. The aborted request surfaces to
 * the browser as a transient `plugin.requestFailureError` 500 (the
 * backend call ate a `context canceled`), but it is invisible to the
 * user — the panel renders the newer request's result. A non-2xx whose
 * signature appears here is one of those superseded ghosts, not a
 * server fault. A genuinely-broken query never has a 2xx sibling, so it
 * is NOT suppressed — this is reconciliation, not an escape hatch.
 */
export function succeededDsQuerySignatures(
  captured: ReadonlyArray<DsResponseView>,
): Set<string> {
  const out = new Set<string>();
  for (const resp of captured) {
    if (!resp.url.includes('/api/ds/query')) continue;
    if (resp.status >= 200 && resp.status <= 299) {
      out.add(dsQuerySignature(resp));
    }
  }
  return out;
}

/**
 * True iff `resp` is a non-2xx ds/query whose exact query signature
 * later (or earlier) succeeded in the same window — i.e. a superseded,
 * Grafana-aborted in-flight request that the user never saw fail.
 */
export function isSupersededDsQueryFailure(
  resp: DsResponseView,
  succeededSigs: ReadonlySet<string>,
): boolean {
  if (!resp.url.includes('/api/ds/query')) return false;
  if (resp.status >= 200 && resp.status <= 299) return false;
  return succeededSigs.has(dsQuerySignature(resp));
}

// The dangling-operand TraceQL init-race reconciler
// (isTransientMalformedTraceQLFailure + DANGLING_TRACEQL_OPERAND) moved to
// helpers/reconcile.ts — imported + re-exported at the top of this file.

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

export function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…<truncated, ${s.length - max} more char(s)>`;
}
