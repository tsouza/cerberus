// forbid-parity-duplicate-samples.mjs — a parity-enrolled TXTAR fixture must
// not seed two metric samples that share one (series, timestamp) and carry
// DIFFERENT payloads.
//
// # WHY THIS EXISTS
//
// A fixture carrying `-- parity --` is answered twice: once by cerberus's own
// emitted SQL against chDB, and once by the real upstream engine over the same
// seeded data (test/spec/parity.go). The two answers must agree. That contract
// is only meaningful where both engines PROMISE an answer.
//
// Duplicate metric samples are where the promise runs out. Prometheus's TSDB
// appender (test/spec/parityoracle/promql/oracle.go feeds a real
// `teststorage` head) stores at most ONE sample per (series, timestamp): a
// second sample at an already-occupied timestamp is dropped at commit, and
// WHICH one survives is the ingestion order — a fact about the appender, not
// about PromQL. ClickHouse stores both rows, and which one a cerberus lowering
// surfaces depends on the emitted shape: the array-fold path's
// `arraySort(groupArray((ts, value)))` orders ties by VALUE, the sorted-slab
// over_time path sums both, and the rate family deduplicates by design.
//
// So a fixture that both enrols for parity AND seeds a differing-value
// duplicate asserts two things that cannot both hold: "every sample counts
// individually" (cerberus) and "matches the reference" (which kept one,
// chosen by ingestion order). The parity assertion is not testing cerberus, it
// is testing an accident — and it goes red the moment either side's tie-break
// shifts. Three such fixtures reached main and one of them was a live red lane
// (cerberus issue #2905); a fourth had already been fixed in passing without
// the class ever being closed.
//
// # WHY IDENTICAL-VALUE DUPLICATES ARE NOT A VIOLATION
//
// A duplicate whose rows carry the SAME payload still means Prometheus keeps
// one row and ClickHouse keeps two, so a counting reducer can still diverge —
// but that divergence has a RIGHT answer (the reference's), and cerberus can
// be made to match it by deduplicating. That is an ordinary bug to fix at the
// source, which is exactly what `increase_duplicate_timestamp_dedup.txtar`
// pins: it seeds an identical-value duplicate, stays enrolled, and passes
// because the rate family deduplicates. Failing it here would delete the only
// fixture proving that contract against a real reference engine.
//
// A DIFFERING-value duplicate has no right answer to converge on. That is the
// line this gate draws, and it is a line about the seed, not about the
// operator: it is checkable without knowing which lowering the fixture selects.
//
// # SCOPE
//
// The hazard is metric-shaped, and the gate says so structurally rather than
// by naming tables: a seeded table participates when its own DDL declares BOTH
// a `MetricName` and a `TimeUnix` column — the OTel-CH metric contract
// internal/schema/otel.go fixes. Log lines (otel_logs) and spans (otel_traces)
// are not samples keyed by (series, timestamp): Loki keeps every line at a
// timestamp and Tempo keys spans by span id, so neither reference store
// collapses them and neither can produce this divergence. All three head
// corpora are scanned regardless, so the day a LogQL or TraceQL fixture seeds
// a metric table it is covered without a change here.
//
// Rows are grouped ACROSS the seed's metric tables, not per table, because a
// `merge(otel_metrics_gauge|otel_metrics_sum)` scan reads them as one series
// universe and the reference oracle folds them into one label set — as does a
// native-histogram row, which reaches Prometheus under the BARE metric name
// carrying a histogram-valued sample.
//
// The one split is the CLASSIC histogram, and it is read off the DDL rather
// than off a table name: a table declaring `ExplicitBounds` is expanded by
// test/spec/parity_chdb.go's readSeededClassicHistograms into `<name>_bucket`,
// `<name>_count` and `<name>_sum` series, a namespace of its own that can
// never collide with a bare-named sample. `pinned_name_selector_no_histogram_
// fanout.txtar` is exactly that shape — a `up` gauge row and a decoy `up`
// classic-histogram row at one timestamp — and it is correct, enrolled and
// green, so conflating the two families would fail a good fixture.
//
// # KNOWN BLIND SPOT
//
// A seed may fill a metric table from a generated `INSERT INTO ... SELECT`
// rather than a literal `VALUES` list (`quantile_over_time_p95_exact_interp
// .txtar` seeds 10 000 rows from `numbers()`). Those rows exist only once the
// statement runs, so no static scan can group them. The gate counts such
// statements and reports the count in its pass notice rather than pretending
// the corpus was fully inspected — a divergence there still surfaces as a red
// round-trip lane, which is how this class was found in the first place.
//
// # ENV CONTRACT
//   REPO_ROOT — repository root the fixture directories resolve against.
//               Default `process.cwd()`.
//   SPEC_DIRS — comma-separated, repo-relative fixture directories to scan.
//               Default `test/spec/promql,test/spec/logql,test/spec/traceql`.
//
// Exit codes: 0 = clean; 1 = at least one violation, or a vacuous scan.

