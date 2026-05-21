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

// LogQL aggregation `by(...)` keys — the LogQL aggregation syntax
// mirrors PromQL's (`sum by(k) (rate({...}[5m]))`), so `extractByKeys`
// already covers it. The standalone helper here exists for symmetry
// with the TraceQL extractor and so the iterator-spec doesn't have to
// know that LogQL re-uses the PromQL regex.
//
// Examples:
//   extractLogQLByKeys('sum by (SeverityText) (rate({service_name=~".+"}[5m]))')
//     → ['SeverityText']
//   extractLogQLByKeys('{service_name="cerberus"} | SeverityText="ERROR"')
//     → []                                  // not an aggregation
export function extractLogQLByKeys(expr: string): string[] {
  return extractByKeys(expr);
}

// TraceQL aggregation `by(...)` clauses sit on the pipeline metric
// functions (`| rate() by (k)`, `| count_over_time() by (k)`), NOT
// behind a PromQL-style aggregator prefix. The PromQL `BY_REGEX`
// therefore misses them; this extractor walks every `\| <fn>() by(...)`
// occurrence and unions the labels.
//
// The dotted attribute keys TraceQL uses (`resource.service.name`,
// `span.http.method`) survive verbatim — the inner regex is
// `[^)]*` which permits dots.
//
// Examples:
//   extractTraceQLByKeys('{ status = error } | rate() by (resource.service.name)')
//     → ['resource.service.name']
//   extractTraceQLByKeys('{ resource.service.name = "x" } | count_over_time() by (span.kind)')
//     → ['span.kind']
//   extractTraceQLByKeys('{ resource.service.name != "" }')
//     → []                                  // bare spanset, no aggregation
const TRACEQL_BY_REGEX =
  /\|\s*(?:rate|count_over_time|sum_over_time|avg_over_time|max_over_time|min_over_time|quantile_over_time|histogram_over_time|count|sum|avg|max|min)\s*\([^)]*\)\s*by\s*\(\s*([^)]*)\s*\)/g;
export function extractTraceQLByKeys(expr: string): string[] {
  return extractKeysWithRegex(expr, TRACEQL_BY_REGEX);
}

/**
 * Per-dsType by-key extractor — the iterator-spec calls this to pick
 * the set of labels worth drilling on for a panel target.
 *
 *   - `prometheus` → `expectedByKeys` (with `histogram_quantile` → `le`
 *     consumed-label subtraction).
 *   - `loki`       → `extractLogQLByKeys` (LogQL aggregation mirrors
 *     PromQL syntactically; the PromQL regex covers it).
 *   - `tempo`      → `extractTraceQLByKeys` (TraceQL `| <fn>() by(k)`
 *     pipeline syntax).
 *
 * Returns `[]` for any other / unknown dsType — the iterator skips
 * panels with no extractable by-keys, so the default-empty behaviour
 * is safe-by-omission rather than a silent surprise.
 */
export function expectedByKeysForDsType(
  expr: string,
  dsType: string,
): string[] {
  if (dsType === 'prometheus') return expectedByKeys(expr);
  if (dsType === 'loki') return extractLogQLByKeys(expr);
  if (dsType === 'tempo') return extractTraceQLByKeys(expr);
  return [];
}

