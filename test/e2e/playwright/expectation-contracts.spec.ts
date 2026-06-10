/**
 * cerberus.expect contract meta-rules + unit tests — stack-free.
 *
 * Two halves:
 *
 *   1. Unit tests for helpers/expectations.ts: readPanelExpectation
 *      parsing (default / declared / malformed) and the BIDIRECTIONAL
 *      enforceExpectation semantics (a declared 'empty' that returns
 *      series fails; a declared 'error:<s>' that succeeds or errors
 *      differently fails; the default 'nonempty' with zero series
 *      fails — the pre-existing sweep rule).
 *
 *   2. Meta-rules over BOTH provisioned dashboard directories
 *      (test/e2e/grafana/compose/dashboards + test/e2e/grafana/
 *      dashboards), read from disk at spec-build time:
 *
 *        - ops-family dashboards — every file NOT prefixed
 *          `showcase-` (today: cerberus, clickhouse, host, otelcol)
 *          — must not carry ANY cerberus.expect declaration.
 *          Non-default expectations are a showcase-family privilege.
 *        - every non-default declaration anywhere must carry a
 *          non-empty `why`.
 *        - every declaration that exists must parse — a malformed
 *          contract fails here, before any live sweep consumes it.
 *
 *      Today zero declarations exist, so the meta-rules pass on the
 *      current tree; they are the ratchet for the P2+ showcase
 *      dashboards.
 *
 * Run via:
 *   cd test/e2e/playwright && npx playwright test expectation-contracts.spec.ts
 */

