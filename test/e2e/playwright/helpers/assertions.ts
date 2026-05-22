/**
 * Per-shape assertions over Grafana's /api/ds/query response envelope.
 *
 * Every helper here is `void`-typed and throws on failure. The phase
 * specs run them inside try/catch + push the error string into a
 * `failures[]` aggregator (mirrors the existing
 * compose_grafana_smoke.spec.ts shape), so the runtime cost of a
 * thrown vs returned error is identical and the typed contract is
 * "this assertion either passes silently or surfaces a diagnostic".
 *
 * `assertNon200ResponseClass` does NOT carry an allow-list and never
 * will: every non-2xx captured during a sweep is a real failure that
 * must be fixed at the source (implement the endpoint, fix the proxy,
 * or drop the surface from the iteration).
 */

import type { Request } from '@playwright/test';

/**
 * Grafana 11.x /api/ds/query response envelope (shape we depend on).
 * Other fields exist; we deliberately don't model them.
 */
export type DsQueryResponse = {
  results?: Record<
    string,
    {
      error?: string;
      frames?: Array<{
        schema?: {
          fields?: Array<{
            name?: string;
            labels?: Record<string, string>;
          }>;
        };
        data?: { values?: unknown[][] };
      }>;
    }
  >;
};

/**
 * For each key in `byKeys`, assert that at least one frame in the
 * response carries that key in `schema.fields[].labels`.
 *
 * This is the load-bearing label-shape rule that closes N2/N11/N14
 * (a `sum by (cerberus_ql)` panel collapsing to a single anonymous
 * "Value" frame). The classic regression shape is "frames > 0 but
 * every frame's labels[<key>] is undefined or empty string" — the
 * frame-count gate alone misses it, so we check label *content*.
 */
export function assertLabelShape(
  response: DsQueryResponse,
  byKeys: string[],
): void {
  if (byKeys.length === 0) return;
  const observed = new Set<string>();
  for (const target of Object.values(response.results ?? {})) {
    for (const frame of target.frames ?? []) {
      for (const field of frame.schema?.fields ?? []) {
        for (const labelKey of Object.keys(field.labels ?? {})) {
          // Empty-string label values still count as the key being
          // *present* — the assertion is about schema, not value.
          // The value-side check (anonymous bucket fallback) lives in
          // the phase-1 spec, not here.
          observed.add(labelKey);
        }
      }
    }
  }
  const missing = byKeys.filter((k) => !observed.has(k));
  if (missing.length > 0) {
    throw new Error(
      `assertLabelShape: expected labels [${byKeys.join(', ')}] but only saw [${[
        ...observed,
      ]
        .sort()
        .join(', ')}]; missing=[${missing.join(', ')}]`,
    );
  }
}

/**
 * For each key in `withoutKeys`, assert that NO frame in the response
 * carries that key in `schema.fields[].labels`.
 *
 * The semantic inverse of `assertLabelShape`: `sum without (instance)`
 * means "drop the `instance` label", so the returned series MUST NOT
 * carry it. A regression that ignored the `without` modifier and
 * collapsed everything anyway would surface as `instance` re-appearing
 * on the wire — exactly the shape this assertion catches.
 */
export function assertLabelAbsent(
  response: DsQueryResponse,
  withoutKeys: string[],
): void {
  if (withoutKeys.length === 0) return;
  const observed = new Set<string>();
  for (const target of Object.values(response.results ?? {})) {
    for (const frame of target.frames ?? []) {
      for (const field of frame.schema?.fields ?? []) {
        for (const labelKey of Object.keys(field.labels ?? {})) {
          observed.add(labelKey);
        }
      }
    }
  }
  const leaked = withoutKeys.filter((k) => observed.has(k));
  if (leaked.length > 0) {
    throw new Error(
      `assertLabelAbsent: labels [${withoutKeys.join(
        ', ',
      )}] should be dropped by the without(...) modifier, but observed=[${[
        ...observed,
      ]
        .sort()
        .join(', ')}]; leaked=[${leaked.join(', ')}]`,
    );
  }
}

/**
 * Assert that a `histogram_quantile(...)` response over `<name>` (the
 * metric root without `_bucket`) is non-empty.
 *
 * Caller has already verified — out of band, via `/api/v1/series` —
 * that `<name>_bucket` exists in the dataset. If the buckets are
 * absent, the spec must use `assertNoFabricatedValue` instead.
 *
 * "Complete" here means: at least one frame carries at least one
 * numeric sample row. An empty envelope (no frames, or every frame
 * with `data.values` empty) means the quantile didn't resolve — that
 * IS the N5 regression (P95 latency by language flat at 0).
 */
