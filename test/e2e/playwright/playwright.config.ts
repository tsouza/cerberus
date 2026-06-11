import { defineConfig, devices } from '@playwright/test';

// Safe at config-load time: stacks.ts and everything it pulls in
// (crawl/lib.ts, helpers/drilldown.ts) only TYPE-imports
// @playwright/test, so requiring the registry here executes no
// test-runner code.
import { stackByName } from './crawl/stacks.js';

/**
 * Playwright config for the Grafana smoke suite.
 *
 * Run via `just e2e-playwright` (which assumes `just e2e-up` has already
 * brought up the k3d stack on localhost:3000).
 *
 * Env:
 *   GRAFANA_URL   default http://localhost:3000  (Grafana base URL)
 *   CERBERUS_URL  default http://localhost:8080  (cerberus HTTP, for
 *                                                  direct API-shape tests)
 *   CRAWL_STACK   crawl-suite stack selector (see crawl/stacks.ts):
 *                 unset → crawl/** is ignored (0 crawl tests);
 *                 a registered name → crawl/** runs against that
 *                 stack's config; an unknown name → loud config error
 *                 right here, never a silent skip.
 */
const grafanaURL = process.env.GRAFANA_URL ?? 'http://localhost:3000';

// The crawl suite (crawl/**) asserts a PER-STACK surface inventory —
// running it without naming a stack would diff an arbitrary live
// surface set against an arbitrary pin and fail on coverage, not on
// bugs. CRAWL_STACK is therefore the opt-in: the compose-smoke job
// sets `compose`, the k3d dashboard job's crawl step sets `k3d`, and
// a lane that doesn't select a stack (e.g. the dashboard job's
// auto-discovery run) keeps its existing coverage with crawl/**
// ignored. This is lane targeting (which stack a suite asserts
// against), not failure masking — the crawl rules run unchanged
// wherever the suite runs. A NON-EMPTY unknown value fails the whole
// config load via stackByName's throw: a typo'd stack name must
// never silently become "0 crawl tests".
const crawlStack = process.env.CRAWL_STACK ?? '';
if (crawlStack !== '') stackByName(crawlStack);

export default defineConfig({
  testDir: '.',
  testIgnore: crawlStack !== '' ? [] : ['crawl/**'],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  // A pass-on-retry is a masked intermittent bug; the suite must
  // surface it red in CI (run 27284868985 went green over a real
  // non-determinism bug — the trace-by-id batch-order flake — exactly
  // this way). Locally (no CI env) retries still help iteration.
  failOnFlakyTests: !!process.env.CI,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',

  use: {
    baseURL: grafanaURL,
    trace: 'on-first-retry',
    video: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
