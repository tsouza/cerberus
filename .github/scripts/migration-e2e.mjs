// migration-e2e.mjs — the Layer-14 migration-scenario lane's coverage
// ratchet, tier-matrix emitter and scenario runner (migration-e2e.yml).
//
// What this gates
// ---------------
// docs/migration-testing.md section 4 lists 26 migration user-stories. Each
// one is executed by a Gherkin `Scenario` under test/e2e/migration/features,
// tagged `@MIG-nn` (story), `@tier0`/`@tier1`/`@tier2` (tier) and
// `@archetype:<name>` (fixture set). `test/e2e/migration/cmd/scenarios` walks
// those features with godog's own parser and emits one JSON record per
// Scenario node, so exactly one Gherkin parser exists in the tree. This
// script consumes that JSON — it never parses Gherkin itself — and holds the
// feature tree to the story list.
//
// Three modes (env MODE, or argv[2]; default `verify`):
//
//   - verify : the ratchet. Derives its anchors LIVE from the doc (the story
//              table in section 4, the Tier(s) column in section 6, the
//              archetype table in section 7) and asserts the scenario set
//              against them. Every violation is collected and reported —
//              never fail-fast — as `::error::` plus a summary naming the
//              file to edit, then exit 1. Pure file walking, seconds, so it
//              runs on the required `lint` job and a drift is a blocking PR
//              failure rather than a nightly surprise.
//   - emit   : re-runs every verify assertion first (a removed verify step
//              must never let a silently-incomplete matrix ship), then writes
//              the `strategy.matrix` JSON for the tier job(s) the workflow
//              runs.
//   - run    : drives the godog suite for the requested tier filter and
//              reports. `go test`'s exit status is the verdict — this script
//              parses no logs and re-derives no result.
//
// The coverage floor in COVERAGE_BASELINE is an AGGREGATE integer, the same
// shape compat-ratchet.mjs pins. It is NOT an allow-list: it names no story,
// excuses no scenario, and every structural assertion below applies
// unconditionally to every scenario that exists, so a WRONG story fails
// regardless of the count. Coverage growing is also a failure — the baseline
// must be raised in the same reviewed diff, which is what makes the ratchet
// tighten instead of ossify.
//
// Env:
//   MODE               verify | emit | run                    (default: verify)
//   SCENARIOS_JSON     path to the cmd/scenarios output
//                                      (default: build/migration-scenarios.json)
//   STORIES_DOC        the 26-story anchor
//                                          (default: docs/migration-testing.md)
//   ARCHETYPE_ROOT     archetype fixture root
//                                  (default: test/e2e/migration/archetypes)
//   FEATURE_ROOT       feature dir, for the one-file-per-story rule
//                                    (default: test/e2e/migration/features)
//   COVERAGE_BASELINE  the raise-only coverage floor
//                            (default: test/e2e/migration/coverage-baseline.json)
//   TIER               all | tier0 | tier1 | tier2            (emit, run; default: all)
//   STORY              a single MIG id to drive               (run; optional)
//   CERBERUS_BIN       prebuilt binary the runner execs       (run; optional)
//   GITHUB_OUTPUT      runner file the matrix JSON is appended to (emit)
//
// Exit: 0 = clean, 1 = a coverage/correctness violation, an unknown MODE, a
//       missing or malformed SCENARIOS_JSON/baseline, a requested tier the
//       workflow has no job for, or a failed suite run.
//
// node: builtins only (via lib/gh.mjs) — no npm deps, no setup-node needed.