/**
 * Re-write a LogQL `expr` to add a `<key>="<value>"` matcher to every
 * stream-selector `{...}` block. The LogQL grammar splits an
 * expression into:
 *
 *   - The stream selector — a brace block of `key=value` matchers.
 *     This is the part the filter-drill spec needs to constrain.
 *   - An optional pipeline — `| <stage> | <stage> …` where each stage
 *     is a parser (`json`, `logfmt`), a label filter
 *     (`| SeverityText="ERROR"`), or a line-format expression.
 *
 * We inject into the stream selector ONLY: the pipeline part is left
 * untouched even when stages carry `key="value"`-shaped label filters
 * (those are post-parsing predicates, not stream matchers).
 *
 * Two injection paths, mirroring `addLabelFilter`:
 *
 *   - Stream selector already has matchers: append `,<key>="<value>"`
 *     just before the closing `}`. An empty selector (`{}`) becomes
 *     `{<key>="<value>"}`.
 *   - Stream selector already constrains `<key>` via any matcher: the
 *     rewrite is a no-op for that block (idempotent — drilling on a
 *     label that's already pinned would either tautologise or
 *     contradict).
 *
 * Aggregating queries such as `sum by (X) (rate({…}[5m]))` are
 * handled implicitly: the brace block inside is the stream selector,
 * and the walker injects into it just like in the bare case.
 *
 * The walker skips string literals (`"…"`, `` `…` ``) so a
 * `| line_format "{{.foo}}"` stage doesn't get parsed as a brace
 * block.
 *
 * Examples:
 *   addLogQLLabelFilter('{service_name="cerberus"}', 'level', 'error')
 *     → '{service_name="cerberus",level="error"}'
 *   addLogQLLabelFilter(
 *     '{service_name="cerberus"} | json | line_format "{{.foo}}"',
 *     'level', 'error',
 *   )
 *     → '{service_name="cerberus",level="error"} | json | line_format "{{.foo}}"'
 *   addLogQLLabelFilter(
 *     'sum by (SeverityText) (rate({service_name=~".+"} [5m]))',
 *     'service_name', 'cerberus',
 *   )
 *     → 'sum by (SeverityText) (rate({service_name=~".+",service_name="cerberus"} [5m]))'
 *     (note: the iterator filters out a key that already has a matcher,
 *      so in practice this idempotent-by-spec path is unreached; see
 *      the idempotent-key variant below for the no-op case)
 *   addLogQLLabelFilter('{}', 'service_name', 'cerberus')
 *     → '{service_name="cerberus"}'
 *   addLogQLLabelFilter('{level="error"}', 'level', 'error')
 *     → '{level="error"}'                                // idempotent
 */
export function addLogQLLabelFilter(
  expr: string,
  key: string,
  value: string,
): string {
  const matcher = `${key}="${escapeMatcherValue(value)}"`;
  return rewriteTopLevelBraceBlocks(expr, (inner) =>
    injectIntoCommaSeparatedBlock(inner, key, matcher),
  );
}

/**
 * Re-write a TraceQL `expr` to add an attribute filter
 * `<key>="<value>"` to every spanset `{ … }` block. TraceQL spansets
 * differ from PromQL/LogQL selectors in two ways:
 *
 *   - Conjunction is `&&` (whitespace-padded), not `,`.
 *   - Attribute keys are dotted paths (`resource.service.name`,
 *     `span.http.method`), not bare identifiers.
 *
 * Injection paths:
 *
 *   - Spanset already has conditions: append ` && <key>="<value>"`
 *     just before the closing `}`. The surrounding whitespace is
 *     preserved: a TraceQL idiom is `{ a = b }` (single-space pads
 *     on both sides of the brace contents), so we emit the same.
 *   - Empty spanset (`{}` or `{ }`): becomes `{ <key>="<value>" }`.
 *   - Spanset already constrains `<key>` via any matcher: no-op
 *     (idempotent — the iterator filters these out upstream, this is
 *     defence-in-depth).
 *
 * Aggregating queries such as `{ status = error } | rate() by (k)`
 * are handled implicitly: only the leading spanset is touched; the
 * pipeline is left as-is.
 *
 * The walker skips string literals (`"…"`) so an attribute value
 * containing `{` or `}` doesn't get mistaken for a brace block.
 *
 * Examples:
 *   addTraceQLAttributeFilter('{ status = error }', 'resource.service.name', 'cerberus')
 *     → '{ status = error && resource.service.name="cerberus" }'
 *   addTraceQLAttributeFilter('{}', 'resource.service.name', 'cerberus')
 *     → '{ resource.service.name="cerberus" }'
 *   addTraceQLAttributeFilter(
 *     '{ status = error } | rate() by (resource.service.name)',
 *     'span.kind', 'server',
 *   )
 *     → '{ status = error && span.kind="server" } | rate() by (resource.service.name)'
 *   addTraceQLAttributeFilter(
 *     '{ resource.service.name = "cerberus" }', 'resource.service.name', 'cerberus',
 *   )
 *     → '{ resource.service.name = "cerberus" }'                  // idempotent
 */
export function addTraceQLAttributeFilter(
  expr: string,
  key: string,
  value: string,
): string {
  const matcher = `${key}="${escapeMatcherValue(value)}"`;
  return rewriteTopLevelBraceBlocks(expr, (inner) =>
    injectIntoSpansetBlock(inner, key, matcher),
  );
}

