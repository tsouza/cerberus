/**
 * Validity-oracle library — level-3 ("valid-data") frame contracts.
 *
 * The three-level oracle for every Grafana consumption surface is:
 *
 *   1. shows-data  — the response carries ≥ 1 series / stream / trace
 *                    (enforced by the sweeps via helpers/expectations.ts).
 *   2. no-errors   — 2xx status, no tunneled envelope error.
 *   3. valid-data  — the *contents* of the frames honour the wire
 *                    contract of the head that produced them. That is
 *                    this module.
 *
 * Every validator here is a PURE function over a captured response
 * body: no I/O, no Playwright fixtures, fully unit-testable
 * (helpers-validity.spec.ts runs them stack-free). Each returns a
 * `string[]` of violations — empty means valid — so sweeps can
 * AGGREGATE findings across panels rather than throw on the first.
 *
 * The validators deliberately re-implement nothing that already
 * exists: by-key extraction lives in helpers/query-shape.ts
 * (`expectedByKeysForDsType`), and the label-key set diff is shared
 * with helpers/assertions.ts (`diffLabelKeys`).
 */

import { diffLabelKeys } from './assertions.js';

/** Prometheus API result types the validators understand. */
export type PromResultType = 'matrix' | 'vector' | 'scalar' | 'string';

/**
 * Expression classes that constrain the sample-value domain.
 *
 *   - 'counter-rate'  — rate() / increase() / irate() / resets() /
 *     count_over_time()-style counter shapes: every value must be
 *     >= 0 (a negative rate means the engine mis-handled a counter
 *     reset or emitted a fabricated delta).
 *   - 'unconstrained' — no domain constraint beyond finiteness.
 */
export type ExprClass = 'counter-rate' | 'unconstrained';

/**
 * Per-query context the caller derives from the request it fired.
 *
 * `allowNaN` exists because a small set of PromQL expression classes
 * define NaN as a legitimate result (e.g. histogram_quantile over
 * empty buckets, 0/0 divisions). The CALLER opts in per expression;
 * the default is the strict contract: every sample must be finite.
 */
export type ValidityContext = {
  /** Inclusive lower bound of the query window, unix seconds. */
  fromSec: number;
  /** Inclusive upper bound of the query window, unix seconds. */
  toSec: number;
  /**
   * Range-query step in seconds. When set, every matrix timestamp
   * must be step-aligned to the `fromSec` anchor (Prometheus range
   * semantics: samples land on `start + k*step`).
   */
  stepSec?: number;
  /**
   * The resultType the endpoint contract promises — `query_range`
   * answers `matrix`, instant `query` answers `vector` (or `scalar`
   * for scalar expressions). Unset = any resultType accepted.
   */
  expectResultType?: PromResultType;
  /** Accept NaN/±Inf sample values (see type doc). Default: reject. */
  allowNaN?: boolean;
  /** Domain constraint derived from the expression shape. */
  exprClass?: ExprClass;
  /**
   * When non-empty: every result series' label keyset (minus
   * `__name__`) must equal EXACTLY this set — the post-aggregation
   * contract of `sum by (k1, k2) (…)`. Derive via
   * `expectedByKeysForDsType` from helpers/query-shape.ts.
   */
  byKeys?: string[];
  /** Free-form prefix for violation strings (panel / target id). */
  where?: string;
};

// ---------------------------------------------------------------------------
// Shared numeric / timestamp primitives
// ---------------------------------------------------------------------------

// Strict float token — what a well-formed finite Prometheus sample
// value string looks like. `Number('')` is 0 and `Number('1x')` is
// NaN, so a regex gate distinguishes "garbage string" from the
// well-known non-finite tokens below.
const FLOAT_RE = /^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$/;
const NONFINITE_RE = /^([+-]?Inf(inity)?|NaN)$/;

type ParsedSample =
  | { kind: 'finite'; value: number }
  | { kind: 'nonfinite'; token: string }
  | { kind: 'garbage'; token: string };

