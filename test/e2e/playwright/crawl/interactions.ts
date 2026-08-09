/**
 * Interaction sweep — the crawler's "everything clickable that
 * changes the consumption mode" layer.
 *
 * The BFS crawl (crawl.spec.ts) visits every URL-harvested surface at
 * its DEFAULT control state only. The 2026-06-10 maintainer find
 * proved that insufficient: clicking the Traces Drilldown breakdown
 * groupBy "kind" attribute — a state (`var-groupBy=kind`) no
 * harvested link ever encodes — fired a TraceQL query
 * (`{… && kind != nil} | rate() by(kind)`) cerberus 422'd, and the
 * crawler never saw it. The gap class is INTERACTIVE CONTROLS that
 * change which queries a page fires without being links: pulldowns,
 * tab strips, radio toggles, attribute pickers, adhoc-filter
 * builders, datasource pickers.
 *
 * This module owns:
 *   1. CONTROL DISCOVERY — enumerate the view-affecting controls on a
 *      visited page (tab strips, radio groups, comboboxes, titled
 *      option lists, metric select tiles, adhoc-filter builders),
 *      excluding anything mutating (save / delete / create / add-tab
 *      / settings — the crawl's read-only doctrine) and anything
 *      whose state another spec layer owns (time-range and refresh
 *      pickers → iterate-time-ranges.spec.ts; panel kiosk menus →
 *      iterate-panel-kiosk.spec.ts).
 *   2. BOUNDED PLANNING — the locked pairwise design:
 *        - structural low-cardinality controls (≤ STRUCTURAL_MAX_OPTIONS
 *          options: groupBy attributes, actionView tabs, metric type,
 *          layout) enumerate FULLY;
 *        - high-cardinality controls (filter values, metric-name
 *          tiles, tag filters) take ONE representative option;
 *        - cross-control combos are PAIRWISE via surface chaining: a
 *          deviation that encodes to the URL becomes a first-class
 *          surface (see StructuralParamRule in lib.ts); sweeping a
 *          surface that already pins ONE structural param uses the
 *          representative plan (each interaction there forms a pair),
 *          and surfaces pinning ≥2 params are terminal — visited
 *          with the universal oracles, never expanded. Every
 *          surface's plan is HARD-CAPPED (SINGLE_SWEEP_CAP at the
 *          base, PAIRWISE_SWEEP_CAP=16 for combo-forming surfaces);
 *          exceeding a cap FAILS the crawl listing every planned
 *          interaction — never a silent truncation.
 *   3. GESTURE DRIVING — perform one planned interaction on a freshly
 *      navigated page (fresh navigation per interaction keeps every
 *      state's provenance deterministic), returning the post-click
 *      URL so the spec can either enqueue it as a discovered surface
 *      (URL-encoding controls) or evaluate the universal oracles
 *      in-place and record the state as `<canonical>#<control>=<value>`
 *      in the inventory (non-URL controls).
 *
 * Selector conventions verified live against grafana/grafana:12.2.9
 * (2026-06-11): tab strips are `[role="tablist"] [role="tab"]` with
 * `data-testid Tab <name>` testids; radio groups are
 * `[role="radiogroup"] input[type=radio]` with `label[for]` text or
 * `option-<value>-radiogroup-N` ids; selects/comboboxes are
 * `input[role="combobox"]` whose dropdown renders
 * `[role="option"]` / `data-testid Select option` entries; the
 * Traces Drilldown attribute picker is a bare `ul > li[title]` list;
 * Metrics Drilldown metric tiles are `[data-testid^="select-action-"]`.
 */

import type { Locator, Page } from '@playwright/test';

// ---------------------------------------------------------------------------
// Bounds (the locked design)
// ---------------------------------------------------------------------------

/**
 * A control with more options than this is HIGH-cardinality: its
 * value set is data-derived (attribute values, metric names, tags),
 * so it contributes one representative option instead of a full
 * enumeration. 12 covers every structural control verified live
 * (the largest, the Traces Drilldown favorites attribute list,
 * carries 10) while excluding the data-derived lists (the "All"
 * attributes scope renders 51).
 *
 * Applies to 'tab' / 'radio' / 'option-list' controls only — Grafana
 * core's TabsBar and RadioButtonGroup iterate a literal array baked
 * into the plugin bundle at build time, so their SIZE really is a
 * fact about the app. 'combobox' controls get no such guarantee (see
 * COMBOBOX_CARDINALITY_RULES below): a react-select/downshift input
 * can be bound to either a bundle-fixed menu (sort direction) or a
 * datasource query result (field names, tag values), and nothing
 * about ITS SIZE tells the two apart — an 11-tag dashboard-browse
 * "Tag filter" and a 13-field Logs Drilldown "Filter by fields" are
 * on OPPOSITE sides of this threshold despite both being data.
 */
export const STRUCTURAL_MAX_OPTIONS = 12;

/**
 * Which sweep mode a 'combobox' control's option set gets, decided by
 * CONTROL IDENTITY (its discovery `key` — a testid/aria-label/
 * placeholder-derived string, fixed by the pinned app version) rather
 * than by how many options happen to render today. Same doctrine as
 * lib.ts's StructuralParamRule, generalized from URL query params to
 * in-place interaction fragments (`#control=value`):
 *
 *   - 'enumerate' — the option set is a fact about the APP: a fixed
 *     menu (sort direction, sort function, result-count picker, log
 *     severity levels, matcher operators, …) baked into the widget's
 *     own contract. Every option keys its own state.
 *   - 'parameterize' — the DEFAULT for every combobox key this table
 *     does not name. A control whose options come from a datasource
 *     query (detected field/label/tag names, metric names, dashboard
 *     tags, …) collapses to one representative (`{rep}`) regardless
 *     of how many options the CURRENT seed happens to produce — #1872
 *     was `Filter by fields` and `Tag filter` pinning literal seeded
 *     names purely because their count landed on the wrong side of
 *     STRUCTURAL_MAX_OPTIONS that day.
 *
 * A key with no matching rule parameterizes — the safe default. A
 * control genuinely misclassified 'parameterize' loses enumeration
 * (a coverage gap the interaction-count ratchet can catch and a
 * reviewer can promote to 'enumerate' deliberately); one
 * misclassified 'enumerate' pins the seeded dataset into the key,
 * which is the defect class this table exists to close. The two
 * failure modes are not symmetric, so the default favors the safe
 * one.
 */
export type ComboboxCardinalityRule = {
  /** Matched against the control's discovery KEY — its identity,
   * never an option VALUE. */
  keyPattern: RegExp;
  mode: 'enumerate' | 'parameterize';
  /**
   * Optional closed option set. When present, a discovered option
   * outside it fails the crawl loudly instead of silently mining the
   * value into the plan — the same self-check StructuralParamRule
   * runs for 'enumerate' query params: either the app gained an
   * option (add it here deliberately) or the list is not actually
   * closed (the rule is miscategorised; make it 'parameterize').
   */
  values?: ReadonlyArray<string>;
};

