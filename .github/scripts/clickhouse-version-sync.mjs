// clickhouse-version-sync.mjs - the ClickHouse version-consistency gate.
//
// cerberus pins ClickHouse versions in several places that MUST agree:
//   - versions.yaml                       the single source of truth (SoT)
//   - docker-compose.yml                  the quickstart `clickhouse` image tag
//   - compatibility/*/docker-compose.yml  the per-head test-substrate image tag
//   - internal/preflight/preflight.go     the boot-time min-version floor
//   - internal/config/config.go (comment) the chDB substrate version
//   - test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml
//                                         the migration lane's deployment-surface tag
//   - .github/workflows/compatibility.yml the prometheus-floor lane's CH_IMAGE
//                                         override (the ONE lane that deliberately
//                                         runs BELOW chdb_substrate — see (f))
//
// and the quickstart MUST be new enough to actually demo every optimization
// it enables. This script reads versions.yaml as the SoT and asserts:
//
//   (a) the root docker-compose quickstart `clickhouse` image tag
//       == quickstart_clickhouse.
//   (b) preflight minCHBase == min_clickhouse, and minCHNativeRate
//       == min_native_rate (the two preflight floors are the SoT mirror).
//   (c) the chDB substrate (the compatibility prometheus/loki/tempo image
//       tags) == chdb_substrate. compatibility/prometheus/docker-compose.yml
//       parametrises its tag via `${CH_IMAGE:-clickhouse/clickhouse-server:X}`
//       so the floor lane (f) can override it at run time; this check reads
//       the `:-` DEFAULT, which is the honest "what does this file pin"
//       answer for every trigger that doesn't set CH_IMAGE.
//   (d) THE CRITICAL ONE: quickstart_clickhouse >= the highest version floor
//       among the features the quickstart's CERBERUS_CH_OPTIMIZATIONS enables.
//       The per-feature floors are NOT duplicated here - they are read from
//       internal/chopt/registry.go, which stays the SoT for feature floors.
//   (e) the migration lane's Tier-1 `clickhouse` image tag
//       == quickstart_clickhouse (NOT chdb_substrate). The two roles differ:
//       chdb_substrate is the SQL-parity substrate the chDB suites and the
//       compatibility harnesses run on; quickstart_clickhouse is the
//       deployment surface operators run. The migration lane models the
//       operator deployment, so it tracks the quickstart pin.
//   (f) compatibility.yml's `prometheus-floor` job's CH_IMAGE == min_clickhouse.
//       That job (see #1500) exists specifically to run the prometheus
//       differential corpus against cerberus's declared floor instead of
//       chdb_substrate, forcing internal/chopt's auto-picker to resolve every
//       above-floor feature OFF. If minCHBase moves without moving this job's
//       CH_IMAGE (or vice versa), the floor lane silently stops testing the
//       floor it claims to — (f) is what makes checks (b) and (c)'s
//       DELIBERATE exception (this one lane, and only this lane, runs below
//       chdb_substrate) a tracked invariant instead of an unenforced escape
//       hatch from (c).
//
// No version floor is re-pinned here; floor data is derived from the registry.
// When a future optimization raises the floor above the quickstart, (d) fails
// until the quickstart (and this file's pins) are bumped - see
// docs/optimization-rules.md (Rule 1, step 4).
//
// Dependency-light by design (node: builtins only - no setup-node / npm
// install), matching the other .github/scripts/*.mjs modules.
//
// Invocation:
//   node .github/scripts/clickhouse-version-sync.mjs            run the gate
//   node .github/scripts/clickhouse-version-sync.mjs --self-test  pin the logic
//
// Exit codes: 0 = consistent (or self-test green), 1 = a drift was found.

import { error, log } from './lib/gh.mjs';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import process from 'node:process';

// repoRoot - .github/scripts/ is two levels under the repo root.
const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