import { readFileSync, readdirSync } from 'node:fs';
import { isAbsolute, join, relative } from 'node:path';
import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';

/** The TXTAR section that enrols a fixture against a real reference engine. */
const PARITY_SECTION = 'parity';

/** The TXTAR section holding the fixture's DDL + INSERT statements. */
const SEED_SECTION = 'seed';

/** The metric-identity column of the OTel-CH metric tables. */
const METRIC_NAME_COLUMN = 'MetricName';

/** The sample-timestamp column of the OTel-CH metric tables. */
const TIMESTAMP_COLUMN = 'TimeUnix';

/**
 * The column whose presence makes a metric table a CLASSIC histogram, whose
 * rows the reference engine expands into a `_bucket`/`_count`/`_sum` series
 * namespace instead of a bare-named sample. See this file's SCOPE note.
 */
const CLASSIC_HISTOGRAM_COLUMN = 'ExplicitBounds';

/** Fixture directories scanned when SPEC_DIRS is unset. */
const DEFAULT_SPEC_DIRS = ['test/spec/promql', 'test/spec/logql', 'test/spec/traceql'];

/**
 * A declared column type that carries part of the SERIES IDENTITY rather than
 * the sample payload: the label maps (`Attributes`, `ResourceAttributes`) and
 * the string-valued identity columns (`MetricName`, `ServiceName`). Everything
 * else a metric table declares — `Value`, `Count`, `Sum`, `BucketCounts`,
 * `ExplicitBounds`, `Scale`, the exponential-histogram bucket arrays,
 * `AggregationTemporality` — is payload, and two rows differing in any of them
 * at one (series, timestamp) is the violation.
 */
