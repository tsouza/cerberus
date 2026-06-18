// clickhouse-version-sync.mjs - the ClickHouse version-consistency gate.
//
// cerberus pins ClickHouse versions in several places that MUST agree:
//   - versions.yaml                       the single source of truth (SoT)
//   - docker-compose.yml                  the quickstart `clickhouse` image tag
//   - compatibility/*/docker-compose.yml  the per-head test-substrate image tag
//   - internal/preflight/preflight.go     the boot-time min-version floor
//   - internal/config/config.go (comment) the chDB substrate version
//
// and the quickstart MUST be new enough to actually demo every optimization
// it enables. This script reads versions.yaml as the SoT and asserts:
//
//   (a) the root docker-compose quickstart `clickhouse` image tag
//       == quickstart_clickhouse.
//   (b) preflight minCHBase == min_clickhouse, and minCHNativeRate
//       == min_native_rate (the two preflight floors are the SoT mirror).
//   (c) the chDB substrate (the compatibility prometheus/loki/tempo image
//       tags) == chdb_substrate.
//   (d) THE CRITICAL ONE: quickstart_clickhouse >= the highest version floor
//       among the features the quickstart's CERBERUS_CH_OPTIMIZATIONS enables.
//       The per-feature floors are NOT duplicated here - they are read from
//       internal/chopt/registry.go, which stays the SoT for feature floors.
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

// The quickstart image tag from `image: clickhouse/clickhouse-server:<tag>`.
function readComposeCHTag(text) {
  const m = /image:\s*clickhouse\/clickhouse-server:(\S+)/.exec(text);
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
// the four checks (a)/(b)/(c)/(d) FAILS when its source is deliberately
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
      'compatibility/prometheus/docker-compose.yml': 'image: clickhouse/clickhouse-server:25.8\n',
      'compatibility/loki/docker-compose.yml': 'image: clickhouse/clickhouse-server:25.8\n',
      'compatibility/tempo/docker-compose.yml': 'image: clickhouse/clickhouse-server:25.8\n',
    },
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