// ---------------------------------------------------------------------------
// version helpers - major.minor only, mirroring internal/chopt.ParseVersion /
// AtLeast (ClickHouse feature availability lands at minor granularity; patch /
// build suffixes are dropped). Kept tiny and self-contained.
// ---------------------------------------------------------------------------

// parseVersion("26.5") / ("25.8.2.1-lts") -> { major, minor } or null. Reads
// only the leading major.minor; trailing patch/build/suffix fields are ignored.
function parseVersion(s) {
  const fields = String(s).trim().split('.');
  if (fields.length < 2) return null;
  const major = leadingInt(fields[0]);
  const minor = leadingInt(fields[1]);
  if (major === null || minor === null) return null;
  return { major, minor };
}

// leadingInt("8-alpine") -> 8; leadingInt("lts") -> null.
function leadingInt(s) {
  const m = /^\s*(\d+)/.exec(String(s));
  return m ? Number(m[1]) : null;
}

// atLeast(v, min) - true when v >= min, major first then minor (== AtLeast).
function atLeast(v, min) {
  if (v.major !== min.major) return v.major > min.major;
  return v.minor >= min.minor;
}

function vstr(v) {
  return `${v.major}.${v.minor}`;
}

// ---------------------------------------------------------------------------
// readers - each extracts ONE pinned version (or set) from a source file. They
// are deliberately narrow regexes over the canonical literal forms; a refactor
// that changes the literal must update the reader (the self-test pins them).
// ---------------------------------------------------------------------------

function readFile(rel) {
  return readFileSync(join(repoRoot, rel), 'utf8');
}

// versions.yaml is read with a flat `key: "value"` scanner rather than a YAML
// dependency (the file is intentionally flat). Returns a string map.
function parseVersionsYaml(text) {
  const out = {};
  for (const line of text.split('\n')) {
    const m = /^([a-z_]+):\s*"?([^"#\s]+)"?/.exec(line);
    if (m) out[m[1]] = m[2];
  }
  return out;
}

// The compose image tag, from either the literal form
// `image: clickhouse/clickhouse-server:<tag>` or the parametrised form
// `image: ${VAR:-clickhouse/clickhouse-server:<tag>}` (used by
// compatibility/prometheus/docker-compose.yml so the prometheus-floor CI job
// can override the tag at run time — see #1500). Either way this returns
// what the file itself PINS: the literal tag, or the `:-` default.
function readComposeCHTag(text) {
  const literal = /image:\s*clickhouse\/clickhouse-server:(\S+)/.exec(text);
  if (literal) return literal[1];
  const interpolated = /image:\s*\$\{[A-Za-z_][A-Za-z0-9_]*:-clickhouse\/clickhouse-server:([^}]+)\}/.exec(text);
  return interpolated ? interpolated[1] : null;
}

// extractJobBlock pulls one top-level GitHub Actions job's raw text out of a
// workflow file, bounded by the next 2-space-indented `key:` line (or EOF).
// Not a full YAML parser — cerberus's workflows use consistent 2-space job
// indentation under `jobs:`, and the self-test pins this against a synthetic
// fixture. Returns null when the job name isn't found.
function extractJobBlock(text, jobName) {
  const re = new RegExp(`\\n  ${jobName}:\\n([\\s\\S]*?)(?=\\n  [A-Za-z0-9_-]+:\\n|$)`);
  const m = re.exec(text);
  return m ? m[1] : null;
}

// The compatibility/prometheus-floor job's CH_IMAGE override — the tag it
// pins the floor lane's compose ClickHouse service to. Scoped to the job's
// own block (rather than a blind file-wide grep) so a future unrelated
// CH_IMAGE use elsewhere in compatibility.yml can't be mistaken for the
// floor lane's pin.
function readFloorJobCHImage(text) {
  const block = extractJobBlock(text, 'prometheus-floor');
  if (!block) return null;
  const m = /CH_IMAGE:\s*"clickhouse\/clickhouse-server:([^"]+)"/.exec(block);
  return m ? m[1] : null;
}