const IDENTITY_TYPE = /^(?:Map\s*\(|String\b|LowCardinality\s*\(\s*String)/i;

/** A ClickHouse datetime constructor whose first argument is the instant. */
const DATETIME_CALL = /^toDateTime(?:64)?\s*\(/i;

/**
 * Splits TXTAR text into its named sections. The leading comment block before
 * the first `-- name --` marker is not a section and is dropped.
 */
export function splitSections(text) {
  const sections = new Map();
  let current = null;
  const buffer = [];
  const flush = () => {
    if (current !== null) sections.set(current, buffer.join('\n'));
    buffer.length = 0;
  };
  for (const line of text.split('\n')) {
    const marker = /^--\s+(.+?)\s+--\s*$/.exec(line);
    if (marker) {
      flush();
      current = marker[1];
      continue;
    }
    if (current !== null) buffer.push(line);
  }
  flush();
  return sections;
}

/**
 * Splits `text` on top-level `separator`, ignoring separators nested inside
 * parentheses, brackets or single-quoted literals. ClickHouse escapes a quote
 * inside a literal as `\'`; `''` never appears in this corpus but is handled
 * for free because the closing quote simply reopens.
 */
export function splitTopLevel(text, separator = ',') {
  const parts = [];
  let depth = 0;
  let quoted = false;
  let start = 0;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (quoted) {
      if (ch === '\\') i++;
      else if (ch === "'") quoted = false;
      continue;
    }
    if (ch === "'") quoted = true;
    else if (ch === '(' || ch === '[') depth++;
    else if (ch === ')' || ch === ']') depth--;
    else if (ch === separator && depth === 0) {
      parts.push(text.slice(start, i));
      start = i + 1;
    }
  }
  parts.push(text.slice(start));
  return parts;
}

/**
 * Returns the index just past the `(` ... `)` group that opens at `open`,
 * or -1 when the group is unterminated.
 */
function matchParen(text, open) {
  let depth = 0;
  let quoted = false;
  for (let i = open; i < text.length; i++) {
    const ch = text[i];
    if (quoted) {
      if (ch === '\\') i++;
      else if (ch === "'") quoted = false;
      continue;
    }
    if (ch === "'") quoted = true;
    else if (ch === '(') depth++;
    else if (ch === ')') {
      depth--;
      if (depth === 0) return i + 1;
    }
  }
  return -1;
}

/**
 * Every `CREATE TABLE` in `seed`, as table name -> declared columns. A column
 * is `{ name, type }`, with `DEFAULT`/`CODEC`/`MATERIALIZED` tails trimmed off
 * the type so the identity/payload classification reads the declared type
 * alone. Non-column body entries (index and constraint clauses) carry no
 * recognisable `<name> <type>` pair and are dropped.
 */
export function parseTables(seed) {
  const tables = new Map();
  const head = /CREATE\s+(?:OR\s+REPLACE\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z_0-9]*)\s*\(/gi;
  for (const m of seed.matchAll(head)) {
    const open = m.index + m[0].length - 1;
    const close = matchParen(seed, open);
    if (close < 0) continue;
    const columns = [];
    for (const entry of splitTopLevel(seed.slice(open + 1, close - 1))) {
      const decl = entry.trim();
      if (decl === '') continue;
      const parts = /^([A-Za-z_][A-Za-z_0-9]*)\s+(.+)$/s.exec(decl);
      if (!parts) continue;
      if (/^(?:INDEX|CONSTRAINT|PRIMARY|PROJECTION)$/i.test(parts[1])) continue;
      const type = parts[2].split(/\s+(?:DEFAULT|CODEC|MATERIALIZED|ALIAS|TTL)\b/i)[0].trim();
      columns.push({ name: parts[1], type });
    }
    tables.set(m[1], columns);
  }
  return tables;
}

/**
 * Every `INSERT INTO ... VALUES` row in `seed`, as
 * `{ table, columns, cells }` where `columns` is the effective column list
 * (explicit when the statement names one, the table's DDL order otherwise)
 * and `cells` are the raw literal texts, positionally aligned.
 */
export function parseInserts(seed, tables) {
  const rows = [];
  const head = /INSERT\s+INTO\s+([A-Za-z_][A-Za-z_0-9]*)\s*(\([^)]*\))?\s*VALUES/gi;
  for (const m of seed.matchAll(head)) {
    const table = m[1];
    const declared = tables.get(table);
    const columns = m[2]
      ? splitTopLevel(m[2].slice(1, -1)).map((c) => c.trim())
      : (declared ?? []).map((c) => c.name);
    let cursor = m.index + m[0].length;
    // Consume the comma-separated tuple list up to the statement terminator.
    for (;;) {
      while (cursor < seed.length && /[\s,]/.test(seed[cursor])) cursor++;
      if (seed[cursor] !== '(') break;
      const close = matchParen(seed, cursor);
      if (close < 0) break;
      const cells = splitTopLevel(seed.slice(cursor + 1, close - 1)).map((c) => c.trim());
      rows.push({ table, columns, cells });
      cursor = close;
    }
  }
  return rows;
}

/**
 * How many statements fill a metric-shaped table from something other than a
 * literal `VALUES` list — an `INSERT ... SELECT`, whose rows do not exist
 * until the statement runs. See this file's KNOWN BLIND SPOT note.
 */
export function opaqueMetricInserts(seed, tables) {
  let count = 0;
  const head = /INSERT\s+INTO\s+([A-Za-z_][A-Za-z_0-9]*)\s*(?:\([^)]*\))?\s*([A-Za-z]+)/gi;
  for (const m of seed.matchAll(head)) {
    const declared = tables.get(m[1]);
    if (!declared || !isMetricTable(declared)) continue;
    if (!/^VALUES$/i.test(m[2])) count++;
  }
  return count;
}

/**
 * Canonical text for one seeded literal, so two spellings of one value are one
 * value. Numeric literals compare NUMERICALLY (`40` and `40.0` are the same
 * sample), and a `toDateTime64('...', 9)` compares on the instant it names
 * rather than on the call's own punctuation.
 */
