/**
 * Data-quality lints — API-level, deterministic.
 *
 * Each lint pins a bug CLASS the 2026-06-09 AI sweep found on the
 * live stack as a deterministic check. The doctrine: the AI sweep's
 * job is to DISCOVER new oracle classes off-CI; once a find is named,
 * its deterministic version lands here and CI carries it forever.
 *
 * Lint 1 — histogram degeneracy. Incident: cerberus's own
 * query-duration histogram shipped with default millisecond-scaled
 * bucket boundaries while observations were recorded in seconds, so
 * EVERY observation landed in a single bucket and
 * histogram_quantile() fabricated a constant 4.75s p95 (the
 * interpolation midpoint of the one populated bucket) regardless of
 * real latency. Deterministic signal: for every histogram family a
 * provisioned quantile panel consumes, once the family has a
 * meaningful observation count, the observations must span more than
 * one bucket. The lint is scoped to quantile-CONSUMED families on
 * purpose: a histogram nobody quantiles can be legitimately
 * single-bucket (e.g. http_server_request_body_size on a stack whose
 * seed traffic is all GET requests — every body is 0 bytes), so an
 * all-families rule would either false-positive or grow an exclusion
 * list; scoping to the families that feed histogram_quantile panels
 * keeps the rule honest and exclusion-free.
 *
 * Lint 2 — identical-series suspicion. Same incident, second
 * signature: with all observations in one bucket, the p50 and p95
 * targets of the same panel family returned BITWISE-IDENTICAL series
 * (interpolation collapses to the same midpoint for every quantile).
 * Deterministic signal: a panel with ≥2 histogram_quantile targets
 * (distinct quantile parameters) whose result sets are bitwise
 * identical over the probe window fails.
 *
 * NOT duplicated here: catalog↔query consistency (every advertised
 * __name__ is queryable) and metadata↔storage type consistency are
 * already pinned at the Go layer by the tests landed with PRs #765
 * and #768 — this file only carries the lint classes that need live
 * Grafana-shaped traffic.
 *
 * Adding a lint when a new bug class is found: name the deterministic
 * signal in a comment (cite the incident), implement it against the
 * datasource-proxy API, and aggregate violations into the failures[]
 * array — never per-lint tolerance lists; if a lint can't be made
 * deterministic without one, its scope is wrong (see lint 1's
 * dashboard-scoping for the pattern).
 *
 * Env:
 *   GRAFANA_URL / GRAFANA_BASE_URL   default http://localhost:3000
 *   CERBERUS_URL                     default http://localhost:8080
 */

import {
  expect,
  test,
  type APIRequestContext,
} from '@playwright/test';

import {
  awaitSelfTelemetryRangeSignal,
  generateSelfTraffic,
  iterateDashboards,
  type Dashboard,
} from '../helpers/index.js';
import { truncate } from './lib.js';

const SEED_TRAFFIC_SECONDS = 30;

// Minimum total observation count before lint 1 judges a family.
// Below the floor the bucket spread carries no signal (a family with
// 3 observations can legitimately sit in one bucket); the seed
// traffic drives every quantile-consumed family well past it.
const HISTOGRAM_COUNT_FLOOR = 10;

const QUERY_WINDOW_SECONDS = 5 * 60;
const QUERY_STEP_SECONDS = 15;

function baseURL(): string {
  return (
    process.env.GRAFANA_URL ??
    process.env.GRAFANA_BASE_URL ??
    'http://localhost:3000'
  );
}

type PromVector = Array<{
  metric: Record<string, string>;
  value: [number, string];
}>;

type PromMatrix = Array<{
  metric: Record<string, string>;
  values: Array<[number, string]>;
}>;

async function promInstant(
  request: APIRequestContext,
  dsUid: string,
  query: string,
): Promise<PromVector> {
  const url =
    `${baseURL()}/api/datasources/proxy/uid/${dsUid}/api/v1/query` +
    `?query=${encodeURIComponent(query)}`;
  const resp = await request.get(url);
  expect(resp.status(), `GET ${url}`).toBe(200);
  const body = (await resp.json()) as {
    status?: string;
    data?: { result?: PromVector };
  };
  expect(body.status, `prom envelope status for ${query}`).toBe('success');
  return body.data?.result ?? [];
}

