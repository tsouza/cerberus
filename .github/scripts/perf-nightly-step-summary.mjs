// perf-nightly-step-summary.mjs — render the #2370 nightly lane's own
// per-sentinel verdict as a clear markdown table in the job's
// $GITHUB_STEP_SUMMARY, ahead of (and independent from) the raw `go test
// -v` log.
//
// Why this exists
// ----------------
// realch_perfnightly_integration_test.go's own doc comment explains that 2
// of the 4 sentinels are CALIBRATED to be rejected with HTTP 422 (issue
// #2429) — that rejection is the correct, expected outcome. cerberus's own
// engine.go logs a WARN-level "cerberus query failed" line for EVERY
// non-2xx query outcome unconditionally (docs/observability.md) — that is
// correct application logging, not a bug, and this script does not touch
// it. The actual gap a human hit reading a real nightly run
// (https://github.com/tsouza/cerberus/actions/runs/32555114292) was
// visibility: nothing in the run summarised "2 sentinels passed cleanly, 2
// were correctly rejected as designed, 0 regressed" before the raw log —
// a reader had to manually correlate each WARN line against sentinels.go's
// own ExpectedStatus field to tell an expected rejection apart from a real
// regression.
//
// Go is the single source of truth for the verdict: results.go's
// SentinelResult.Pass is computed once, in the same test code that
// performs the actual assertions (realch_perfnightly_integration_test.go).
// renderSummary() below only renders that JSON — it must never re-derive
// pass/fail from raw fields, or the two could silently disagree.
//
// Env contract:
//   PERF_NIGHTLY_RESULTS_JSON   path to the JSON results.go's
//                               writeResultsJSON produced (same variable
//                               name .github/workflows/perf-nightly.yml
//                               sets before the test step runs).
//
// Runs under `if: always()` after the test step, so it renders a verdict
// table even when TestPerfNightlyRealCH itself failed — that is precisely
// the case a clear summary matters most for. When the results file is
// missing entirely (the job died before writing anything — e.g. the
// ClickHouse container never started), this prints a plain notice instead
// of failing, the same escape-hatch shape compat-step-summary.mjs already
// uses for a missing compat-score.json: the "Fail job" semantics belong to
// the test step's own exit code, never to this housekeeping step.
//
// Exit codes: always 0 (housekeeping step; never gates — see above).

import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { appendStepSummary, log } from './lib/gh.mjs';

export function formatBytes(n) {
  if (n === undefined || n === null) return '—';
  const mib = n / (1024 * 1024);
  return `${mib.toFixed(1)} MiB`;
}

export function formatPct(n) {
  if (n === undefined || n === null) return '—';
  return `${n.toFixed(1)}%`;
}

// verdictCell renders one sentinel's own SentinelResult.Pass (Go's single
// source of truth, see the header) as a human verdict — it inspects the
// supporting fields ONLY to choose which failure reason to print, never to
// decide pass/fail itself.
export function verdictCell(sentinel) {
  if (sentinel.pass && sentinel.rejected) return '✅ correctly rejected (expected)';
  if (sentinel.pass) return '✅ pass';
  if (!sentinel.status_ok) {
    return `❌ FAIL — status ${sentinel.actual_status} (want ${sentinel.expected_status})`;
  }
  if (!sentinel.cap_ok) return '❌ FAIL — exceeds absolute cap-relative ceiling';
  if (sentinel.has_baseline && !sentinel.baseline_ok) return '❌ FAIL — exceeds committed baseline ceiling';
  return '❌ FAIL';
}

// renderSummary builds the full markdown block from a parsed
// NightlyResults document (results.go's JSON shape). Pure function — no
// env/fs access — so it is the one thing this module's own test exercises
// directly.
export function renderSummary(doc) {
  const sentinels = doc.sentinels || [];
  const headline = doc.all_pass
    ? `✅ all ${sentinels.length} sentinels passed`
    : `❌ ${sentinels.filter((s) => !s.pass).length} of ${sentinels.length} sentinel(s) regressed`;

  const rows = sentinels.map((s) => {
    const baselineCell = s.has_baseline ? formatBytes(s.baseline_ceiling_bytes) : 'n/a (calibration run)';
    return `| ${s.name} | ${s.family} | ${s.expected_status} | ${s.actual_status} | ${formatBytes(s.max_of_n_bytes)} | ${formatPct(s.cap_fraction_pct)} | ${baselineCell} | ${verdictCell(s)} |`;
  });

  return [
    '### perf-nightly',
    '',
    `**${headline}**`,
    '',
    '| sentinel | family | expected status | actual status | max-of-N memory | % of cap | baseline ceiling | verdict |',
    '|---|---|---|---|---|---|---|---|',
    ...rows,
    '',
    '`WARN cerberus query failed` lines in the raw log below for the sentinels marked "correctly ' +
      "rejected (expected)\" above are cerberus's own intended application logging for a query the " +
      'engine genuinely rejected (see docs/observability.md) — not evidence of a problem by ' +
      'themselves. Only a row marked FAIL above is a real regression.',
    '',
  ].join('\n');
}

function main() {
  const resultsPath = process.env.PERF_NIGHTLY_RESULTS_JSON || '';

  if (!resultsPath) {
    log('perf-nightly-step-summary.mjs: PERF_NIGHTLY_RESULTS_JSON is unset; nothing to render');
    process.exit(0);
  }

  if (!existsSync(resultsPath)) {
    appendStepSummary(
      [
        '### perf-nightly',
        '',
        `no results were produced at \`${resultsPath}\` — the run likely failed before any ` +
          'sentinel completed (e.g. the ClickHouse container never started, or sample loading ' +
          'failed). See the raw log below for the actual failure.',
        '',
      ].join('\n'),
    );
    process.exit(0);
  }

  let doc;
  try {
    doc = JSON.parse(readFileSync(resultsPath, 'utf8'));
  } catch (e) {
    appendStepSummary(['### perf-nightly', '', `could not parse ${resultsPath}: ${e.message}`, ''].join('\n'));
    process.exit(0);
  }

  appendStepSummary(renderSummary(doc));
  process.exit(0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
