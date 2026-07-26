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
// Five modes (env MODE, or argv[2]; default `verify`):
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
//              parses no logs and re-derives no result. It also OWNS the
//              cucumber-JSON run report's path (see MIGRATION_REPORT below),
//              so a tier that runs always leaves an execution record behind.
//   - attest : the execution gate. Reads the tier run reports back and holds
//              every scenario the ratchet COUNTS to "appeared in a report,
//              and passed there". See "Enumeration is not execution" below.
//   - rollup : the lane verdict from the tier jobs' results.
//
// Enumeration is not execution
// ----------------------------
// `verify` walks feature FILES. On 2026-07-26 it reported "scenarios 30/30
// across 26/26 stories; 0 violations" on a branch whose five tier-2 scenarios
// had never executed once — the tier-2 job had been skipped by a `needs:`
// cascade after tier 1 failed. A scenario that never ran counted exactly the
// same as one that passed, which is the green-but-meaningless shape this repo
// treats as the cardinal sin.
//
// `attest` closes that: each tier's godog suite writes a cucumber-JSON report
// at a path MODE=run chooses, the tier job uploads it, and the aggregate job
// downloads them all and proves every counted scenario is in one with every
// step `passed`. It attests only the tiers this run SELECTED (emit's `tiers`
// output, the same value the roll-up reads) — a dispatch narrowed to
// `tier=tier0` must not fail because tier1's scenarios did not run — and, when
// the dispatch narrowed to one story, only that story's scenarios.
//
// `verify`'s own notice reports enumerated and attested coverage as two
// DIFFERENT numbers, so the caveat is in the gate's output rather than in
// prose somebody has to remember to write.
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
//   MODE               verify | emit | run | attest | rollup  (default: verify)
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
//   PASS_ASSERTION_PIN the section-6 PASS-cell hash pin
//                        (default: test/e2e/migration/pass-assertions.pin.json)
//   REPORT_DIR         where tier run reports are written / read back
//                                     (run, attest, verify's notice;
//                                      default: build/migration-reports)
//   TIER               all | tier0 | tier1 | tier2            (emit, run; default: all)
//   STORY              a single MIG id to drive               (run, attest; optional)
//   CERBERUS_BIN       prebuilt binary the runner execs       (run; optional)
//   MIGRATION_REPORT   NOT an input — MODE=run EXPORTS it to the `go test`
//                      child as the absolute cucumber-JSON report path, and
//                      test/e2e/migration/lib.SuiteFormat reads it. Owned
//                      here so a tier job cannot ship without a report by
//                      forgetting a workflow step.
//   GITHUB_OUTPUT      runner file the matrix + tiers outputs append to (emit)
//   SETUP_RESULT       migration-setup's job result                  (rollup)
//   EXPECTED_TIERS     emit's `tiers` output, verbatim        (rollup, attest)
//   RESULT_TIER0/1/2   each tier job's result, one var per tier      (rollup)
//
// Exit: 0 = clean, 1 = a coverage/correctness violation, an unattested
//       scenario, a PASS-cell pin mismatch, an unknown MODE, a missing or
//       malformed SCENARIOS_JSON/baseline/pin, a requested tier the workflow
//       has no job for, or a failed suite run.
//
// node: builtins only (via lib/gh.mjs) — no npm deps, no setup-node needed.

import { readFileSync, readdirSync, existsSync, mkdirSync, statSync } from 'node:fs';
import { resolve, join } from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import process from 'node:process';
import { error, notice, log, setOutput, appendStepSummary } from './lib/gh.mjs';

const SCENARIOS_JSON = process.env.SCENARIOS_JSON || 'build/migration-scenarios.json';
const STORIES_DOC = process.env.STORIES_DOC || 'docs/migration-testing.md';
const ARCHETYPE_ROOT = process.env.ARCHETYPE_ROOT || 'test/e2e/migration/archetypes';
const FEATURE_ROOT = process.env.FEATURE_ROOT || 'test/e2e/migration/features';
const COVERAGE_BASELINE =
  process.env.COVERAGE_BASELINE || 'test/e2e/migration/coverage-baseline.json';
const PASS_ASSERTION_PIN =
  process.env.PASS_ASSERTION_PIN || 'test/e2e/migration/pass-assertions.pin.json';
const REPORT_DIR = process.env.REPORT_DIR || 'build/migration-reports';

// The enumerator stamps this into its output; a reader that does not
// understand the version rejects the document rather than misreading it.
export const SCENARIOS_SCHEMA_VERSION = 1;