/**
 * Parse a Prometheus-style sample value. Accepts the wire encodings
 * the upstream APIs emit: a JSON string (`"1.5"`, `"NaN"`, `"+Inf"`)
 * or a raw JSON number (Tempo metrics emit float64 directly).
 */
export function parseSampleValue(raw: unknown): ParsedSample {
  if (typeof raw === 'number') {
    return Number.isFinite(raw)
      ? { kind: 'finite', value: raw }
      : { kind: 'nonfinite', token: String(raw) };
  }
  if (typeof raw === 'string') {
    if (FLOAT_RE.test(raw)) return { kind: 'finite', value: Number(raw) };
    if (NONFINITE_RE.test(raw)) return { kind: 'nonfinite', token: raw };
    return { kind: 'garbage', token: raw };
  }
  return { kind: 'garbage', token: JSON.stringify(raw) };
}

function checkSampleValue(
  raw: unknown,
  ctx: ValidityContext,
  where: string,
  out: string[],
): void {
  const parsed = parseSampleValue(raw);
  if (parsed.kind === 'garbage') {
    out.push(`${where}: sample value ${JSON.stringify(parsed.token)} is not numeric`);
    return;
  }
  if (parsed.kind === 'nonfinite') {
    if (!ctx.allowNaN) {
      out.push(
        `${where}: non-finite sample value "${parsed.token}" (NaN/Inf rejected; ` +
          `set allowNaN only for expr classes where NaN is the defined result)`,
      );
    }
    return;
  }
  if (ctx.exprClass === 'counter-rate' && parsed.value < 0) {
    out.push(
      `${where}: counter-shaped expression produced negative value ${parsed.value}`,
    );
  }
}

// Step-alignment float tolerance. Prom timestamps are second-precision
// floats; 1ms slack absorbs the float noise without admitting a real
// misalignment (the smallest step the dashboards use is 15s).
const STEP_EPSILON_SEC = 1e-3;

function checkTimestampSec(
  ts: unknown,
  ctx: ValidityContext,
  stepAligned: boolean,
  where: string,
  out: string[],
): void {
  if (typeof ts !== 'number' || !Number.isFinite(ts)) {
    out.push(`${where}: timestamp ${JSON.stringify(ts)} is not a finite number`);
    return;
  }
  if (ts < ctx.fromSec || ts > ctx.toSec) {
    out.push(
      `${where}: timestamp ${ts} outside query window [${ctx.fromSec}, ${ctx.toSec}]`,
    );
  }
  if (stepAligned && ctx.stepSec !== undefined && ctx.stepSec > 0) {
    const offset = (ts - ctx.fromSec) % ctx.stepSec;
    const dist = Math.min(Math.abs(offset), Math.abs(ctx.stepSec - Math.abs(offset)));
    if (dist > STEP_EPSILON_SEC) {
      out.push(
        `${where}: timestamp ${ts} not step-aligned to anchor ${ctx.fromSec} (step=${ctx.stepSec}s)`,
      );
    }
  }
}

function prefix(ctx: ValidityContext): string {
  return ctx.where ? `${ctx.where}` : '';
}

function loc(ctx: ValidityContext, detail: string): string {
  const p = prefix(ctx);
  return p === '' ? detail : `${p} ${detail}`;
}

// ---------------------------------------------------------------------------
// Prometheus
// ---------------------------------------------------------------------------

type PromSeries = {
  metric?: Record<string, string>;
  values?: unknown[];
  value?: unknown;
};

