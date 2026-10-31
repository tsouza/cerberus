// clickhouse-version-sync.mjs - the ClickHouse version-consistency gate.
//
// cerberus pins ClickHouse versions in several places that MUST agree:
//   - versions.yaml                       the single source of truth (SoT)
//   - docker-compose.yml                  the quickstart `clickhouse` image tag
//   - compatibility/*/docker-compose.yml  the per-head differential-harness image tag
//   - internal/preflight/preflight.go     the boot-time min-version floor
//   - test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml
//                                         the migration lane's deployment-surface tag
//   - deploy/helm/cerberus/values.yaml    the bundled-ClickHouse chart image
//   - .github/workflows/compatibility.yml the prometheus-floor lane's CH_IMAGE
//                                         override (the ONE lane that deliberately
//                                         runs BELOW quickstart_clickhouse — see (f))
//   - .github/workflows/perf-nightly.yml  the sentinel legs' ClickHouse matrix, and
//     .github/workflows/perf-nightly-selfcheck.yml
//                                         the self-check's, with
//     test/perf/nightly-baseline.json     one bound set per server (see (h))
//
// and the quickstart MUST be new enough to actually demo every optimization
// it enables. This script reads versions.yaml as the SoT and asserts:
//
//   (a) the root docker-compose quickstart `clickhouse` image tag
//       == quickstart_clickhouse.
//   (b) preflight minCHBase == min_clickhouse, and minCHNativeRate
//       == min_native_rate (the two preflight floors are the SoT mirror).
//   (c) the compatibility prometheus/loki/tempo image tags
//       == quickstart_clickhouse. The differential harnesses are the source
//       of truth for all three heads, so they run on the server operators
//       deploy, not on chdb_substrate (the in-process chDB engine's version,
//       which chdb-version-sync.mjs holds). compatibility/prometheus/
//       docker-compose.yml parametrises its tag via
//       `${CH_IMAGE:-clickhouse/clickhouse-server:X}` so the floor lane (f)
//       can override it at run time; this check reads the `:-` DEFAULT, which
//       is the honest "what does this file pin" answer for every trigger that
//       doesn't set CH_IMAGE.
//   (d) THE CRITICAL ONE: quickstart_clickhouse >= the highest version floor
//       among the features the quickstart's CERBERUS_CH_OPTIMIZATIONS enables.
//       The per-feature floors are NOT duplicated here - they are read from
//       internal/chopt/registry.go, which stays the SoT for feature floors.
//   (e) the migration lane's Tier-1 `clickhouse` image tag
//       == quickstart_clickhouse (NOT chdb_substrate). The two roles differ:
//       chdb_substrate is the in-process chDB engine the parity suites run
//       on; quickstart_clickhouse is the deployment surface operators run.
//       The migration lane models the operator deployment, so it tracks the
//       quickstart pin.
//   (g) the Helm chart's bundled ClickHouse image == quickstart_clickhouse.
//       Same role as (e): the chart IS the deployment surface, so it tracks
//       the quickstart pin, not chdb_substrate. This tag was the one pin no
//       check read, and it drifted a full minor below the floor of the
//       optimizations `CERBERUS_CH_OPTIMIZATIONS=auto` is documented to
//       select — invisibly, because every CI lane overrides the chart's image,
//       so the shipped default was exercised nowhere.
//   (f) compatibility.yml's `prometheus-floor` job's CH_IMAGE == min_clickhouse.
//       That job (see #1500) exists specifically to run the prometheus
//       differential corpus against cerberus's declared floor instead of
//       quickstart_clickhouse, forcing internal/chopt's auto-picker to resolve every
//       above-floor feature OFF. If minCHBase moves without moving this job's
//       CH_IMAGE (or vice versa), the floor lane silently stops testing the
//       floor it claims to — (f) is what makes check (c)'s DELIBERATE
//       exception (this one lane, and only this lane, runs below
//       quickstart_clickhouse) a tracked invariant instead of an unenforced
//       escape hatch from (c).
//   (h) perf-nightly's sentinel matrix measures both of move_to_prewhere's
//       PREWHERE paths on the servers that matter: it contains
//       min_clickhouse (the supported floor) and quickstart_clickhouse (the
//       deployment pin), at least one server inside move_to_prewhere's
//       AutoRange (read from the registry, where ClickHouse places PREWHERE)
//       and one outside it (where the emitter does). Each server's image,
//       `clickhouse/clickhouse-server:<server>-alpine`, is one just/common.just
//       pins; the self-check's matrix is the same list; and
//       test/perf/nightly-baseline.json carries one bound set per server and
//       no other. A bump of either pin that leaves perf-nightly on the old
//       server, or a server added without its calibrated bounds, fails here.
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
import {
  PERF_NIGHTLY_JOB,
  PERF_NIGHTLY_WORKFLOW,
  SELFCHECK_JOB,
  SELFCHECK_WORKFLOW,
  readClickHouseMatrix,
} from './lib/perf-nightly-matrix.mjs';
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