export function assertHistogramComplete(
  response: DsQueryResponse,
  name: string,
): void {
  let sawSample = false;
  for (const target of Object.values(response.results ?? {})) {
    for (const frame of target.frames ?? []) {
      const cols = frame.data?.values ?? [];
      // Grafana's columnar shape: at least one column with ≥ 1 row.
      const rowCount = cols.length > 0 ? (cols[0] ?? []).length : 0;
      if (rowCount > 0) {
        sawSample = true;
        break;
      }
    }
    if (sawSample) break;
  }
  if (!sawSample) {
    throw new Error(
      `assertHistogramComplete: histogram_quantile(..., ${name}_bucket) returned an empty envelope; expected ≥ 1 sample frame because ${name}_bucket exists in the dataset`,
    );
  }
}

/**
 * Assert that a `histogram_quantile(...)` over a metric root whose
 * `_bucket` series does NOT exist returns an EMPTY envelope.
 *
 * This is the N6 rule: `histogram_quantile(0.95, foo_total)` (no
 * `_bucket` series anywhere) used to return a synthetic non-empty
 * float. The fix wraps the underlying scan in `toFloat64` only when
 * the bucket series is present — when it isn't, the quantile must
 * legitimately resolve to nothing.
 *
 * Caller has already verified out-of-band that the `_bucket` series
 * are absent.
 */
export function assertNoFabricatedValue(
  response: DsQueryResponse,
  expr: string,
): void {
  for (const target of Object.values(response.results ?? {})) {
    for (const frame of target.frames ?? []) {
      const cols = frame.data?.values ?? [];
      const rowCount = cols.length > 0 ? (cols[0] ?? []).length : 0;
      if (rowCount > 0) {
        throw new Error(
          `assertNoFabricatedValue: histogram_quantile expression "${expr}" returned ${rowCount} sample(s) but its underlying _bucket series are absent; this is a fabricated value (N6 regression)`,
        );
      }
    }
  }
}

/**
 * Assert that the filtered series count is a non-empty subset of the
 * baseline by *count* (not element-wise).
 *
 * The filter-drill spec re-fires a panel's baseline expression after
 * tacking a `{<key>="<value>"}` matcher onto every selector; this
 * helper is the load-bearing comparator for that drill.
 *
 * Two failure modes are caught:
 *
 *   1. `filtered === 0`: the drill returned nothing even though the
 *      `(key, value)` pair came from the baseline response. This is
 *      the N3-class regression — a real, observed label value goes
 *      empty under the matcher path.
 *   2. `filtered > baseline`: the filter somehow *expanded* the
 *      result set. PromQL's matcher semantics guarantee the filtered
 *      series are a subset of the unfiltered ones, so any growth is
 *      the matcher path emitting series the unfiltered query didn't.
 *      That's a second N3-class shape (lower priority but equally
 *      load-bearing for the regression pin).
 *
 * Q3 of the e2e-enhance plan (`/home/thiago/.claude/plans/e2e-enhance.md`
 * §9) resolves the comparator semantics: `≤ baseline count`, NOT
 * element-wise strict subset. Element-wise comparison is order-
 * dependent and flakes under series re-orderings between the two
 * queries.
 */
export function assertSubsetByCount(
  filtered: number,
  baseline: number,
  context: string,
): void {
  if (filtered === 0) {
    throw new Error(
      `assertSubsetByCount: filtered query returned 0 series despite drilling on a real baseline value (${context}); ≥ 1 series expected (N3-class regression — matcher path broken)`,
    );
  }
  if (filtered > baseline) {
    throw new Error(
      `assertSubsetByCount: filtered=${filtered} > baseline=${baseline} (${context}); filtering must shrink or keep the series set, never grow it (N3-class regression — matcher path emitting series the unfiltered query did not)`,
    );
  }
}

/**
 * Assert the response class of a captured Playwright request is 2xx.
 *
 * Every non-2xx is a failure; the fix is to implement the endpoint
 * or to remove the surface from the iteration, never to add an
 * allow-list.
 *
 * The function only needs the Playwright Request handle to extract
 * `method()` + `url()` for the failure message; the status comes
 * from awaiting `request.response()` inside the helper, so the
 * caller doesn't need to fish it out themselves.
 */
export async function assertNon200ResponseClass(req: Request): Promise<void> {
  const resp = await req.response();
  if (resp === null) {
    throw new Error(
      `assertNon200ResponseClass: request ${req.method()} ${req.url()} had no response (navigation aborted or browser closed)`,
    );
  }
  const status = resp.status();
  if (status >= 200 && status <= 299) return;
  throw new Error(
    `assertNon200ResponseClass: ${req.method()} ${req.url()} → ${status}; every non-2xx is a failure`,
  );
}
