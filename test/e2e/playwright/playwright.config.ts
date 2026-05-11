import { defineConfig, devices } from '@playwright/test';

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
 */
const grafanaURL = process.env.GRAFANA_URL ?? 'http://localhost:3000';

export default defineConfig({
  testDir: '.',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
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
