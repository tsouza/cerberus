/**
 * DOM-level helpers.
 *
 * The phase specs care about three things browser-side:
 *   1. Did the browser log any console errors? (catches React render
 *      errors, network failures the Playwright sweep can miss.)
 *   2. Did Grafana surface any `role="alert"` banner with a known
 *      regression substring?
 *   3. Did a kiosk repaint race the network-idle wait?
 *
 * The first two return a string[] of captured messages; the third is
 * a `void` waiter that absorbs repaint flicker without polluting the
 * spec's main timeline.
 *
 * Tolerated console-message families: NONE. We do not carry an
 * allow-list (Q5 in /home/thiago/.claude/plans/e2e-enhance.md §9 —
 * every error surfaced is a failure). If a third-party plugin emits
 * noise, the spec should filter that surface OUT of the iteration,
 * not extend an allow-list here.
 */

import type { ConsoleMessage, Page } from '@playwright/test';

/**
 * Wire a console listener that returns every `error`-level message
 * captured while the listener is attached.
 *
 * Returns a tear-down function — call it before reading the captured
 * messages array, the way the existing trace-click flow does with
 * `page.off('response', onResponse)`.
 *
 * Usage:
 *   const { messages, stop } = await captureConsoleErrors(page);
 *   // … drive the page …
 *   stop();
 *   expect(messages).toEqual([]);
 *
 * The return shape is unusual (object with two fields) on purpose:
 * a bare `Promise<string[]>` would force callers to await first +
 * miss anything fired before they wired the listener. The "start +
 * stop" handle pattern mirrors page.on/off everywhere else.
 */
export async function captureConsoleErrors(
  page: Page,
): Promise<{ messages: string[]; stop: () => void }> {
  const messages: string[] = [];
  const listener = (msg: ConsoleMessage) => {
    if (msg.type() === 'error') {
      messages.push(msg.text());
    }
  };
  page.on('console', listener);
  return {
    messages,
    stop: () => page.off('console', listener),
  };
}

/**
 * Read every `role="alert"` banner currently rendered on the page and
 * return their visible text content.
 *
 * Grafana's red error-state banner + the trace-view "Query error"
 * banner both expose themselves as `role="alert"`. The N4 regression
 * (illegal wireType) and the N6 regression (fabricated value tooltip)
 * both surface a banner whose text mentions the failure verbatim, so
 * substring filtering against the returned array is the canonical
 * post-flight check.
 *
 * The function is read-only; it does NOT clear the alerts, so a
 * second call returns the same array. If the spec wants per-step
 * deltas, it must snapshot before/after itself.
 */
export async function captureRoleAlertBanners(page: Page): Promise<string[]> {
  return await page
    .locator('[role="alert"]')
    .evaluateAll((nodes) =>
      nodes
        .map((n) => (n.textContent ?? '').trim())
        .filter((s) => s.length > 0),
    );
}

/**
 * Options for `tolerateRepaintFlicker`.
 *
 * The kiosk pass (`?viewPanel=N`) re-mounts the panel into a
 * full-screen wrapper. Grafana 11.x repaints the panel chrome
 * between the old container detaching and the new one settling,
 * which races a naïve `networkidle` wait — the wait resolves during
 * the gap and the spec immediately probes a half-rendered DOM.
 *
 * Options:
 *   - `settleMs`: how long to wait once `networkidle` resolves, to
 *     let the repaint commit. Default 500ms.
 *   - `timeoutMs`: hard cap for the whole wait. Default 45_000.
 */
export type TolerateRepaintFlickerOpts = {
  settleMs?: number;
  timeoutMs?: number;
};

/**
 * Wait for the page to be done repainting after a kiosk-view or
 * back-nav transition.
 *
 * The flicker handler:
 *   1. waits for `networkidle` (capped by `timeoutMs`),
 *   2. then waits an additional `settleMs` real ms,
 *   3. then waits for `networkidle` *again* (also capped),
 * so a repaint that fired mid-step (1) gets a chance to settle.
 *
 * No assertions — this is a synchronization helper. The spec should
 * call it once between a navigation and its DOM probes; never twice
 * back-to-back (the second call is a no-op and adds latency).
 */
export async function tolerateRepaintFlicker(
  page: Page,
  opts: TolerateRepaintFlickerOpts = {},
): Promise<void> {
  const settleMs = opts.settleMs ?? 500;
  const timeoutMs = opts.timeoutMs ?? 45_000;

  await page
    .waitForLoadState('networkidle', { timeout: timeoutMs })
    .catch(() => {});
  await new Promise((r) => setTimeout(r, settleMs));
  await page
    .waitForLoadState('networkidle', { timeout: timeoutMs })
    .catch(() => {});
}