/**
 * Validate a Prometheus `/api/v1/query` / `/api/v1/query_range`
 * response body against the level-3 frame contract:
 *
 *   - envelope `status === 'success'`;
 *   - `data.resultType` matches the endpoint expectation;
 *   - every sample value parses numeric — NaN/Inf rejected unless
 *     `ctx.allowNaN`;
 *   - every timestamp within `[fromSec, toSec]` inclusive and (for
 *     matrix results) step-aligned to `ctx.stepSec`;
 *   - `ctx.exprClass === 'counter-rate'` ⇒ all values >= 0;
 *   - histogram frames (`le` label present): `le` values ascending
 *     and per-timestamp cumulative counts non-decreasing across the
 *     ascending buckets;
 *   - `ctx.byKeys` non-empty ⇒ every series' label keyset (minus
 *     `__name__`) equals exactly that set.
 */
export function validatePromResponse(
  body: unknown,
  ctx: ValidityContext,
): string[] {
  const out: string[] = [];
  const env = promEnvelope(body, ctx, out);
  if (env === null) return out;
  const { resultType, result } = env;

  if (
    ctx.expectResultType !== undefined &&
    resultType !== ctx.expectResultType
  ) {
    out.push(
      loc(ctx, `envelope: resultType "${resultType}" (want "${ctx.expectResultType}")`),
    );
  }

  if (resultType === 'scalar' || resultType === 'string') {
    // data.result is a single [ts, value] pair.
    const pair = result as unknown;
    if (!Array.isArray(pair) || pair.length !== 2) {
      out.push(loc(ctx, `scalar: result is not a [ts, value] pair`));
      return out;
    }
    checkTimestampSec(pair[0], ctx, false, loc(ctx, 'scalar'), out);
    if (resultType === 'scalar') {
      checkSampleValue(pair[1], ctx, loc(ctx, 'scalar'), out);
    }
    return out;
  }

  if (!Array.isArray(result)) {
    out.push(loc(ctx, `envelope: data.result is not an array`));
    return out;
  }
  const series = result as PromSeries[];

  for (const [i, s] of series.entries()) {
    const sWhere = loc(ctx, `series[${i}]`);
    if (resultType === 'matrix') {
      for (const pair of asPairs(s.values, sWhere, out)) {
        checkTimestampSec(pair[0], ctx, true, sWhere, out);
        checkSampleValue(pair[1], ctx, sWhere, out);
      }
    } else {
      // vector
      const pair = s.value;
      if (!Array.isArray(pair) || pair.length !== 2) {
        out.push(`${sWhere}: vector sample is not a [ts, value] pair`);
      } else {
        checkTimestampSec(pair[0], ctx, false, sWhere, out);
        checkSampleValue(pair[1], ctx, sWhere, out);
      }
    }

    if (ctx.byKeys !== undefined && ctx.byKeys.length > 0) {
      const keys = Object.keys(s.metric ?? {}).filter((k) => k !== '__name__');
      const diff = diffLabelKeys(keys, ctx.byKeys);
      if (diff.missing.length > 0 || diff.extra.length > 0) {
        out.push(
          `${sWhere}: label keyset [${keys.sort().join(', ')}] != by-keys ` +
            `[${[...ctx.byKeys].sort().join(', ')}] ` +
            `(missing=[${diff.missing.join(', ')}] extra=[${diff.extra.join(', ')}])`,
        );
      }
    }
  }

  out.push(...validateHistogramBuckets(series, ctx));
  return out;
}

function promEnvelope(
  body: unknown,
  ctx: ValidityContext,
  out: string[],
): { resultType: string; result: unknown } | null {
  if (body === null || typeof body !== 'object') {
    out.push(loc(ctx, `envelope: body is not a JSON object`));
    return null;
  }
  const b = body as Record<string, unknown>;
  if (b.status !== 'success') {
    out.push(
      loc(
        ctx,
        `envelope: status=${JSON.stringify(b.status)} (want "success")` +
          (typeof b.error === 'string' ? ` error=${b.error}` : ''),
      ),
    );
    return null;
  }
  const data = b.data;
  if (data === null || typeof data !== 'object') {
    out.push(loc(ctx, `envelope: data is not a JSON object`));
    return null;
  }
  const d = data as Record<string, unknown>;
  if (typeof d.resultType !== 'string') {
    out.push(loc(ctx, `envelope: data.resultType missing`));
    return null;
  }
  return { resultType: d.resultType, result: d.result };
}

