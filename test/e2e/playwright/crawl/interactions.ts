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
 */
export const STRUCTURAL_MAX_OPTIONS = 12;

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
    const structural =
      !control.forcedHighCardinality &&
      control.options.length <= STRUCTURAL_MAX_OPTIONS;
    let first = true;
    for (let i = 0; i < control.options.length; i++) {
      if (i === control.selectedIndex) continue;
      const option = control.options[i]!;
      plan.push({
        control,
        option,
        stateValue: structural ? option : '{rep}',
        leanRepresentative: first,
      });
      first = false;
      // High-cardinality → exactly one representative option.
      if (!structural) break;
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
    ({ chromeSelector }) => {
      const out: Array<{
        kind: string;
        key: string;
        options: string[];
        selectedIndex: number;
        locatorKind: string;
        optionHints: string[];
        controlHint: string;
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
        // Two render-order / selection-dependent token classes must
        // never reach an inventory key:
        //   - React useId tokens (`:rNN:` / React 19's `«rNN»`) —
        //     downshift inputs fall back to them for element ids;
        //   - count-bearing placeholders (`1000 logs`, `500 logs`) —
        //     the line-limit picker's placeholder mirrors the current
        //     selection, so it flickers per surface.
        // Normalize both to fixed tokens; assignUniqueKeys' DOM-order
        // `#n` suffix keeps same-page twins distinct.
        const key = rawKey
          .replace(/(:r[0-9a-z]+:|«r[0-9a-z]+»)/g, '{rid}')
          .replace(/^\d+ (logs|lines|rows)$/, '{n} $1');
        if (key === '') continue;
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
          currentText,
        });
      }

      return out;
    },
    { chromeSelector: CHROME_ANCESTOR_SELECTOR },
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

/** Locate a combobox input from its discovery hint. */
function locateCombobox(page: Page, hint: string): Locator {
  if (hint.startsWith('testid=')) {
    return page
      .locator(`input[data-testid="${hint.slice('testid='.length)}"]`)
      .first();
  }
  return page.locator(`input[id="${hint.slice('id='.length)}"]`).first();
}

/** The options currently rendered in an open select/combobox menu. */
function openMenuOptions(page: Page): Locator {
  return page.locator(
    '[data-testid="data-testid Select option"], [role="listbox"] [role="option"]',
  );
}

const PROBE_OPEN_MS = 900;
const PROBE_CLOSE_MS = 300;

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
  const raw = assignUniqueKeys(await discoverStaticControls(page));
  const out: DiscoveredControl[] = [];

  for (const c of raw) {
    if (isExcludedControlName(c.key)) continue;
    if (c.locatorKind === 'combobox') {
      const input = locateCombobox(page, c.controlHint);
      if ((await input.count()) === 0) continue;
      try {
        // The visible current value (captured by the static pass —
        // react-select keeps the input itself empty).
        const currentValue = (c.currentText ?? '').trim();
        await input.click({ timeout: 3_000 });
        await page.waitForTimeout(PROBE_OPEN_MS);
        let options = (await openMenuOptions(page).allTextContents())
          .map((t) => t.trim())
          .filter((t) => t !== '');
        await page.keyboard.press('Escape');
        await page.waitForTimeout(PROBE_CLOSE_MS);
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
        });
      } catch {
        // A combobox that can't open (covered by an overlay, detached
        // mid-probe) contributes no states; the surface's other
        // controls still sweep. Not a tolerance: if the control
        // matters it encodes elsewhere (URL param / radio twin) or a
        // future selector fix re-discovers it.
        await page.keyboard.press('Escape').catch(() => {});
      }
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
  control: DiscoveredControl,
): Promise<Locator> {
  const raw = assignUniqueKeys(await discoverStaticControls(page));
  const match = raw.find(
    (c) => c.locatorKind === 'combobox' && c.key === control.key,
  );
  if (match !== undefined) {
    return locateCombobox(page, match.controlHint);
  }
  // Re-discovery didn't re-derive the key — but a combobox addressed
  // by a STABLE element hint (its own data-testid, not a render-order
  // id) survives a remount, so fall back to the discovery-time hint
  // when it's testid-based and still present. This covers controls on
  // dynamic surfaces (e.g. native /explore, whose visible control set
  // depends on the booted datasource) without masking a real drift:
  // an id-based hint or an absent locator still throws.
  if (control.controlHint.startsWith('testid=')) {
    const fallback = locateCombobox(page, control.controlHint);
    if ((await fallback.count()) > 0) return fallback;
  }
  throw new Error(
    `combobox ${control.key} not re-found by key (or stable hint) after fresh navigation`,
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
      await target.click({ timeout: 5_000 });
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
        .click({ timeout: 5_000, position: { x: 12, y: 12 } });
      return;
    }
    case 'select-tile': {
      await page
        .locator(`[data-testid="${hint}"]`)
        .first()
        .click({ timeout: 5_000 });
      return;
    }
    case 'combobox': {
      const input = await relocateCombobox(page, control);
      await input.click({ timeout: 5_000 });
      await page.waitForTimeout(PROBE_OPEN_MS);
      await openMenuOptions(page)
        .filter({ hasText: option })
        .first()
        .click({ timeout: 5_000 });
      return;
    }
    case 'adhoc-filter': {
      const input = await relocateCombobox(page, control);
      await input.click({ timeout: 5_000 });
      await page.waitForTimeout(PROBE_OPEN_MS);
      // Step 1: pick the representative KEY (first option).
      await openMenuOptions(page).first().click({ timeout: 5_000 });
      await page.waitForTimeout(PROBE_OPEN_MS);
      // Step 2: the value dropdown auto-opens (scenes adhoc flow);
      // pick the first value. Some builders interpose an operator
      // step that defaults to '=' — the first option click covers
      // both shapes.
      const valueOptions = openMenuOptions(page);
      if ((await valueOptions.count()) > 0) {
        await valueOptions.first().click({ timeout: 5_000 });
      }
      return;
    }
  }
}