import { readFileSync, readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { expect, test } from '@playwright/test';

import {
  enforceExpectation,
  isNonDefaultExpectation,
  readPanelExpectation,
} from './helpers/index.js';

// --- Unit tests: readPanelExpectation ---------------------------------------

test('readPanelExpectation defaults to undeclared nonempty when absent', () => {
  expect(readPanelExpectation({ title: 'p' })).toEqual({
    expect: 'nonempty',
    declared: false,
  });
  expect(readPanelExpectation(undefined)).toEqual({
    expect: 'nonempty',
    declared: false,
  });
  expect(readPanelExpectation(null)).toEqual({
    expect: 'nonempty',
    declared: false,
  });
});

test('readPanelExpectation parses each declared kind', () => {
  expect(
    readPanelExpectation({ cerberus: { expect: 'nonempty' } }),
  ).toEqual({ expect: 'nonempty', declared: true });
  expect(
    readPanelExpectation({
      cerberus: { expect: 'empty', why: 'quantile over absent buckets' },
    }),
  ).toEqual({
    expect: 'empty',
    declared: true,
    why: 'quantile over absent buckets',
  });
  expect(
    readPanelExpectation({
      cerberus: { expect: 'error:parse error', why: 'showcases the 400 path' },
    }),
  ).toEqual({
    expect: 'error:parse error',
    declared: true,
    why: 'showcases the 400 path',
  });
});

test('readPanelExpectation throws on malformed declarations', () => {
  expect(() => readPanelExpectation({ cerberus: 'nonempty' })).toThrow(
    /must be an object/,
  );
  expect(() => readPanelExpectation({ cerberus: {} })).toThrow(
    /expect must be a string/,
  );
  expect(() =>
    readPanelExpectation({ cerberus: { expect: 'sometimes' } }),
  ).toThrow(/'nonempty' \| 'empty' \| 'error:<substring>'/);
  expect(() =>
    readPanelExpectation({ cerberus: { expect: 'error:' } }),
  ).toThrow(/'nonempty' \| 'empty' \| 'error:<substring>'/);
  expect(() =>
    readPanelExpectation({ cerberus: { expect: 'empty', why: 42 } }),
  ).toThrow(/why must be a string/);
});

test('isNonDefaultExpectation flags only declared non-nonempty contracts', () => {
  expect(
    isNonDefaultExpectation({ expect: 'nonempty', declared: false }),
  ).toBe(false);
  expect(
    isNonDefaultExpectation({ expect: 'nonempty', declared: true }),
  ).toBe(false);
  expect(isNonDefaultExpectation({ expect: 'empty', declared: true })).toBe(
    true,
  );
  expect(
    isNonDefaultExpectation({ expect: 'error:boom', declared: true }),
  ).toBe(true);
});

// --- Unit tests: enforceExpectation (bidirectional) --------------------------

test('enforce nonempty: series present passes, zero series violates', () => {
  const e = readPanelExpectation({});
  expect(enforceExpectation(e, { seriesCount: 3, status: 200 })).toEqual([]);
  const v = enforceExpectation(e, { seriesCount: 0, status: 200 });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('returned no series');
});

test('enforce nonempty: non-2xx status violates', () => {
  const e = readPanelExpectation({});
  const v = enforceExpectation(e, {
    seriesCount: 0,
    status: 500,
    errorBody: 'upstream exploded',
  });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('HTTP 500');
  expect(v[0]).toContain('upstream exploded');
});

test('enforce empty: zero series passes, any series violates (bidirectional pin)', () => {
  const e = readPanelExpectation({
    cerberus: { expect: 'empty', why: 'defined-empty result' },
  });
  expect(enforceExpectation(e, { seriesCount: 0, status: 200 })).toEqual([]);
  const v = enforceExpectation(e, { seriesCount: 2, status: 200 });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain(
    'declared-empty panel returned 2 series (the feature it showcases broke)',
  );
});

test('enforce empty: non-2xx still violates (empty != error)', () => {
  const e = readPanelExpectation({
    cerberus: { expect: 'empty', why: 'defined-empty result' },
  });
  const v = enforceExpectation(e, { seriesCount: 0, status: 502 });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('HTTP 502');
});

test('enforce error: matching error passes', () => {
  const e = readPanelExpectation({
    cerberus: { expect: 'error:parse error', why: 'showcases the 400 path' },
  });
  expect(
    enforceExpectation(e, {
      seriesCount: 0,
      status: 400,
      errorBody: '{"status":"error","error":"parse error at char 3"}',
    }),
  ).toEqual([]);
});

test('enforce error: a 2xx success violates (the showcased error stopped firing)', () => {
  const e = readPanelExpectation({
    cerberus: { expect: 'error:parse error', why: 'showcases the 400 path' },
  });
  const v = enforceExpectation(e, { seriesCount: 1, status: 200 });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('but the probe succeeded with HTTP 200');
});

test('enforce error: an error without the declared substring violates', () => {
  const e = readPanelExpectation({
    cerberus: { expect: 'error:parse error', why: 'showcases the 400 path' },
  });
  const v = enforceExpectation(e, {
    seriesCount: 0,
    status: 500,
    errorBody: 'something else broke',
  });
  expect(v).toHaveLength(1);
  expect(v[0]).toContain('does not contain the declared substring');
  expect(v[0]).toContain('something else broke');
});

// --- Meta-rules over the provisioned dashboard directories -------------------

const DASHBOARD_DIRS = [
  resolve(__dirname, '..', 'grafana', 'compose', 'dashboards'),
  resolve(__dirname, '..', 'grafana', 'dashboards'),
];

type RawPanel = {
  type?: string;
  title?: string;
  cerberus?: unknown;
  panels?: RawPanel[];
};

type Declaration = {
  file: string;
  panelTitle: string;
  expectation: ReturnType<typeof readPanelExpectation>;
};

function walkPanels(
  panels: RawPanel[],
  visit: (p: RawPanel) => void,
): void {
  for (const p of panels) {
    visit(p);
    if (p.panels !== undefined) walkPanels(p.panels, visit);
  }
}

function scanDashboards(): {
  files: string[];
  declarations: Declaration[];
} {
  const files: string[] = [];
  const declarations: Declaration[] = [];
  for (const dir of DASHBOARD_DIRS) {
    for (const entry of readdirSync(dir)) {
      if (!entry.endsWith('.json')) continue;
      const rel = `${dir}/${entry}`;
      files.push(rel);
      const json = JSON.parse(readFileSync(join(dir, entry), 'utf8')) as {
        panels?: RawPanel[];
      };
      walkPanels(json.panels ?? [], (p) => {
        // readPanelExpectation throws on malformed declarations —
        // a broken contract fails this meta-spec before any live
        // sweep consumes it.
        const expectation = readPanelExpectation(p);
        if (expectation.declared) {
          declarations.push({
            file: rel,
            panelTitle: p.title ?? '<untitled>',
            expectation,
          });
        }
      });
    }
  }
  return { files, declarations };
}

test('dashboard catalog: both provisioning dirs are non-empty and parseable', () => {
  const { files } = scanDashboards();
  for (const dir of DASHBOARD_DIRS) {
    expect(
      files.filter((f) => f.startsWith(dir)).length,
      `at least one dashboard JSON under ${dir}`,
    ).toBeGreaterThan(0);
  }
});

test('ops-family dashboards carry no cerberus.expect declarations', () => {
  // Every dashboard file not prefixed `showcase-` is ops-family —
  // its panels graph the live stack, so a non-default expectation
  // would mask a real regression. Declarations (even an explicit
  // default 'nonempty') are a showcase-family privilege.
  const { declarations } = scanDashboards();
  const opsDeclarations = declarations.filter(
    (d) => !d.file.split('/').pop()!.startsWith('showcase-'),
  );
  expect(
    opsDeclarations.map((d) => `${d.file} panel "${d.panelTitle}"`),
    'ops-family dashboards must not declare cerberus.expect',
  ).toEqual([]);
});

test('every non-default cerberus.expect declaration carries a non-empty why', () => {
  const { declarations } = scanDashboards();
  const missingWhy = declarations.filter(
    (d) =>
      isNonDefaultExpectation(d.expectation) &&
      (d.expectation.why === undefined || d.expectation.why.trim() === ''),
  );
  expect(
    missingWhy.map(
      (d) =>
        `${d.file} panel "${d.panelTitle}" expect=${d.expectation.expect}`,
    ),
    'non-default declarations must explain themselves via why',
  ).toEqual([]);
});
