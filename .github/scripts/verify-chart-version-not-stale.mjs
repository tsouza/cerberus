// verify-chart-version-not-stale.mjs — fails loudly the moment main's
// deploy/helm/cerberus/Chart.yaml falls BEHIND a version already published as
// a git tag, for either the app line (`v*`) or the chart line (`chart-v*`).
//
// This closes the gap that let PRs #3756/#3757 attempt to re-cut v1.21.1 and
// chart-v0.17.1 — both already published from the release/1.21.x maintenance
// branch. That branch's own release commits bump Chart.yaml on release/1.21.x
// and tag from there; nothing round-trips the bump back onto main (unlike
// CHANGELOG.md, which the maintenance-changelog-sync step already keeps
// current on main — see docs/operations.md's maintenance-lines section). So
// main's Chart.yaml silently drifted for six days, undetected until the next
// release cut recomputed the already-published versions and collided.
//
// This check makes that drift loud within minutes of the maintenance tag
// landing, instead of silent until the next release attempt: it runs on
// every push to main and every PR (cheap — two `git tag -l` calls and two
// regex reads, no build), and fails the instant `main`'s own Chart.yaml
// lines report a version older than the highest already-tagged one.
//
// It is deliberately ONE-DIRECTIONAL: Chart.yaml running AHEAD of the latest
// tag is the normal, expected state of an open release PR (which stages the
// next version before its tag exists) — only BEHIND is ever wrong.
//
// Env contract:
//   CHART_DIR   path to the chart dir (default: deploy/helm/cerberus)
//
// argv `--self-test` runs the in-process assertion suite and exits.
//
// Imports only node: builtins plus compareVersions from brew-smoke.mjs (the
// one semver-enough comparator this repo already has, reused rather than
// re-implemented). Run with:
//   node .github/scripts/verify-chart-version-not-stale.mjs

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import process from 'node:process';
import { compareVersions } from './brew-smoke.mjs';

const CHART_DIR = process.env.CHART_DIR || 'deploy/helm/cerberus';

