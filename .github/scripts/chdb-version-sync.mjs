// chdb-version-sync.mjs - the chDB substrate version-consistency gate.
//
// The libchdb.so build every chdb-tagged lane runs on is pinned in three
// places that MUST agree, and nothing read any of them against the others:
//
//   - versions.yaml `chdb_substrate`   the single source of truth (major.minor)
//   - just/chdb.just `CHDB_VERSION`    the chdb-core release tag the installer
//                                      downloads (e.g. "v26.5.0")
//   - .github/workflows/*.yml          every `actions/cache` key of the form
//                                      `libchdb-<os>-<arch>-<tag>`
//
// The cache keys sat under a comment claiming the key "is pinned to the
// recipe's CHDB_VERSION so a version bump busts the cache automatically".
// It is not: they are nine hand-typed copies of the tag in front of an
// INSTALLER THAT SKIPS WHEN THE FILE ALREADY EXISTS. Bumping CHDB_VERSION
// without editing all nine restores the OLD libchdb.so from cache, the
// idempotent recipe then declines to replace it, and every chdb-tagged lane
// silently keeps running on the previous engine while the repo believes it
// moved (cerberus issue #3186).
//
// This gate asserts:
//
//   (a) just/chdb.just CHDB_VERSION parses as a `vMAJOR.MINOR.PATCH` tag and
//       its major.minor equals versions.yaml chdb_substrate. chdb-core release
//       tags track the ClickHouse version directly, so the two are the same
//       number written two ways; nothing bound them before.
//   (b) EVERY `libchdb-…` cache key across .github/workflows carries exactly
//       that CHDB_VERSION tag. A key that names any other tag fails, and so
//       does a workflow that caches /usr/local/lib/libchdb.so under a key
//       shape this reader does not recognise — an unreadable key is a key
//       this gate cannot pin, which is the same hazard as a wrong one.
//   (c) at least one such cache key exists. The scan's own pathspec is the
//       thing most likely to silently stop matching; a zero-key run reports
//       success while checking nothing, so it fails instead.
//
// Dependency-light by design (node: builtins only), matching the other
// .github/scripts/*.mjs modules.
//
// Invocation:
//   node .github/scripts/chdb-version-sync.mjs              run the gate
//   node .github/scripts/chdb-version-sync.mjs --self-test  pin the logic

import { readFileSync, readdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

// ---------------------------------------------------------------------------
// readers
// ---------------------------------------------------------------------------

// versions.yaml is read with the same flat `key: "value"` scanner
// clickhouse-version-sync.mjs uses (the file is intentionally flat, and a YAML
// dependency is not worth it for one key).
export function readChdbSubstrate(text) {
  const m = /^chdb_substrate:\s*"?([0-9]+\.[0-9]+)"?/m.exec(text);
  return m ? m[1] : null;
}

// just/chdb.just's `CHDB_VERSION := "v26.5.0"` assignment.
export function readChdbVersion(text) {
  const m = /^CHDB_VERSION\s*:=\s*"([^"]+)"/m.exec(text);
  return m ? m[1] : null;
}

// Every libchdb cache key in one workflow file, as { line, key } records.
// The reader is deliberately two-pronged: it collects keys that MATCH the
// expected `libchdb-<...>-<tag>` shape, and separately flags any
// actions/cache step whose `path:` is the libchdb install path but whose key
// this reader could not parse — see check (b).
export function readLibchdbCacheKeys(text) {
  const keys = [];
  const unreadable = [];
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/^\s*path:\s*\/usr\/local\/lib\/libchdb\.so\s*$/.test(line)) {
      // The key line is conventionally the next non-blank line of the same
      // `with:` block. Look ahead a short, bounded distance.
      let found = null;
      for (let j = i + 1; j < Math.min(i + 5, lines.length); j++) {
        const km = /^\s*key:\s*(.+?)\s*$/.exec(lines[j]);
        if (km) {
          found = { line: j + 1, key: km[1] };
          break;
        }
      }
      if (found) keys.push(found);
      else unreadable.push({ line: i + 1, key: null });
    }
  }
  return { keys, unreadable };
}