export function normalizeCell(raw, { timestamp = false } = {}) {
  const text = raw.trim().replace(/\s+/g, ' ');
  if (timestamp && DATETIME_CALL.test(text)) {
    const literal = /'((?:[^'\\]|\\.)*)'/.exec(text);
    if (literal) return `@${literal[1]}`;
  }
  if (text !== '' && Number.isFinite(Number(text))) return `#${Number(text)}`;
  return text;
}

/** Reports whether a table's DDL declares the OTel-CH metric-sample shape. */
export function isMetricTable(columns) {
  const names = new Set(columns.map((c) => c.name));
  return names.has(METRIC_NAME_COLUMN) && names.has(TIMESTAMP_COLUMN);
}

/**
 * The reference-side SERIES NAMESPACE a metric table's rows land in. Rows in
 * different namespaces never describe one series, so they are grouped apart.
 */
export function seriesNamespace(columns) {
  return columns.some((c) => c.name === CLASSIC_HISTOGRAM_COLUMN) ? 'classic-histogram' : 'sample';
}

/**
 * Every differing-value duplicate a seed carries, as
 * `{ series, timestamp, payloads }`. `payloads` holds the distinct payload
 * renderings found at that one (series, timestamp).
 *
 * Returns `{ duplicates, metricRows, opaqueInserts }`; `metricRows` is how many
 * metric-shaped rows the seed contributed, which the caller uses to tell
 * "clean" apart from "nothing was looked at".
 */
export function duplicateSamples(seed) {
  const tables = parseTables(seed);
  const groups = new Map();
  const misaligned = [];
  let metricRows = 0;

  for (const row of parseInserts(seed, tables)) {
    const declared = tables.get(row.table);
    if (!declared || !isMetricTable(declared)) continue;

    // A tuple whose arity does not match its column list cannot be read
    // column-wise at all, and ClickHouse would reject the statement, so this
    // means the DDL or VALUES parse above went wrong. Reported rather than
    // truncated silently: a misaligned row would be compared against the
    // wrong columns and could hide a duplicate or invent one.
    if (row.cells.length !== row.columns.length) {
      misaligned.push(`${row.table}: ${row.columns.length} column(s), ${row.cells.length} value(s)`);
      continue;
    }
    const typeOf = new Map(declared.map((c) => [c.name, c.type]));

    const identity = [];
    const payload = [];
    for (let i = 0; i < row.columns.length && i < row.cells.length; i++) {
      const name = row.columns[i];
      const type = typeOf.get(name);
      if (type === undefined) continue;
      if (name === TIMESTAMP_COLUMN) {
        identity.push([name, normalizeCell(row.cells[i], { timestamp: true })]);
      } else if (IDENTITY_TYPE.test(type)) {
        identity.push([name, normalizeCell(row.cells[i])]);
      } else {
        payload.push([name, normalizeCell(row.cells[i])]);
      }
    }
    if (identity.length === 0) continue;
    metricRows++;

    identity.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    payload.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    const key = JSON.stringify([seriesNamespace(declared), identity]);
    const rendered = payload.map(([n, v]) => `${n}=${v}`).join(' ');
    if (!groups.has(key)) groups.set(key, { identity, payloads: new Set() });
    groups.get(key).payloads.add(rendered);
  }

  const duplicates = [];
  for (const { identity, payloads } of groups.values()) {
    if (payloads.size < 2) continue;
    const fields = new Map(identity);
    duplicates.push({
      series: identity
        .filter(([n]) => n !== TIMESTAMP_COLUMN)
        .map(([n, v]) => `${n}=${v}`)
        .join(' '),
      timestamp: (fields.get(TIMESTAMP_COLUMN) ?? '').replace(/^@/, ''),
      payloads: [...payloads].sort(),
    });
  }
  return { duplicates, metricRows, misaligned, opaqueInserts: opaqueMetricInserts(seed, tables) };
}

/**
 * Scans one fixture's text. Returns null when the fixture is not in scope
 * (unenrolled, or carrying no seed), and a report otherwise.
 */
export function scanFixture(text) {
  const sections = splitSections(text);
  if (!sections.has(PARITY_SECTION)) return null;
  const seed = sections.get(SEED_SECTION);
  if (seed === undefined) return null;
  return duplicateSamples(seed);
}