// Every pattern tolerates assignUniqueKeys' `#<n>` disambiguation
// suffix (two same-family controls on one page) so a duplicate sibling
// doesn't silently fall through to the default.
export const COMBOBOX_CARDINALITY_RULES: ReadonlyArray<ComboboxCardinalityRule> = [
  // Logs/Traces Drilldown table controls: layout, sort and page-size
  // menus are fixed UI vocabulary, not data — how a table SORTS or how
  // many rows it FETCHES does not depend on what the seeded logs say.
  { keyPattern: /^select\[SortBy direction\](#\d+)?$/, mode: 'enumerate' },
  { keyPattern: /^select\[SortBy function\](#\d+)?$/, mode: 'enumerate' },
  {
    keyPattern: /^select\[\{n\} (logs|lines|rows)\](#\d+)?$/,
    mode: 'enumerate',
  },
  // Loki's OWN normalized severity vocabulary (critical/error/warn/
  // info/debug/trace/unknown), not a set of arbitrary detected values.
  { keyPattern: /^select\[detected_level\](#\d+)?$/, mode: 'enumerate' },
  // A downshift input with neither a testid nor an aria-label falls
  // back to its bare React useId-derived element id (erased to the
  // shared `{rid}` placeholder — see REACT_ID_PLACEHOLDER), so this ONE
  // key pattern addresses two different, both fixed-vocabulary, widgets
  // verified live: the Logs Drilldown per-value operator step
  // (Include | Exclude) and the Traces Drilldown duration-metric
  // percentile picker (p50 | p90 | p99).
  {
    keyPattern: /^select\[downshift-\{rid\}-input\](#\d+)?$/,
    mode: 'enumerate',
  },
  // Prometheus/Loki adhoc-filter matcher operator: exactly the four
  // matcher symbols the query languages define — a protocol constant,
  // not app or data state.
  {
    keyPattern: /^select\[Select match operator\](#\d+)?$/,
    mode: 'enumerate',
    values: ['=', '!=', '=~', '!~'],
  },
  // Dashboard-browse "sort by": a fixed alphabetical/recency menu.
  { keyPattern: /^select\[Sort\](#\d+)?$/, mode: 'enumerate' },
  // Traces Drilldown "primary signal" picker (the Select half — the
  // RadioButtonGroup half carrying "Root spans" / "All spans" is a
  // separate 'radio' control, already structural by size). The widget
  // carries NEITHER a testid NOR an aria-label NOR a placeholder — a
  // plugin-side accessibility gap verified live against
  // grafana-exploretraces-app, so discovery's identity chain falls all
  // the way through to the react-select mount-order id, normalized to
  // the generic `react-select-{rid}-input` every unlabeled react-select
  // on ANY page shares. That generic pattern is deliberately NOT enough
  // on its own to earn 'enumerate' — a coincidentally-anonymous,
  // genuinely data-derived select elsewhere must not inherit it. The
  // `values` set closes that gap: it is the SAME three primarySignal
  // labels lib.ts's `var-primarySignal` StructuralParamRule already
  // declares (`kind=server` / `kind=consumer` /
  // `span.db.system.name!=""`, minus the two the RadioButtonGroup
  // holds), read off the plugin bundle rather than a seed. A control
  // that matches the key but NOT this option set throws in
  // planInteractions's self-check below instead of silently borrowing
  // the classification — the crawl was reaching this exact set,
  // uncatalogued, before: `representativeOption`'s codepoint-order pick
  // landed on "Consumer spans…" ('C' < 'D' < 'S'), so the sweep drove
  // Consumer every run and never Server, no matter how much backing
  // data Server carried (#1992).
  {
    keyPattern: /^select\[react-select-\{rid\}-input\](#\d+)?$/,
    mode: 'enumerate',
    values: [
      'Server spansExplore server-specific segments traces',
      'Consumer spansAnalyze interactions initiated by consumer services',
      'Database callsEvaluate performance issues in database interactions',
    ],
  },
];

export function comboboxCardinalityRule(
  key: string,
): ComboboxCardinalityRule | undefined {
  return COMBOBOX_CARDINALITY_RULES.find((r) => r.keyPattern.test(key));
}

/**
 * Hard cap on a base surface's (0 pinned structural params)
 * single-control enumeration. Generous — the busiest verified
 * surface (Traces Drilldown entry) plans ~20 — but HARD: overflow
 * fails the crawl listing the full plan, forcing a deliberate
 * redesign instead of a silent partial sweep.
 */
export const SINGLE_SWEEP_CAP = 24;

/**
 * Hard cap on a combo-forming surface's (1 pinned structural param)
 * representative plan — every interaction there pairs the pinned
 * param with one other control, so this bounds the pairwise combos
 * attributable to a surface at ≤16 per the locked design.
 */
export const PAIRWISE_SWEEP_CAP = 16;

// ---------------------------------------------------------------------------
// Control model
// ---------------------------------------------------------------------------

export type ControlKind =
  | 'tab' // [role=tablist] strip — actionView tabs, attribute scopes
  | 'radio' // [role=radiogroup] — layout, primary signal, metric type
  | 'combobox' // select/combobox — sort-by, datasource, levels, limits
  | 'option-list' // ul > li[title] picker — Traces Drilldown groupBy
  | 'select-tile' // [data-testid^=select-action-] — metric tiles
  | 'adhoc-filter'; // adhoc filter builder — key → representative value

export type DiscoveredControl = {
  kind: ControlKind;
  /** Stable human-readable key — joins the in-place state notation. */
  key: string;
  /** Option labels in DOM order (comboboxes: probed open). */
  options: string[];
  /** Index of the currently-selected option, -1 when undeterminable. */
  selectedIndex: number;
  /**
   * True for controls whose option set is data-derived by
   * construction (metric tiles, adhoc-filter keys) — they take one
   * representative regardless of how many options happen to render.
   */
  forcedHighCardinality: boolean;
  /** Per-option locator hints (tab testids, radio input ids, li titles). */
  optionHints: string[];
  /** Locator hint addressing the control itself (combobox inputs). */
  controlHint: string;
  /**
   * Comboboxes: how many parentElement hops separate the input named
   * by `controlHint` from the element a click must land on. See
   * COMBOBOX_CLICK_TARGET_MAX_HOPS.
   */
  clickHops: number;
};

export type PlannedInteraction = {
  control: DiscoveredControl;
  /** Option label to drive. */
  option: string;
  /**
   * Value used in the in-place state notation. High-cardinality
   * representatives parameterize to `{rep}` so data-derived values
   * (first tag, first metric name) can't flicker the inventory.
   */
  stateValue: string;
  /**
   * True for the first planned interaction of each control — the
   * lean lane's representative subset (depth changes STATES, never
   * RULES: lean drives one state per control, full drives them all).
   */
  leanRepresentative: boolean;
};

/**
 * Mutating or out-of-scope affordances the discovery NEVER plans:
 * write affordances (read-only doctrine), auth, and Grafana chrome
 * whose states other spec layers own.
 */
export const EXCLUDED_CONTROL_PATTERNS: ReadonlyArray<RegExp> = [
  /save|delete|remove|create|import|export|upload|share|snapshot|sign\s?out|settings/i,
  /add label tab|add to filters|add panel|new (dashboard|folder|panel)/i,
  /time ?picker|refresh ?picker|time ?zone/i, // iterate-time-ranges owns time state
  /search/i, // free-text inputs filter client-side lists, not queries
];

export function isExcludedControlName(name: string): boolean {
  return EXCLUDED_CONTROL_PATTERNS.some((re) => re.test(name));
}

// ---------------------------------------------------------------------------
// Planning (pure — unit-pinnable)
// ---------------------------------------------------------------------------

/**
 * The ONE option a high-cardinality (data-backed) control contributes
 * to a plan: the codepoint-smallest label in the set.
 *
 * Order-independence is the point. A control whose options come from
 * the ingested data — adhoc-filter label keys, metric-name tiles, tag
 * values — renders them in whatever order the app's query returned,
 * and that order moves as the stack ingests. Taking "the first option
 * the DOM happened to carry" therefore drives a DIFFERENT option run
 * to run, which fires a different query and (when the gesture encodes
 * to the URL) mints a different surface — the crawl stops being a
 * function of the application. Deriving the pick from the SET removes
 * the render order from the answer entirely.
 *
 * `localeCompare` is deliberately avoided: its collation is
 * host-locale- and ICU-version-dependent, which would reintroduce the
 * very cross-run instability this function exists to remove.
 */
export function representativeOption(options: ReadonlyArray<string>): string {
  let best = options[0]!;
  for (const option of options) if (option < best) best = option;
  return best;
}

/**
 * Build the bounded interaction plan for one surface.
 *
 * `pinnedParams` is the surface's pinned-structural-param count (see
 * lib.ts pinnedStructuralParamCount): 0 → full plan under
 * SINGLE_SWEEP_CAP; 1 → representative plan (first option per
 * control) under PAIRWISE_SWEEP_CAP; ≥2 → empty plan (terminal).
 *
 * Throws on cap overflow with the COMPLETE plan in the message —
 * the deliberate-redesign escape, never silent truncation.
 */
export function planInteractions(
  controls: ReadonlyArray<DiscoveredControl>,
  pinnedParams: number,
): PlannedInteraction[] {
  if (pinnedParams >= 2) return [];
  const representativeOnly = pinnedParams === 1;

  const plan: PlannedInteraction[] = [];
  for (const control of controls) {
    // 'combobox' controls decide structural-vs-data-derived by
    // DECLARED identity (COMBOBOX_CARDINALITY_RULES), never by how
    // many options the current seed happens to render — see #1872.
    // Every other kind keeps the size-based check: Grafana core's
    // TabsBar / RadioButtonGroup / option-list widgets iterate a
    // bundle-fixed array, so their SIZE is a fact about the app (see
    // STRUCTURAL_MAX_OPTIONS).
    let structural: boolean;
    if (control.kind === 'combobox') {
      const rule = comboboxCardinalityRule(control.key);
      if (rule?.mode === 'enumerate' && rule.values !== undefined) {
        const unknown = control.options.filter(
          (o) => !rule.values!.includes(o),
        );
        if (unknown.length > 0) {
          throw new Error(
            `interaction sweep: combobox ${control.key} offers option(s) ` +
              `${unknown.map((o) => JSON.stringify(o)).join(', ')} outside its declared ` +
              `closed set [${rule.values.map((v) => JSON.stringify(v)).join(', ')}] in ` +
              `COMBOBOX_CARDINALITY_RULES — either the pinned app version gained an option ` +
              `(add it to the rule's values deliberately) or the option list is not actually ` +
              `closed, in which case the rule must become mode 'parameterize'.`,
          );
        }
      }
      structural = rule?.mode === 'enumerate';
    } else {
      structural =
        !control.forcedHighCardinality &&
        control.options.length <= STRUCTURAL_MAX_OPTIONS;
    }
    const drivable = control.options.filter(
      (_, i) => i !== control.selectedIndex,
    );
    if (drivable.length === 0) continue;
    // High-cardinality → exactly ONE representative. The pick is the
    // lexicographically smallest option so it is a function of the
    // option SET, never of the order the app rendered it in:
    // data-backed lists (adhoc label keys, metric-name tiles, tag
    // values) reorder as the stack ingests, and a DOM-order pick then
    // drives a different option — and mints a different surface — on
    // every run.
    const options = structural ? drivable : [representativeOption(drivable)];
    for (let i = 0; i < options.length; i++) {
      plan.push({
        control,
        option: options[i]!,
        stateValue: structural ? options[i]! : '{rep}',
        leanRepresentative: i === 0,
      });
      // Representative plan → one option per control.
      if (representativeOnly) break;
    }
  }

  const cap = representativeOnly ? PAIRWISE_SWEEP_CAP : SINGLE_SWEEP_CAP;
  if (plan.length > cap) {
    const listing = plan
      .map((p) => `${p.control.kind}:${p.control.key}=${p.option}`)
      .join('\n  - ');
    throw new Error(
      `interaction sweep: surface plans ${plan.length} interaction(s), exceeding the ` +
        `${representativeOnly ? 'pairwise' : 'single-sweep'} cap ${cap} — the bound forces a ` +
        `deliberate redesign (tighten control discovery, reclassify a control, or raise the cap ` +
        `in interactions.ts with review), never a silent truncation. Full plan:\n  - ${listing}`,
    );
  }
  return plan;
}

/**
 * In-place state notation: a control deviation that does NOT encode
 * to the URL is recorded in the inventory as
 * `<canonical>#<control-key>=<value>`.
 */
export function interactionStateKey(
  canonical: string,
  control: DiscoveredControl,
  stateValue: string,
): string {
  return `${canonical}#${control.key}=${stateValue}`;
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

/**
 * In-page chrome the discovery never looks inside: navigation, the
 * mega menu, modals, toasts, and the top app chrome (time picker
 * lives there).
 */
const CHROME_ANCESTOR_SELECTOR = [
  'nav',
  'header',
  '[role="dialog"]',
  '[role="menu"]',
  '[data-testid*="mega-menu"]',
  '[aria-label="Toast container"]',
].join(', ');

/**
 * How far above a combobox `input` its clickable element may sit.
 *
 * A non-searchable react-select renders its `input[role=combobox]` as
 * a sub-pixel, fully transparent `dummyInput`, and the visible control
 * is the `input-wrapper` two levels up
 * (`input → value-container → control → input-wrapper`). Clicking the
 * input therefore never succeeds — Playwright's hit test lands on a
 * sibling and it retries until the timeout, surfacing as a bare
 * `locator.click: Timeout`. Four hops covers the deepest such nesting.
 *
 * The bound doubles as the RENDERED test: a control with no clickable
 * element within it is not laid out at all.
 */
const COMBOBOX_CLICK_TARGET_MAX_HOPS = 4;

/**
 * React `useId` tokens — `:rNN:`, and React 19's `«rNN»` — as a source
 * pattern, so the one definition serves both the in-page key
 * derivation and the settle signature (a RegExp cannot cross the
 * page.evaluate boundary, a string can).
 *
 * The token is a MOUNT COUNTER: it is reassigned every time the
 * component remounts, so any two renders of the same control carry
 * different ids. Every place that compares controls across renders has
 * to erase it or it compares mount generations instead of controls.
 */
const REACT_ID_TOKEN_PATTERN = String.raw`(:r[0-9a-z]+:|«r[0-9a-z]+»)`;

/** What an erased React `useId` token reads as. */
const REACT_ID_PLACEHOLDER = '{rid}';

/**
 * react-select's own element-id prefix (`react-select-<n>-input`,
 * `react-select-<n>-listbox`), as a source pattern for the same two
 * consumers as REACT_ID_TOKEN_PATTERN.
 *
 * `<n>` is the same kind of token by a different mechanism: a
 * MODULE-GLOBAL instance counter that react-select increments on every
 * mount for the lifetime of the page, so the number a given control
 * carries is a function of how many selects mounted before it — which
 * varies with the route that mounted it and with how much of an app's
 * scene had rendered by the time the control appeared. A control keyed
 * by it is keyed by mount order.
 *
 * Traces Drilldown's primary-signal picker is one: it has no testid and
 * no aria-label, so its key falls all the way through to the element id.
 */
const REACT_SELECT_INSTANCE_ID_PATTERN = String.raw`react-select-\d+-`;

/**
 * What an erased react-select instance counter reads as. The
 * `react-select-` prefix survives erasure so an inventory row still
 * names the widget family the key addresses.
 */
const REACT_SELECT_INSTANCE_ID_REPLACEMENT = `react-select-${REACT_ID_PLACEHOLDER}-`;

type RawControl = {
  kind: ControlKind;
  key: string;
  options: string[];
  selectedIndex: number;
  /** Locator recipe — see driveInteraction. */
  locatorKind: 'tab' | 'radio-signature' | 'combobox' | 'li-title' | 'tile';
  /** Per-option locator hints (testids, option names, li titles, …). */
  optionHints: string[];
  /** Hint addressing the control itself (combobox input). */
  controlHint: string;
  /** See DiscoveredControl.clickHops. */
  clickHops: number;
  /**
   * Comboboxes only: the VISIBLE current-value text (react-select
   * renders it in a sibling div, not the input's value attribute) —
   * lets the probe mark the selected option so plans don't waste a
   * gesture re-selecting the default.
   */
  currentText?: string;
};

/**
 * Enumerate the static (non-combobox) controls currently in the DOM.
 * Pure DOM read — no gestures. Combobox option sets need an
 * open-the-dropdown probe and are handled by discoverControls.
 */
async function discoverStaticControls(page: Page): Promise<RawControl[]> {
  return await page.evaluate(
    ({
      chromeSelector,
      maxClickHops,
      reactIdPattern,
      reactIdPlaceholder,
      reactSelectPattern,
      reactSelectReplacement,
    }) => {
      const out: Array<{
        kind: string;
        key: string;
        options: string[];
        selectedIndex: number;
        locatorKind: string;
        optionHints: string[];
        controlHint: string;
        clickHops: number;
        currentText?: string;
      }> = [];
      const inChrome = (el: Element) => el.closest(chromeSelector) !== null;

      // --- Tab strips -----------------------------------------------------
      // Two stable testid schemes carry the tab identity:
      //   - Grafana core: `data-testid Tab <name>`
      //   - scenes apps (Logs Drilldown): `data-testid tab-<name>`
      // The VISIBLE label is unusable as identity — it carries a live
      // result count baked into the text ("Traces200", "Logs1K",
      // "Fields6") that flickers the inventory and (by inflating the
      // option set) breaches the plan cap. When neither testid is
      // present the count is stripped off the trailing edge of the
      // text as a last resort.
      const tabName = (t: Element) => {
        const testid = t.getAttribute('data-testid') ?? '';
        const core = /^data-testid Tab (.+)$/.exec(testid);
        if (core) return core[1]!;
        const scenes = /^data-testid tab-(.+)$/.exec(testid);
        if (scenes) return scenes[1]!;
        // Strip a trailing live-count run ("Logs1K" → "Logs",
        // "Traces200" → "Traces") from the bare text fallback.
        return (t.textContent ?? '')
          .trim()
          .replace(/\d+(\.\d+)?[KMB]?$/, '')
          .trim();
      };
      document.querySelectorAll('[role="tablist"]').forEach((tl) => {
        if (inChrome(tl)) return;
        const tabs = [...tl.querySelectorAll('[role="tab"]')];
        if (tabs.length < 2) return;
        // Navigation tab strips — every tab is an <a href> that swaps
        // the URL path (Logs Drilldown's logs/labels/fields/patterns
        // strip) — are already first-class BFS surfaces (harvested as
        // links + expandSiblingTabs). Re-driving them as in-place
        // controls would double-count them under a volatile key, so
        // skip any strip whose tabs all carry an href.
        const allNavLinks = tabs.every(
          (t) =>
            t.tagName === 'A' ||
            t.closest('a[href]') !== null ||
            t.getAttribute('href') !== null,
        );
        if (allNavLinks) return;
        const names = tabs.map(tabName);
        out.push({
          kind: 'tab',
          key: `tabs[${names.join('|')}]`,
          options: names,
          selectedIndex: tabs.findIndex(
            (t) => t.getAttribute('aria-selected') === 'true',
          ),
          locatorKind: 'tab',
          optionHints: tabs.map(
            (t) => t.getAttribute('data-testid') ?? tabName(t),
          ),
          controlHint: '',
          clickHops: 0,
        });
      });

      // --- Radio groups ---------------------------------------------------
      // Grafana RadioButtonGroup: hidden inputs + label[for]. Option
      // identity: label text, else the value embedded in the input id
      // (`option-<value>-radiogroup-N` — the numeric group counter is
      // MOUNT-ORDER dependent and useless as a locator across
      // navigations; the driver re-finds the group by its option-name
      // signature instead), else the input name (the Traces Drilldown
      // metric selector renders three one-radio groups named
      // metric-rate / metric-errors / metric-duration). KEEP optName
      // IN LOCKSTEP with the copy in driveInteraction's radio case.
      document.querySelectorAll('[role="radiogroup"]').forEach((rg) => {
        if (inChrome(rg)) return;
        const inputs = [...rg.querySelectorAll('input[type="radio"]')] as
          HTMLInputElement[];
        if (inputs.length === 0) return;
        const optName = (i: HTMLInputElement) => {
          const lbl = i.id
            ? rg.querySelector(`label[for="${CSS.escape(i.id)}"]`)
            : null;
          const text = (lbl?.textContent ?? '').trim();
          if (text !== '') return text;
          const m = /^option-(.+)-radiogroup-\d+$/.exec(i.id);
          if (m) return m[1]!;
          return i.name || i.id;
        };
        const options = inputs.map(optName);
        out.push({
          kind: 'radio',
          key: `radio[${options.join('|')}]`,
          options,
          selectedIndex: inputs.findIndex((i) => i.checked),
          locatorKind: 'radio-signature',
          optionHints: options,
          controlHint: '',
          clickHops: 0,
        });
      });

      // --- Titled option lists ---------------------------------------------
      // The Traces Drilldown attribute picker: a bare ul whose li
      // children carry title="<attribute>" (no roles, no testids).
      // Selection is CSS-only; when exactly one li's class set
      // differs from the rest it is the selected row — otherwise
      // selectedIndex stays -1 and the current value's click lands
      // as a harmless in-place no-op.
      //
      // Tempo TRACE-SCOPED intrinsics (rootName / rootServiceName /
      // traceDuration / …) are offered by the Drilldown's groupBy
      // picker but are STRUCTURALLY unanswerable on the per-span
      // OTel-ClickHouse schema cerberus serves: `rate() by(rootName)`
      // needs whole-trace values no span row materialises, so cerberus
      // returns a deliberate, reference-pinned 422 (rejection-parity
      // catalogue; test/rejection-parity). A `by(<trace-scoped>)`
      // breakdown is therefore not a cerberus CONSUMPTION MODE — it is
      // out of the backend's domain, the same structural-exclusion
      // logic as the foreign `grafanacloud-*` datasource options. Drop
      // them so the sweep doesn't mint a surface whose only possible
      // render is the documented rejection banner. NOT a tolerance:
      // the rejection itself is asserted by the rejection-parity gate.
      const traceScopedIntrinsics = new Set([
        'rootName',
        'rootServiceName',
        'rootTraceName',
        'traceDuration',
        'trace:rootName',
        'trace:rootService',
        'trace:duration',
      ]);
      document.querySelectorAll('ul').forEach((ul) => {
        if (inChrome(ul)) return;
        const items = [...ul.children];
        // Identify a titled-list control on the RAW titles (every li
        // must carry a title) so the structural shape check is
        // unaffected by the intrinsic filter below.
        const rawTitles = items.map((li) => li.getAttribute('title') ?? '');
        const allTitled = rawTitles.every((t) => t !== '');
        if (!allTitled || rawTitles.length < 2) return;
        let selectedIndex = -1;
        if (items.length >= 3) {
          const classes = items.map((li) => li.className);
          const counts = new Map<string, number>();
          for (const c of classes) counts.set(c, (counts.get(c) ?? 0) + 1);
          const unique = classes.filter((c) => counts.get(c) === 1);
          if (unique.length === 1) {
            selectedIndex = classes.indexOf(unique[0]!);
          }
        }
        const selectedTitle =
          selectedIndex >= 0 ? rawTitles[selectedIndex] : undefined;
        // Drop the structurally-unanswerable trace-scoped intrinsics
        // from the driveable option set (see the comment above).
        const titles = rawTitles.filter((t) => !traceScopedIntrinsics.has(t));
        if (titles.length < 2) return;
        out.push({
          kind: 'option-list',
          key: 'attribute-list',
          options: titles,
          // Re-resolve the selected index against the filtered set so
          // a representative plan never re-selects the current value.
          selectedIndex:
            selectedTitle !== undefined ? titles.indexOf(selectedTitle) : -1,
          locatorKind: 'li-title',
          optionHints: titles,
          controlHint: '',
          clickHops: 0,
        });
      });

      // --- Select tiles ------------------------------------------------------
      // Metrics Drilldown per-metric Select actions. One control,
      // high-cardinality by construction (one tile per metric name).
      const tiles = [
        ...document.querySelectorAll('[data-testid^="select-action-"]'),
      ];
      if (tiles.length > 0) {
        const names = tiles.map((t) =>
          (t.getAttribute('data-testid') ?? '').replace(
            /^select-action-/,
            '',
          ),
        );
        out.push({
          kind: 'select-tile',
          key: 'select-tile',
          options: names,
          selectedIndex: -1,
          locatorKind: 'tile',
          optionHints: tiles.map((t) => t.getAttribute('data-testid') ?? ''),
          controlHint: '',
          clickHops: 0,
        });
      }

      // --- Comboboxes (identity only — options probed by the caller) -------
      const comboboxes = [
        ...document.querySelectorAll('input[role="combobox"]'),
      ] as HTMLInputElement[];
      for (const cb of comboboxes) {
        if (inChrome(cb)) continue;
        const testid = cb.getAttribute('data-testid') ?? '';
        const aria = cb.getAttribute('aria-label') ?? '';
        const placeholder = cb.getAttribute('placeholder') ?? '';
        const isAdhoc =
          cb.id.startsWith('var-adhoc') ||
          /^\+ ?label/i.test(placeholder) ||
          /filter by label/i.test(placeholder);
        // When the input carries no testid of its own, the nearest
        // ancestor data-testid is a far more STABLE identity than the
        // placeholder/value text: Grafana's scenes variables wrap each
        // picker in `<… data-testid="detected_level filter variable">`
        // / `"<var> filter variable"`, whereas the placeholder mirrors
        // the CURRENT selection (the line-limit picker's placeholder is
        // literally "1000 logs", the level filter's "All levels") and
        // flickers across navigations. Prefer the ancestor testid;
        // strip the boilerplate so `… filter variable` → `<var>`.
        const ancestorTestid =
          testid === ''
            ? (cb
                .closest('[data-testid]')
                ?.getAttribute('data-testid') ?? '')
            : '';
        const ancestorKey = ancestorTestid
          .replace(/^data-testid /, '')
          .replace(/ (filter )?variable$/, '')
          .trim();
        const rawKey =
          testid
            .replace(/^data-testid /, '')
            .replace(/-input$/, '')
            // The datasource-variable testid embeds the CURRENT value
            // ("…value link text cerberus-tempo"); normalize to the
            // control family so the key survives a selection change.
            .replace(
              /^Dashboard template variables Variable Value DropDown value link text .*$/,
              'datasource-picker',
            ) ||
          aria ||
          // Only fall through to the bare placeholder when there is no
          // usable ancestor identity (`input-wrapper` and `template
          // variable` are non-distinguishing boilerplate).
          (ancestorKey !== '' &&
          ancestorKey !== 'input-wrapper' &&
          ancestorKey !== 'template'
            ? ancestorKey
            : '') ||
          placeholder ||
          cb.id;
        // Three render-order / selection-dependent token classes must
        // never reach an inventory key:
        //   - React useId tokens (`:rNN:` / React 19's `«rNN»`) —
        //     downshift inputs fall back to them for element ids;
        //   - react-select instance counters (`react-select-3-input`) —
        //     the same thing from react-select's own module-global
        //     counter, reached by any select with neither a testid nor
        //     an aria-label;
        //   - count-bearing placeholders (`1000 logs`, `500 logs`) —
        //     the line-limit picker's placeholder mirrors the current
        //     selection, so it flickers per surface.
        // Normalize all three to fixed tokens; assignUniqueKeys'
        // DOM-order `#n` suffix keeps same-page twins distinct.
        const key = rawKey
          .replace(new RegExp(reactIdPattern, 'g'), reactIdPlaceholder)
          .replace(new RegExp(reactSelectPattern, 'g'), reactSelectReplacement)
          .replace(/^\d+ (logs|lines|rows)$/, '{n} $1');
        if (key === '') continue;
        // Resolve the click target and the rendered test in one walk
        // (see COMBOBOX_CLICK_TARGET_MAX_HOPS). "Clickable" is the
        // browser's own hit test, which is also the check Playwright
        // runs before every click: the element whose centre point
        // actually receives the press must be the candidate itself or
        // one of its descendants. A react-select dummyInput fails it
        // twice over — sub-pixel box, and its centre lands on a
        // sibling — and so does the value-container one level up,
        // whose centre lands on the dropdown chevron NEXT to it. The
        // input-wrapper that owns both passes, and that is the element
        // a user presses.
        const hittable = (el: Element) => {
          const r = el.getBoundingClientRect();
          const cx = r.left + r.width / 2;
          const cy = r.top + r.height / 2;
          // Below the fold the hit test is meaningless — nothing is at
          // a point outside the viewport until Playwright scrolls it
          // in, which it does for us. Fall back to "has a box".
          if (
            cx < 0 ||
            cy < 0 ||
            cx > window.innerWidth ||
            cy > window.innerHeight
          ) {
            return r.width > 0 && r.height > 0;
          }
          const hit = document.elementFromPoint(cx, cy);
          return hit !== null && el.contains(hit);
        };
        let clickHops = 0;
        let target: Element = cb;
        while (!hittable(target) && clickHops < maxClickHops) {
          const parent = target.parentElement;
          if (parent === null) break;
          target = parent;
          clickHops++;
        }
        // Nothing within the bound can receive a press: the control is
        // not laid out (Grafana renders a HIDDEN scenes template
        // variable's picker at zero size), so it is not a control the
        // user can operate and not one the sweep may plan a gesture
        // for.
        if (!hittable(target)) continue;
        // The visible current value: react-select renders it in a
        // sibling/ancestor div, not the input. Walk up a few levels
        // and take the first short non-empty text.
        let currentText = cb.value.trim();
        let ancestor: Element | null = cb.parentElement;
        for (let depth = 0; currentText === '' && ancestor && depth < 3; depth++) {
          const text = (ancestor.textContent ?? '').trim();
          if (text !== '' && text.length <= 60) currentText = text;
          ancestor = ancestor.parentElement;
        }
        out.push({
          kind: isAdhoc ? 'adhoc-filter' : 'combobox',
          key: `${isAdhoc ? 'adhoc' : 'select'}[${key}]`,
          options: [],
          selectedIndex: -1,
          locatorKind: 'combobox',
          optionHints: [],
          controlHint: testid !== '' ? `testid=${testid}` : `id=${cb.id}`,
          clickHops,
          currentText,
        });
      }

      return out;
    },
    {
      chromeSelector: CHROME_ANCESTOR_SELECTOR,
      maxClickHops: COMBOBOX_CLICK_TARGET_MAX_HOPS,
      reactIdPattern: REACT_ID_TOKEN_PATTERN,
      reactIdPlaceholder: REACT_ID_PLACEHOLDER,
      reactSelectPattern: REACT_SELECT_INSTANCE_ID_PATTERN,
      reactSelectReplacement: REACT_SELECT_INSTANCE_ID_REPLACEMENT,
    },
  ) as RawControl[];
}

/**
 * Disambiguate key collisions with a deterministic DOM-order suffix
 * (`#1`, `#2`, …) instead of dropping rows: several scenes apps
 * render multiple variable pickers under one testid family
 * (datasource + sort-by + wingman on the metrics-drilldown entry),
 * and dropping all but the first would silently hide real controls.
 * Runs over the FULL raw list (excluded / unprobeable controls
 * consume slots too) so a key is a pure function of the page's DOM
 * order — drive-time re-resolution (relocateCombobox) re-derives the
 * same keys on a fresh navigation.
 */
function assignUniqueKeys(raw: RawControl[]): RawControl[] {
  const counts = new Map<string, number>();
  for (const c of raw) {
    const n = counts.get(c.key) ?? 0;
    counts.set(c.key, n + 1);
    if (n > 0) c.key = `${c.key}#${n}`;
  }
  return raw;
}

/**
 * Locate a combobox's CLICK TARGET from its discovery hint: the input
 * the hint names, ascended `clickHops` levels to the element that
 * actually carries the control's rendered box (see
 * COMBOBOX_CLICK_TARGET_MAX_HOPS).
 */
function locateCombobox(page: Page, hint: string, clickHops: number): Locator {
  const input = hint.startsWith('testid=')
    ? page.locator(`input[data-testid="${hint.slice('testid='.length)}"]`)
    : page.locator(`input[id="${hint.slice('id='.length)}"]`);
  const first = input.first();
  if (clickHops === 0) return first;
  // Ascend in the LOCATOR rather than clicking a coordinate, so
  // Playwright still runs its actionability checks (visible, stable,
  // enabled, not occluded) against the element the user would press.
  return first.locator(
    `xpath=${Array.from({ length: clickHops }, () => '..').join('/')}`,
  );
}

/** The options currently rendered in an open select/combobox menu. */
function openMenuOptions(page: Page): Locator {
  return page.locator(
    '[data-testid="data-testid Select option"], [role="listbox"] [role="option"]',
  );
}

// ---------------------------------------------------------------------------
// Settling — the determinism substrate
// ---------------------------------------------------------------------------
//
// Every synchronisation point below used to be a fixed sleep on top of
// a `networkidle` settle. Neither observes the thing the sweep
// actually reads. A scenes app mounts its controls as each of its
// queries resolves, so `networkidle` resolves at an arbitrary point in
// that sequence and a fixed sleep either clears it or does not,
// depending on the machine and the moment. The consequences were not
// cosmetic: a control missing from one pass and present in the next
// shifts assignUniqueKeys' occurrence suffixes (`combobox … not
// re-found by key`), and a control missing from the DISCOVERY pass
// simply never mints its surfaces — which is how two runs of the same
// command produced different surface sets.
//
// The primitives here wait for the observable itself to stop changing,
// with a hard cap and a loud failure when it does not. They are
// synchronisation, not retry: nothing here re-attempts a failed
// gesture, and no bound is ever exceeded silently.

/**
 * Consecutive identical samples that make an observable SETTLED. Two
 * would prove only that one interval elapsed without a repaint; three
 * spans two full intervals, so a re-render whose period straddles one
 * of them still breaks the streak.
 */
const SETTLE_STABLE_SAMPLES = 3;

/** Gap between successive samples of a settling observable. */
const SETTLE_SAMPLE_INTERVAL_MS = 300;

/**
 * Hard cap on waiting for a page's control set to stop changing. A
 * surface still re-rendering its controls after this long is not a
 * state the sweep can attribute a gesture to, so the wait fails the
 * crawl rather than sampling a moving DOM.
 */
const CONTROL_SET_SETTLE_TIMEOUT_MS = 30_000;

/**
 * Hard cap on waiting for an OPEN dropdown's option list to stop
 * changing. Shorter than the page-level cap: the menu is already open
 * and its options are one query away.
 */
const MENU_SETTLE_TIMEOUT_MS = 15_000;

/**
 * How long an opened combobox gets to mount its FIRST option. A
 * control that renders none within this bound is a free-text input,
 * not a select, and the discovery drops it as such — the one verdict
 * an empty option list is allowed to produce. Generous because
 * data-backed lists (adhoc label keys, tag values) fetch their options
 * over the wire before rendering any.
 */
const MENU_MOUNT_TIMEOUT_MS = 8_000;

/** Hard cap on an Escape actually dismissing an open dropdown. */
const MENU_CLOSE_TIMEOUT_MS = 5_000;

/**
 * The label Grafana renders as the SOLE item of an option list whose
 * options are still in flight — its shared `loadingMessage`, used by
 * core Select, by Combobox and by the scenes ad-hoc filter pickers
 * (Metrics Drilldown's `+ label = value` is one).
 *
 * It is a placeholder, not an option: it carries no value, it is not
 * clickable to any effect, and — this is what makes it dangerous — a
 * list showing it holds perfectly still for as long as the fetch takes,
 * so it settles. Two consecutive crawl runs disagreed on the SAME
 * control in opposite directions because of it: one discovered the real
 * option set and then drove against the placeholder, the next
 * discovered the placeholder and then drove against the real set. Both
 * died in clickMenuOption, and an interaction that dies takes the
 * surfaces it would have minted with it.
 *
 * Excluding it from "settled" is synchronisation, not tolerance: the
 * read still has to reach a stable list of REAL options within
 * MENU_SETTLE_TIMEOUT_MS, and a list that never loads throws with the
 * placeholder named in the message.
 */
const MENU_LOADING_PLACEHOLDER_LABEL = 'Loading options...';

/**
 * How long a page must be watched before an EMPTY control set is
 * believed — see readSettledControls. Sized against the thing it has to
 * outlast: a first navigation to a Grafana app route, where the app's
 * JS chunk is fetched, parsed and mounted before any control exists.
 * A page that mounts a control never waits it out.
 */
const EMPTY_CONTROL_SET_DWELL_MS = 10_000;

/**
 * Timeout on a single control gesture. The gesture runs on a page
 * whose control set has already settled, so this bounds the click
 * itself (Playwright's visible/stable/enabled actionability wait), not
 * a page load.
 */
const CONTROL_CLICK_TIMEOUT_MS = 5_000;

/**
 * Sample `probe` until it returns the same value `SETTLE_STABLE_SAMPLES`
 * times running, then return it. Throws — never returns a moving
 * sample — when the observable is still changing at `timeoutMs`.
 *
 * `ready` is the caller's "this sample counts" test. A sample that
 * fails it resets the streak, so "settled" can never mean "settled on
 * a state that is not yet the one we need". An app that mounts nothing
 * for a second would otherwise settle instantly on its empty
 * pre-mount plateau — three identical readings of nothing.
 */
async function sampleUntilStable<T>(
  page: Page,
  probe: () => Promise<T>,
  signature: (value: T) => string,
  timeoutMs: number,
  describe: string,
  ready: (value: T) => boolean = () => true,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let previous: string | undefined;
  let identical = 0;
  let everReady = false;
  let lastChange = '';
  for (;;) {
    const value = await probe();
    const current = signature(value);
    if (ready(value)) {
      everReady = true;
      if (current === previous) {
        identical++;
      } else {
        if (previous !== undefined) {
          lastChange = describeSignatureChange(previous, current);
        }
        identical = 1;
      }
      previous = current;
      if (identical >= SETTLE_STABLE_SAMPLES) return value;
    } else {
      identical = 0;
      previous = undefined;
    }
    if (Date.now() >= deadline) {
      throw new Error(
        `${describe} never settled after ${timeoutMs}ms ` +
          `(${SETTLE_STABLE_SAMPLES} identical samples ${SETTLE_SAMPLE_INTERVAL_MS}ms apart required, ` +
          `reached ${identical}${everReady ? '' : '; no sample ever met the precondition'}). ` +
          `${lastChange === '' ? '' : `Last change: ${lastChange}. `}` +
          `Last sample: ${current}`,
      );
    }
    await page.waitForTimeout(SETTLE_SAMPLE_INTERVAL_MS);
  }
}

/**
 * Characters of context either side of the first byte at which two
 * successive samples of a settling observable diverge. Wide enough to
 * carry the surrounding JSON keys (so the churning FIELD is named),
 * narrow enough that a failure message stays readable.
 */
const SIGNATURE_DIFF_CONTEXT_CHARS = 120;

/**
 * Name WHAT moved between two successive samples: the offset of the
 * first differing byte plus a window of each side around it.
 *
 * Without this a settle failure reports only the final sample, which
 * says what the page looked like but not which control refused to hold
 * still — and "which one moved" is the entire diagnosis when a surface
 * fails to quiesce.
 */
function describeSignatureChange(before: string, after: string): string {
  let i = 0;
  while (i < before.length && i < after.length && before[i] === after[i]) i++;
  const from = Math.max(0, i - SIGNATURE_DIFF_CONTEXT_CHARS);
  const to = i + SIGNATURE_DIFF_CONTEXT_CHARS;
  return (
    `at char ${i}: …${before.slice(from, to)}… became …${after.slice(from, to)}…`
  );
}

/**
 * The page's control set, read once it has stopped changing — the
 * single entry point for every DOM read of the controls.
 *
 * Both the discovery pass and every drive pass go through it, so both
 * observe the same quiesced DOM and assignUniqueKeys derives the same
 * keys on each. That is what makes a key re-findable across the fresh
 * navigation, rather than a longer timeout on a moving target.
 */
async function readSettledControls(
  page: Page,
  requiredKey?: string,
): Promise<RawControl[]> {
  // "There are no controls here" is the one answer this read can give
  // that silently deletes a surface's entire interaction subtree, so it
  // is the one answer that has to be earned. A page still booting its
  // app bundle presents an empty control set that is perfectly stable
  // for as long as the boot takes — three identical readings of nothing
  // is a measurement of the boot, not of the page. Emptiness therefore
  // only counts once the page has been watched for the dwell below;
  // a page that does mount controls pays nothing for this.
  const emptyBelievableAt = Date.now() + EMPTY_CONTROL_SET_DWELL_MS;
  return await sampleUntilStable(
    page,
    async () => assignUniqueKeys(await discoverStaticControls(page)),
    // Mount-counter tokens — React `useId` and react-select's instance
    // counter alike — are erased from the SIGNATURE only: the returned
    // controls keep the live hints the locators need, but a remount that
    // changes nothing except a counter must not read as the page still
    // moving. The keys already erase both, so leaving them in the
    // signature would compare mount generations the control model itself
    // declares meaningless — and, when a component remounts on every
    // render, would make settling impossible.
    (controls) =>
      JSON.stringify(controls)
        .replace(new RegExp(REACT_ID_TOKEN_PATTERN, 'g'), REACT_ID_PLACEHOLDER)
        .replace(
          new RegExp(REACT_SELECT_INSTANCE_ID_PATTERN, 'g'),
          REACT_SELECT_INSTANCE_ID_REPLACEMENT,
        ),
    CONTROL_SET_SETTLE_TIMEOUT_MS,
    requiredKey === undefined
      ? 'the page control set'
      : `the page control set carrying control ${requiredKey}`,
    requiredKey === undefined
      ? (controls) => controls.length > 0 || Date.now() >= emptyBelievableAt
      : (controls) => controls.some((c) => c.key === requiredKey),
  );
}

/**
 * The option labels of the currently-open dropdown, read once the list
 * has stopped changing.
 *
 * Returns `[]` only when NO option mounts within
 * MENU_MOUNT_TIMEOUT_MS — the free-text-input verdict discoverControls
 * acts on. A list that mounts and then keeps changing throws, and so
 * does one still showing MENU_LOADING_PLACEHOLDER_LABEL at the settle
 * deadline.
 *
 * Labels are trimmed but NOT filtered: the returned array is
 * positionally aligned with `openMenuOptions(page)`, which is what
 * lets clickMenuOption address an option by its own index.
 */
async function readSettledMenuOptions(page: Page): Promise<string[]> {
  const options = openMenuOptions(page);
  const mounted = await options
    .first()
    .waitFor({ state: 'visible', timeout: MENU_MOUNT_TIMEOUT_MS })
    .then(() => true)
    .catch(() => false);
  if (!mounted) return [];
  return await sampleUntilStable(
    page,
    async () => (await options.allTextContents()).map((t) => t.trim()),
    (labels) => JSON.stringify(labels),
    MENU_SETTLE_TIMEOUT_MS,
    'the open dropdown option list',
    // A list still fetching its options is not the list — see
    // MENU_LOADING_PLACEHOLDER_LABEL. Failing the readiness test resets
    // the streak, so the placeholder's own stillness can never be
    // mistaken for the option set having settled.
    (labels) => !labels.includes(MENU_LOADING_PLACEHOLDER_LABEL),
  );
}

/**
 * Click the option whose settled label is EXACTLY `option` in the
 * currently-open dropdown.
 *
 * Exactness is the point on both axes. Substring matching
 * (`filter({ hasText })`) resolves to whichever matching option comes
 * first in the DOM, so two options sharing a prefix pick by render
 * order; and indexing by the DISCOVERY-time position is wrong wherever
 * the discovered option set was filtered (the datasource picker keeps
 * only the cerberus-backed options). Reading the live settled list and
 * addressing the label's own position in it is right on both.
 */
async function clickMenuOption(
  page: Page,
  controlKey: string,
  option: string,
): Promise<void> {
  const labels = await readSettledMenuOptions(page);
  const index = labels.indexOf(option);
  if (index < 0) {
    throw new Error(
      `control ${controlKey}: option ${JSON.stringify(option)} is absent from the settled ` +
        `dropdown [${labels.map((l) => JSON.stringify(l)).join(', ')}] — the option set moved ` +
        `between discovery and driving`,
    );
  }
  await openMenuOptions(page)
    .nth(index)
    .click({ timeout: CONTROL_CLICK_TIMEOUT_MS });
}

/**
 * Dismiss an open dropdown and WAIT for it to go. openMenuOptions is a
 * page-wide locator, so a menu still on screen when the next control
 * is probed contributes its options to that control's option set —
 * a probe-order-dependent, and therefore run-dependent, control model.
 */
async function closeOpenMenu(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await openMenuOptions(page)
    .first()
    .waitFor({ state: 'hidden', timeout: MENU_CLOSE_TIMEOUT_MS });
}

/**
 * CSS locator for a scenes AdHocFiltersVariable combobox — the
 * "+ add filter" builder. This is the out-of-page twin of
 * discoverStaticControls' in-page `isAdhoc` test (a `var-adhoc*` input
 * id, or a `+ label…` / `filter by labels` placeholder); KEEP THE TWO
 * IN LOCKSTEP so the settle below waits for exactly what discovery then
 * classifies as an adhoc-filter control.
 */
export const ADHOC_FILTER_INPUT_SELECTOR =
  'input[id^="var-adhoc"], ' +
  'input[role="combobox"][placeholder^="+ label" i], ' +
  'input[role="combobox"][placeholder*="filter by label" i]';

/**
 * Cap on the positive wait for a drilldown app's adhoc bar to mount.
 * The bar settles within a second or two once the app clears its init
 * race; this bound is only the safety net for a surface that never
 * renders one (callers gate the wait to surfaces that DO).
 */
export const ADHOC_FILTER_SETTLE_TIMEOUT_MS = 15_000;

/**
 * Positive settle for a drilldown app's adhoc "+ add filter" bar.
 *
 * The Traces Drilldown app boots through a primarySignal-init race: it
 * mounts, fires a transient dangling-operand query (`{ && …} | rate()`,
 * which cerberus correctly rejects — reference Tempo rejects the same
 * syntax error), then sets primarySignal and RE-renders with its
 * steady-state chrome, the adhoc filter bar included. A networkidle
 * settle (tolerateRepaintFlicker) can resolve in the gap between those
 * two phases, so a control-discovery pass that runs then intermittently
 * misses the adhoc control — and the surface it mints. The Metrics /
 * Logs Drilldown apps carry no such race; their adhoc bar is already
 * present within the normal settle, which is why only the traces
 * surface needs this.
 *
 * Waiting for the adhoc input to become visible makes the control set
 * deterministic before discovery (and before re-driving an adhoc
 * interaction). Best-effort and bounded: a surface whose settled state
 * has no adhoc bar falls through once the cap elapses, so callers gate
 * this to surfaces that render one.
 */
export async function settleAdhocFilterBar(page: Page): Promise<void> {
  await page
    .locator(ADHOC_FILTER_INPUT_SELECTOR)
    .first()
    .waitFor({ state: 'visible', timeout: ADHOC_FILTER_SETTLE_TIMEOUT_MS })
    .catch(() => {});
}

/**
 * Discover every view-affecting control on the current page.
 * Comboboxes are PROBED — opened, options read, closed via Escape —
 * so the planner knows their cardinality up front; probing performs
 * no selection. Controls whose name matches the exclusion patterns,
 * comboboxes that render no options (free-text inputs), and
 * single-tab strips are dropped here.
 */
export async function discoverControls(
  page: Page,
): Promise<DiscoveredControl[]> {
  const raw = await readSettledControls(page);
  const out: DiscoveredControl[] = [];

  for (const c of raw) {
    if (isExcludedControlName(c.key)) continue;
    if (c.locatorKind === 'combobox') {
      const input = locateCombobox(page, c.controlHint, c.clickHops);
      // The hint was read from the SETTLED DOM a moment ago, and
      // probing a control only opens and Escapes its own menu. A hint
      // that no longer resolves is therefore selector drift, and
      // dropping the control would silently shrink the surface set —
      // exactly the coverage loss the inventory ratchet exists to
      // catch, laundered into the baseline as if it were intended.
      if ((await input.count()) === 0) {
        throw new Error(
          `combobox ${c.key} (${c.controlHint}) vanished between the settled control-set read ` +
            `and its probe — selector drift, not a control the sweep may drop`,
        );
      }
      // The visible current value (captured by the static pass —
      // react-select keeps the input itself empty).
      const currentValue = (c.currentText ?? '').trim();
      // Name the control in any failure: "locator.click: Timeout" on
      // its own says nothing about WHICH of a surface's dozen controls
      // could not be opened, and that identity is the whole diagnosis.
      await input.click({ timeout: CONTROL_CLICK_TIMEOUT_MS }).catch((err) => {
        throw new Error(
          `probing combobox ${c.key} (${c.controlHint}): ${(err as Error).message}`,
        );
      });
      const labels = await readSettledMenuOptions(page);
      // Blur either way, so a focused input can never swallow the next
      // control's gesture; a menu that did mount must be verifiably
      // gone before the next probe reads the page-wide option locator.
      if (labels.length > 0) await closeOpenMenu(page);
      else await page.keyboard.press('Escape');
      let options = labels.filter((t) => t !== '');
      if (options.length === 0) continue; // free-text input, not a select
      // The datasource picker lists EVERY provisioned datasource,
      // including Grafana's built-in `grafanacloud-*` cloud stubs
      // (present in the DS list, backed by no real backend in the
      // crawl stack). Driving the picker to one of those navigates
      // OUT of the cerberus consumption domain and 404s the proxy —
      // not a cerberus defect, just a control option that points
      // away from cerberus. The crawl audits cerberus surfaces only,
      // so keep the picker swept but restrict it to cerberus-backed
      // options. Not a tolerance: a foreign-DS render is genuinely
      // out of scope, the same doctrine as the connections/datasource
      // path exclusion.
      if (/datasource-picker/.test(c.key)) {
        options = options.filter((o) => /cerberus/i.test(o));
        if (options.length === 0) continue;
      }
      out.push({
        kind: c.kind,
        key: c.key,
        options,
        selectedIndex: options.findIndex((o) => o === currentValue),
        // Adhoc-filter key lists are data-derived (label names)
        // whatever their size — force the one-representative path.
        forcedHighCardinality: c.kind === 'adhoc-filter',
        optionHints: options,
        controlHint: c.controlHint,
        clickHops: c.clickHops,
      });
      continue;
    }
    out.push({
      kind: c.kind,
      key: c.key,
      options: c.options,
      selectedIndex: c.selectedIndex,
      forcedHighCardinality: c.kind === 'select-tile',
      optionHints: c.optionHints,
      controlHint: c.controlHint,
      clickHops: c.clickHops,
    });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Driving
// ---------------------------------------------------------------------------

/**
 * Re-find a combobox on the freshly navigated page by its discovery
 * KEY, not its discovery-time element hint: downshift inputs are
 * addressed by React useId-derived ids that need not survive a
 * remount, and the datasource-variable testid embeds the currently
 * selected value. Re-running the static pass re-derives the same
 * deterministic keys (assignUniqueKeys), so the match is exact —
 * absence is selector drift and must fail the crawl loudly.
 */
async function relocateCombobox(
  page: Page,
  settled: ReadonlyArray<RawControl>,
  control: DiscoveredControl,
): Promise<Locator> {
  const match = settled.find(
    (c) => c.locatorKind === 'combobox' && c.key === control.key,
  );
  if (match !== undefined) {
    return locateCombobox(page, match.controlHint, match.clickHops);
  }
  // Re-discovery didn't re-derive the key — but a combobox addressed
  // by a STABLE element hint (its own data-testid, not a render-order
  // id) survives a remount, so fall back to the discovery-time hint
  // when it's testid-based and still present. This covers controls on
  // dynamic surfaces (e.g. native /explore, whose visible control set
  // depends on the booted datasource) without masking a real drift:
  // an id-based hint or an absent locator still throws.
  if (control.controlHint.startsWith('testid=')) {
    const fallback = locateCombobox(
      page,
      control.controlHint,
      control.clickHops,
    );
    if ((await fallback.count()) > 0) return fallback;
  }
  throw new Error(
    `combobox ${control.key} not re-found by key (or stable hint) after fresh navigation. ` +
      `Settled control set: ${settled
        .filter((c) => c.locatorKind === 'combobox')
        .map((c) => c.key)
        .join(', ')}`,
  );
}

/**
 * Perform one planned interaction on the (already navigated, settled)
 * page. Throws when the control or option can't be addressed — a
 * selector drift on a Grafana bump must fail the crawl loudly, never
 * silently shrink the interaction surface.
 */
export async function driveInteraction(
  page: Page,
  planned: PlannedInteraction,
): Promise<void> {
  const { control, option } = planned;
  const idx = control.options.indexOf(option);
  const hint = control.optionHints[idx] ?? option;
  // Every gesture — not just the combobox ones — runs against a
  // control set that has stopped changing AND that carries the control
  // this gesture is for. Clicking mid-render is what exhausted the 5 s
  // actionability budget (Playwright waits for a bounding box stable
  // across two frames, which a scenes app still laying out panels
  // never offers), and it is also what made a control's identity
  // depend on the instant it was read. Requiring the key is what makes
  // "re-findable by key across a fresh navigation" a precondition the
  // driver waits for rather than a coin toss it reports on.
  const settled = await readSettledControls(page, control.key);

  switch (control.kind) {
    case 'tab': {
      // Prefer the stable testid; fall back to role+name for tabs
      // without one. Exact-match on the testid avoids the live-count
      // suffix problem ("Traces200").
      const byTestid = page.locator(
        `[role="tab"][data-testid="${hint}"]`,
      );
      const target =
        (await byTestid.count()) > 0
          ? byTestid.first()
          : page.getByRole('tab', { name: option }).first();
      await target.click({ timeout: CONTROL_CLICK_TIMEOUT_MS });
      return;
    }
    case 'radio': {
      // RadioButtonGroup input ids embed a mount-order counter
      // (`option-<value>-radiogroup-13`), so ids captured at
      // discovery don't survive the fresh navigation, and the input
      // visually overlays its label (pointer interception). Re-find
      // the group by its option-name SIGNATURE and fire a JS click on
      // the target input — that triggers the React onChange and
      // bypasses the overlay. The occurrence index (the `#n` key
      // suffix) disambiguates identical signatures.
      const occurrence = Number(/#(\d+)$/.exec(control.key)?.[1] ?? '0');
      const clicked = await page.evaluate(
        ({ options, optionIndex, occurrenceIndex }) => {
          // KEEP IN LOCKSTEP with discoverStaticControls' optName.
          const optName = (rg: Element, i: HTMLInputElement) => {
            const lbl = i.id
              ? rg.querySelector(`label[for="${CSS.escape(i.id)}"]`)
              : null;
            const text = (lbl?.textContent ?? '').trim();
            if (text !== '') return text;
            const m = /^option-(.+)-radiogroup-\d+$/.exec(i.id);
            if (m) return m[1]!;
            return i.name || i.id;
          };
          let seen = 0;
          for (const rg of document.querySelectorAll('[role="radiogroup"]')) {
            const inputs = [
              ...rg.querySelectorAll('input[type="radio"]'),
            ] as HTMLInputElement[];
            if (inputs.length !== options.length) continue;
            if (inputs.map((i) => optName(rg, i)).join('|') !== options.join('|')) {
              continue;
            }
            if (seen++ < occurrenceIndex) continue;
            inputs[optionIndex]!.click();
            return true;
          }
          return false;
        },
        {
          options: control.options,
          optionIndex: idx,
          occurrenceIndex: occurrence,
        },
      );
      if (!clicked) {
        throw new Error(
          `radio group ${control.key} not re-found by signature after fresh navigation`,
        );
      }
      return;
    }
    case 'option-list': {
      // Click the li's label area (top-left quadrant) — the row also
      // hosts a favorite-toggle button at its right edge.
      await page
        .locator(`ul > li[title="${hint}"]`)
        .first()
        .click({
          timeout: CONTROL_CLICK_TIMEOUT_MS,
          position: { x: 12, y: 12 },
        });
      return;
    }
    case 'select-tile': {
      await page
        .locator(`[data-testid="${hint}"]`)
        .first()
        .click({ timeout: CONTROL_CLICK_TIMEOUT_MS });
      return;
    }
    case 'combobox': {
      const input = await relocateCombobox(page, settled, control);
      await input.click({ timeout: CONTROL_CLICK_TIMEOUT_MS });
      await clickMenuOption(page, control.key, option);
      return;
    }
    case 'adhoc-filter': {
      const input = await relocateCombobox(page, settled, control);
      await input.click({ timeout: CONTROL_CLICK_TIMEOUT_MS });
      // Step 1: the representative KEY. `option` is the deterministic
      // representative planInteractions derived from the discovered
      // key SET, and it is addressed by exact label — "whatever the
      // menu renders first" would apply a different filter, and fire a
      // different query, every time the label list reordered.
      await clickMenuOption(page, control.key, option);
      // Step 2: the value dropdown auto-opens (scenes adhoc flow).
      // Some builders interpose an operator step that defaults to '=';
      // both shapes are a menu whose settled labels pick their own
      // deterministic representative. A builder that opens no second
      // menu leaves the key-only filter, which is a complete state.
      const valueOptions = await readSettledMenuOptions(page);
      if (valueOptions.length > 0) {
        await clickMenuOption(
          page,
          control.key,
          representativeOption(valueOptions),
        );
      }
      return;
    }
  }
}
