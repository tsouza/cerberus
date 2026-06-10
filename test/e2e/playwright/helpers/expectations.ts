/**
 * cerberus.expect panel contracts.
 *
 * A dashboard panel may declare its expected sweep outcome via a
 * custom `cerberus` field in its JSON:
 *
 *   { "cerberus": { "expect": "nonempty", "why": "…" } }
 *   { "cerberus": { "expect": "empty", "why": "…" } }
 *   { "cerberus": { "expect": "error:<substring>", "why": "…" } }
 *
 * Absent declaration = the default contract: the panel must return
 * data ('nonempty'). Declarations are BIDIRECTIONAL pins, not
 * tolerances:
 *
 *   - a panel declared 'empty' that returns series is a violation —
 *     the feature it showcases (a query shape whose defined result
 *     is empty) broke;
 *   - a panel declared 'error:<s>' that succeeds, or that errors
 *     with a body not containing <s>, is a violation;
 *   - the default 'nonempty' panel returning zero series is a
 *     violation (the pre-existing sweep behaviour).
 *
 * Family policy (enforced stack-free by
 * expectation-contracts.spec.ts): ops-family dashboards (any file
 * not prefixed `showcase-`) must not carry ANY cerberus.expect
 * declaration — non-default expectations are a showcase-family
 * privilege — and every non-default declaration anywhere must carry
 * a non-empty `why`.
 */

/** The declared outcome contract for a panel. */
export type ExpectKind = 'nonempty' | 'empty' | `error:${string}`;

export type PanelExpectation = {
  expect: ExpectKind;
  /** true iff the dashboard JSON carries an explicit declaration. */
  declared: boolean;
  why?: string;
};

/** What the sweep observed when it probed the panel's target. */
export type ObservedOutcome = {
  /** Series / streams / traces count; <= 0 means no data. */
  seriesCount: number;
  /** HTTP status of the probe response. */
  status: number;
  /** Raw response body, used for error-substring matching. */
  errorBody?: string;
};

/**
 * Parse `panel.cerberus` into a `PanelExpectation`. An absent
 * declaration yields the default `{ expect: 'nonempty', declared:
 * false }`. A malformed declaration THROWS — a contract that cannot
 * be parsed must fail the sweep loudly, never degrade to a default.
 */
export function readPanelExpectation(panelJson: unknown): PanelExpectation {
  if (panelJson === null || typeof panelJson !== 'object') {
    return { expect: 'nonempty', declared: false };
  }
  const cerberus = (panelJson as Record<string, unknown>).cerberus;
  if (cerberus === undefined) {
    return { expect: 'nonempty', declared: false };
  }
  if (cerberus === null || typeof cerberus !== 'object') {
    throw new Error(
      `readPanelExpectation: panel.cerberus must be an object, got ${JSON.stringify(cerberus)}`,
    );
  }
  const c = cerberus as Record<string, unknown>;
  const expect = c.expect;
  if (typeof expect !== 'string') {
    throw new Error(
      `readPanelExpectation: panel.cerberus.expect must be a string, got ${JSON.stringify(expect)}`,
    );
  }
  const valid =
    expect === 'nonempty' ||
    expect === 'empty' ||
    (expect.startsWith('error:') && expect.length > 'error:'.length);
  if (!valid) {
    throw new Error(
      `readPanelExpectation: panel.cerberus.expect must be ` +
        `'nonempty' | 'empty' | 'error:<substring>', got ${JSON.stringify(expect)}`,
    );
  }
  if (c.why !== undefined && typeof c.why !== 'string') {
    throw new Error(
      `readPanelExpectation: panel.cerberus.why must be a string, got ${JSON.stringify(c.why)}`,
    );
  }
  return {
    expect: expect as ExpectKind,
    declared: true,
    ...(c.why !== undefined ? { why: c.why } : {}),
  };
}

/** True iff the expectation deviates from the default contract. */
export function isNonDefaultExpectation(e: PanelExpectation): boolean {
  return e.declared && e.expect !== 'nonempty';
}

/**
 * Enforce a panel expectation against the observed probe outcome.
 * Returns a string[] of violations (empty = the contract holds), so
 * sweeps aggregate across panels.
 *
 * The check is bidirectional in every branch — a declared
 * expectation that is no longer met is just as much a failure as
 * the default contract breaking.
 */
export function enforceExpectation(
  expectation: PanelExpectation,
  observed: ObservedOutcome,
): string[] {
  const out: string[] = [];
  const ok2xx = observed.status >= 200 && observed.status <= 299;

  if (expectation.expect === 'nonempty' || expectation.expect === 'empty') {
    if (!ok2xx) {
      out.push(
        `expected a successful response but got HTTP ${observed.status}` +
          (observed.errorBody !== undefined && observed.errorBody !== ''
            ? `: ${truncate(observed.errorBody, 300)}`
            : ''),
      );
      return out;
    }
    if (expectation.expect === 'empty' && observed.seriesCount > 0) {
      out.push(
        `declared-empty panel returned ${observed.seriesCount} series ` +
          `(the feature it showcases broke)`,
      );
    }
    if (expectation.expect === 'nonempty' && observed.seriesCount <= 0) {
      out.push(
        `panel returned no series (count=${observed.seriesCount}); ` +
          `the default contract is nonempty — fix the bug at the source ` +
          `(cerberus code, seed, dashboard, or panel expression)`,
      );
    }
    return out;
  }

  // 'error:<substring>' — the panel showcases a defined error.
  const want = expectation.expect.slice('error:'.length);
  if (ok2xx) {
    out.push(
      `declared error:${JSON.stringify(want)} but the probe succeeded with ` +
        `HTTP ${observed.status} (${observed.seriesCount} series) — the ` +
        `error the panel showcases no longer fires`,
    );
    return out;
  }
  const body = observed.errorBody ?? '';
  if (!body.includes(want)) {
    out.push(
      `declared error:${JSON.stringify(want)} and got HTTP ` +
        `${observed.status}, but the error body does not contain the ` +
        `declared substring: ${truncate(body, 300)}`,
    );
  }
  return out;
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n)}…<truncated, ${s.length} chars>`;
}