// The quickstart's CERBERUS_CH_OPTIMIZATIONS setting, as a discriminated
// value: { mode: 'auto' } | { mode: 'off' } | { mode: 'explicit', ids: [...] }.
//
// `auto` is NOT "no features". It is the setting the quickstart actually
// ships, and it selects every AutoSelect feature the server is new enough
// for — so reading it as an empty list (which an earlier `.filter(s => s !==
// 'auto')` did) made check (d), the one this file calls THE CRITICAL ONE,
// pass vacuously on the real repository: it derived a floor from nothing.
function readComposeOptimizations(text) {
  const m = /CERBERUS_CH_OPTIMIZATIONS:\s*"([^"]*)"/.exec(text);
  if (!m) return null;
  const raw = m[1]
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
  if (raw.length === 1 && (raw[0] === 'auto' || raw[0] === 'off')) return { mode: raw[0] };
  return { mode: 'explicit', ids: raw.filter((s) => s !== 'auto' && s !== 'off') };
}

// preflight minCHBase / minCHNativeRate from their Version struct literals:
//   var minCHBase = chopt.Version{Major: 24, Minor: 8}
function readPreflightFloor(text, name) {
  const re = new RegExp(`var ${name} = chopt\\.Version\\{Major:\\s*(\\d+),\\s*Minor:\\s*(\\d+)\\}`);
  const m = re.exec(text);
  return m ? { major: Number(m[1]), minor: Number(m[2]) } : null;
}