// The quickstart's enabled optimization list from CERBERUS_CH_OPTIMIZATIONS.
function readComposeOptimizations(text) {
  const m = /CERBERUS_CH_OPTIMIZATIONS:\s*"([^"]*)"/.exec(text);
  if (!m) return null;
  return m[1]
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0 && s !== 'auto' && s !== 'off');
}

// preflight minCHBase / minCHNativeRate from their Version struct literals:
//   var minCHBase = chopt.Version{Major: 24, Minor: 8}
function readPreflightFloor(text, name) {
  const re = new RegExp(`var ${name} = chopt\\.Version\\{Major:\\s*(\\d+),\\s*Minor:\\s*(\\d+)\\}`);
  const m = re.exec(text);
  return m ? { major: Number(m[1]), minor: Number(m[2]) } : null;
}

// The per-feature floors from internal/chopt/registry.go. Returns a map of
// feature id -> { major, minor } | null (null = AlwaysAvailable / no floor).
// Parses the registry literal block: each entry is an `ID: FeatureX,` line
// followed by a `MinVersion: Version{Major: M, Minor: N}` (or
// `MinVersion: AlwaysAvailable`) line. The id-constant -> string mapping is
// read from the `const ( FeatureX = "x" )` block so the registry stays the
// only place the floor literals live.
function readRegistryFloors(text) {
  // id constant -> string literal, e.g. FeatureTSGridRange -> "ts_grid_range".
  const idToString = {};
  const constRe = /(\bFeature[A-Za-z0-9]+)\s*=\s*"([^"]+)"/g;
  for (let m; (m = constRe.exec(text)); ) idToString[m[1]] = m[2];

  const floors = {};
  // Each registry entry: ID: FeatureX, ... MinVersion: <Version{...}|AlwaysAvailable>.
  const entryRe =
    /ID:\s*(Feature[A-Za-z0-9]+),[\s\S]*?MinVersion:\s*(?:Version\{Major:\s*(\d+),\s*Minor:\s*(\d+)\}|(AlwaysAvailable))/g;
  for (let m; (m = entryRe.exec(text)); ) {
    const id = idToString[m[1]] ?? m[1];
    floors[id] = m[4] === 'AlwaysAvailable' ? null : { major: Number(m[2]), minor: Number(m[3]) };
  }
  return floors;
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// runChecks(sources) -> { failures: string[], notes: string[] }. Pure over the
// already-read source strings so the self-test can drive it with fixtures.
export function runChecks(sources) {
  const failures = [];
  const notes = [];

  const v = parseVersionsYaml(sources.versionsYaml);
  const want = (key) => {
    const parsed = parseVersion(v[key]);
    if (!parsed) failures.push(`versions.yaml: ${key} is missing or unparseable (got "${v[key]}")`);
    return parsed;
  };

  const quickstart = want('quickstart_clickhouse');
  const minCH = want('min_clickhouse');
  const minNative = want('min_native_rate');
  const substrate = want('chdb_substrate');

  // (a) docker-compose quickstart image tag == quickstart_clickhouse.
  const composeTag = parseVersion(readComposeCHTag(sources.compose));
  if (!composeTag) {
    failures.push('docker-compose.yml: could not read the clickhouse image tag');
  } else if (quickstart && !sameMM(composeTag, quickstart)) {
    failures.push(
      `(a) docker-compose.yml clickhouse tag ${vstr(composeTag)} != versions.yaml quickstart_clickhouse ${vstr(quickstart)}`,
    );
  } else if (quickstart) {
    notes.push(`(a) quickstart image == ${vstr(quickstart)}`);
  }

  // (b) preflight minCHBase == min_clickhouse; minCHNativeRate == min_native_rate.
  const base = readPreflightFloor(sources.preflight, 'minCHBase');
  const native = readPreflightFloor(sources.preflight, 'minCHNativeRate');
  if (!base) {
    failures.push('internal/preflight: could not read minCHBase');
  } else if (minCH && !sameMM(base, minCH)) {
    failures.push(`(b) preflight minCHBase ${vstr(base)} != versions.yaml min_clickhouse ${vstr(minCH)}`);
  } else if (minCH) {
    notes.push(`(b) min_clickhouse == ${vstr(minCH)}`);
  }
  if (!native) {
    failures.push('internal/preflight: could not read minCHNativeRate');
  } else if (minNative && !sameMM(native, minNative)) {
    failures.push(
      `(b) preflight minCHNativeRate ${vstr(native)} != versions.yaml min_native_rate ${vstr(minNative)}`,
    );
  } else if (minNative) {
    notes.push(`(b) min_native_rate == ${vstr(minNative)}`);
  }

  // (c) chDB substrate == every compatibility-lane clickhouse image tag.
  for (const [name, text] of Object.entries(sources.compatibility)) {
    const tag = parseVersion(readComposeCHTag(text));
    if (!tag) {
      failures.push(`${name}: could not read the clickhouse image tag`);
    } else if (substrate && !sameMM(tag, substrate)) {
      failures.push(
        `(c) ${name} clickhouse tag ${vstr(tag)} != versions.yaml chdb_substrate ${vstr(substrate)}`,
      );
    }
  }
  if (substrate) notes.push(`(c) chdb_substrate == ${vstr(substrate)} (compatibility lanes pinned to it)`);

  // (d) THE CRITICAL ONE: quickstart >= highest floor among enabled features.
  const floors = readRegistryFloors(sources.registry);
  const enabled = readComposeOptimizations(sources.compose);
  if (!enabled) {
    failures.push('docker-compose.yml: could not read CERBERUS_CH_OPTIMIZATIONS');
  } else if (quickstart) {
    let highest = null;
    let highestFeature = null;
    for (const id of enabled) {
      if (!(id in floors)) {
        failures.push(`(d) docker-compose enables "${id}", which is not a known chopt registry feature`);
        continue;
      }
      const floor = floors[id]; // null = AlwaysAvailable (no floor)
      if (floor && (highest === null || atLeast(floor, highest))) {
        highest = floor;
        highestFeature = id;
      }
    }
    if (highest && !atLeast(quickstart, highest)) {
      failures.push(
        `(d) quickstart_clickhouse ${vstr(quickstart)} is TOO OLD for enabled feature "${highestFeature}" ` +
          `(floor ${vstr(highest)}). Bump the quickstart (docker-compose + compatibility image tags) and ` +
          `versions.yaml to a ClickHouse that supports it - see docs/optimization-rules.md (Rule 1, step 4).`,
      );
    } else if (highest) {
      notes.push(
        `(d) quickstart ${vstr(quickstart)} >= highest enabled floor ${vstr(highest)} ("${highestFeature}")`,
      );
    } else {
      notes.push('(d) quickstart enables no version-gated feature (all AlwaysAvailable)');
    }
  }

  // (e) the migration lane's Tier-1 stack tracks the DEPLOYMENT surface.
  const migrationTag = parseVersion(readComposeCHTag(sources.migrationTier1));
  if (!migrationTag) {
    failures.push('migration tier-1 compose: could not read the clickhouse image tag');
  } else if (quickstart && !sameMM(migrationTag, quickstart)) {
    failures.push(
      `(e) migration tier-1 clickhouse tag ${vstr(migrationTag)} != versions.yaml quickstart_clickhouse ` +
        `${vstr(quickstart)}. The migration lane models the operator deployment surface, so it tracks ` +
        `quickstart_clickhouse - not chdb_substrate.`,
    );
  } else if (quickstart) {
    notes.push(`(e) migration tier-1 image == quickstart ${vstr(quickstart)}`);
  }

  // (f) the prometheus-floor lane's CH_IMAGE == min_clickhouse. This is the
  // ONE lane that deliberately runs below chdb_substrate (see #1500); this
  // check is what keeps that exception honest instead of an unenforced
  // drift point once (c) stopped being able to see it.
  const floorImage = parseVersion(readFloorJobCHImage(sources.compatibilityWorkflow));
  if (!floorImage) {
    failures.push("compatibility.yml: could not read the prometheus-floor job's CH_IMAGE");
  } else if (minCH && !sameMM(floorImage, minCH)) {
    failures.push(
      `(f) compatibility.yml prometheus-floor CH_IMAGE ${vstr(floorImage)} != versions.yaml min_clickhouse ${vstr(minCH)}`,
    );
  } else if (minCH) {
    notes.push(`(f) prometheus-floor CH_IMAGE == min_clickhouse ${vstr(minCH)}`);
  }

  return { failures, notes };
}