// The baseline carries its own version for the same reason.
export const BASELINE_SCHEMA_VERSION = 1;

// So does the PASS-assertion pin.
export const PIN_SCHEMA_VERSION = 1;

// The tiers a Scenario may declare.
export const KNOWN_TIERS = ['tier0', 'tier1', 'tier2'];

// Per-tier `timeout-minutes` for the matrix entry. It travels ON the entry so
// the ceiling cannot drift from the shard it bounds. Tier 0 is an offline
// godog suite over committed fixtures — one `go build` plus seconds of file
// reading — so 15 minutes is generous headroom over a healthy run and still
// fails fast on a hang.
export const TIER0_TIMEOUT_MIN = 15;

// Tier 1 brings up the full dual-backend compose stack (ClickHouse, OTel
// collector, reference Prometheus/Loki/Tempo, cerberus), seeds every
// archetype the @tier1 scenario set needs, then runs the substrate
// self-checks and the Gherkin suite against it — mirrors the
// migration-tier1 job's own `timeout-minutes: 45` (image pulls + compose
// --wait + seeding + the suite itself), so the two ceilings cannot drift
// apart.
export const TIER1_TIMEOUT_MIN = 45;

// Tier 2 brings up everything Tier 1 does PLUS the ruler leg (Grafana with
// provisioned alerting, the relay-prom -> otel-collector-writeback write-back
// bridge, the dead-end receiver), and its scenarios then wait on real
// evaluation cadences — a rule group tick, a hold-down, a notification-policy
// flush — that no amount of runner speed shortens. It therefore gets a
// strictly larger ceiling than Tier 1's, and mirrors the migration-tier2 job's
// own `timeout-minutes: 60` so the two cannot drift apart.
export const TIER2_TIMEOUT_MIN = 60;

// The tiers migration-e2e.yml has a job for — one table, so a tier cannot
// have a job without a bound or a bound without a job. A scenario tagged with
// a tier absent from here is a scenario that would silently never execute, so
// emit rejects it rather than producing a matrix entry no job consumes. Each
// tier's dispatch option, job stanza and ceiling land with its first scenario.
//
// `matrixDriven` records WHICH job shape the tier's stanza has, because the
// two are not interchangeable. The matrix job is one generic runner fanned out
// by `strategy.matrix`, so its tiers must appear in the emitted matrix.
// migration-tier1 is a fixed job — it brings a compose stack up, seeds it and
// tears it down around the suite, steps a matrix shard cannot express — so it
// must NOT appear there. migration-tier2 is the same fixed shape for the same
// reason, and additionally `needs:` migration-tier1 — docs section 8's build
// order is that firing parity cannot be proven before query parity is, so a
// tier-2 verdict reached while tier-1 is red would be meaningless.
//
// Getting this wrong is quiet rather than loud: a tier1 entry in the matrix
// spawns a shard that runs the tier-1 suite on a bare runner with no stack
// behind it. Both job shapes render as `migration-<tier>`, so that shard is
// named exactly like the real fixed job — the run would show two
// `migration-tier1` jobs, one of which never had a backend.
export const TIER_JOBS = {
  tier0: { timeoutMinutes: TIER0_TIMEOUT_MIN, matrixDriven: true },
  tier1: { timeoutMinutes: TIER1_TIMEOUT_MIN, matrixDriven: false },
  tier2: { timeoutMinutes: TIER2_TIMEOUT_MIN, matrixDriven: false },
};
export const RUNNABLE_TIERS = Object.keys(TIER_JOBS);

// The Go package the tier-0 suite lives in, and the tag that selects it.
const TIER0_PACKAGE = './test/e2e/migration/tiers/tier0-offline/...';
const MIGRATION_BUILD_TAG = 'migration';

// The Go package the tier-1 dual-backend suite lives in, and the tag that
// selects it — shared with tier1_stack_test.go / tier1_parity_test.go, the
// plain-Go substrate self-checks, so one `go test` invocation exercises the
// substrate and the Gherkin suite together against the same live stack.
const TIER1_PACKAGE = './test/e2e/migration/tiers/tier1-dual/...';
const MIGRATION_TIER1_BUILD_TAG = 'migration_tier1';

