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