function ghError(msg) {
  process.stdout.write(`::error::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function ghNotice(msg) {
  process.stdout.write(`::notice::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

// parseChartFields — pull the top-level `version:` and `appVersion:` lines
// out of a Chart.yaml body. Quoted or bare. Throws on a missing field so a
// malformed chart fails loud rather than silently skipping the check.
export function parseChartFields(chartYaml) {
  const version = chartYaml.match(/^version:\s*["']?([^"'\s]+)["']?\s*$/m);
  const appVersion = chartYaml.match(/^appVersion:\s*["']?([^"'\s]+)["']?\s*$/m);
  if (!version) throw new Error('could not find a top-level version: in Chart.yaml');
  if (!appVersion) throw new Error('could not find a top-level appVersion: in Chart.yaml');
  return { version: version[1], appVersion: appVersion[1] };
}

// highestTagged — the highest semver value among tags of the shape
// `<prefix><semver>`, or null when no tag matches. Ignores any tag that
// doesn't parse as bare `X.Y.Z[-pre]` after stripping the prefix, so an
// unrelated tag shape never corrupts the comparison.
export function highestTagged(tags, prefix) {
  let highest = null;
  for (const tag of tags) {
    if (!tag.startsWith(prefix)) continue;
    const v = tag.slice(prefix.length);
    if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(v)) continue;
    if (highest === null || compareVersions(v, highest) > 0) highest = v;
  }
  return highest;
}

// staleness — the pure decision. Given Chart.yaml's own version/appVersion
// and the full tag list, returns the list of problem strings (empty ==
// fresh). Pure: no I/O, no process.exit — the self-test pins the exact
// boundary.
export function staleness({ version, appVersion }, tags) {
  const problems = [];
  const highestApp = highestTagged(tags, 'v');
  if (highestApp !== null && compareVersions(appVersion, highestApp) < 0) {
    problems.push(
      `Chart.yaml appVersion "${appVersion}" is BEHIND the already-published tag v${highestApp} — ` +
        `sync appVersion (and the artifacthub.io/images tag) to at least ${highestApp} in this PR`,
    );
  }
  const highestChart = highestTagged(tags, 'chart-v');
  if (highestChart !== null && compareVersions(version, highestChart) < 0) {
    problems.push(
      `Chart.yaml version "${version}" is BEHIND the already-published tag chart-v${highestChart} — ` +
        `sync version to at least ${highestChart} in this PR`,
    );
  }
  return problems;
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

function listTags(pattern) {
  const r = spawnSync('git', ['tag', '-l', pattern], { encoding: 'utf8' });
  if (r.status !== 0) {
    ghError(`git tag -l ${pattern} failed (exit ${r.status}): ${(r.stderr || '').trim()}`);
    process.exit(1);
  }
  return (r.stdout || '')
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
}

function main() {
  let fields;
  try {
    fields = parseChartFields(readFileSync(join(CHART_DIR, 'Chart.yaml'), 'utf8'));
  } catch (e) {
    ghError(`${e.message} (in ${CHART_DIR}/Chart.yaml)`);
    process.exit(1);
  }
  const tags = [...listTags('v*'), ...listTags('chart-v*')];
  const problems = staleness(fields, tags);
  if (problems.length > 0) {
    for (const p of problems) ghError(p);
    process.exit(1);
  }
  ghNotice(
    `Chart.yaml appVersion ${fields.appVersion} / version ${fields.version} are not behind any published tag`,
  );
  process.exit(0);
}

// ---------------------------------------------------------------------------
// self-test
// ---------------------------------------------------------------------------

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };

  assert(
    parseChartFields('name: cerberus\nversion: 0.6.3\nappVersion: "2.0.0"\n').version === '0.6.3',
    'version field',
  );
  assert(
    parseChartFields('name: cerberus\nversion: 0.6.3\nappVersion: "2.0.0"\n').appVersion === '2.0.0',
    'quoted appVersion field',
  );
  let threw = false;
  try {
    parseChartFields('name: cerberus\n');
  } catch {
    threw = true;
  }
  assert(threw, 'missing fields must throw');

  assert(highestTagged(['v1.2.0', 'v1.10.0', 'v1.3.0'], 'v') === '1.10.0', 'numeric, not lexical, max');
  assert(highestTagged(['chart-v0.17.0', 'chart-v0.17.1'], 'chart-v') === '0.17.1', 'chart-prefixed max');
  assert(highestTagged(['v1.0.0'], 'chart-v') === null, 'no matching prefix -> null');
  assert(highestTagged(['release/1.21.x'], 'v') === null, 'non-semver tag ignored, not corrupting max');

  // The exact regression this check exists for: main's Chart.yaml stuck at
  // 1.21.0/0.17.0 after v1.21.1/chart-v0.17.1 published from a maintenance
  // branch — must fail on BOTH lines.
  let p = staleness(
    { version: '0.17.0', appVersion: '1.21.0' },
    ['v1.21.0', 'v1.21.1', 'chart-v0.17.0', 'chart-v0.17.1'],
  );
  assert(p.length === 2, 'both lines stale must report both problems');
  assert(p[0].includes('v1.21.1'), 'app problem names the tag it is behind');
  assert(p[1].includes('chart-v0.17.1'), 'chart problem names the tag it is behind');

  // Synced main (the state this PR restores) -> no problems.
  p = staleness(
    { version: '0.17.1', appVersion: '1.21.1' },
    ['v1.21.0', 'v1.21.1', 'chart-v0.17.0', 'chart-v0.17.1'],
  );
  assert(p.length === 0, 'synced Chart.yaml must report nothing');

  // An OPEN release PR staging the NEXT version ahead of any tag -> no
  // problems. Ahead is never stale; only behind is.
  p = staleness(
    { version: '0.17.1', appVersion: '1.22.0' },
    ['v1.21.0', 'v1.21.1', 'chart-v0.17.0', 'chart-v0.17.1'],
  );
  assert(p.length === 0, 'a version ahead of every tag must not be flagged stale');

  // No tags at all (fresh repo) -> no problems (nothing to be behind).
  p = staleness({ version: '0.1.0', appVersion: '0.1.0' }, []);
  assert(p.length === 0, 'no tags yet -> nothing to be behind');

  ghNotice('verify-chart-version-not-stale --self-test: all assertions passed');
}

if (process.argv.includes('--self-test')) {
  selfTest();
  process.exit(0);
}

main();