// The per-feature registry from internal/chopt/registry.go. Returns a map of
// feature id -> { floor, autoSelect } where floor is { major, minor } or null
// (AlwaysAvailable / no floor). The id-constant -> string mapping is read from
// the `const ( FeatureX = "x" )` block so the registry stays the only place
// the floor literals live.
//
// The text is SPLIT on each `ID: FeatureX,` rather than scanned with one
// cross-entry regex: a lazy `[\s\S]*?` between ID and a later field will
// happily run past the end of its own entry into the next one when a field is
// absent, silently attributing a neighbour's value.
function readRegistryEntries(text) {
  // id constant -> string literal, e.g. FeatureTSGridRange -> "ts_grid_range".
  const idToString = {};
  const constRe = /(\bFeature[A-Za-z0-9]+)\s*=\s*"([^"]+)"/g;
  for (let m; (m = constRe.exec(text)); ) idToString[m[1]] = m[2];

  const entries = {};
  const chunks = text.split(/ID:\s*(Feature[A-Za-z0-9]+),/);
  // chunks = [preamble, id1, body1, id2, body2, ...].
  for (let i = 1; i + 1 < chunks.length; i += 2) {
    const id = idToString[chunks[i]] ?? chunks[i];
    const body = chunks[i + 1];
    const mv = /MinVersion:\s*(?:Version\{Major:\s*(\d+),\s*Minor:\s*(\d+)\}|(AlwaysAvailable))/.exec(body);
    const auto = /AutoSelect:\s*(true|false)/.exec(body);
    const range =
      /AutoRange:\s*VersionRange\{From:\s*Version\{Major:\s*(\d+),\s*Minor:\s*(\d+)\},\s*Until:\s*Version\{Major:\s*(\d+),\s*Minor:\s*(\d+)\}\}/.exec(
        body,
      );
    entries[id] = {
      floor: !mv || mv[3] === 'AlwaysAvailable' ? null : { major: Number(mv[1]), minor: Number(mv[2]) },
      autoSelect: auto ? auto[1] === 'true' : false,
      autoRange: range
        ? {
            from: { major: Number(range[1]), minor: Number(range[2]) },
            until: { major: Number(range[3]), minor: Number(range[4]) },
          }
        : null,
    };
  }
  return entries;
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

  // (c) every compatibility-lane clickhouse image tag == quickstart_clickhouse.
  for (const [name, text] of Object.entries(sources.compatibility)) {
    const tag = parseVersion(readComposeCHTag(text));
    if (!tag) {
      failures.push(`${name}: could not read the clickhouse image tag`);
    } else if (quickstart && !sameMM(tag, quickstart)) {
      failures.push(
        `(c) ${name} clickhouse tag ${vstr(tag)} != versions.yaml quickstart_clickhouse ${vstr(quickstart)}. ` +
          'The differential harnesses run on the deployment surface, not on chdb_substrate.',
      );
    }
  }
  if (quickstart) notes.push(`(c) compatibility lanes == quickstart ${vstr(quickstart)}`);

  // (d) THE CRITICAL ONE: quickstart >= highest floor among enabled features.
  // With CERBERUS_CH_OPTIMIZATIONS="auto" (what the quickstart ships) the
  // enabled set is every AutoSelect feature in the registry, so the derived
  // floor is the highest AutoSelect floor: the quickstart must be able to
  // demonstrate everything `auto` is documented to turn on.
  const registry = readRegistryEntries(sources.registry);
  const enabled = readComposeOptimizations(sources.compose);
  let selected = null;
  if (!enabled) {
    failures.push('docker-compose.yml: could not read CERBERUS_CH_OPTIMIZATIONS');
  } else if (enabled.mode === 'auto') {
    selected = Object.keys(registry).filter((id) => registry[id].autoSelect);
    if (selected.length === 0) {
      failures.push('(d) CERBERUS_CH_OPTIMIZATIONS="auto" but the chopt registry declares no AutoSelect feature');
      selected = null;
    }
  } else if (enabled.mode === 'off') {
    notes.push('(d) quickstart sets CERBERUS_CH_OPTIMIZATIONS="off" — no feature floor to satisfy');
  } else {
    selected = enabled.ids;
  }
  if (selected && quickstart) {
    let highest = null;
    let highestFeature = null;
    for (const id of selected) {
      if (!(id in registry)) {
        failures.push(`(d) docker-compose enables "${id}", which is not a known chopt registry feature`);
        continue;
      }
      const floor = registry[id].floor; // null = AlwaysAvailable (no floor)
      if (floor && (highest === null || atLeast(floor, highest))) {
        highest = floor;
        highestFeature = id;
      }
    }
    const how = enabled.mode === 'auto' ? `auto-selected (${selected.length} features)` : 'explicitly enabled';
    if (highest && !atLeast(quickstart, highest)) {
      failures.push(
        `(d) quickstart_clickhouse ${vstr(quickstart)} is TOO OLD for ${how} feature "${highestFeature}" ` +
          `(floor ${vstr(highest)}). Bump the quickstart (docker-compose + compatibility image tags) and ` +
          `versions.yaml to a ClickHouse that supports it - see docs/optimization-rules.md (Rule 1, step 4).`,
      );
    } else if (highest) {
      notes.push(
        `(d) quickstart ${vstr(quickstart)} >= highest ${how} floor ${vstr(highest)} ("${highestFeature}")`,
      );
    } else {
      notes.push(`(d) quickstart enables no version-gated feature (${how}, all AlwaysAvailable)`);
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

  // (g) the Helm chart's bundled ClickHouse tracks the DEPLOYMENT surface too.
  const chartTag = parseVersion(readComposeCHTag(sources.chartValues));
  if (!chartTag) {
    failures.push('deploy/helm/cerberus/values.yaml: could not read clickhouse.bundled.image');
  } else if (quickstart && !sameMM(chartTag, quickstart)) {
    failures.push(
      `(g) chart clickhouse.bundled.image ${vstr(chartTag)} != versions.yaml quickstart_clickhouse ` +
        `${vstr(quickstart)}. The chart is the deployment surface operators run, so its bundled server ` +
        `tracks quickstart_clickhouse - not chdb_substrate. A lower tag ships a default install on which ` +
        `CERBERUS_CH_OPTIMIZATIONS=auto resolves fewer optimizations than the docs promise, and no CI lane ` +
        `would notice because every lane overrides this image.`,
    );
  } else if (quickstart) {
    notes.push(`(g) chart bundled image == quickstart ${vstr(quickstart)}`);
  }

  // (f) the prometheus-floor lane's CH_IMAGE == min_clickhouse. This is the
  // ONE lane that deliberately runs below quickstart_clickhouse (see #1500); this
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

  checkPerfNightlyMatrix(sources, { minCH, quickstart, registry }, failures, notes);

  return { failures, notes };
}

// PERF_NIGHTLY_SPLIT_FEATURE is the chopt feature whose auto range decides
// who places PREWHERE, the decision perf-nightly's matrix measures both
// sides of.
const PERF_NIGHTLY_SPLIT_FEATURE = 'move_to_prewhere';

// alpineImage — the image a perf-nightly leg boots for a server.
function alpineImage(server) {
  return `clickhouse/clickhouse-server:${server}-alpine`;
}

// readJustImages — every clickhouse-server image a `NAME := "image"` line
// of just/common.just pins.
function readJustImages(text) {
  const images = new Set();
  const re = /^[A-Z0-9_]+\s*:=\s*"(clickhouse\/clickhouse-server:[^"]+)"/gm;
  for (let m; (m = re.exec(text)); ) images.add(m[1]);
  return images;
}