function asPairs(
  values: unknown,
  where: string,
  out: string[],
): unknown[][] {
  if (!Array.isArray(values)) {
    out.push(`${where}: matrix series carries no values array`);
    return [];
  }
  const pairs: unknown[][] = [];
  for (const v of values) {
    if (!Array.isArray(v) || v.length !== 2) {
      out.push(`${where}: matrix sample is not a [ts, value] pair`);
      continue;
    }
    pairs.push(v);
  }
  return pairs;
}

// Cumulative-bucket float tolerance: rate() over adjacent buckets can
// produce sub-nano float noise where a higher bucket dips below a
// lower one without any real monotonicity break.
const CUMULATIVE_EPSILON = 1e-9;

/**
 * Histogram bucket-frame contract: among the series that carry an
 * `le` label, group by the remaining label set; within each group
 * the parsed `le` boundaries must be strictly ascending (after
 * sorting; a duplicate boundary is a violation) and, per timestamp,
 * the cumulative counts must be non-decreasing as `le` grows.
 */
function validateHistogramBuckets(
  series: PromSeries[],
  ctx: ValidityContext,
): string[] {
  const out: string[] = [];
  type Bucket = { le: number; leRaw: string; samples: Map<number, number> };
  const groups = new Map<string, Bucket[]>();

  for (const s of series) {
    const metric = s.metric ?? {};
    const leRaw = metric.le;
    if (leRaw === undefined) continue;
    const le = parseLe(leRaw);
    const groupKey = JSON.stringify(
      Object.entries(metric)
        .filter(([k]) => k !== 'le')
        .sort(([a], [b]) => a.localeCompare(b)),
    );
    const samples = new Map<number, number>();
    const pairs: unknown[] =
      s.values !== undefined ? (Array.isArray(s.values) ? s.values : []) : s.value !== undefined ? [s.value] : [];
    for (const p of pairs) {
      if (!Array.isArray(p) || p.length !== 2) continue;
      const parsed = parseSampleValue(p[1]);
      if (parsed.kind !== 'finite' || typeof p[0] !== 'number') continue;
      samples.set(p[0], parsed.value);
    }
    const bucket: Bucket = { le, leRaw: String(leRaw), samples };
    const existing = groups.get(groupKey);
    if (existing === undefined) groups.set(groupKey, [bucket]);
    else existing.push(bucket);
  }

  for (const [groupKey, buckets] of groups) {
    if (buckets.some((b) => Number.isNaN(b.le))) {
      const bad = buckets.filter((b) => Number.isNaN(b.le)).map((b) => b.leRaw);
      out.push(
        loc(ctx, `histogram ${groupKey}: unparseable le boundary [${bad.join(', ')}]`),
      );
      continue;
    }
    buckets.sort((a, b) => a.le - b.le);
    for (let i = 1; i < buckets.length; i++) {
      const prev = buckets[i - 1]!;
      const cur = buckets[i]!;
      if (cur.le === prev.le) {
        out.push(
          loc(
            ctx,
            `histogram ${groupKey}: duplicate le boundary "${cur.leRaw}" — le values must be ascending`,
          ),
        );
        continue;
      }
      for (const [ts, prevVal] of prev.samples) {
        const curVal = cur.samples.get(ts);
        if (curVal === undefined) continue;
        if (curVal < prevVal - CUMULATIVE_EPSILON) {
          out.push(
            loc(
              ctx,
              `histogram ${groupKey}: cumulative count decreasing at ts=${ts} ` +
                `(le="${prev.leRaw}" → ${prevVal}, le="${cur.leRaw}" → ${curVal})`,
            ),
          );
        }
      }
    }
  }
  return out;
}

function parseLe(raw: string): number {
  if (NONFINITE_RE.test(raw)) {
    return raw.startsWith('-') ? -Infinity : raw === 'NaN' ? NaN : Infinity;
  }
  return FLOAT_RE.test(raw) ? Number(raw) : NaN;
}

