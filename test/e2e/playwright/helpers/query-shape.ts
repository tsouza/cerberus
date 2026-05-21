/**
 * Query-shape classification.
 *
 * Each panel target carries either a PromQL `expr`, a LogQL `expr`,
 * or a TraceQL `query`. The shape of that expression drives which
 * assertion the spec phases apply:
 *
 *   - `sum by (k1, k2) (…)` → label-shape rule (every byKey must
 *     surface on at least one frame).
 *   - `histogram_quantile(…, foo[bucket])` → histogram-completeness
 *     rule (foo_bucket series MUST exist; the response must be
 *     non-empty when the buckets exist, MUST be empty otherwise).
 *   - other shapes → handled by the wire-level sweep only.
 *
 * The classifier is intentionally regex-based and small. Misclassifying
 * a target should never *introduce* a false positive — the worst case
 * is that we fall through to the opaque branch. If a shape needs new
 * coverage, add a new helper here and a corresponding assertion in
 * helpers/assertions.ts; don't reach into the parser.
 */

// The two clause regexes are kept separate so the label-shape rule
// can distinguish them: `by(k)` requires `k` to be PRESENT on every
// returned series; `without(k)` requires `k` to be ABSENT. The
// previous unified regex returned both modes' keys collapsed into a
// single set, which inverts the semantics for any `without` panel.
const BY_REGEX =
  /\b(?:sum|count|avg|min|max|stddev|stdvar|topk|bottomk|group)\s+by\s*\(\s*([^)]*)\s*\)/g;
const WITHOUT_REGEX =
  /\b(?:sum|count|avg|min|max|stddev|stdvar|topk|bottomk|group)\s+without\s*\(\s*([^)]*)\s*\)/g;
const HISTOGRAM_REGEX = /\bhistogram_quantile\s*\(\s*[^,]+,\s*([\s\S]+?)\s*\)\s*$/;
const METRIC_NAME_REGEX = /([a-zA-Z_:][a-zA-Z0-9_:]*)_bucket/;

/**
 * Extract the union of every `by (…)` key list found in `expr`.
 *
 * Returns an empty array when the expression has no aggregation
 * by-clause. The result is deduplicated and order-preserving — the
 * label-shape assertion uses it as a *set* of expected labels, so
 * a duplicate (e.g. `sum by (a) (sum by (a) (…))`) is collapsed.
 *
 * Crucially, this function ONLY matches the `by(…)` modifier; the
 * inverse `without(…)` modifier carries opposite semantics ("drop
 * these labels", not "keep these labels") and is handled by
 * `extractWithoutKeys`. Conflating the two — which an earlier
 * draft of this helper did — inverts the label-shape rule for any
 * `without` panel and produces false-positive failures.
 *
 * NOTE: this is the *syntactic* extractor — it returns every label
 * keyword the user wrote inside `by(…)`. Some PromQL functions
 * consume their inner aggregation labels and do NOT propagate them
 * to the result series (notably `histogram_quantile(...)` consumes
 * the `le` bucket-boundary label). The label-shape assertion runs
 * against the *result* series, so callers comparing the rule
 * against a response must use `expectedByKeys` (below), which
 * subtracts the consumed labels per top-level call. Using
 * `extractByKeys` directly against a response produces
 * mathematically-impossible-to-satisfy assertions for any
 * `histogram_quantile(... by (le, …) ...)` panel.
 *
 * Examples:
 *   extractByKeys('sum by (a, b) (foo)')              → ['a', 'b']
 *   extractByKeys('count by (k) (rate(foo[5m]))')     → ['k']
 *   extractByKeys('sum(foo)')                          → []
 *   extractByKeys('sum by (a) (count by (b) (foo))')  → ['a', 'b']
 *   extractByKeys('sum without (instance) (foo)')      → []
 */
export function extractByKeys(expr: string): string[] {
  return extractKeysWithRegex(expr, BY_REGEX);
}

/**
 * Labels that the listed top-level PromQL calls consume from their
 * inner aggregation and therefore strip from the result series. The
 * label-shape rule asserts against the *result*, so these labels
 * MUST be subtracted before comparing the rule to the response.
 *
 * `histogram_quantile(q, <inner-bucketed-sum>)` — `le` is the bucket
 * boundary label; the quantile collapses it into a scalar per
 * remaining grouping. A panel like `histogram_quantile(0.95,
 * sum by (le, cerberus_ql) (rate(foo_bucket[5m])))` produces series
 * with `cerberus_ql` only, never `le`.
 *
 * The map is intentionally narrow — extend it only when a new
 * top-level call is added that consumes inner aggregation labels.
 * The default (function not listed) is "consumes nothing", which is
 * the safe-by-default choice for label-shape: a missing entry
 * surfaces as a real label-shape failure rather than a silent pass.
 */
const CONSUMED_BY_TOP_LEVEL_CALL: Record<string, readonly string[]> = {
  histogram_quantile: ['le'],
};