// Walk `expr`, identify every top-level `{…}` brace block (i.e. a
// brace pair not nested inside another brace block and not inside a
// string literal), and call `rewriteInner(inner)` to produce the
// replacement contents. Returns the rewritten expression.
//
// LogQL stream selectors and TraceQL spansets share this top-level
// brace shape; only the *contents* differ (comma- vs `&&`-separated).
function rewriteTopLevelBraceBlocks(
  expr: string,
  rewriteInner: (inner: string) => string,
): string {
  let out = '';
  let i = 0;
  while (i < expr.length) {
    const c = expr[i] ?? '';
    if (c === '"' || c === "'" || c === '`') {
      // String literal — copy verbatim, respecting backslash escapes
      // for the double-/single-quote forms (`backtick` strings are raw
      // in PromQL/LogQL, no escape processing).
      const quote = c;
      out += c;
      i++;
      while (i < expr.length) {
        const ch = expr[i] ?? '';
        out += ch;
        if (ch === '\\' && quote !== '`') {
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
    if (c === '{') {
      // Find the matching closing `}` at this depth, skipping nested
      // string literals. LogQL/TraceQL brace blocks don't nest, so a
      // flat counter suffices.
      let depth = 1;
      let j = i + 1;
      let innerEnd = -1;
      while (j < expr.length) {
        const ch = expr[j] ?? '';
        if (ch === '"' || ch === "'" || ch === '`') {
          const q = ch;
          j++;
          while (j < expr.length) {
            const sc = expr[j] ?? '';
            if (sc === '\\' && q !== '`') {
              j++;
              if (j < expr.length) j++;
              continue;
            }
            if (sc === q) {
              j++;
              break;
            }
            j++;
          }
          continue;
        }
        if (ch === '{') {
          depth++;
          j++;
          continue;
        }
        if (ch === '}') {
          depth--;
          if (depth === 0) {
            innerEnd = j;
            break;
          }
          j++;
          continue;
        }
        j++;
      }
      if (innerEnd === -1) {
        // Unbalanced — copy the `{` and continue. The expression is
        // malformed; the caller will see whatever the parser does
        // with it.
        out += c;
        i++;
        continue;
      }
      const inner = expr.slice(i + 1, innerEnd);
      out += `{${rewriteInner(inner)}}`;
      i = innerEnd + 1;
      continue;
    }
    out += c;
    i++;
  }
  return out;
}

// Inject `matcher` into the comma-separated contents of a
// LogQL/PromQL brace block. Idempotent on `key`: if any existing
// matcher in the block already constrains `key`, returns `inner`
// unchanged.
function injectIntoCommaSeparatedBlock(
  inner: string,
  key: string,
  matcher: string,
): string {
  if (innerHasMatcherForKey(inner, key)) return inner;
  const trimmed = inner.trim();
  if (trimmed === '') return matcher;
  // Preserve trailing whitespace that some LogQL examples carry
  // between matchers and the closing brace (e.g. `{a="b"} `). The
  // typical compact form has no trailing space.
  return `${inner},${matcher}`;
}

// Inject `matcher` into the `&&`-separated contents of a TraceQL
// spanset. Idempotent on `key`: if any existing matcher in the
// spanset already constrains `key`, returns `inner` unchanged.
function injectIntoSpansetBlock(
  inner: string,
  key: string,
  matcher: string,
): string {
  if (innerHasMatcherForKey(inner, key)) return inner;
  const trimmed = inner.trim();
  if (trimmed === '') return ` ${matcher} `;
  // Preserve the leading/trailing pad style that TraceQL examples
  // commonly use (`{ status = error }`). When the original block was
  // pad-free (`{status=error}`), we still emit a single space on
  // either side of the `&&` for readability; the TraceQL parser
  // accepts both forms.
  const leading = /^\s*/.exec(inner)?.[0] ?? '';
  const trailing = /\s*$/.exec(inner)?.[0] ?? '';
  return `${leading}${trimmed} && ${matcher}${trailing === '' ? ' ' : trailing}`;
}

// True iff `inner` (the body of a brace block) already constrains
// `key` via any matcher operator. Shared by both injection paths.
//
// The boundary class admits: start-of-block, whitespace, `,`, `{`,
// and `&` (the second `&` of `&&` is whitespace-padded in idiomatic
// TraceQL, but a packed form `a=b&&c=d` is also valid).
function innerHasMatcherForKey(inner: string, key: string): boolean {
  const re = new RegExp(
    `(?:^|[\\s,{&])${escapeRegex(key)}\\s*(=~|!~|!=|>=|<=|=|>|<)`,
  );
  return re.test(inner);
}
