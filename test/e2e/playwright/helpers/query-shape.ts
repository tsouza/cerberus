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

// PromQL identifiers that aren't metric-name selectors — they appear
// in identifier position but introduce keywords / call-shaped clauses.
// `by` / `without` / `on` / `ignoring` / `group_left` / `group_right`
// are all followed by `(`, which the selector-finder already excludes;
// the entries here cover the bare-keyword cases (`and`, `or`, `unless`,
// `offset`, `bool`) plus the call-style ones for defence in depth.
const PROMQL_KEYWORDS = new Set<string>([
  // Binary operators / modifiers in identifier position.
  'and',
  'or',
  'unless',
  'offset',
  'bool',
  'by',
  'without',
  'on',
  'ignoring',
  'group_left',
  'group_right',
  'start',
  'end',
  'atan2',
  // Aggregation operators. These always take a `(...)` argument
  // list (optionally with a `by(...)` or `without(...)` clause
  // *between* the name and the parens). The walker peeks one token
  // ahead and would otherwise see the `by` / `without` keyword
  // rather than the `(`, so we have to mark the aggregator names
  // explicitly. (When the aggregator is followed directly by `(`,
  // the walker's `next === '('` branch catches it; this set covers
  // the `sum by (...) (...)` shape.)
  'sum',
  'avg',
  'min',
  'max',
  'count',
  'stddev',
  'stdvar',
  'topk',
  'bottomk',
  'group',
  'quantile',
  'count_values',
]);

/**
 * True iff `expr` already constrains label `key` via a matcher of any
 * kind (`=`, `!=`, `=~`, `!~`) in any of its `{...}` selector blocks.
 *
 * Used by the filter-drill spec to skip targets whose expression
 * already carries a hardcoded matcher for the label we'd otherwise
 * drill on — drilling there would either be a no-op (the value
 * matches the hardcoded one) or contradict the existing matcher (the
 * value differs, producing an empty set that isn't a regression
 * signal). Either way the drill isn't informative; the spec excludes
 * the target.
 *
 * The match is conservative — we only look at `<key>` appearing in
 * matcher position (`<key><op>`); a label appearing as a `by(...)`
 * key or as a function-arg name doesn't count.
 *
 * Examples:
 *   expressionHasMatcherFor('rate(foo{cerberus_ql="promql"}[5m])', 'cerberus_ql')
 *     → true
 *   expressionHasMatcherFor('rate(foo[5m])', 'cerberus_ql')         → false
 *   expressionHasMatcherFor('rate(foo{job="x"}[5m])', 'cerberus_ql') → false
 *   expressionHasMatcherFor('sum by (cerberus_ql) (foo)', 'cerberus_ql')
 *     → false                                                       // by() isn't a matcher
 */
export function expressionHasMatcherFor(expr: string, key: string): boolean {
  // Find every `{...}` block and look for `<key>\s*(=|!=|=~|!~)`
  // inside it. The outer block matcher is non-greedy and balanced-
  // brace-free, which matches PromQL's selector syntax — selectors
  // don't nest.
  const blocks = expr.match(/\{[^{}]*\}/g) ?? [];
  const matcherRegex = new RegExp(
    `(?:^|[\\s,{])${escapeRegex(key)}\\s*(=~|!~|!=|=)`,
  );
  for (const b of blocks) {
    if (matcherRegex.test(b)) return true;
  }
  return false;
}

/**
 * Re-write `expr` to add a `<key>="<value>"` matcher to every vector
 * selector. This is the load-bearing helper for the phase-3
 * filter-drill spec: given a panel's baseline expression and a
 * (label, value) pair observed in the baseline response, produce
 * the filtered expression to fire as the drill-down probe.
 *
 * Two injection paths:
 *
 *   - Selector already has a `{...}` block: append `,<key>="<value>"`
 *     just before the closing `}`. An empty block (`{}`) becomes
 *     `{<key>="<value>"}`.
 *   - Bare metric name (no `{...}` block): synthesise
 *     `<metric>{<key>="<value>"}`.
 *
 * Identifiers immediately followed by `(` are PromQL function calls
 * (`rate(...)`, `histogram_quantile(...)`, `sum(...)`) and are NOT
 * vector selectors — they're skipped. Bare keywords (`and`, `or`,
 * `unless`, `offset`, `bool`, plus the call-shape ones for defence
 * in depth) are likewise skipped.
 *
 * The value is quoted with double quotes; embedded `"` and `\` are
 * escaped per PromQL string-literal grammar. Callers shouldn't pass
 * a value containing a literal newline — those don't occur in real
 * label values and the helper doesn't try to model them.
 *
 * Examples:
 *   addLabelFilter('rate(foo[5m])', 'cerberus_ql', 'promql')
 *     → 'rate(foo{cerberus_ql="promql"}[5m])'
 *   addLabelFilter('sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))', 'cerberus_ql', 'promql')
 *     → 'sum by (cerberus_ql) (rate(cerberus_queries_total{cerberus_ql="promql"}[5m]))'
 *   addLabelFilter('rate(foo{job="x"}[5m])', 'cerberus_ql', 'promql')
 *     → 'rate(foo{job="x",cerberus_ql="promql"}[5m])'
 *   addLabelFilter('rate({__name__=~".+"}[5m])', 'service_name', 'cerberus')
 *     → 'rate({__name__=~".+",service_name="cerberus"}[5m])'
 *   addLabelFilter('histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(foo_bucket[5m])))', 'cerberus_ql', 'promql')
 *     → 'histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(foo_bucket{cerberus_ql="promql"}[5m])))'
 */