// The Go package the tier-2 ruler suite lives in, and the tag that selects
// it. Deliberately NOT the whole `./test/e2e/migration/...` tree: the
// package-root substrate self-check (tier2_ruler_test.go) carries the same
// build tag and drives the SAME dead-end receiver, and go test gives no
// ordering guarantee across packages in one invocation — see the
// migration-tier2-run recipe's comment in the Justfile.
const TIER2_PACKAGE = './test/e2e/migration/tiers/tier2-ruler/...';
const MIGRATION_TIER2_BUILD_TAG = 'migration_tier2';

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

// --- the PASS-assertion pin -------------------------------------------------
//
// WHY THIS EXISTS — read before "simplifying" it away.
//
// The ratchet above derives its anchors LIVE from section 6, which means the
// anchor is editable in the same commit as the code it anchors. It checks a
// scenario's tier tag against the Tier(s) column but never looks at the PASS
// assertion's TEXT, so narrowing a PASS cell has always been a valid route to
// "full coverage": weaken what the doc demands, implement the weaker thing,
// and every gate stays green. That happened twice in one session — MIG-23's
// "the old backend is kept read-only as a historical tier" clause was deleted,
// and MIG-18's PASS assertion was narrowed from an incumbent-vs-shadow
// notification-stream diff to a single-ruler lifecycle IN THE SAME COMMIT that
// implemented the narrower thing.
//
// The pin does NOT forbid narrowing. A spec legitimately evolves, and a gate
// that froze section 6 would be a gate people route around. What it forbids is
// narrowing SILENTLY: the hash of every PASS cell is committed beside the
// coverage baseline, so changing a cell fails `MODE=verify` until the pin is
// updated in the SAME diff — one reviewed line that says "this story now
// demands less", sitting next to the commit that implements less. That line is
// the whole point. There is deliberately no `MODE=pin` regeneration mode: the
// failure prints the new hash to paste, because a one-command re-pin is a
// re-pin nobody reads.

// A section-6 row is `| ID | Tier(s) | CLI | Fixtures | PASS assertion |`.
// The count is asserted rather than assumed, so a table that grows a column
// fails loudly instead of hashing the wrong cell.
const SECTION6_COLUMNS = 5;
const SECTION6_TIERS_COLUMN = 1;
const SECTION6_PASS_COLUMN = 4;

// The hash algorithm the pin file declares and this module computes with.
const PIN_HASH_ALGORITHM = 'sha256';

// hashPassAssertion — the pinned digest of one PASS cell. Normalising
// whitespace first means a markdownlint reflow or a column-width realignment
// (both of which this doc's wide tables attract) is not a spurious failure,
// while any change to the WORDS is.
export function hashPassAssertion(text) {
  const normalized = text.trim().replace(/\s+/g, ' ');
  return createHash(PIN_HASH_ALGORITHM).update(normalized, 'utf8').digest('hex');
}