function sameMM(a, b) {
  return a.major === b.major && a.minor === b.minor;
}

// loadSources() - read every source file off disk for a real run.
function loadSources() {
  return {
    versionsYaml: readFile('versions.yaml'),
    compose: readFile('docker-compose.yml'),
    preflight: readFile('internal/preflight/preflight.go'),
    registry: readFile('internal/chopt/registry.go'),
    compatibility: {
      'compatibility/prometheus/docker-compose.yml': readFile('compatibility/prometheus/docker-compose.yml'),
      'compatibility/loki/docker-compose.yml': readFile('compatibility/loki/docker-compose.yml'),
      'compatibility/tempo/docker-compose.yml': readFile('compatibility/tempo/docker-compose.yml'),
    },
    migrationTier1: readFile('test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml'),
    compatibilityWorkflow: readFile('.github/workflows/compatibility.yml'),
  };
}

function main() {
  const { failures, notes } = runChecks(loadSources());
  for (const n of notes) log(`ok: ${n}`);
  if (failures.length > 0) {
    for (const f of failures) error(f);
    process.exit(1);
  }
  log('clickhouse-version-sync: all version references are consistent');
  process.exit(0);
}

// ---------------------------------------------------------------------------
// self-test - pins the parse / compare / drift-detection logic against
// synthetic fixtures, the same contract scripts/test-forbid-skip.sh provides
// for forbid-skip.mjs. Asserts a consistent fixture passes and that each of
// the checks (a)/(b)/(c)/(d)/(e) FAILS when its source is deliberately
// drifted - so a future refactor that breaks a reader is caught here.
// ---------------------------------------------------------------------------