// ---------------------------------------------------------------------------
// Loki
// ---------------------------------------------------------------------------

/**
 * Severity vocabulary for the level-3 log contract. The
 * `detected_level` / `SeverityText` labels, when present on a
 * stream, must case-insensitively match one of these.
 */
export const LOG_SEVERITY_LEVELS = new Set([
  'TRACE',
  'DEBUG',
  'INFO',
  'WARN',
  'ERROR',
  'FATAL',
]);

/**
 * Validate a Loki `/loki/api/v1/query_range` (or instant `query`)
 * response body:
 *
 *   - envelope `status === 'success'` + recognised resultType;
 *   - streams: every log line's timestamp within the query window,
 *     severity label (detected_level / SeverityText, when present)
 *     within {TRACE, DEBUG, INFO, WARN, ERROR, FATAL} case-
 *     insensitively, and the line body non-empty;
 *   - metric results (matrix / vector): the same numeric + timestamp
 *     rules as the Prometheus validator.
 */
export function validateLokiResponse(
  body: unknown,
  ctx: ValidityContext,
): string[] {
  const out: string[] = [];
  const env = promEnvelope(body, ctx, out);
  if (env === null) return out;
  const { resultType, result } = env;

  if (resultType === 'matrix' || resultType === 'vector') {
    // Metric-producing LogQL — identical frame rules to Prometheus.
    // Re-wrap so the prom validator sees the envelope it expects and
    // the endpoint-expectation check still applies.
    return validatePromResponse(
      { status: 'success', data: { resultType, result } },
      ctx,
    );
  }
  if (resultType !== 'streams') {
    out.push(loc(ctx, `envelope: unrecognised loki resultType "${resultType}"`));
    return out;
  }
  if (ctx.expectResultType !== undefined) {
    // The caller asked for a metric shape but got streams.
    out.push(
      loc(ctx, `envelope: resultType "streams" (want "${ctx.expectResultType}")`),
    );
  }

  if (!Array.isArray(result)) {
    out.push(loc(ctx, `envelope: data.result is not an array`));
    return out;
  }
  const fromNs = ctx.fromSec * 1e9;
  const toNs = ctx.toSec * 1e9;
  for (const [i, entry] of (result as unknown[]).entries()) {
    const sWhere = loc(ctx, `stream[${i}]`);
    if (entry === null || typeof entry !== 'object') {
      out.push(`${sWhere}: not an object`);
      continue;
    }
    const e = entry as { stream?: Record<string, string>; values?: unknown[] };
    const labels = e.stream ?? {};

    const severity = labels.detected_level ?? labels.SeverityText;
    if (
      severity !== undefined &&
      !LOG_SEVERITY_LEVELS.has(severity.toUpperCase())
    ) {
      out.push(
        `${sWhere}: severity "${severity}" outside ` +
          `{${[...LOG_SEVERITY_LEVELS].join(', ')}} (case-insensitive)`,
      );
    }

    if (!Array.isArray(e.values)) {
      out.push(`${sWhere}: stream carries no values array`);
      continue;
    }
    for (const [j, v] of e.values.entries()) {
      const lWhere = `${sWhere} line[${j}]`;
      if (!Array.isArray(v) || v.length < 2) {
        out.push(`${lWhere}: not a [tsNano, line] pair`);
        continue;
      }
      const [tsRaw, line] = v as [unknown, unknown];
      if (typeof tsRaw !== 'string' || !/^\d+$/.test(tsRaw)) {
        out.push(`${lWhere}: timestamp ${JSON.stringify(tsRaw)} is not a nanosecond string`);
      } else {
        const tsNs = Number(tsRaw);
        if (tsNs < fromNs || tsNs > toNs) {
          out.push(
            `${lWhere}: timestamp ${tsRaw} outside query window ` +
              `[${ctx.fromSec}, ${ctx.toSec}] (seconds)`,
          );
        }
      }
      if (typeof line !== 'string' || line.length === 0) {
        out.push(`${lWhere}: log line body is empty`);
      }
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Tempo
// ---------------------------------------------------------------------------

// Canonical trace id — 32 lowercase hex chars (cerberus emits the
// zero-padded canonical form; see PR #656).
const TRACE_ID_RE = /^[0-9a-f]{32}$/;

const SPAN_KIND_STRINGS = new Set([
  'SPAN_KIND_UNSPECIFIED',
  'SPAN_KIND_INTERNAL',
  'SPAN_KIND_SERVER',
  'SPAN_KIND_CLIENT',
  'SPAN_KIND_PRODUCER',
  'SPAN_KIND_CONSUMER',
]);

/**
 * Validate a Tempo response body. The three response families the
 * head serves are dispatched on shape:
 *
 *   - search (`{traces: [...]}`)  — every traceID is 32-lowercase-hex
 *     (cerberus emits canonical zero-padded ids, PR #656), durations
 *     are >= 0, and startTimeUnixNano parses as a positive integer;
 *   - trace-by-id (`{batches: [...]}` / `{resourceSpans: [...]}`) —
 *     every span's parentSpanId is either empty or present in the
 *     trace's own span set, and span kind is within the OTLP enum;
 *   - metrics (`{series: [...]}`) — the same numeric + timestamp
 *     rules as the Prometheus validator (timestampMs within the
 *     window, values finite unless ctx.allowNaN).
 */
export function validateTempoResponse(
  body: unknown,
  ctx: ValidityContext,
): string[] {
  const out: string[] = [];
  if (body === null || typeof body !== 'object') {
    out.push(loc(ctx, `envelope: body is not a JSON object`));
    return out;
  }
  const b = body as Record<string, unknown>;
  if (Array.isArray(b.traces) || ('traces' in b && b.traces === null)) {
    validateTempoSearch(b, ctx, out);
    return out;
  }
  if (Array.isArray(b.batches) || Array.isArray(b.resourceSpans)) {
    validateTempoTrace(b, ctx, out);
    return out;
  }
  if (Array.isArray(b.series)) {
    validateTempoMetrics(b, ctx, out);
    return out;
  }
  out.push(
    loc(
      ctx,
      `envelope: unrecognised tempo response shape (no traces/batches/resourceSpans/series key)`,
    ),
  );
  return out;
}

function validateTempoSearch(
  b: Record<string, unknown>,
  ctx: ValidityContext,
  out: string[],
): void {
  const traces = Array.isArray(b.traces) ? b.traces : [];
  for (const [i, t] of (traces as unknown[]).entries()) {
    const tWhere = loc(ctx, `trace[${i}]`);
    if (t === null || typeof t !== 'object') {
      out.push(`${tWhere}: not an object`);
      continue;
    }
    const trace = t as Record<string, unknown>;
    if (typeof trace.traceID !== 'string' || !TRACE_ID_RE.test(trace.traceID)) {
      out.push(
        `${tWhere}: traceID ${JSON.stringify(trace.traceID)} is not ` +
          `32-lowercase-hex (canonical form per PR #656)`,
      );
    }
    if (trace.durationMs !== undefined) {
      if (typeof trace.durationMs !== 'number' || trace.durationMs < 0) {
        out.push(`${tWhere}: durationMs ${JSON.stringify(trace.durationMs)} is negative or non-numeric`);
      }
    }
    if (trace.startTimeUnixNano !== undefined) {
      const raw = trace.startTimeUnixNano;
      const okString = typeof raw === 'string' && /^\d+$/.test(raw) && raw !== '0';
      const okNumber = typeof raw === 'number' && Number.isFinite(raw) && raw > 0;
      if (!okString && !okNumber) {
        out.push(
          `${tWhere}: startTimeUnixNano ${JSON.stringify(raw)} does not parse as a positive integer`,
        );
      }
    }
  }
}

type RawSpan = Record<string, unknown>;

function validateTempoTrace(
  b: Record<string, unknown>,
  ctx: ValidityContext,
  out: string[],
): void {
  // Tempo serves the OTLP trace JSON under `batches` (tempopb) or
  // `resourceSpans` (raw OTLP); scopes nest under `scopeSpans` (or
  // the legacy `instrumentationLibrarySpans`).
  const batches = (
    Array.isArray(b.batches) ? b.batches : (b.resourceSpans as unknown[])
  ) as unknown[];
  const spans: RawSpan[] = [];
  for (const batch of batches) {
    if (batch === null || typeof batch !== 'object') continue;
    const bb = batch as Record<string, unknown>;
    const scopes = (
      Array.isArray(bb.scopeSpans)
        ? bb.scopeSpans
        : Array.isArray(bb.instrumentationLibrarySpans)
          ? bb.instrumentationLibrarySpans
          : []
    ) as unknown[];
    for (const scope of scopes) {
      if (scope === null || typeof scope !== 'object') continue;
      const ss = (scope as Record<string, unknown>).spans;
      if (!Array.isArray(ss)) continue;
      for (const s of ss) {
        if (s !== null && typeof s === 'object') spans.push(s as RawSpan);
      }
    }
  }
  if (spans.length === 0) {
    out.push(loc(ctx, `trace: no spans found in trace-by-id response`));
    return;
  }
  const spanIds = new Set<string>();
  for (const s of spans) {
    if (typeof s.spanId === 'string') spanIds.add(s.spanId);
  }
  for (const [i, s] of spans.entries()) {
    const sWhere = loc(ctx, `span[${i}]`);
    const parent = s.parentSpanId;
    if (
      parent !== undefined &&
      parent !== null &&
      parent !== '' &&
      !(typeof parent === 'string' && spanIds.has(parent))
    ) {
      out.push(
        `${sWhere}: parentSpanId ${JSON.stringify(parent)} not present in the trace's span set`,
      );
    }
    const kind = s.kind;
    const kindOk =
      kind === undefined ||
      (typeof kind === 'string' && SPAN_KIND_STRINGS.has(kind)) ||
      (typeof kind === 'number' && Number.isInteger(kind) && kind >= 0 && kind <= 5);
    if (!kindOk) {
      out.push(`${sWhere}: span kind ${JSON.stringify(kind)} outside the OTLP enum`);
    }
  }
}

function validateTempoMetrics(
  b: Record<string, unknown>,
  ctx: ValidityContext,
  out: string[],
): void {
  const fromMs = ctx.fromSec * 1000;
  const toMs = ctx.toSec * 1000;
  const series = b.series as unknown[];
  for (const [i, s] of series.entries()) {
    const sWhere = loc(ctx, `series[${i}]`);
    if (s === null || typeof s !== 'object') {
      out.push(`${sWhere}: not an object`);
      continue;
    }
    const samples = (s as Record<string, unknown>).samples;
    if (!Array.isArray(samples)) {
      out.push(`${sWhere}: series carries no samples array`);
      continue;
    }
    for (const [j, sample] of samples.entries()) {
      const pWhere = `${sWhere} sample[${j}]`;
      if (sample === null || typeof sample !== 'object') {
        out.push(`${pWhere}: not an object`);
        continue;
      }
      const p = sample as Record<string, unknown>;
      const tsMs = p.timestampMs;
      if (typeof tsMs !== 'number' || !Number.isFinite(tsMs)) {
        out.push(`${pWhere}: timestampMs ${JSON.stringify(tsMs)} is not a finite number`);
      } else if (tsMs < fromMs || tsMs > toMs) {
        out.push(
          `${pWhere}: timestampMs ${tsMs} outside query window ` +
            `[${fromMs}, ${toMs}]`,
        );
      }
      checkSampleValue(p.value, ctx, pWhere, out);
    }
  }
}
