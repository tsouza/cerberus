/**
 * Pinned resource-bound rejection contracts (issue #3468).
 *
 * Several dashboard-lane specs probe a live cerberus with real,
 * organically-seeded traffic, and one provisioned panel — the
 * self-observability dashboard's P95-latency-by-language panel,
 * `histogram_quantile(0.95, sum by (cerberus_ql)
 * (rate(cerberus_queries_duration_exp_hist[5m])))` — can legitimately
 * cross a resource bound at wide-enough (range, step) tuples or dense-
 * enough traffic. That is the guard working as documented, not a bug:
 * cerberus either lets ClickHouse itself abort at its own per-query
 * memory cap (MEMORY_LIMIT_MESSAGE), or — for a shape one of cerberus's
 * own emitter-planted pre-rejection guards recognises — aborts BEFORE
 * ClickHouse would have to (RESOURCE_BOUND_GUARD_MESSAGES).
 *
 * A 422 carrying one of these, byte-for-byte, is a PINNED CONTRACT, not
 * a tolerance: any other status, or a 422 with a different body, stays a
 * hard failure. See iterate-time-ranges.spec.ts's own file header for
 * the fuller "Memory-limit / resource-bound multi-way contract" doc —
 * this module exists so that doc's actual message list is not
 * hand-copied into every spec that needs to recognise the same
 * rejection.
 */

// ClickHouse per-query memory cap pinned by both stacks via
// CERBERUS_CH_QUERY_MAX_MEMORY (test/e2e/k3s/cerberus-values.yaml,
// docker-compose.yml). Kept as a literal so a stack-config drift breaks
// this pin instead of silently changing the contract.
export const CH_QUERY_MAX_MEMORY_BYTES = 1_073_741_824;

// The exact 422 errorType=execution wire message cerberus's Prom head
// emits when ClickHouse aborts a query for exceeding the per-query
// memory cap (CH error 241, MEMORY_LIMIT_EXCEEDED). Byte-for-byte the
// production message from internal/api/prom/handler.go
// (promMemoryLimitMessage) — pinned in lock-step with
// internal/api/prom/handler_memory_limit_test.go.
export const MEMORY_LIMIT_MESSAGE = `query processing would use too much memory in query execution (ClickHouse memory limit exceeded; per-query cap ${CH_QUERY_MAX_MEMORY_BYTES} bytes)`;

// cerberus's own emitter-planted resource-bound guard messages (issue
// #3468) — the pre-rejection alternative to MEMORY_LIMIT_MESSAGE above.
// Byte-for-byte the production constants:
// internal/chplan.HistogramMergeBudgetMessage (the native-histogram
// cross-series merge, issue #2385),
// internal/chplan.ExpHistogramWindowSampleBudgetMessage (the
// samples-per-series-per-window pre-rejection, issue #3252), and
// internal/chsql.RangeBucketFanoutGroupBudgetMessage (the
// RangeBucketFanout collapse's own group-count bound, issue #3468).
export const RESOURCE_BOUND_GUARD_MESSAGES = [
  'native histogram merge exceeds the series-per-group or merged-bucket-width resource bound',
  'exponential-histogram window exceeds the samples-per-series-per-window resource bound',
  'histogram window fold exceeds the anchors-times-series group-count resource bound',
];

type PinnedErrorEnvelope = {
  status?: string;
  errorType?: string;
  error?: string;
};

/**
 * True iff status/body is exactly a pinned resource-exhausted rejection:
 * a 422 with errorType=execution and either MEMORY_LIMIT_MESSAGE or one
 * of RESOURCE_BOUND_GUARD_MESSAGES, byte-for-byte. A malformed body, a
 * different status, or a 422 with any other message is NOT a pinned
 * rejection — the caller is expected to treat those as hard failures.
 */
export function isPinnedResourceBoundRejection(
  status: number,
  body: string,
): boolean {
  if (status !== 422) return false;
  let parsed: PinnedErrorEnvelope | null = null;
  try {
    parsed = JSON.parse(body) as PinnedErrorEnvelope;
  } catch {
    return false;
  }
  if (parsed?.status !== 'error' || parsed?.errorType !== 'execution') {
    return false;
  }
  return (
    parsed.error === MEMORY_LIMIT_MESSAGE ||
    RESOURCE_BOUND_GUARD_MESSAGES.includes(parsed.error ?? '')
  );
}

/**
 * True iff body's raw text CONTAINS one of the pinned rejection
 * messages anywhere — a looser match than isPinnedResourceBoundRejection
 * above, for callers that go through Grafana's own `/api/ds/query`
 * datasource-proxy rather than cerberus's `/api/v1/query_range`
 * directly. Grafana re-wraps a failed target's error into its own
 * envelope (`{"results":{"<refId>":{"error":"execution: <cerberus's
 * message>", ...}}}`, HTTP status re-mapped to its own convention —
 * observed 400, not cerberus's own 422) and prefixes the message
 * ("execution: "), so neither the exact envelope shape nor the exact
 * status code cerberus itself returns survives the proxy hop. A plain
 * substring test on the raw body is what actually stays robust across
 * that rewrap, since none of the pinned messages is a plausible
 * accidental substring of anything else a dashboard datasource-proxy
 * response would contain.
 */
export function bodyContainsPinnedResourceBoundMessage(body: string): boolean {
  return (
    body.includes(MEMORY_LIMIT_MESSAGE) ||
    RESOURCE_BOUND_GUARD_MESSAGES.some((m) => body.includes(m))
  );
}