// The version tag a `libchdb-${{ runner.os }}-${{ runner.arch }}-v26.5.0`
// cache key pins, or null when the key is not of that shape.
export function tagFromCacheKey(key) {
  const m = /^libchdb-.*-(v[0-9]+\.[0-9]+\.[0-9]+)$/.exec(key);
  return m ? m[1] : null;
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// runChecks(sources) -> { failures, notes }. Pure over already-read strings so
// the self-test can drive it with fixtures. `sources.workflows` is a map of
// file name -> file text.
export function runChecks(sources) {
  const failures = [];
  const notes = [];

  const substrate = readChdbSubstrate(sources.versionsYaml);
  const version = readChdbVersion(sources.chdbJust);

  if (!substrate) failures.push('versions.yaml: chdb_substrate is missing or unparseable');
  if (!version) failures.push('just/chdb.just: CHDB_VERSION is missing or unparseable');

  // (a) CHDB_VERSION major.minor == chdb_substrate.
  if (substrate && version) {
    const m = /^v([0-9]+)\.([0-9]+)\.[0-9]+$/.exec(version);
    if (!m) {
      failures.push(`(a) just/chdb.just CHDB_VERSION "${version}" is not a vMAJOR.MINOR.PATCH chdb-core tag`);
    } else if (`${m[1]}.${m[2]}` !== substrate) {
      failures.push(
        `(a) just/chdb.just CHDB_VERSION ${version} != versions.yaml chdb_substrate ${substrate} — ` +
          `the chdb-core release tag tracks the ClickHouse version, so these are one number written two ways`,
      );
    } else {
      notes.push(`(a) CHDB_VERSION ${version} == chdb_substrate ${substrate}`);
    }
  }

  // (b) + (c) every libchdb cache key carries that same tag, and there is at
  // least one to check.
  let total = 0;
  for (const [name, text] of Object.entries(sources.workflows)) {
    const { keys, unreadable } = readLibchdbCacheKeys(text);
    for (const u of unreadable) {
      failures.push(`(b) ${name}:${u.line} caches libchdb.so with no key this gate can read`);
    }
    for (const k of keys) {
      total++;
      const tag = tagFromCacheKey(k.key);
      if (!tag) {
        failures.push(
          `(b) ${name}:${k.line} libchdb cache key "${k.key}" is not of the form ` +
            `libchdb-<...>-vMAJOR.MINOR.PATCH, so its pinned engine version cannot be checked`,
        );
      } else if (version && tag !== version) {
        failures.push(
          `(b) ${name}:${k.line} libchdb cache key pins ${tag} but just/chdb.just CHDB_VERSION is ${version} — ` +
            `the install recipe SKIPS when the file already exists, so a stale key silently keeps the old engine`,
        );
      }
    }
  }
  if (total === 0) {
    failures.push(
      '(c) no libchdb cache key was found in .github/workflows — either the caching was removed ' +
        'or this scan stopped matching; a zero-key run checks nothing and must not report success',
    );
  } else if (failures.length === 0) {
    notes.push(`(b) all ${total} libchdb cache key(s) pin ${version}`);
  }

  return { failures, notes };
}

// ---------------------------------------------------------------------------
// self-test - pins each assertion by proving it FAILS on a deliberate mismatch
// ---------------------------------------------------------------------------

const OK_WORKFLOW = `jobs:
  x:
    steps:
      - uses: actions/cache@v6
        with:
          path: /usr/local/lib/libchdb.so
          key: libchdb-\${{ runner.os }}-\${{ runner.arch }}-v26.5.0
`;

function selfTest() {
  const base = {
    versionsYaml: 'chdb_substrate: "26.5"\n',
    chdbJust: 'CHDB_VERSION := "v26.5.0"\n',
    workflows: { 'a.yml': OK_WORKFLOW },
  };

  const cases = [
    ['the repository as it stands passes', base, 0],
    [
      '(a) a CHDB_VERSION whose minor drifts from chdb_substrate fails',
      { ...base, chdbJust: 'CHDB_VERSION := "v26.7.0"\n' },
      1,
    ],
    [
      '(a) a CHDB_VERSION that is not a release tag fails',
      { ...base, chdbJust: 'CHDB_VERSION := "latest"\n' },
      1,
    ],
    [
      '(b) a cache key naming a different tag fails',
      { ...base, workflows: { 'a.yml': OK_WORKFLOW.replace('v26.5.0', 'v26.4.0') } },
      1,
    ],
    [
      '(b) a cache key shape this gate cannot read fails',
      { ...base, workflows: { 'a.yml': OK_WORKFLOW.replace(/key:.*/, 'key: libchdb-whatever') } },
      1,
    ],
    [
      '(b) caching libchdb.so with no key at all fails',
      {
        ...base,
        workflows: { 'a.yml': OK_WORKFLOW.split('\n').filter((l) => !l.includes('key:')).join('\n') },
      },
      1,
    ],
    ['(c) finding no cache key at all fails', { ...base, workflows: { 'a.yml': 'jobs:\n' } }, 1],
  ];

  let bad = 0;
  for (const [name, sources, wantFailures] of cases) {
    const { failures } = runChecks(sources);
    const got = failures.length === 0 ? 0 : 1;
    if (got !== wantFailures) {
      bad++;
      console.error(`::error::self-test: ${name} — expected ${wantFailures ? 'failure' : 'success'}, got: ${failures.join('; ') || 'success'}`);
    } else {
      console.log(`self-test OK: ${name}`);
    }
  }
  if (bad > 0) process.exit(1);
  console.log('chdb-version-sync self-test: all assertions pinned.');
}

// ---------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------

function main() {
  if (process.argv.includes('--self-test')) {
    selfTest();
    return;
  }

  const workflowsDir = join(repoRoot, '.github', 'workflows');
  const workflows = {};
  for (const f of readdirSync(workflowsDir)) {
    if (f.endsWith('.yml') || f.endsWith('.yaml')) {
      workflows[`.github/workflows/${f}`] = readFileSync(join(workflowsDir, f), 'utf8');
    }
  }

  const { failures, notes } = runChecks({
    versionsYaml: readFileSync(join(repoRoot, 'versions.yaml'), 'utf8'),
    chdbJust: readFileSync(join(repoRoot, 'just', 'chdb.just'), 'utf8'),
    workflows,
  });

  for (const n of notes) console.log(`::notice::chdb-version-sync: ${n}`);
  for (const f of failures) console.error(`::error::chdb-version-sync: ${f}`);
  if (failures.length > 0) process.exit(1);
  console.log('chdb-version-sync: the chDB substrate pin agrees everywhere.');
}

main();