function selfTest() {
  let passes = 0;
  const fails = [];
  const ok = (label, cond) => (cond ? passes++ : fails.push(label));

  // --- unit: version parse + compare ---
  ok('parse 26.5', JSON.stringify(parseVersion('26.5')) === JSON.stringify({ major: 26, minor: 5 }));
  ok('parse 25.8.2.1-lts', JSON.stringify(parseVersion('25.8.2.1-lts')) === JSON.stringify({ major: 25, minor: 8 }));
  ok('parse 8-alpine tag', leadingInt('8-alpine') === 8);
  ok('parse junk -> null', parseVersion('lts') === null);
  ok('atLeast 26.5 >= 25.6', atLeast({ major: 26, minor: 5 }, { major: 25, minor: 6 }));
  ok('atLeast 25.0 < 25.6', !atLeast({ major: 25, minor: 0 }, { major: 25, minor: 6 }));
  ok('atLeast 25.6 >= 25.6', atLeast({ major: 25, minor: 6 }, { major: 25, minor: 6 }));

  // --- unit: registry floor parsing (incl. AlwaysAvailable) ---
  const fakeRegistry = `
const (
  FeatureAggregationInOrder = "aggregation_in_order"
  FeatureConditionCache     = "condition_cache"
  FeatureTSGridRange        = "ts_grid_range"
  FeatureColumnarResultDecode = "columnar_result_decode"
)
var registry = []Feature{
  { ID: FeatureAggregationInOrder, MinVersion: Version{Major: 24, Minor: 8}, },
  { ID: FeatureConditionCache, MinVersion: Version{Major: 25, Minor: 3}, },
  { ID: FeatureTSGridRange, MinVersion: Version{Major: 25, Minor: 6}, },
  { ID: FeatureColumnarResultDecode, MinVersion: AlwaysAvailable, },
}`;
  const floors = readRegistryFloors(fakeRegistry);
  ok('registry ts_grid_range floor 25.6', floors['ts_grid_range']?.major === 25 && floors['ts_grid_range']?.minor === 6);
  ok('registry condition_cache floor 25.3', floors['condition_cache']?.minor === 3);
  ok('registry columnar_result_decode AlwaysAvailable -> null floor', floors['columnar_result_decode'] === null);

  // --- unit: readComposeCHTag handles both the literal and the
  // parametrised-default forms (the floor lane needs the latter) ---
  ok(
    'readComposeCHTag literal',
    readComposeCHTag('image: clickhouse/clickhouse-server:26.5\n') === '26.5',
  );
  ok(
    'readComposeCHTag interpolated default',
    readComposeCHTag('image: ${CH_IMAGE:-clickhouse/clickhouse-server:26.5}\n') === '26.5',
  );
  ok('readComposeCHTag no image -> null', readComposeCHTag('services:\n  foo:\n') === null);

  // --- unit: extractJobBlock / readFloorJobCHImage ---
  const fakeWorkflow = [
    'jobs:',
    '  prometheus:',
    '    steps:',
    '      - run: echo not-the-floor-job',
    '  prometheus-floor:',
    '    steps:',
    '      - run: |',
    '          echo hi',
    '        env:',
    '          CH_IMAGE: "clickhouse/clickhouse-server:24.8"',
    '  promql-surface:',
    '    steps:',
    '      - run: echo unrelated',
    '',
  ].join('\n');
  ok('extractJobBlock finds the named job', extractJobBlock(fakeWorkflow, 'prometheus-floor')?.includes('CH_IMAGE'));
  ok(
    "extractJobBlock doesn't bleed into the next job",
    !extractJobBlock(fakeWorkflow, 'prometheus-floor')?.includes('promql-surface'),
  );
  ok('readFloorJobCHImage reads the pinned tag', readFloorJobCHImage(fakeWorkflow) === '24.8');
  ok('readFloorJobCHImage missing job -> null', readFloorJobCHImage('jobs:\n  other:\n    steps: []\n') === null);

  // --- integration: a consistent fixture set passes with zero failures ---
  const goodSources = {
    versionsYaml:
      'min_clickhouse: "24.8"\nmin_native_rate: "25.6"\nquickstart_clickhouse: "26.5"\nchdb_substrate: "25.8"\n',
    compose:
      'services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:26.5\n  cerberus:\n    environment:\n      CERBERUS_CH_OPTIMIZATIONS: "aggregation_in_order,condition_cache,ts_grid_range"\n',
    preflight:
      'var minCHBase = chopt.Version{Major: 24, Minor: 8}\nvar minCHNativeRate = chopt.Version{Major: 25, Minor: 6}\n',
    registry: fakeRegistry,
    compatibility: {
      // Prometheus uses the real parametrised-default form (the floor lane
      // overrides CH_IMAGE at run time); loki/tempo stay literal — both
      // forms of readComposeCHTag are exercised by this one fixture set.
      'compatibility/prometheus/docker-compose.yml': 'image: ${CH_IMAGE:-clickhouse/clickhouse-server:25.8}\n',
      'compatibility/loki/docker-compose.yml': 'image: clickhouse/clickhouse-server:25.8\n',
      'compatibility/tempo/docker-compose.yml': 'image: clickhouse/clickhouse-server:25.8\n',
    },
    // The migration tier-1 stack tracks the quickstart tag (26.5 here), NOT
    // the 25.8 chDB substrate the compatibility lanes pin.
    migrationTier1: 'image: clickhouse/clickhouse-server:26.5\n',
    compatibilityWorkflow:
      'jobs:\n  prometheus-floor:\n    steps:\n      - run: echo seed\n        env:\n          CH_IMAGE: "clickhouse/clickhouse-server:24.8"\n',
  };
  ok('consistent fixtures => no failures', runChecks(goodSources).failures.length === 0);

  // --- integration: each check fails on a deliberate drift ---
  const drift = (mutate) => {
    const s = structuredClone(goodSources);
    mutate(s);
    return runChecks(s).failures;
  };
  ok(
    '(a) compose tag drift is caught',
    drift((s) => (s.compose = s.compose.replace('26.5', '25.3'))).some((f) => f.startsWith('(a)')),
  );
  ok(
    '(b) preflight minCHBase drift is caught',
    drift((s) => (s.preflight = s.preflight.replace('Major: 24, Minor: 8', 'Major: 23, Minor: 8'))).some((f) =>
      f.startsWith('(b)'),
    ),
  );
  ok(
    '(c) compatibility substrate drift is caught',
    drift((s) => (s.compatibility['compatibility/loki/docker-compose.yml'] = 'image: clickhouse/clickhouse-server:25.6\n')).some(
      (f) => f.startsWith('(c)'),
    ),
  );
  ok(
    '(d) quickstart too old for an enabled floor is caught',
    // Quickstart 25.3 but the demo enables ts_grid_range (floor 25.6).
    drift((s) => {
      s.versionsYaml = s.versionsYaml.replace('quickstart_clickhouse: "26.5"', 'quickstart_clickhouse: "25.3"');
      s.compose = s.compose.replace('clickhouse-server:26.5', 'clickhouse-server:25.3');
    }).some((f) => f.startsWith('(d)')),
  );
  ok(
    '(d) unknown enabled feature is caught',
    drift((s) => (s.compose = s.compose.replace('ts_grid_range', 'ts_grid_bogus'))).some((f) =>
      f.startsWith('(d)') && f.includes('not a known'),
    ),
  );

  ok(
    '(e) migration tier-1 tag drift is caught',
    drift((s) => (s.migrationTier1 = 'image: clickhouse/clickhouse-server:25.3\n')).some((f) => f.startsWith('(e)')),
  );
  ok(
    '(e) migration tier-1 pinned to the chDB substrate instead of the quickstart is caught',
    // 25.8 is chdb_substrate in the fixture - the exact wrong-role mistake (e) exists to reject.
    drift((s) => (s.migrationTier1 = 'image: clickhouse/clickhouse-server:25.8\n')).some((f) => f.startsWith('(e)')),
  );
  ok(
    '(f) prometheus-floor CH_IMAGE drifting from min_clickhouse is caught',
    // The floor lane's CH_IMAGE moved to 24.12 but min_clickhouse stayed 24.8.
    drift((s) => (s.compatibilityWorkflow = s.compatibilityWorkflow.replace('24.8', '24.12'))).some((f) =>
      f.startsWith('(f)'),
    ),
  );
  ok(
    "(f) missing prometheus-floor job is caught",
    drift((s) => (s.compatibilityWorkflow = 'jobs:\n  other-job:\n    steps: []\n')).some(
      (f) => f.includes('prometheus-floor'),
    ),
  );

  if (fails.length > 0) {
    for (const f of fails) error(`self-test FAIL: ${f}`);
    log(`self-test: ${passes} passed, ${fails.length} failed`);
    process.exit(1);
  }
  log(`self-test: all ${passes} cases passed`);
  process.exit(0);
}

if (process.argv.includes('--self-test')) {
  selfTest();
} else {
  main();
}