export function addLabelFilter(
  expr: string,
  key: string,
  value: string,
): string {
  const matcher = `${key}="${escapeMatcherValue(value)}"`;

  // First pass: inject into every existing `{...}` selector block.
  // Empty `{}` becomes `{<matcher>}`; non-empty appends `,<matcher>`.
  // This catches the `{__name__=~".+"}`-style metric-less selectors
  // that the bare-name pass below wouldn't see.
  let out = expr.replace(/\{([^{}]*)\}/g, (_full, inner: string) => {
    const trimmed = inner.trim();
    if (trimmed === '') return `{${matcher}}`;
    return `{${inner},${matcher}}`;
  });

  // Second pass: synthesise `{<matcher>}` after any bare metric name
  // (identifier NOT followed by `(`, NOT already followed by `{...}`,
  // and not a reserved word). We walk the string token-by-token so
  // we can skip `{...}` blocks and string literals as a whole — the
  // identifiers *inside* those (e.g. label keys, regex literals) are
  // not selectors and must not be touched.
  return walkAndInjectBare(out, matcher);
}

// Walk `expr` left to right; for every bare metric-name selector
// (identifier in selector position, not followed by `{...}` or `(`),
// emit `<ident>{matcher}` in place of `<ident>`. Skips strings and
// `{...}` blocks wholesale — identifiers inside them are not
// selectors.
function walkAndInjectBare(expr: string, matcher: string): string {
  let out = '';
  let i = 0;
  while (i < expr.length) {
    const c = expr[i] ?? '';
    // String literal — copy until the matching closing quote,
    // respecting backslash escapes. PromQL accepts ", ', and `
    // delimiters.
    if (c === '"' || c === "'" || c === '`') {
      const quote = c;
      out += c;
      i++;
      while (i < expr.length) {
        const ch = expr[i] ?? '';
        out += ch;
        if (ch === '\\' && quote !== '`') {
          // Escape sequence: copy next char verbatim.
          i++;
          if (i < expr.length) {
            out += expr[i];
            i++;
          }
          continue;
        }
        if (ch === quote) {
          i++;
          break;
        }
        i++;
      }
      continue;
    }
    // `{...}` block — copy the whole thing. Selectors don't nest,
    // so a flat counter suffices.
    if (c === '{') {
      let depth = 1;
      out += c;
      i++;
      while (i < expr.length && depth > 0) {
        const ch = expr[i] ?? '';
        out += ch;
        if (ch === '"' || ch === "'" || ch === '`') {
          // skip string in case label values include `{` / `}`
          const q = ch;
          i++;
          while (i < expr.length) {
            const sc = expr[i] ?? '';
            out += sc;
            if (sc === '\\' && q !== '`') {
              i++;
              if (i < expr.length) {
                out += expr[i];
                i++;
              }
              continue;
            }
            if (sc === q) {
              i++;
              break;
            }
            i++;
          }
          continue;
        }
        if (ch === '{') depth++;
        else if (ch === '}') depth--;
        i++;
      }
      continue;
    }
    // Identifier — collect, then decide whether to inject.
    if (/[a-zA-Z_:]/.test(c)) {
      // Word-boundary: an identifier preceded by `.` is a field
      // access (TraceQL `resource.service.name`), not a fresh
      // selector. The PromQL parser doesn't use dots in identifiers
      // but the spec uses this helper for promql only — defence in
      // depth.
      const prev = i > 0 ? (expr[i - 1] ?? '') : '';
      if (/[A-Za-z0-9_:.]/.test(prev)) {
        out += c;
        i++;
        continue;
      }
      let j = i;
      while (j < expr.length && /[a-zA-Z0-9_:]/.test(expr[j] ?? '')) j++;
      const ident = expr.slice(i, j);
      out += ident;
      i = j;
      // Skip whitespace to peek at the next significant char.
      let k = i;
      while (k < expr.length && /\s/.test(expr[k] ?? '')) k++;
      const next = k < expr.length ? (expr[k] ?? '') : '';
      // Grouping-modifier keywords (`by`, `without`, `on`,
      // `ignoring`, `group_left`, `group_right`) followed by `(` —
      // the `(...)` block is a label list (or a series of label
      // names), not a selector. Skip the whole block so the walker
      // doesn't inject into the label names. This check has to come
      // BEFORE the generic `next === '('` branch, otherwise `by` is
      // mistaken for a function call and the inner label-list is
      // walked as if it were a function argument list.
      if (
        next === '(' &&
        (ident === 'by' ||
          ident === 'without' ||
          ident === 'on' ||
          ident === 'ignoring' ||
          ident === 'group_left' ||
          ident === 'group_right')
      ) {
        // Copy whitespace + balanced (...) block verbatim.
        out += expr.slice(i, k + 1); // through the opening `(`
        i = k + 1;
        let depth = 1;
        while (i < expr.length && depth > 0) {
          const ch = expr[i] ?? '';
          out += ch;
          if (ch === '(') depth++;
          else if (ch === ')') depth--;
          i++;
        }
        continue;
      }
      // Function call → not a selector.
      if (next === '(') continue;
      // Already has a `{...}` block → first pass handled it.
      if (next === '{') continue;
      // Other keywords (bare `and`, `or`, `unless`, `offset`, `bool`,
      // plus the aggregator names when used with a `by(...)` /
      // `without(...)` modifier between the aggregator and the
      // `(args)` block) → not selectors.
      if (PROMQL_KEYWORDS.has(ident)) continue;
      // Inject the synthesised selector block.
      out += `{${matcher}}`;
      continue;
    }
    // Any other character — copy verbatim.
    out += c;
    i++;
  }
  return out;
}

function escapeMatcherValue(v: string): string {
  // PromQL string literal escaping: backslash, double quote.
  return v.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