// (h) — see the header.
function checkPerfNightlyMatrix(sources, { minCH, quickstart, registry }, failures, notes) {
  const failuresBefore = failures.length;
  const matrix = readClickHouseMatrix(sources.perfNightlyWorkflow, PERF_NIGHTLY_JOB);
  if (!matrix || matrix.length === 0) {
    failures.push(`(h) ${PERF_NIGHTLY_WORKFLOW}: could not read job ${PERF_NIGHTLY_JOB}'s clickhouse matrix`);
    return;
  }
  if (matrix.some((raw) => !/^\d+\.\d+$/.test(raw))) {
    failures.push(`(h) ${PERF_NIGHTLY_WORKFLOW}: the clickhouse matrix [${matrix}] holds a value that is not major.minor`);
    return;
  }
  const servers = matrix.map(parseVersion);
  if (new Set(matrix).size !== matrix.length) {
    failures.push(`(h) ${PERF_NIGHTLY_WORKFLOW}: the clickhouse matrix [${matrix}] repeats a server`);
  }
  const has = (want) => servers.some((v) => sameMM(v, want));
  for (const [key, want] of [
    ['min_clickhouse', minCH],
    ['quickstart_clickhouse', quickstart],
  ]) {
    if (want && !has(want)) {
      failures.push(
        `(h) perf-nightly's clickhouse matrix [${matrix}] does not run on versions.yaml ${key} ${vstr(want)}`,
      );
    }
  }

  const range = registry[PERF_NIGHTLY_SPLIT_FEATURE]?.autoRange;
  if (!range) {
    failures.push(`(h) internal/chopt/registry.go: could not read ${PERF_NIGHTLY_SPLIT_FEATURE}'s AutoRange`);
  } else {
    const inside = (v) => atLeast(v, range.from) && !atLeast(v, range.until);
    const bounds = `[${vstr(range.from)}, ${vstr(range.until)})`;
    if (!servers.some(inside)) {
      failures.push(
        `(h) perf-nightly's clickhouse matrix [${matrix}] has no server inside ${PERF_NIGHTLY_SPLIT_FEATURE}'s ` +
          `auto range ${bounds}, so ClickHouse's own PREWHERE placement is measured nowhere`,
      );
    }
    if (!servers.some((v) => !inside(v))) {
      failures.push(
        `(h) perf-nightly's clickhouse matrix [${matrix}] has no server outside ${PERF_NIGHTLY_SPLIT_FEATURE}'s ` +
          `auto range ${bounds}, so the emitter's explicit PREWHERE split is measured nowhere`,
      );
    }
  }

  const pinned = readJustImages(sources.justCommon);
  for (const server of matrix) {
    if (!pinned.has(alpineImage(server))) {
      failures.push(`(h) perf-nightly runs ${alpineImage(server)}, which no just/common.just image variable pins`);
    }
  }

  const selfcheck = readClickHouseMatrix(sources.selfcheckWorkflow, SELFCHECK_JOB);
  if (!selfcheck || [...selfcheck].sort().join() !== [...matrix].sort().join()) {
    failures.push(
      `(h) ${SELFCHECK_WORKFLOW}'s clickhouse matrix [${selfcheck ?? 'unreadable'}] is not perf-nightly's [${matrix}]`,
    );
  }

  let baselineServers = null;
  try {
    baselineServers = (JSON.parse(sources.nightlyBaseline).versions ?? []).map((v) => v.clickhouse);
  } catch (e) {
    failures.push(`(h) test/perf/nightly-baseline.json: ${e.message}`);
  }
  if (baselineServers && [...baselineServers].sort().join() !== [...matrix].sort().join()) {
    failures.push(
      `(h) test/perf/nightly-baseline.json carries bound sets for [${baselineServers}], not for perf-nightly's ` +
        `servers [${matrix}] — calibrate a server's bounds when adding it, drop them when removing it`,
    );
  }
  if (failures.length === failuresBefore) {
    notes.push(`(h) perf-nightly runs on [${matrix}], with a bound set per server`);
  }
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
    chartValues: readFile('deploy/helm/cerberus/values.yaml'),
    compatibilityWorkflow: readFile('.github/workflows/compatibility.yml'),
    perfNightlyWorkflow: readFile(PERF_NIGHTLY_WORKFLOW),
    selfcheckWorkflow: readFile(SELFCHECK_WORKFLOW),
    nightlyBaseline: readFile('test/perf/nightly-baseline.json'),
    justCommon: readFile('just/common.just'),
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
// synthetic fixtures, the same contract forbid-skip.test.mjs provides for
// forbid-skip.mjs. Asserts a consistent fixture passes and that each of
// the checks (a)/(b)/(c)/(d)/(e)/(f)/(g)/(h) FAILS when its source is deliberately
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
  FeatureMoveToPrewhere = "move_to_prewhere"
)
var registry = []Feature{
  { ID: FeatureAggregationInOrder, MinVersion: Version{Major: 24, Minor: 8}, AutoSelect: true, },
  { ID: FeatureConditionCache, MinVersion: Version{Major: 25, Minor: 3}, AutoSelect: true, },
  { ID: FeatureTSGridRange, MinVersion: Version{Major: 25, Minor: 6}, AutoSelect: true, },
  { ID: FeatureColumnarResultDecode, MinVersion: AlwaysAvailable, AutoSelect: false, },
  { ID: FeatureMoveToPrewhere, MinVersion: AlwaysAvailable, AutoSelect: true,
    AutoRange:  VersionRange{From: Version{Major: 24, Minor: 11}, Until: Version{Major: 26, Minor: 2}}, },
}`;
  const entries = readRegistryEntries(fakeRegistry);
  ok(
    'registry ts_grid_range floor 25.6',
    entries['ts_grid_range']?.floor?.major === 25 && entries['ts_grid_range']?.floor?.minor === 6,
  );
  ok('registry condition_cache floor 25.3', entries['condition_cache']?.floor?.minor === 3);
  ok('registry columnar_result_decode AlwaysAvailable -> null floor', entries['columnar_result_decode'].floor === null);
  ok('registry reads AutoSelect true', entries['ts_grid_range']?.autoSelect === true);
  ok('registry reads AutoSelect false', entries['columnar_result_decode']?.autoSelect === false);
  ok(
    'registry reads an AutoRange',
    JSON.stringify(entries['move_to_prewhere']?.autoRange) ===
      JSON.stringify({ from: { major: 24, minor: 11 }, until: { major: 26, minor: 2 } }),
  );
  ok('registry reads no AutoRange as null', entries['ts_grid_range']?.autoRange === null);
  ok(
    'registry does not attribute a neighbour AutoSelect to an entry that declares none',
    readRegistryEntries(
      'const (\n  FeatureA = "a"\n  FeatureB = "b"\n)\n' +
        'var registry = []Feature{\n' +
        '  { ID: FeatureA, MinVersion: AlwaysAvailable, },\n' +
        '  { ID: FeatureB, MinVersion: AlwaysAvailable, AutoSelect: true, },\n}',
    )['a'].autoSelect === false,
  );

  // --- unit: CERBERUS_CH_OPTIMIZATIONS reader ---
  ok(
    'readComposeOptimizations reads "auto" as a mode, not an empty list',
    readComposeOptimizations('CERBERUS_CH_OPTIMIZATIONS: "auto"\n')?.mode === 'auto',
  );
  ok(
    'readComposeOptimizations reads "off" as a mode',
    readComposeOptimizations('CERBERUS_CH_OPTIMIZATIONS: "off"\n')?.mode === 'off',
  );
  ok(
    'readComposeOptimizations reads an explicit list',
    readComposeOptimizations('CERBERUS_CH_OPTIMIZATIONS: "a, b"\n')?.ids?.join(',') === 'a,b',
  );

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
      'compatibility/prometheus/docker-compose.yml': 'image: ${CH_IMAGE:-clickhouse/clickhouse-server:26.5.7.64}\n',
      'compatibility/loki/docker-compose.yml': 'image: clickhouse/clickhouse-server:26.5.7.64\n',
      'compatibility/tempo/docker-compose.yml': 'image: clickhouse/clickhouse-server:26.5\n',
    },
    // The migration tier-1 stack tracks the quickstart tag (26.5 here), NOT
    // the 25.8 chDB substrate.
    migrationTier1: 'image: clickhouse/clickhouse-server:26.5\n',
    // The chart's bundled ClickHouse tracks the quickstart tag for the same
    // deployment-surface reason.
    chartValues: '  bundled:\n    image: clickhouse/clickhouse-server:26.5\n',
    compatibilityWorkflow:
      'jobs:\n  prometheus-floor:\n    steps:\n      - run: echo seed\n        env:\n          CH_IMAGE: "clickhouse/clickhouse-server:24.8"\n',
    // perf-nightly runs the floor, one server inside move_to_prewhere's auto
    // range and the quickstart, each pinned in just/common.just and each with
    // a bound set.
    perfNightlyWorkflow:
      "jobs:\n  perf-nightly-sentinels:\n    strategy:\n      matrix:\n        clickhouse: ['24.8', '25.9', '26.5']\n" +
      '  perf-nightly:\n    steps: []\n',
    selfcheckWorkflow: "jobs:\n  perf-nightly-selfcheck:\n    strategy:\n      matrix:\n        clickhouse: ['24.8', '25.9', '26.5']\n",
    nightlyBaseline: JSON.stringify({
      versions: [{ clickhouse: '24.8' }, { clickhouse: '25.9' }, { clickhouse: '26.5' }],
    }),
    justCommon:
      'CH_TEST_IMAGE := "clickhouse/clickhouse-server:25.9-alpine"\n' +
      'CH_TEST_IMAGE_PRIOR := "clickhouse/clickhouse-server:24.8-alpine"\n' +
      'CH_STRICT_SCAN_IMAGE := "clickhouse/clickhouse-server:26.5-alpine"\n',
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
    '(c) compatibility tag drift is caught',
    drift((s) => (s.compatibility['compatibility/loki/docker-compose.yml'] = 'image: clickhouse/clickhouse-server:25.6\n')).some(
      (f) => f.startsWith('(c)'),
    ),
  );
  ok(
    '(c) compatibility pinned to the chDB substrate instead of the quickstart is caught',
    // 25.8 is chdb_substrate in the fixture.
    drift((s) => (s.compatibility['compatibility/tempo/docker-compose.yml'] = 'image: clickhouse/clickhouse-server:25.8\n')).some(
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
    '(d) "auto" derives the floor from the registry\'s AutoSelect set, not from nothing',
    // Quickstart 25.3 with auto: ts_grid_range (AutoSelect, floor 25.6) still binds.
    drift((s) => {
      s.versionsYaml = s.versionsYaml.replace('quickstart_clickhouse: "26.5"', 'quickstart_clickhouse: "25.3"');
      s.compose = s.compose
        .replace('clickhouse-server:26.5', 'clickhouse-server:25.3')
        .replace('"aggregation_in_order,condition_cache,ts_grid_range"', '"auto"');
      s.migrationTier1 = 'image: clickhouse/clickhouse-server:25.3\n';
      s.chartValues = '  bundled:\n    image: clickhouse/clickhouse-server:25.3\n';
    }).some((f) => f.startsWith('(d)') && f.includes('auto-selected')),
  );
  ok(
    '(d) "auto" ignores an opt-in-only feature\'s floor',
    // columnar_result_decode is AutoSelect:false, so a 25.3 quickstart under
    // "auto" is bounded by ts_grid_range's 25.6 and nothing higher.
    drift((s) => {
      s.compose = s.compose.replace('"aggregation_in_order,condition_cache,ts_grid_range"', '"auto"');
    }).length === 0,
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
    '(g) chart bundled-ClickHouse tag drift is caught',
    drift((s) => (s.chartValues = '  bundled:\n    image: clickhouse/clickhouse-server:25.3\n')).some((f) =>
      f.startsWith('(g)'),
    ),
  );
  ok(
    '(g) chart pinned to the chDB substrate instead of the quickstart is caught',
    // 25.8 is chdb_substrate in the fixture - the exact wrong-role mistake (g) rejects.
    drift((s) => (s.chartValues = '  bundled:\n    image: clickhouse/clickhouse-server:25.8\n')).some((f) =>
      f.startsWith('(g)'),
    ),
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

  const hFails = (mutate) => drift(mutate).filter((f) => f.startsWith('(h)'));
  ok(
    'readClickHouseMatrix reads the sentinel job\'s list',
    readClickHouseMatrix(goodSources.perfNightlyWorkflow, PERF_NIGHTLY_JOB)?.join() === '24.8,25.9,26.5',
  );
  ok('readClickHouseMatrix missing job -> null', readClickHouseMatrix('jobs:\n  other:\n', PERF_NIGHTLY_JOB) === null);
  ok(
    '(h) a quickstart bump perf-nightly does not follow is caught',
    hFails((s) => {
      s.versionsYaml = s.versionsYaml.replace('quickstart_clickhouse: "26.5"', 'quickstart_clickhouse: "26.6"');
      s.compose = s.compose.replace('clickhouse-server:26.5', 'clickhouse-server:26.6');
      s.migrationTier1 = 'image: clickhouse/clickhouse-server:26.6\n';
      s.chartValues = '  bundled:\n    image: clickhouse/clickhouse-server:26.6\n';
    }).some((f) => f.includes('quickstart_clickhouse')),
  );
  ok(
    '(h) a matrix without the supported floor is caught',
    hFails((s) => (s.perfNightlyWorkflow = s.perfNightlyWorkflow.replace("'24.8', ", ''))).some((f) =>
      f.includes('min_clickhouse'),
    ),
  );
  ok(
    "(h) a matrix with no server inside move_to_prewhere's auto range is caught",
    hFails((s) => (s.perfNightlyWorkflow = s.perfNightlyWorkflow.replace("'25.9'", "'24.8'"))).some((f) =>
      f.includes('inside'),
    ),
  );
  ok(
    "(h) a matrix with no server outside move_to_prewhere's auto range is caught",
    hFails((s) => {
      s.registry = s.registry.replace('Until: Version{Major: 26, Minor: 2}', 'Until: Version{Major: 99, Minor: 0}');
      s.registry = s.registry.replace('From: Version{Major: 24, Minor: 11}', 'From: Version{Major: 24, Minor: 1}');
    }).some((f) => f.includes('outside')),
  );
  ok(
    '(h) a server whose image just/common.just does not pin is caught',
    hFails((s) => (s.justCommon = s.justCommon.replace('26.5-alpine', '26.4-alpine'))).some((f) =>
      f.includes('pins'),
    ),
  );
  ok(
    "(h) a self-check matrix that differs from perf-nightly's is caught",
    hFails((s) => (s.selfcheckWorkflow = s.selfcheckWorkflow.replace("'26.5'", "'26.4'"))).some((f) =>
      f.includes('perf-nightly-selfcheck'),
    ),
  );
  ok(
    '(h) a server without a bound set is caught',
    hFails((s) => (s.nightlyBaseline = JSON.stringify({ versions: [{ clickhouse: '25.9' }] }))).some((f) =>
      f.includes('nightly-baseline.json'),
    ),
  );
  ok(
    '(h) a non major.minor matrix value is caught',
    hFails((s) => (s.perfNightlyWorkflow = s.perfNightlyWorkflow.replace("'25.9'", "'25.9-alpine'"))).length > 0,
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