async function promRange(
  request: APIRequestContext,
  dsUid: string,
  query: string,
  nowSec: number,
): Promise<PromMatrix> {
  const url =
    `${baseURL()}/api/datasources/proxy/uid/${dsUid}/api/v1/query_range` +
    `?query=${encodeURIComponent(query)}` +
    `&start=${nowSec - QUERY_WINDOW_SECONDS}&end=${nowSec}&step=${QUERY_STEP_SECONDS}`;
  const resp = await request.get(url);
  expect(resp.status(), `GET ${url}`).toBe(200);
  const body = (await resp.json()) as {
    status?: string;
    data?: { result?: PromMatrix };
  };
  expect(body.status, `prom envelope status for ${query}`).toBe('success');
  return body.data?.result ?? [];
}

/**
 * Histogram families consumed by histogram_quantile(...) panel
 * targets, with the prometheus datasource uid each panel queries
 * through, keyed `<dsUid>|<family>`. Extraction matches the classic
 * `<family>_bucket` token inside a histogram_quantile call — native
 * (bucket-less) histogram quantiles have no classic bucket family to
 * lint and are exercised by their own showcase fixtures.
 */
function quantileConsumedFamilies(
  dashboards: Dashboard[],
): Map<string, { dsUid: string; family: string; where: string }> {
  const out = new Map<string, { dsUid: string; family: string; where: string }>();
  for (const d of dashboards) {
    for (const p of d.panels) {
      for (const t of p.targets) {
        const expr = (t.expr ?? t.query ?? '').trim();
        if (!expr.includes('histogram_quantile')) continue;
        const dsUid = t.datasource?.uid ?? p.datasource?.uid ?? '';
        if (dsUid === '') continue;
        for (const m of expr.matchAll(
          /([a-zA-Z_:][a-zA-Z0-9_:]*)_bucket/g,
        )) {
          const family = m[1] ?? '';
          if (family === '') continue;
          out.set(`${dsUid}|${family}`, {
            dsUid,
            family,
            where: `${d.uid} :: ${p.title}`,
          });
        }
      }
    }
  }
  return out;
}