import { readFileSync, readdirSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import process from 'node:process';
import { error, notice, log, setOutput, appendStepSummary } from './lib/gh.mjs';

const SCENARIOS_JSON = process.env.SCENARIOS_JSON || 'build/migration-scenarios.json';
const STORIES_DOC = process.env.STORIES_DOC || 'docs/migration-testing.md';
const ARCHETYPE_ROOT = process.env.ARCHETYPE_ROOT || 'test/e2e/migration/archetypes';
const FEATURE_ROOT = process.env.FEATURE_ROOT || 'test/e2e/migration/features';
const COVERAGE_BASELINE =
  process.env.COVERAGE_BASELINE || 'test/e2e/migration/coverage-baseline.json';

// The enumerator stamps this into its output; a reader that does not
// understand the version rejects the document rather than misreading it.
export const SCENARIOS_SCHEMA_VERSION = 1;

// The baseline carries its own version for the same reason.
export const BASELINE_SCHEMA_VERSION = 1;

// The tiers a Scenario may declare.
export const KNOWN_TIERS = ['tier0', 'tier1', 'tier2'];

// Per-tier `timeout-minutes` for the matrix entry. It travels ON the entry so
// the ceiling cannot drift from the shard it bounds. Tier 0 is an offline
// godog suite over committed fixtures — one `go build` plus seconds of file
// reading — so 15 minutes is generous headroom over a healthy run and still
// fails fast on a hang.
export const TIER0_TIMEOUT_MIN = 15;

// The tiers migration-e2e.yml has a job for, keyed by their ceiling — one
// table, so a tier cannot have a job without a bound or a bound without a job.
// A scenario tagged with a tier absent from here is a scenario that would
// silently never execute, so emit rejects it rather than producing a matrix
// entry no job consumes. Each tier's dispatch option, job stanza and ceiling
// land with its first scenario.
export const TIER_TIMEOUT_MINUTES = { tier0: TIER0_TIMEOUT_MIN };
export const RUNNABLE_TIERS = Object.keys(TIER_TIMEOUT_MINUTES);

// The Go package the tier-0 suite lives in, and the tag that selects it.
const TIER0_PACKAGE = './test/e2e/migration/tiers/tier0-offline/...';
const MIGRATION_BUILD_TAG = 'migration';

// The Go package the tier-1 dual-backend suite lives in, and the tag that
// selects it — shared with tier1_stack_test.go / tier1_parity_test.go, the
// plain-Go substrate self-checks, so one `go test` invocation exercises the
// substrate and the Gherkin suite together against the same live stack.
//
// TIER1_PACKAGE/MIGRATION_TIER1_BUILD_TAG exist so `MODE=run TIER=tier1`
// works manually/locally today (`just migration-tier1-run` drives the whole
// `./test/e2e/migration/...` tree, which already includes this package).
// tier1 is deliberately NOT added to TIER_TIMEOUT_MINUTES/RUNNABLE_TIERS in
// this change: doing so would flip V16 (every RUNNABLE tier a story
// declares in section 6 must have a matching Scenario) on for every OTHER
// tier1 story section 6 lists (MIG-02, MIG-06 .. MIG-15, MIG-20 .. MIG-26's
// tier-1 half) — the required `lint` job's coverage ratchet, not merely this
// lane — long before those scenarios exist. Flipping RUNNABLE_TIERS is the
// same kind of gate-flip moment `compatibility.yml` went through once its
// three heads were actually green (see CLAUDE.md); it lands in the PR that
// finishes tier1's section-6 coverage and wires migration-e2e.yml's tier1
// job (deliberately out of THIS change's scope), not here.
const TIER1_PACKAGE = './test/e2e/migration/tiers/tier1-dual/...';
const MIGRATION_TIER1_BUILD_TAG = 'migration_tier1';

// A story id is exactly `MIG-` plus two digits; the doc's list runs from
// MIG-01 to MIG-26 with no gaps, which V1 asserts structurally.
const STORY_ID_RE = /^MIG-\d{2}$/;
const FIRST_STORY_ORDINAL = 1;

// --- doc parsing: the durable anchors ---------------------------------------

// sectionOf — the text between the `## <n>.` heading and the next `## `
// heading. The anchors are markdown tables inside numbered sections, so the
// parsers below scope themselves to a section rather than scanning the whole
// document and picking up an example from the harness-shape prose.
export function sectionOf(docText, number) {
  const start = new RegExp(String.raw`^##\s+${number}\.\s`, 'm').exec(docText);
  if (start === null) return '';
  const rest = docText.slice(start.index + start[0].length);
  const end = /^##\s+\d+\.\s/m.exec(rest);
  return end === null ? rest : rest.slice(0, end.index);
}

// parseStories — the story ids from section 4's tables. This list is the
// durable anchor the ratchet diffs against: deleting a row here is what V1/V2
// turn into a failure, so a story cannot quietly leave the lane.
export function parseStories(docText) {
  const ids = [];
  const re = /^\|\s*(MIG-\d{2})\s*\|/gm;
  let m;
  const section = sectionOf(docText, 4);
  while ((m = re.exec(section)) !== null) ids.push(m[1]);
  return [...new Set(ids)].sort();
}

// parseTierMap — story -> the tiers section 6's Tier(s) column declares. The
// cell is prose ("0", "0, 1", "1 (three-signal)"), so the tiers are the
// digits it contains: every digit is a tier ordinal, and a story declaring
// two tiers carries one Scenario per tier rather than one Scenario with two
// tier tags.
export function parseTierMap(docText) {
  const map = new Map();
  const re = /^\|\s*(MIG-\d{2})\s*\|([^|]*)\|/gm;
  let m;
  const section = sectionOf(docText, 6);
  while ((m = re.exec(section)) !== null) {
    const tiers = [...new Set((m[2].match(/\d/g) || []).map((d) => `tier${d}`))].sort();
    map.set(m[1], tiers);
  }
  return map;
}

// parseArchetypes — the archetype names from section 7's table. The first
// column IS the directory name under ARCHETYPE_ROOT, the `@archetype:` tag
// value and the matrix key, so one string carries all three.
export function parseArchetypes(docText) {
  const names = [];
  const re = /^\|\s*([a-z][a-z0-9-]*)\s*\|/gm;
  let m;
  const section = sectionOf(docText, 7);
  while ((m = re.exec(section)) !== null) names.push(m[1]);
  return [...new Set(names)].sort();
}

// --- filesystem anchors -----------------------------------------------------

function readDirNames(dir, kind) {
  let entries;
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch (e) {
    error(`migration-e2e: cannot read ${kind} directory ${dir}: ${e.message}`);
    process.exit(1);
  }
  return entries;
}

// archetypeDirs — the directories that actually ship fixtures.
export function archetypeDirs(root = ARCHETYPE_ROOT) {
  return readDirNames(root, 'archetype')
    .filter((e) => e.isDirectory())
    .map((e) => e.name)
    .sort();
}

// featureFileNames — the `.feature` basenames under the feature root. A file
// that contributes no Scenario is caught by V8, so an empty feature cannot sit
// in the tree looking like coverage.
export function featureFileNames(root = FEATURE_ROOT) {
  return readDirNames(root, 'feature')
    .filter((e) => e.isFile() && e.name.endsWith('.feature'))
    .map((e) => e.name)
    .sort();
}

// --- the violation set ------------------------------------------------------

// stripPlaceholders — a `<name>` Scenario Outline placeholder is the one
// legal use of angle brackets in step text; the operator-token scan looks at
// what is left after they are removed. Strips to a fixed point rather than a
// single pass, so a removal can never re-expose a match it would have missed
// (e.g. `<<foo>bar>` collapsing to a new `<bar>` after the inner tag goes).
function stripPlaceholders(text) {
  let prev;
  let stripped = text;
  do {
    prev = stripped;
    stripped = stripped.replace(/<[A-Za-z_][A-Za-z0-9_]*>/g, '');
  } while (stripped !== prev);
  return stripped;
}

// A step may not carry a numeric literal (an inline epsilon IS a per-case
// allow-list) nor a comparison/arithmetic operator (the relation is named in
// prose; the arithmetic lives in Go). An Examples table row is data, not a
// step, so the Scenario Outline exception needs no special case here.
const STEP_DIGIT_RE = /\d/;
const STEP_OPERATOR_RE = /[<>]=?|==|!=|\+|\*/;

// The Gherkin keyword type godog reports for a `Then`. A Scenario without one
// asserts nothing.
const OUTCOME_KEYWORD_TYPE = 'Outcome';

// collectViolations — every structural rule, evaluated over the whole
// scenario set. Collect-all, never fail-fast: one run reports every problem so
// a fix lands in one pass. Each violation is { code, message }.
export function collectViolations({
  scenarios,
  stories,
  tierMap,
  archetypes,
  archetypeDirNames,
  featureFiles,
  baseline,
}) {
  const v = [];
  const add = (code, message) => v.push({ code, message });

  // V1 — the story anchor in section 4 must still be a contiguous MIG-01..
  // run. A hole means a row was deleted or renumbered; the run's LENGTH is
  // pinned separately by the baseline's stories_total (V2), so truncating the
  // tail is caught there rather than here.
  const expectedIds = stories.map((_, i) => `MIG-${String(i + FIRST_STORY_ORDINAL).padStart(2, '0')}`);
  if (stories.length === 0 || stories.join(',') !== expectedIds.join(',')) {
    add(
      'V1',
      `the story anchor in ${STORIES_DOC} section 4 is not a contiguous MIG-01.. run: parsed [${stories.join(', ')}]`,
    );
  }

  // V2 — the baseline's anchors are derived from the doc, never trusted.
  if (baseline.schema_version !== BASELINE_SCHEMA_VERSION) {
    add(
      'V2',
      `${COVERAGE_BASELINE} declares schema_version ${baseline.schema_version}, want ${BASELINE_SCHEMA_VERSION}`,
    );
  }
  if (baseline.stories_total !== stories.length) {
    add(
      'V2',
      `${COVERAGE_BASELINE} pins stories_total ${baseline.stories_total} but section 4 lists ${stories.length} stories — reconcile the baseline`,
    );
  }
  const declaredScenarioTotal = [...tierMap.values()].reduce((n, tiers) => n + tiers.length, 0);
  if (baseline.scenarios_total !== declaredScenarioTotal) {
    add(
      'V2',
      `${COVERAGE_BASELINE} pins scenarios_total ${baseline.scenarios_total} but section 6's Tier(s) column declares ${declaredScenarioTotal} scenarios — reconcile the baseline`,
    );
  }

  const storySet = new Set(stories);
  const archetypeSet = new Set(archetypes);
  const dirSet = new Set(archetypeDirNames);
  const seenPairs = new Map();
  const filesByStory = new Map();
  const contributingFiles = new Set();

  for (const sc of scenarios) {
    const where = `${sc.feature}:${sc.line} "${sc.name}"`;
    contributingFiles.add(sc.feature.split('/').pop());

    // V3 — exactly one `@MIG-nn` tag. Zero means the scenario belongs to no
    // story; two means the ratchet cannot say which story it covers.
    if (sc.stories.length !== 1) {
      add('V3', `${where} carries ${sc.stories.length} @MIG-nn tags; every Scenario carries exactly one`);
    }

    for (const story of sc.stories) {
      // V4 — a scenario naming a story the doc does not list is stale.
      if (!storySet.has(story)) {
        add('V4', `${where} references @${story}, which section 4 does not list`);
        continue;
      }
      const files = filesByStory.get(story) || new Set();
      files.add(sc.feature);
      filesByStory.set(story, files);
    }

    // V5 — one tier tag per Scenario. A split-tier story carries one Scenario
    // per tier, so a tier list on a single Scenario would hide one of them.
    if (sc.tiers.length !== 1) {
      add('V5', `${where} carries ${sc.tiers.length} tier tags; a split-tier story carries one Scenario per tier`);
    }

    for (const tier of sc.tiers) {
      if (!KNOWN_TIERS.includes(tier)) {
        add('V6', `${where} declares tier "${tier}", which is not one of ${KNOWN_TIERS.join(', ')}`);
        continue;
      }
      // V6 — the tier tag must match what section 6 declares for the story.
      for (const story of sc.stories) {
        if (!storySet.has(story)) continue;
        const declared = tierMap.get(story) || [];
        if (!declared.includes(tier)) {
          add(
            'V6',
            `${where} is tagged @${tier} but section 6 declares ${story} at ${declared.length === 0 ? 'no tier' : declared.join(', ')}`,
          );
        }
      }
      // V7 — one Scenario per (story, tier).
      for (const story of sc.stories) {
        const key = `${story}/${tier}`;
        if (seenPairs.has(key)) {
          add('V7', `${where} duplicates ${key}, already covered by ${seenPairs.get(key)}`);
        } else {
          seenPairs.set(key, where);
        }
      }
    }

    // V9 — an unrecognised tag. This is how `@wip` / `@skip` / `@manual` and
    // every typo reach the ratchet: the enumerator preserves them verbatim.
    for (const tag of sc.unknown_tags) {
      add(
        'V9',
        `${where} carries unrecognised tag ${tag}; a tag is @MIG-nn, @tier0..@tier2 or @archetype:<name>, and scenario-suppressing tags are banned outright`,
      );
    }

    // V10 — an archetype must be one of section 7's and must ship fixtures.
    for (const a of sc.archetypes) {
      if (!archetypeSet.has(a)) {
        add('V10', `${where} references archetype "${a}", which section 7 does not list`);
      } else if (!dirSet.has(a)) {
        add('V10', `${where} references archetype "${a}", which has no directory under ${ARCHETYPE_ROOT}`);
      }
    }

    // V11 — a Scenario with no `Then` asserts nothing. The keyword type comes
    // from the Gherkin AST, so an `And` chained after a `Then` reports as a
    // Conjunction and cannot stand in for the Outcome step.
    if (!sc.steps.some((s) => s.type === OUTCOME_KEYWORD_TYPE)) {
      add('V11', `${where} has no Then step — a Scenario that asserts nothing is a vacuous pass`);
    }

    for (const step of sc.steps) {
      const bare = stripPlaceholders(step.text);
      // V12 — no number in feature text. An inline epsilon is a per-case
      // allow-list; the value belongs in Go beside the relation it bounds.
      if (STEP_DIGIT_RE.test(bare)) {
        add('V12', `${where} step "${step.text}" carries a numeric literal — the arithmetic lives in Go`);
      }
      // V13 — no operators in feature text; name the relation in prose.
      if (STEP_OPERATOR_RE.test(bare)) {
        add('V13', `${where} step "${step.text}" carries an operator — name the relation in prose`);
      }
    }
  }

  // V16 — every RUNNABLE tier a story declares in section 6 must have a
  // matching Scenario. V7 rejects a DUPLICATE (story, tier) pair; this is its
  // required-once twin. Without it, a scenario deleted from the feature tree
  // is invisible here as long as V14/V15's aggregate `scenarios.length` vs
  // `scenarios_covered` floor is kept in lockstep by the same diff — this
  // check is keyed per (story, tier) pulled straight from the doc, so it
  // cannot be silenced by decrementing one baseline number.
  for (const [story, tiers] of tierMap) {
    if (!storySet.has(story)) continue;
    for (const tier of tiers) {
      if (!RUNNABLE_TIERS.includes(tier)) continue;
      const key = `${story}/${tier}`;
      if (!seenPairs.has(key)) {
        add(
          'V16',
          `${STORIES_DOC} section 6 declares ${story} at ${tier}, but no Scenario carries both @${story} and @${tier}`,
        );
      }
    }
  }

  // V8 — one feature file per story, named for it, and no feature file that
  // contributes nothing.
  for (const [story, files] of filesByStory) {
    const list = [...files].sort();
    if (list.length > 1) {
      add('V8', `${story} is spread across ${list.length} feature files (${list.join(', ')}); one story, one file`);
      continue;
    }
    const base = list[0].split('/').pop();
    if (base !== `${story}.feature`) {
      add('V8', `${story} lives in ${list[0]}, but a story's feature file is named ${story}.feature`);
    }
  }
  for (const file of featureFiles) {
    if (!contributingFiles.has(file)) {
      add('V8', `${FEATURE_ROOT}/${file} contributes no Scenario — an empty feature file is not coverage`);
    }
  }

  // V14/V15 — the raise-only coverage floor. Losing a scenario fails; gaining
  // one fails until the baseline is raised in the same reviewed diff, so the
  // ratchet tightens rather than ossifying at the number first written down.
  if (scenarios.length < baseline.scenarios_covered) {
    add(
      'V14',
      `${scenarios.length} scenarios is below the floor of ${baseline.scenarios_covered} in ${COVERAGE_BASELINE} — a story lost its scenario`,
    );
  } else if (scenarios.length > baseline.scenarios_covered) {
    add(
      'V15',
      `coverage grew to ${scenarios.length} scenarios; raise scenarios_covered to ${scenarios.length} in ${COVERAGE_BASELINE}`,
    );
  }

  return v;
}

// --- the matrix -------------------------------------------------------------

// nonRunnableTiers — the requested tiers migration-e2e.yml has no job for.
export function nonRunnableTiers(tiers) {
  return tiers.filter((t) => !RUNNABLE_TIERS.includes(t));
}

// buildMatrix — the `strategy.matrix` for the tier job(s). One entry per tier
// that has scenarios, carrying the stories it drives and its own ceiling, in
// GitHub's `{include: [...]}` form so `matrix: ${{ fromJSON(...) }}` binds
// `matrix.name` / `matrix.tier` / `matrix.timeoutMinutes` directly. A tier
// with no declared ceiling has no job either, so it never reaches here — the
// caller rejects it first, and this throws rather than emitting an entry the
// workflow would read a null timeout out of.
export function buildMatrix(scenarios, { tiers }) {
  const include = [];
  for (const tier of tiers) {
    const timeoutMinutes = TIER_TIMEOUT_MINUTES[tier];
    if (timeoutMinutes === undefined) {
      throw new Error(`buildMatrix: ${tier} has no declared ceiling, so migration-e2e.yml has no job for it`);
    }
    const stories = [
      ...new Set(scenarios.filter((s) => s.tiers.includes(tier)).flatMap((s) => s.stories)),
    ].sort();
    if (stories.length === 0) continue;
    include.push({ name: tier, tier, stories, timeoutMinutes });
  }
  return { include };
}

// requestedTiers — the TIER input resolved to a tier list. `all` means every
// tier the feature tree declares.
export function requestedTiers(tierInput, scenarios) {
  const raw = (tierInput || 'all').trim().toLowerCase();
  if (raw === 'all') {
    return KNOWN_TIERS.filter((t) => scenarios.some((s) => s.tiers.includes(t)));
  }
  if (!KNOWN_TIERS.includes(raw)) return null;
  return [raw];
}

// --- IO ---------------------------------------------------------------------

function readJSON(path, label) {
  let text;
  try {
    text = readFileSync(path, 'utf8');
  } catch (e) {
    error(`migration-e2e: cannot read ${label} at ${path}: ${e.message}`);
    process.exit(1);
  }
  try {
    return JSON.parse(text);
  } catch (e) {
    error(`migration-e2e: ${label} at ${path} is not valid JSON: ${e.message}`);
    process.exit(1);
  }
  return null;
}

function loadScenarios() {
  const doc = readJSON(SCENARIOS_JSON, 'the enumerator output');
  if (doc.schema_version !== SCENARIOS_SCHEMA_VERSION) {
    error(
      `migration-e2e: ${SCENARIOS_JSON} declares schema_version ${doc.schema_version}, want ${SCENARIOS_SCHEMA_VERSION} — regenerate it with test/e2e/migration/cmd/scenarios`,
    );
    process.exit(1);
  }
  if (!Array.isArray(doc.scenarios)) {
    error(`migration-e2e: ${SCENARIOS_JSON} carries no scenarios array`);
    process.exit(1);
  }
  return doc.scenarios;
}

function readDoc() {
  try {
    return readFileSync(STORIES_DOC, 'utf8');
  } catch (e) {
    error(`migration-e2e: cannot read the story anchor at ${STORIES_DOC}: ${e.message}`);
    process.exit(1);
  }
  return '';
}

// --- modes ------------------------------------------------------------------

// runVerify — the ratchet. Returns the loaded scenario set so `emit` can reuse
// it without re-reading, and exits 1 on any violation.
function runVerify() {
  const scenarios = loadScenarios();
  const docText = readDoc();
  const stories = parseStories(docText);
  const tierMap = parseTierMap(docText);
  const archetypes = parseArchetypes(docText);
  const baseline = readJSON(COVERAGE_BASELINE, 'the coverage baseline');

  const violations = collectViolations({
    scenarios,
    stories,
    tierMap,
    archetypes,
    archetypeDirNames: archetypeDirs(),
    featureFiles: featureFileNames(),
    baseline,
  });

  if (violations.length > 0) {
    for (const { code, message } of violations) {
      error(message, { title: `migration-coverage ${code}` });
    }
    error(
      `migration-e2e: ${violations.length} coverage violation(s). The story list in ${STORIES_DOC} section 4 is the anchor; the scenarios live under ${FEATURE_ROOT}; the floor lives in ${COVERAGE_BASELINE}.`,
      { title: 'migration-e2e coverage ratchet' },
    );
    process.exit(1);
  }

  const covered = new Set(scenarios.flatMap((s) => s.stories));
  notice(
    `migration-e2e: scenarios ${scenarios.length}/${baseline.scenarios_total} across ${covered.size}/${stories.length} stories; 0 violations`,
  );
  return scenarios;
}

function runEmit() {
  const scenarios = runVerify();
  const tiers = requestedTiers(process.env.TIER, scenarios);
  if (tiers === null) {
    error(`migration-e2e: unknown TIER "${process.env.TIER}" (want all, ${KNOWN_TIERS.join(', ')})`);
    process.exit(1);
  }
  const notRunnable = nonRunnableTiers(tiers);
  if (notRunnable.length > 0) {
    error(
      `migration-e2e: tier(s) ${notRunnable.join(', ')} have scenarios but migration-e2e.yml has no job for them — land the tier job in the same change as its first scenario, so a tagged scenario is never one that silently never runs`,
    );
    process.exit(1);
  }
  const matrix = buildMatrix(scenarios, { tiers });
  if (matrix.include.length === 0) {
    error(`migration-e2e: no scenario matched TIER "${process.env.TIER || 'all'}" — the lane would run nothing`);
    process.exit(1);
  }
  setOutput('matrix', JSON.stringify(matrix));
  appendStepSummary(
    [
      '### migration scenario matrix',
      '',
      '| tier | stories | timeout (min) |',
      '| --- | --- | --- |',
      ...matrix.include.map((e) => `| \`${e.tier}\` | ${e.stories.join(', ')} | ${e.timeoutMinutes} |`),
    ].join('\n'),
  );
  log(`migration-e2e: emitted ${matrix.include.length} tier entr(y|ies).`);
  process.exit(0);
}

// RUN_TIERS is MODE=run's own supported-tier table — deliberately separate
// from RUNNABLE_TIERS (the coverage-ratchet/matrix table): a tier can be
// driven manually here before RUNNABLE_TIERS declares it has a CI job (see
// the TIER1_PACKAGE comment above). Each entry is the Go package and build
// tag `go test` runs for that tier.
const RUN_TIERS = {
  tier0: { pkg: TIER0_PACKAGE, buildTag: MIGRATION_BUILD_TAG },
  tier1: { pkg: TIER1_PACKAGE, buildTag: MIGRATION_TIER1_BUILD_TAG },
};

function runRun() {
  const scenarios = loadScenarios();
  const story = (process.env.STORY || '').trim();
  const tiers = requestedTiers(process.env.TIER, scenarios);
  const tier = tiers === null || tiers.length !== 1 ? null : tiers[0];
  const run = tier === null ? undefined : RUN_TIERS[tier];
  if (run === undefined) {
    error(
      `migration-e2e: MODE=run drives one tier at a time (${Object.keys(RUN_TIERS).join(', ')}); TIER "${process.env.TIER || 'all'}" resolved to ${tiers === null ? 'an unknown tier' : `[${tiers.join(', ')}]`}`,
    );
    process.exit(1);
  }
  let tags = `@${tier}`;
  if (story !== '') {
    if (!STORY_ID_RE.test(story)) {
      error(`migration-e2e: STORY "${story}" is not a MIG id (want e.g. MIG-04)`);
      process.exit(1);
    }
    const matching = scenarios.filter((s) => s.stories.includes(story) && s.tiers.includes(tier));
    if (matching.length === 0) {
      error(`migration-e2e: STORY ${story} has no @${tier} scenario in ${SCENARIOS_JSON}`);
      process.exit(1);
    }
    tags = `@${story} && @${tier}`;
  }

  const args = ['test', `-tags=${run.buildTag}`, run.pkg, '-count=1', '-v'];
  log(`migration-e2e: go ${args.join(' ')} (MIGRATION_TAGS=${tags})`);
  const res = spawnSync('go', args, {
    stdio: 'inherit',
    env: { ...process.env, MIGRATION_TAGS: tags },
  });
  const status = res.error ? 1 : (res.status ?? 1);
  if (status !== 0) {
    if (res.error) error(`migration-e2e: could not run go test: ${res.error.message}`);
    error(`migration-e2e: the migration scenarios failed for tag filter ${tags}`, {
      title: 'migration-e2e',
    });
    process.exit(1);
  }
  notice(`migration-e2e: the migration scenarios passed for tag filter ${tags}`);
  process.exit(0);
}

// Only dispatch when run as a script — importing for the unit guard must not
// exit the test runner.
const invokedDirectly =
  process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) {
  const mode = (process.env.MODE || process.argv[2] || 'verify').toLowerCase();
  if (mode === 'verify') {
    runVerify();
    process.exit(0);
  } else if (mode === 'emit') {
    runEmit();
  } else if (mode === 'run') {
    runRun();
  } else {
    error(`migration-e2e: unknown MODE "${mode}" (want verify|emit|run)`);
    process.exit(1);
  }
}