/** Every `*.txtar` directly under `dir`, as repo-relative paths, sorted. */
function fixturesIn(root, dir) {
  const abs = isAbsolute(dir) ? dir : join(root, dir);
  return readdirSync(abs, { withFileTypes: true })
    .filter((e) => e.isFile() && e.name.endsWith('.txtar'))
    .map((e) => relative(root, join(abs, e.name)))
    .sort();
}

export function scan({ root = process.cwd(), dirs = DEFAULT_SPEC_DIRS } = {}) {
  let enrolled = 0;
  let metricSeeded = 0;
  let opaqueInserts = 0;
  const violations = [];
  const misaligned = [];
  for (const dir of dirs) {
    for (const path of fixturesIn(root, dir)) {
      const report = scanFixture(readFileSync(join(root, path), 'utf8'));
      if (report === null) continue;
      enrolled++;
      if (report.metricRows > 0) metricSeeded++;
      opaqueInserts += report.opaqueInserts;
      for (const problem of report.misaligned) misaligned.push({ path, problem });
      for (const duplicate of report.duplicates) violations.push({ path, ...duplicate });
    }
  }
  return { enrolled, metricSeeded, opaqueInserts, misaligned, violations };
}

function main() {
  const root = process.env.REPO_ROOT || process.cwd();
  const dirs = (process.env.SPEC_DIRS || DEFAULT_SPEC_DIRS.join(','))
    .split(',')
    .map((d) => d.trim())
    .filter(Boolean);

  const { enrolled, metricSeeded, opaqueInserts, misaligned, violations } = scan({ root, dirs });

  // Fail closed on a row this scan could not read column-wise: it would be
  // compared against the wrong columns, which can hide a duplicate as easily
  // as invent one. Zero such rows today across the whole corpus, so a hit
  // means the parser stopped understanding a seed shape.
  for (const m of misaligned) {
    error(
      `${m.path}: a seeded metric row could not be aligned to its column list (${m.problem}). ` +
        'ClickHouse would reject such a statement, so this is a parse failure in ' +
        'forbid-parity-duplicate-samples.mjs, not a fixture defect — fix parseTables/parseInserts.',
      { file: m.path },
    );
  }
  if (misaligned.length > 0) return 1;

  // A gate that passes because it parsed nothing reports the same green as a
  // satisfied one. Metric-shaped seeds are what this scan is ABOUT, so finding
  // none means the parser stopped understanding the corpus, not that the
  // corpus became clean.
  if (metricSeeded === 0) {
    error(
      'forbid-parity-duplicate-samples: scanned ' +
        `${enrolled} parity-enrolled fixture(s) and found no metric-shaped seed at all. ` +
        'The scan is broken, not the corpus — check parseTables/parseInserts against ' +
        `${dirs.join(', ')}.`,
    );
    return 1;
  }

  for (const v of violations) {
    error(
      `${v.path}: seeds ${v.series} twice at ${v.timestamp} with differing payloads ` +
        `(${v.payloads.join(' vs ')}), while enrolling for parity against a reference engine. ` +
        'Which of two samples at one (series, timestamp) survives is implementation-defined on ' +
        'both sides — Prometheus keeps the first ingested, ClickHouse keeps both — so the two ' +
        'claims cannot both hold. Either drop the duplicate and stay enrolled, or keep the ' +
        'duplicate and replace `-- parity --` with a `-- parity_exempt --` section carrying ' +
        'reason `duplicate-timestamp-seed` (test/spec/parity_exempt.go), pinning the contract ' +
        'with a cerberus-vs-cerberus differential instead.',
      { file: v.path },
    );
  }

  if (violations.length > 0) {
    log(`forbid-parity-duplicate-samples: ${violations.length} violation(s) across ${metricSeeded} metric-seeded fixture(s).`);
    return 1;
  }

  notice(
    `forbid-parity-duplicate-samples: ${enrolled} parity-enrolled fixture(s) scanned, ` +
      `${metricSeeded} carrying a metric-shaped seed; no differing-value duplicate timestamps. ` +
      `${opaqueInserts} generated INSERT ... SELECT statement(s) were not statically inspectable ` +
      '(see this script\'s KNOWN BLIND SPOT note).',
  );
  return 0;
}

if (process.argv[1] && import.meta.url === `file://${process.argv[1]}`) {
  process.exit(main());
}