test('lints: histogram families behind quantile panels are non-degenerate + multi-quantile panels differ', async ({
  request,
}, testInfo) => {
  testInfo.setTimeout(5 * 60_000);

  await generateSelfTraffic(request, SEED_TRAFFIC_SECONDS);
  // Both lints judge rate()-over-range shapes; on a freshly-booted
  // stack the telemetry export cadence can lag the seed traffic (see
  // awaitSelfTelemetryRangeSignal). The bounded wait keeps the
  // judged-input assertions below honest without a boot race.
  await awaitSelfTelemetryRangeSignal(request);

  const dashboards = await iterateDashboards(request, baseURL());
  const failures: string[] = [];
  const nowSec = Math.floor(Date.now() / 1000);

  // -------------------------------------------------------------------------
  // Lint 1 — histogram degeneracy on quantile-consumed families.
  // -------------------------------------------------------------------------
  const families = quantileConsumedFamilies(dashboards);
  expect(
    families.size,
    'at least one provisioned quantile panel consumes a classic histogram family',
  ).toBeGreaterThan(0);

  let judgedFamilies = 0;
  for (const { dsUid, family, where } of [...families.values()].sort((a, b) =>
    `${a.dsUid}|${a.family}`.localeCompare(`${b.dsUid}|${b.family}`),
  )) {
    const vector = await promInstant(
      request,
      dsUid,
      `sum by (le) (${family}_bucket)`,
    );
    if (vector.length === 0) {
      failures.push(
        `[lint:histogram-degeneracy] ${family}: quantile panel (${where}) consumes a family ` +
          `with NO bucket series on the wire — the panel is quantiling nothing`,
      );
      continue;
    }
    const buckets = vector
      .map((s) => ({
        le:
          s.metric.le === '+Inf' || s.metric.le === 'Inf'
            ? Number.POSITIVE_INFINITY
            : Number(s.metric.le),
        cumulative: Number(s.value[1]),
      }))
      .sort((a, b) => a.le - b.le);
    const total = buckets[buckets.length - 1]?.cumulative ?? 0;
    if (total < HISTOGRAM_COUNT_FLOOR) {
      // Below the floor the spread carries no signal; the
      // judgedFamilies assertion after this loop catches a seed
      // regression that would leave EVERY family under-observed.
      continue;
    }
    judgedFamilies++;
    let populated = 0;
    let prev = 0;
    for (const b of buckets) {
      if (b.cumulative - prev > 0) populated++;
      prev = b.cumulative;
    }
    if (populated <= 1) {
      failures.push(
        `[lint:histogram-degeneracy] ${family} (panel: ${where}): all ${total} observations ` +
          `landed in a single bucket (of ${buckets.length}) — bucket boundaries don't match the ` +
          `observed value scale (the ms-default-buckets fabricated-p95 incident class); fix the ` +
          `instrument's bucket boundaries or the recorded unit at the source`,
      );
    }
  }

  // Vacuous-pass guard: lint 1 silently `continue`s on under-floor
  // families, so without this assertion a seed/export regression that
  // starves EVERY family would let the lint pass having judged
  // nothing. (This assertion was claimed by the loop comment from day
  // one but only landed with the adversarial-verification fix.)
  expect(
    judgedFamilies,
    `lint 1 judged no histogram family: all ${families.size} quantile-consumed families are below ` +
      `the ${HISTOGRAM_COUNT_FLOOR}-observation floor after seed traffic — seed/export regression`,
  ).toBeGreaterThan(0);

  // -------------------------------------------------------------------------
  // Lint 2 — identical-series suspicion across multi-quantile panels.
  // -------------------------------------------------------------------------
  let multiQuantilePanels = 0;
  let comparedPanels = 0;
  for (const d of dashboards) {
    for (const p of d.panels) {
      const quantileTargets = p.targets
        .map((t) => ({
          expr: (t.expr ?? t.query ?? '').trim(),
          dsUid: t.datasource?.uid ?? p.datasource?.uid ?? '',
        }))
        .filter((t) => /histogram_quantile\s*\(/.test(t.expr));
      const distinctExprs = new Set(quantileTargets.map((t) => t.expr));
      if (distinctExprs.size < 2) continue;
      multiQuantilePanels++;

      const canonicalSets: string[] = [];
      let allNonEmpty = true;
      for (const t of quantileTargets) {
        const matrix = await promRange(request, t.dsUid, t.expr, nowSec);
        if (matrix.length === 0) {
          // The bidirectional nonempty contract on this panel is
          // enforced by iterate-all-dashboards; an empty result here
          // means that gate is already failing — don't double-report.
          allNonEmpty = false;
          continue;
        }
        canonicalSets.push(
          JSON.stringify(
            matrix
              .map((s) => ({
                metric: Object.fromEntries(
                  Object.entries(s.metric).sort(([a], [b]) =>
                    a.localeCompare(b),
                  ),
                ),
                values: s.values.map(([, v]) => v),
              }))
              .sort((a, b) =>
                JSON.stringify(a.metric).localeCompare(JSON.stringify(b.metric)),
              ),
          ),
        );
      }
      if (!allNonEmpty || canonicalSets.length < 2) continue;
      comparedPanels++;
      const first = canonicalSets[0];
      if (canonicalSets.every((c) => c === first)) {
        failures.push(
          `[lint:identical-quantile-series] ${d.uid} :: ${p.title}: all ${canonicalSets.length} ` +
            `histogram_quantile targets returned bitwise-identical series over the ${QUERY_WINDOW_SECONDS}s ` +
            `window — distinct quantiles collapsing to one value is the single-populated-bucket ` +
            `signature (fabricated-quantile incident class). Targets:\n` +
            quantileTargets.map((t) => `    - ${truncate(t.expr, 200)}`).join('\n'),
        );
      }
    }
  }
  expect(
    multiQuantilePanels,
    'at least one provisioned panel carries ≥2 distinct histogram_quantile targets (lint 2 has real input)',
  ).toBeGreaterThan(0);
  // Vacuous-pass guard, same shape as lint 1's: empty matrices make
  // lint 2 `continue` (iterate-all-dashboards owns the nonempty
  // contract — no double-report), so a regression emptying every
  // multi-quantile panel would otherwise let the lint pass having
  // compared nothing.
  expect(
    comparedPanels,
    `lint 2 compared no panel: all ${multiQuantilePanels} multi-quantile panel(s) returned ≥1 empty ` +
      `matrix over the probe window — the underlying series are gone (seed/export/query regression)`,
  ).toBeGreaterThan(0);

  if (failures.length > 0) {
    throw new Error(
      `data-quality lints violated (${failures.length}):\n\n${failures.join('\n\n')}`,
    );
  }
});