// parsePassAssertions — story -> { tiers, pass, columns } from section 6's
// tables. `columns` is carried so a malformed row is reported as malformed
// rather than silently hashing whichever cell happened to land last.
export function parsePassAssertions(docText) {
  const map = new Map();
  const section = sectionOf(docText, 6);
  for (const line of section.split('\n')) {
    if (!/^\|\s*MIG-\d{2}\s*\|/.test(line)) continue;
    const cells = line.split('|').slice(1, -1);
    const story = cells[0].trim();
    if (cells.length !== SECTION6_COLUMNS) {
      map.set(story, { tiers: null, pass: null, columns: cells.length });
      continue;
    }
    map.set(story, {
      tiers: cells[SECTION6_TIERS_COLUMN].trim(),
      pass: cells[SECTION6_PASS_COLUMN].trim(),
      columns: cells.length,
    });
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
  passAssertions,
  pin,
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

  // V17..V21 — the PASS-assertion pin. See the block comment above
  // parsePassAssertions for why a live-derived anchor needs pinning at all.
  if (pin.schema_version !== PIN_SCHEMA_VERSION) {
    add('V17', `${PASS_ASSERTION_PIN} declares schema_version ${pin.schema_version}, want ${PIN_SCHEMA_VERSION}`);
  }
  if (pin.algorithm !== PIN_HASH_ALGORITHM) {
    add('V17', `${PASS_ASSERTION_PIN} declares algorithm "${pin.algorithm}", want "${PIN_HASH_ALGORITHM}"`);
  }
  const pinned = pin.assertions || {};
  for (const [story, cell] of passAssertions) {
    if (cell.columns !== SECTION6_COLUMNS) {
      add(
        'V21',
        `${STORIES_DOC} section 6's ${story} row has ${cell.columns} columns, want ${SECTION6_COLUMNS} (| ID | Tier(s) | CLI | Fixtures | PASS assertion |) — a malformed row cannot be pinned`,
      );
      continue;
    }
    const want = pinned[story];
    if (want === undefined) {
      add(
        'V18',
        `${STORIES_DOC} section 6 lists ${story} but ${PASS_ASSERTION_PIN} pins no PASS assertion for it; add { "tiers": "${cell.tiers}", "${PIN_HASH_ALGORITHM}": "${hashPassAssertion(cell.pass)}" }`,
      );
      continue;
    }
    const got = hashPassAssertion(cell.pass);
    if (want[PIN_HASH_ALGORITHM] !== got) {
      add(
        'V19',
        `${story}'s PASS assertion in ${STORIES_DOC} section 6 changed: pinned ${want[PIN_HASH_ALGORITHM]}, now ${got}. If the change is intended — including a deliberate narrowing — update ${PASS_ASSERTION_PIN} to the new hash IN THE SAME DIFF, so the weakening is a reviewed line rather than a side effect of the commit that implements it.`,
      );
    }
    if (want.tiers !== cell.tiers) {
      add(
        'V20',
        `${story}'s Tier(s) cell in ${STORIES_DOC} section 6 changed: pinned "${want.tiers}", now "${cell.tiers}" — update ${PASS_ASSERTION_PIN} in the same diff`,
      );
    }
  }
  for (const story of Object.keys(pinned)) {
    if (!passAssertions.has(story)) {
      add('V18', `${PASS_ASSERTION_PIN} pins ${story}, which ${STORIES_DOC} section 6 has no row for`);
    }
  }

  return v;
}

// --- the matrix -------------------------------------------------------------

// nonRunnableTiers — the requested tiers migration-e2e.yml has no job for.
export function nonRunnableTiers(tiers) {
  return tiers.filter((t) => !RUNNABLE_TIERS.includes(t));
}

// buildMatrix — the `strategy.matrix` migration-tier0 fans out over. One entry
// per MATRIX-DRIVEN tier that has scenarios, carrying the stories it drives and
// its own ceiling, in GitHub's `{include: [...]}` form so
// `matrix: ${{ fromJSON(...) }}` binds `matrix.name` / `matrix.tier` /
// `matrix.timeoutMinutes` directly. A tier with no declared job never reaches
// here — the caller rejects it first, and this throws rather than emitting an
// entry the workflow would read a null timeout out of. A tier whose job is its
// own fixed stanza (tier1) is skipped: it is runnable, but not by this matrix.
// storiesForTier — the sorted stories a tier's scenarios drive.
export function storiesForTier(scenarios, tier) {
  return [
    ...new Set(scenarios.filter((s) => s.tiers.includes(tier)).flatMap((s) => s.stories)),
  ].sort();
}

// tiersWithScenarios — the requested tiers that actually have a scenario, in
// KNOWN_TIERS order. A requested tier with no scenario is not an error (a
// dispatch may narrow to a tier the tree has not grown yet); it simply does
// not run, and the roll-up must not expect a result from it.
export function tiersWithScenarios(scenarios, tiers) {
  return KNOWN_TIERS.filter((t) => tiers.includes(t) && storiesForTier(scenarios, t).length > 0);
}

export function buildMatrix(scenarios, { tiers }) {
  const include = [];
  for (const tier of tiers) {
    const job = TIER_JOBS[tier];
    if (job === undefined) {
      throw new Error(`buildMatrix: ${tier} has no declared ceiling, so migration-e2e.yml has no job for it`);
    }
    if (!job.matrixDriven) continue;
    const timeoutMinutes = job.timeoutMinutes;
    const stories = storiesForTier(scenarios, tier);
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

// --- attestation: the executed-not-enumerated gate ---------------------------

// godog's cucumber formatter writes a per-STEP status and no element-level
// verdict, so a scenario passed iff every one of its steps did. `skipped`,
// `undefined`, `pending`, `ambiguous` and `failed` are all not-passed — none
// of them is tolerated, and a Scenario with no steps at all is not a pass
// either (there is nothing there to have run).
const STEP_PASSED = 'passed';

// The tag vocabulary the report carries. godog prepends the FEATURE's tags to
// every element, which is where the migration features declare @MIG-nn /
// @tierN, so the element tags alone identify the (story, tier) pair the
// coverage ratchet keys on — the same key V7 pins to one Scenario. Keying on
// tags rather than on scenario name/line survives Scenario Outline expansion,
// where one enumerated node becomes one report element per Examples row.
const STORY_TAG_RE = /^@(MIG-\d{2})$/;
const TIER_TAG_RE = /^@(tier\d)$/;

// The suffix a tier's run report is written with. MODE=run builds the whole
// path from REPORT_DIR + tier + this, so the runner and the attester agree by
// construction rather than by two workflow steps quoting the same string.
export const REPORT_SUFFIX = '.cucumber.json';

// reportPathFor — the absolute path MODE=run hands the godog suite for a tier.
export function reportPathFor(tier, dir = REPORT_DIR) {
  return resolve(join(dir, `${tier}${REPORT_SUFFIX}`));
}

// readReportDir — every `*.json` under `dir`, recursively. Recursive because
// `actions/download-artifact` puts each artifact in its own subdirectory
// unless `merge-multiple` is set, and a gate that silently found nothing
// because of a layout detail is exactly the failure this whole file exists to
// prevent. A missing directory yields an empty list rather than an error: the
// `lint` job runs MODE=verify with no reports at all, and its notice reports
// attested coverage as 0 rather than dying.
export function readReportDir(dir = REPORT_DIR) {
  const found = [];
  const walk = (d) => {
    let entries;
    try {
      entries = readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of entries) {
      const p = join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile() && e.name.endsWith('.json')) found.push(p);
    }
  };
  walk(dir);
  return found.sort();
}

// outcomesFromReports — key -> the report elements that claim it, where key is
// `MIG-nn/tierN`. `unkeyable` collects elements whose tags do not resolve to
// exactly one story and one tier; they are reported rather than dropped, since
// a report the attester cannot read is not evidence of anything.
export function outcomesFromReports(reports) {
  const outcomes = new Map();
  const unkeyable = [];
  for (const { path, features } of reports) {
    for (const feature of features) {
      for (const el of feature.elements || []) {
        const tags = (el.tags || []).map((t) => t.name);
        const stories = tags.map((t) => (STORY_TAG_RE.exec(t) || [])[1]).filter(Boolean);
        const tiers = tags.map((t) => (TIER_TAG_RE.exec(t) || [])[1]).filter(Boolean);
        const where = `${path}: ${feature.uri || feature.name}:${el.line} "${el.name}"`;
        if (stories.length !== 1 || tiers.length !== 1) {
          unkeyable.push({ where, stories, tiers });
          continue;
        }
        const steps = el.steps || [];
        const failed = steps.filter((s) => (s.result || {}).status !== STEP_PASSED);
        const detail =
          steps.length === 0
            ? 'the report records no steps for it'
            : failed.map((s) => `"${s.keyword || ''}${s.name}" ${(s.result || {}).status}`).join('; ');
        const key = `${stories[0]}/${tiers[0]}`;
        const list = outcomes.get(key) || [];
        list.push({ where, passed: steps.length > 0 && failed.length === 0, detail });
        outcomes.set(key, list);
      }
    }
  }
  return { outcomes, unkeyable };
}

// collectAttestations — every attestation rule, collect-all in the same style
// as collectViolations: one run names every scenario that did not run and
// every one that ran red, so a fix lands in one pass.
//
// `tiers` is the set this run SELECTED — emit's `tiers` output, the same value
// the roll-up reads. A dispatch narrowed to `tier=tier0` legitimately never
// runs tier1's scenarios, so attesting them would turn a correct narrowing
// into a failure. `story`, when set, narrows the same way for the workflow's
// single-story dispatch input.
export function collectAttestations({ scenarios, tiers, story, outcomes, unkeyable, reportCount }) {
  const a = [];
  const add = (code, message) => a.push({ code, message });

  const selected = scenarios.filter(
    (s) => s.tiers.some((t) => tiers.includes(t)) && (story === '' || s.stories.includes(story)),
  );

  if (selected.length > 0 && reportCount === 0) {
    add(
      'A0',
      `no run report was found under ${REPORT_DIR}, so none of the ${selected.length} selected scenario(s) can be attested — MODE=run writes one per tier and each tier job uploads it`,
    );
  }

  const wanted = new Map();
  for (const sc of selected) {
    for (const st of sc.stories) {
      for (const t of sc.tiers) {
        if (!tiers.includes(t)) continue;
        const key = `${st}/${t}`;
        wanted.set(key, (wanted.get(key) || 0) + 1);
      }
    }
  }

  for (const [key, want] of [...wanted].sort()) {
    const [st, tier] = key.split('/');
    const got = outcomes.get(key) || [];
    if (got.length === 0) {
      add(
        'A1',
        `${st} is counted as covered at ${tier} but NEVER RAN — no run report contains a scenario tagged @${st} @${tier}. Enumerating a scenario is not executing it.`,
      );
      continue;
    }
    // A Scenario Outline expands to one report element per Examples row, so
    // MORE elements than enumerated nodes is normal; FEWER means an
    // enumerated node produced no element and its execution is unproven.
    if (got.length < want) {
      add(
        'A1',
        `${st}/${tier} enumerates ${want} scenario node(s) but only ${got.length} ran — ${want - got.length} of them never executed`,
      );
    }
    for (const o of got) {
      if (!o.passed) {
        add('A2', `${st}/${tier} ran but did not pass — ${o.where}: ${o.detail}`);
      }
    }
  }

  // A3 is drift between the report and the ENUMERATION, so it is judged
  // against every scenario the selected tiers declare — not against `wanted`,
  // which a single-story dispatch has already narrowed. Narrowing to one story
  // means the other scenarios are not attested; it does not mean a report
  // mentioning them is corrupt.
  const enumerated = new Set(
    scenarios.flatMap((s) => s.stories.flatMap((st) => s.tiers.map((t) => `${st}/${t}`))),
  );
  for (const key of [...outcomes.keys()].sort()) {
    if (enumerated.has(key)) continue;
    const [st, tier] = key.split('/');
    if (!tiers.includes(tier)) continue;
    add(
      'A3',
      `a run report attests @${st} @${tier}, which this run's enumeration does not list — the report and the feature tree disagree about what exists`,
    );
  }

  for (const u of unkeyable) {
    add(
      'A4',
      `${u.where} carries ${u.stories.length} @MIG-nn and ${u.tiers.length} tier tag(s) in its run report, so its execution cannot be attributed to a counted scenario`,
    );
  }

  return a;
}

// attestedCount — how many of `scenarios` are proven executed-and-passed by
// the reports on disk. Used by MODE=verify's notice so enumerated and attested
// coverage are reported as two different numbers rather than one number and a
// caveat somebody has to remember to write in prose.
export function attestedCount(scenarios, outcomes) {
  let n = 0;
  for (const sc of scenarios) {
    const keys = sc.stories.flatMap((st) => sc.tiers.map((t) => `${st}/${t}`));
    const got = keys.flatMap((k) => outcomes.get(k) || []);
    if (got.length > 0 && got.every((o) => o.passed)) n += 1;
  }
  return n;
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

// loadReports — every run report under REPORT_DIR, parsed. A file that is not
// a cucumber-JSON array is a hard error rather than an ignored file: silently
// skipping an unreadable report is how "attested" would quietly become
// "attested by nothing".
function loadReports(dir = REPORT_DIR) {
  const paths = readReportDir(dir);
  return paths.map((path) => {
    const features = readJSON(path, 'a tier run report');
    if (!Array.isArray(features)) {
      error(`migration-e2e: the run report at ${path} is not a cucumber-JSON feature array`);
      process.exit(1);
    }
    return { path, features };
  });
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
  const pin = readJSON(PASS_ASSERTION_PIN, 'the PASS-assertion pin');

  const violations = collectViolations({
    scenarios,
    stories,
    tierMap,
    passAssertions: parsePassAssertions(docText),
    pin,
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
      `migration-e2e: ${violations.length} coverage violation(s). The story list in ${STORIES_DOC} section 4 is the anchor; the scenarios live under ${FEATURE_ROOT}; the floor lives in ${COVERAGE_BASELINE}; the PASS-assertion hashes live in ${PASS_ASSERTION_PIN}.`,
      { title: 'migration-e2e coverage ratchet' },
    );
    process.exit(1);
  }

  // Two numbers, never one. ENUMERATED is what this mode can prove by walking
  // feature files; ATTESTED is what a tier run report proves actually
  // executed and passed. On the `lint` job no report exists, so attested is 0
  // — and saying so out loud is the point: the honest caveat lives in the
  // gate's own output instead of in prose beside it.
  const covered = new Set(scenarios.flatMap((s) => s.stories));
  const { outcomes } = outcomesFromReports(loadReports());
  const attested = attestedCount(scenarios, outcomes);
  notice(
    `migration-e2e: ENUMERATED ${scenarios.length}/${baseline.scenarios_total} scenarios across ${covered.size}/${stories.length} stories, 0 violations; ATTESTED (executed and passed) ${attested}/${scenarios.length} from ${REPORT_DIR}. Enumeration is not execution — MODE=attest in the migration-aggregate roll-up is what closes the gap.`,
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
  // `tiers` is what the run WILL execute: every requested tier that actually
  // has scenarios. The roll-up reads it back so a tier the dispatch narrowed
  // away is judged as legitimately-not-run, while a tier that WAS requested
  // and came back skipped is a failure. Without it, `skipped` is ambiguous.
  const running = tiersWithScenarios(scenarios, tiers);
  if (running.length === 0) {
    error(`migration-e2e: no scenario matched TIER "${process.env.TIER || 'all'}" — the lane would run nothing`);
    process.exit(1);
  }
  const matrix = buildMatrix(scenarios, { tiers });
  setOutput('matrix', JSON.stringify(matrix));
  setOutput('tiers', JSON.stringify(running));
  appendStepSummary(
    [
      '### migration scenario matrix',
      '',
      '| tier | stories | timeout (min) | job |',
      '| --- | --- | --- | --- |',
      // Both job shapes render as `migration-<tier>`: the matrix-driven job is
      // named for the shard it is running, not for itself, so the summary
      // names the job the reader will actually find in the run.
      ...running.map((tier) => {
        const stories = storiesForTier(scenarios, tier).join(', ');
        const { timeoutMinutes } = TIER_JOBS[tier];
        return `| \`${tier}\` | ${stories} | ${timeoutMinutes} | migration-${tier} |`;
      }),
    ].join('\n'),
  );
  log(`migration-e2e: ${running.join(', ')} will run (${matrix.include.length} of them via the matrix).`);
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
  tier2: { pkg: TIER2_PACKAGE, buildTag: MIGRATION_TIER2_BUILD_TAG },
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

  // The run report's path is chosen HERE, not in the workflow. A tier job's
  // YAML stanza can forget an upload step and be caught by MODE=attest's A0/A1
  // ("counted but never ran"); it cannot silently produce no report at all,
  // because every tier goes through this one code path and lib.SuiteFormat
  // turns the variable into a godog formatter. `go test` runs the test binary
  // with the PACKAGE directory as its working directory, so the path must be
  // absolute — lib.SuiteFormat refuses a relative one rather than writing the
  // report somewhere nobody looks.
  const reportPath = reportPathFor(tier);
  mkdirSync(resolve(REPORT_DIR), { recursive: true });

  const args = ['test', `-tags=${run.buildTag}`, run.pkg, '-count=1', '-v'];
  log(`migration-e2e: go ${args.join(' ')} (MIGRATION_TAGS=${tags}, MIGRATION_REPORT=${reportPath})`);
  const res = spawnSync('go', args, {
    stdio: 'inherit',
    env: { ...process.env, MIGRATION_TAGS: tags, MIGRATION_REPORT: reportPath },
  });
  const status = res.error ? 1 : (res.status ?? 1);
  if (status !== 0) {
    if (res.error) error(`migration-e2e: could not run go test: ${res.error.message}`);
    error(`migration-e2e: the migration scenarios failed for tag filter ${tags}`, {
      title: 'migration-e2e',
    });
    process.exit(1);
  }
  // A green suite that wrote no report is a green run nothing can attest, so
  // it is a failure here rather than an A1 storm two jobs later with no
  // evidence to explain it.
  if (!existsSync(reportPath) || statSync(reportPath).size === 0) {
    error(
      `migration-e2e: the ${tier} suite passed but wrote no run report at ${reportPath} — the tier's godog Options.Format must come from lib.SuiteFormat()`,
      { title: 'migration-e2e' },
    );
    process.exit(1);
  }
  notice(`migration-e2e: the migration scenarios passed for tag filter ${tags}; run report at ${reportPath}`);
  process.exit(0);
}

// runAttest — the execution gate. Reads the tier run reports back and proves
// every scenario the ratchet counts for the SELECTED tiers actually executed
// and passed. Selection comes from EXPECTED_TIERS, the same emit output the
// roll-up reads, so "which tiers did this run mean to execute" is decided in
// exactly one place and the two gates cannot disagree.
function runAttest() {
  const scenarios = loadScenarios();
  let tiers;
  try {
    tiers = JSON.parse(process.env.EXPECTED_TIERS || '[]');
  } catch (e) {
    error(`migration-e2e: EXPECTED_TIERS is not JSON: ${e.message}`);
    process.exit(1);
  }
  if (!Array.isArray(tiers)) {
    error(`migration-e2e: EXPECTED_TIERS must be a JSON array of tier names, got ${process.env.EXPECTED_TIERS}`);
    process.exit(1);
  }
  const story = (process.env.STORY || '').trim();
  if (story !== '' && !STORY_ID_RE.test(story)) {
    error(`migration-e2e: STORY "${story}" is not a MIG id (want e.g. MIG-04)`);
    process.exit(1);
  }

  const reports = loadReports();
  const { outcomes, unkeyable } = outcomesFromReports(reports);
  const problems = collectAttestations({
    scenarios,
    tiers,
    story,
    outcomes,
    unkeyable,
    reportCount: reports.length,
  });

  if (problems.length > 0) {
    for (const { code, message } of problems) {
      error(`migration-e2e: ${message}`, { title: `migration-attestation ${code}` });
    }
    error(
      `migration-e2e: ${problems.length} attestation violation(s) across tier(s) ${tiers.join(', ') || 'none'}. Coverage counts EXECUTED scenarios, not enumerated ones — fix the scenario or the tier job, never the count.`,
      { title: 'migration-e2e attestation' },
    );
    process.exit(1);
  }

  const attested = attestedCount(
    scenarios.filter((s) => s.tiers.some((t) => tiers.includes(t))),
    outcomes,
  );
  notice(
    `migration-e2e: attested ${attested} scenario(s) as executed and passed across tier(s) ${tiers.join(', ') || 'none'}, from ${reports.length} run report(s) under ${REPORT_DIR}`,
  );
  process.exit(0);
}

// --- the roll-up ------------------------------------------------------------

// rollUp — the lane's verdict from the tier jobs' results. `contains(needs.*,
// 'failure')` is the shape e2e.yml documents as unsafe: a matrix rolls up to
// `cancelled`, not `failure`, when a child is cancelled, so a cancelled tier
// would pass silently. Every tier that emit said would run must come back
// `success` — nothing else, `skipped` included, counts.
//
// A tier that emit did NOT list (a dispatch narrowed it away) is the one case
// where `skipped` is correct, and the only case: `expected` is the emit
// output, not the set of jobs that happen to exist.
export function rollUp({ expected, results }) {
  const problems = [];
  for (const tier of expected) {
    const got = results[tier];
    if (got === undefined) {
      problems.push(`${tier} was expected to run but reported no result at all`);
    } else if (got !== 'success') {
      problems.push(`${tier} did not succeed (${got})`);
    }
  }
  for (const [tier, got] of Object.entries(results)) {
    if (expected.includes(tier)) continue;
    if (got !== 'skipped') {
      problems.push(`${tier} ran (${got}) but was not in the tier set this run selected`);
    }
  }
  // migration-setup gates every tier job, so its own failure must not be
  // reported as "no tier ran" — it is the first thing to say.
  return problems;
}

function runRollUp() {
  const setup = (process.env.SETUP_RESULT || '').trim();
  if (setup !== 'success') {
    error(`migration-e2e: migration-setup did not succeed (${setup || 'no result'}), so no tier could run`, {
      title: 'migration-e2e',
    });
    process.exit(1);
  }
  let expected;
  try {
    expected = JSON.parse(process.env.EXPECTED_TIERS || '[]');
  } catch (e) {
    error(`migration-e2e: EXPECTED_TIERS is not JSON: ${e.message}`);
    process.exit(1);
  }
  const results = {};
  for (const tier of KNOWN_TIERS) {
    const got = (process.env[`RESULT_${tier.toUpperCase()}`] || '').trim();
    if (got !== '') results[tier] = got;
  }
  const problems = rollUp({ expected, results });
  if (problems.length > 0) {
    for (const p of problems) error(`migration-e2e: ${p}`, { title: 'migration-e2e' });
    process.exit(1);
  }
  notice(`migration-e2e: every selected tier succeeded (${expected.join(', ')})`);
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
  } else if (mode === 'attest') {
    runAttest();
  } else if (mode === 'rollup') {
    runRollUp();
  } else {
    error(`migration-e2e: unknown MODE "${mode}" (want verify|emit|run|attest|rollup)`);
    process.exit(1);
  }
}