/**
 * Match the top-level PromQL call name (the outermost identifier
 * before the first `(`). Returns null when the expression is not a
 * function call — e.g. a bare metric name or a binary expression.
 *
 * The match is anchored on the start of the expression after
 * leading whitespace; nested calls deeper in the AST aren't the
 * top-level. We don't need a full PromQL parser here — the
 * consumed-label table is small and the matched form is
 * unambiguous.
 */
function topLevelCallName(expr: string): string | null {
  const m = /^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\(/.exec(expr);
  return m ? (m[1] ?? null) : null;
}

/**
 * The set of `by(…)` keys that MUST appear on the response series.
 *
 * Same as `extractByKeys`, with one refinement: if the top-level
 * call consumes labels from its inner aggregation (currently only
 * `histogram_quantile` → `le`), those labels are subtracted because
 * they are gone from the result series by the time the response
 * reaches the spec.
 *
 * This is the helper the panel-shape spec should call when asking
 * "which labels must the response carry?" `extractByKeys` remains
 * available for callers that need the raw syntactic extraction
 * (the helpers.spec.ts unit tests use it to pin parser shape).
 *
 * Examples:
 *   expectedByKeys('sum by (a, b) (foo)')
 *     → ['a', 'b']
 *   expectedByKeys('histogram_quantile(0.95, sum by (le, k) (rate(foo_bucket[5m])))')
 *     → ['k']                       // le is consumed by histogram_quantile
 *   expectedByKeys('histogram_quantile(0.95, sum by (le) (rate(foo_bucket[5m])))')
 *     → []                          // every inner key is consumed
 *   expectedByKeys('sum without (instance) (foo)')
 *     → []                          // no by-clause
 */
export function expectedByKeys(expr: string): string[] {
  const raw = extractByKeys(expr);
  if (raw.length === 0) return raw;
  const call = topLevelCallName(expr);
  if (call === null) return raw;
  const consumed = CONSUMED_BY_TOP_LEVEL_CALL[call];
  if (consumed === undefined) return raw;
  const consumedSet = new Set(consumed);
  return raw.filter((k) => !consumedSet.has(k));
}

/**
 * Extract the union of every `without (…)` key list found in `expr`.
 *
 * Returns an empty array when the expression has no `without` clause.
 * The label-shape rule consumes this list as the set of labels that
 * must be ABSENT from every returned series — the semantic inverse
 * of `extractByKeys`.
 *
 * Examples:
 *   extractWithoutKeys('sum without (instance) (foo)')   → ['instance']
 *   extractWithoutKeys('sum by (a) (foo)')                → []
 *   extractWithoutKeys('sum(foo)')                         → []
 *   extractWithoutKeys('sum without (a, b) (sum without (a) (foo))') → ['a', 'b']
 */
export function extractWithoutKeys(expr: string): string[] {
  return extractKeysWithRegex(expr, WITHOUT_REGEX);
}

function extractKeysWithRegex(expr: string, regex: RegExp): string[] {
  const seen = new Set<string>();
  const ordered: string[] = [];
  let match: RegExpExecArray | null;
  // Reset lastIndex defensively — the regex is module-scoped and
  // sticky semantics could leak between calls.
  regex.lastIndex = 0;
  while ((match = regex.exec(expr)) !== null) {
    const inner = match[1] ?? '';
    for (const raw of inner.split(',')) {
      const key = raw.trim();
      if (key === '') continue;
      if (seen.has(key)) continue;
      seen.add(key);
      ordered.push(key);
    }
  }
  return ordered;
}

/**
 * True iff the expression's top-level call is `histogram_quantile(…)`.
 *
 * The check is structural, not just substring: a metric named
 * `histogram_quantile_total` (hypothetical) should NOT match. We
 * anchor on the function-call shape `histogram_quantile(…, …)`.
 */
export function isHistogramQuantile(expr: string): boolean {
  return /\bhistogram_quantile\s*\(/.test(expr);
}

/**
 * For a `histogram_quantile(q, <metric-name>_bucket[…])` expression,
 * extract the `<metric-name>` root (without the `_bucket` suffix).
 *
 * Returns null when the expression isn't a histogram_quantile call,
 * or when the inner expression doesn't reference a `_bucket` series.
 *
 * Examples:
 *   extractHistogramName('histogram_quantile(0.95, rate(foo_bucket[5m]))')
 *     → 'foo'
 *   extractHistogramName('histogram_quantile(0.95, sum by (le) (rate(foo_bucket[5m])))')
 *     → 'foo'
 *   extractHistogramName('histogram_quantile(0.95, foo_total)')
 *     → null   // no _bucket suffix — this is the N6 fabricated-value case
 *   extractHistogramName('rate(foo[5m])')
 *     → null   // not a histogram_quantile call at all
 */
export function extractHistogramName(expr: string): string | null {
  const m = HISTOGRAM_REGEX.exec(expr);
  if (!m) return null;
  const inner = m[1] ?? '';
  const nameMatch = METRIC_NAME_REGEX.exec(inner);
  if (!nameMatch) return null;
  return nameMatch[1] ?? null;
}
