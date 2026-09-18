// semantic-guide.mjs — builds docs/semantic-guide.md + docs/semantic-
// guide.json (cerberus issue #3462), a COMPACT contract index and a
// selected architectural-rule block over the same semantic contract model
// docs/semantic-conformance.md (issue #3435) already reports on in full.
//
// WHY A SECOND GENERATED DOCUMENT RATHER THAN EXTENDING THE FIRST. The full
// report answers "what is the state of every enrolled contract's evidence,
// exhaustively" — 50+ per-contract cards with blind spots, complement gaps
// and observed-vs-bound rollups. That is the right shape for an auditor, but
// the wrong shape for a developer or agent who just wants "I am about to
// touch <path> — which contracts does that affect, what do I run, and is it
// required before I can merge or release." This module answers THAT
// question, and answers it as a VIEW over the exact same buildReport()
// output the full report renders from (imported, never recomputed) — the
// only genuinely new logic here is the compact indexing, the worked
// examples, and the architectural-rule selection. Two documents, one
// editable source (test/semantic/*.json) and one shared computation
// (buildReport), reproducible projections for both — never a second,
// independently-derived model of contract state that could drift from the
// first.
//
// NOT A MERGE GATE. This document DESCRIBES the task -> affected contracts
// -> canonical execution -> counterexample search -> optional adversarial
// probes -> required evidence flow; it never selects, skips, or approves a
// CI workflow, exactly like semantic-impact.mjs (issue #3460) whose
// MERGE_RELEASE_CAVEAT this module reuses verbatim rather than restating.
// The adversarial-evidence section is a projection of the report's own
// mutation_cohort (lib/semantic-mutation-report.mjs), so the guide and the
// full report can never disagree about which contracts a committed mutant
// targets. An agent (or a human) cannot self-approve
// correctness by editing test/semantic/*.json: those files are
// hand-authored/reviewed source like any other in this repository, a
// change to a contract's statement, required evidence class, or binding is
// an ordinary diff line a reviewer sees in the PR like any other, and
// widening or weakening one is exactly as visible as a production code
// change — see METADATA_INTEGRITY_NOTE below, and docs/agent-workflow.md
// for the review ritual this document never replaces.
//
// DETERMINISM. buildGuide/renderMarkdown/renderJSON are pure functions of
// their inputs — no Date.now(), no unsorted iteration — mirroring
// lib/semantic-report.mjs's own determinism contract exactly (issue #3435's
// own acceptance criterion, restated once there rather than here).

import { WORKED_EXAMPLE_CONTRACT_ID, mdEscapeProse } from "./semantic-report.mjs";
import { byId } from "./semantic-model.mjs";
import { classifyTestRef } from "./semantic-evidence-adapter.mjs";
import { MERGE_RELEASE_CAVEAT } from "./semantic-impact.mjs";
import { matchesGlob } from "../ci-lane-contract.mjs";

export const GUIDE_SCHEMA_VERSION = 1;
export const DEFAULT_GUIDE_MD_PATH = "docs/semantic-guide.md";
export const DEFAULT_GUIDE_JSON_PATH = "docs/semantic-guide.json";

// One worked-example contract per head, plus one architecture-scope
// (shared-engine) example — four fixed IDs, each picked for a real,
// documented, DIFFERENT property so the four examples are not four copies
// of the same shape:
//   - PromQL: `authority: specification` — no live reference server is
//     part of this contract's own definition of correctness (contrast with
//     the next two).
//   - LogQL: `authority: reference-implementation` — Loki's own RE2 engine
//     is the comparator.
//   - TraceQL: reused from semantic-report.mjs's own WORKED_EXAMPLE_CONTRACT_ID
//     rather than a second, independently-chosen ID — Tempo
//     reference-implementation authority, a documented count-only blind
//     spot, and a required complement no active binding supplies, so a
//     reader who follows this example straight into the full report lands
//     on the exact same card, never a second unrelated one.
//   - Shared-engine: `ARCH-HEAD-OPTIMIZE-001` (architecture scope) —
//     internal/optimizer's rewrites, which every head's lowered plan passes
//     through, so a change there is the canonical "this is not any one
//     head's contract" example.
export const WORKED_EXAMPLES = Object.freeze({
  "HEAD-PROMQL": "PROMQL-COUNTER-RESET-EXTRAPOLATION",
  "HEAD-LOGQL": "LOGQL-LINE-FILTER-REGEX-SEMANTICS",
  "HEAD-TRACEQL": WORKED_EXAMPLE_CONTRACT_ID,
  ARCHITECTURE: "ARCH-HEAD-OPTIMIZE-001",
});

export const METADATA_INTEGRITY_NOTE =
  "test/semantic/*.json is hand-authored, reviewed source, not a self-service " +
  "approval surface. A pull request that adds, weakens, or removes a contract " +
  "statement, a required evidence class or independence group, or a binding is " +
  "an ordinary diff line like any other production change — visible in `git " +
  "diff`, reviewed under the same merge gate (docs/agent-workflow.md), and " +
  "carried into this guide and the full report the moment `just semantic-report` " +
  "/ `just semantic-guide` regenerate them. Neither generator can mask a " +
  "weakening: both only ever project the checked-in model faithfully, and their " +
  "own `--check` mode fails the build the instant the committed projection stops " +
  "matching it (invariant 9) — so a metadata change that widens what counts as " +
  "assured is exactly as loud in review as a change to production code, never " +
  "quieter for being JSON.";

const HOW_TO_USE_STEPS = [
  {
    title: "Identify the affected contracts",
    body:
      "`just semantic-impact <base> <head>` names every enrolled contract a git " +
      "range touches, and why (the exact file and the CI-lane dependency closure " +
      "that triggered it — never a second, hand-maintained dependency graph). " +
      "Input it cannot resolve, or an import graph it cannot load, never reports " +
      "\"nothing affected\" — both widen conservatively to every lane carrying " +
      "active semantic evidence instead, the one behavior this step's own " +
      "acceptance criterion requires (issue #3460).",
  },
  {
    title: "Run the canonical execution",
    body:
      "Each affected contract's active binding(s) name an exact `test_ref` and " +
      "resolve to an exact CI-lane recipe — the Contract index below, or the " +
      "same lookup live: `jq '.contracts[] | select(.id == \"<ID>\")' " +
      "docs/semantic-conformance.json`.",
  },
  {
    title: "Search for counterexamples",
    body:
      "Where a contract binds a property-based or oracle-differential verifier " +
      "(`just property`), run it — a randomized counterexample search is a " +
      "materially different claim from one fixed golden input, and the " +
      "Evidence-system column below (or the full report) names which " +
      "contracts have it bound.",
  },
  {
    title: "Optional adversarial probes",
    body:
      "The Adversarial evidence section below lists, per contract, the " +
      "hand-authored contract-linked mutants of the semantic mutation pilot " +
      "(`test/semantic/mutants/`, run with `just semantic-mutate <id>`) and each " +
      "one's declared disposition. A contract with none listed has no " +
      "adversarial evidence — not a gap this guide asks a reader to fill before " +
      "the four steps around it are usable; mutation testing (`just mutate-pkg " +
      "<path>`) is available and worth running for extra confidence, never " +
      "required to complete the flow.",
  },
  {
    title: "Required reference/release evidence",
    body:
      "Not every canonical execution is a merge or release OBLIGATION — the " +
      "Contract index's merge-required/release-required columns (sourced from " +
      "the captured CI policy snapshot, the same join semantic-impact.mjs and " +
      "semantic-report.mjs already make) say which. " +
      MERGE_RELEASE_CAVEAT,
  },
];

/** GitHub's Markdown heading slug for a contract ID: plain uppercase + hyphens, so lowercasing is the whole transform. */
function anchor(id) {
  return id.toLowerCase();
}

// Which lane actually RUNS a binding's evidence, by the evidence system its
// test_ref names (lib/semantic-evidence-adapter.mjs's classifyTestRef):
// the property lane runs the property-shape rosters; a TXTAR fixture is
// executed by its head's chDB round trip (and by `check`'s unit suite,
// which also carries the fixtures that have no round-trip lane, such as
// test/spec/optimizer); the parity ledgers are ratcheted by `check`; a
// compatibility harness directory is run by its head's compat lane; any
// other Go test runs under `check`. A binding's obligations are already
// free of tree-wide lanes (resolveBindingLanes), so a lane named here is
// only ever chosen when the binding's own obligations actually contain
// it — this list ranks, it never invents.
const COMPAT_LANE_BY_HARNESS_DIR = Object.freeze({
  "compatibility/prometheus": "compatibility.prometheus",
  "compatibility/loki": "compatibility.loki",
  "compatibility/tempo": "compatibility.tempo",
});
const UNIT_SUITE_LANE = "ci.check";
// `just coverage-default` + `coverage-chdb` run every default- and
// chdb-tagged test under ./..., including packages `ci.check`'s own
// package_globs do not name (test/semantic/resourcefixture,
// test/consumer-corpus). When a Go test's most specific owning globs tie —
// which in this registry means two lanes both claiming `test/**`, one of
// them a scan that runs no Go test at all — the lane that runs every Go
// test wins the tie.
const WHOLE_TREE_COVERAGE_LANE = "quality.coverage-measured";
const PROPERTY_LANE = "quality.property";

function preferredLaneIds(binding) {
  const classified = classifyTestRef(binding.test_ref);
  switch (classified.system) {
    case "property-shape":
      return [PROPERTY_LANE];
    case "txtar-fixture": {
      const [, , head] = classified.path.split("/");
      return [`chdb.roundtrip-${head}`, UNIT_SUITE_LANE];
    }
    case "source-path": {
      const harness = Object.keys(COMPAT_LANE_BY_HARNESS_DIR).find(
        (dir) => classified.path === dir || classified.path.startsWith(`${dir}/`),
      );
      if (harness) return [COMPAT_LANE_BY_HARNESS_DIR[harness]];
      if (classified.path.startsWith("test/property/")) return [PROPERTY_LANE, UNIT_SUITE_LANE];
      // A gate script is run by the lane whose registry `command` names it
      // (ci.agpl-clean -> agpl-clean.mjs); that join is made in
      // resolveBindingLanes and lands in the obligations. Nothing to rank
      // here beyond the unit suite — the specificity tiebreak below picks
      // the lane whose declared scope names the file most narrowly
      // (migration.e2e for test/e2e/migration/**, chdb.perf-guards for
      // test/perf/**).
      return [UNIT_SUITE_LANE];
    }
    default:
      return [UNIT_SUITE_LANE];
  }
}

// The static prefix of a glob (everything before its first wildcard) is
// how narrowly the lane declared its interest in a path: `test/e2e/
// migration/**` is a stronger claim to own test/e2e/migration/tier1_
// parity_test.go than `test/**` is. Used only as the tiebreak when no
// evidence-system preference applies.
function globSpecificity(glob) {
  const wildcard = glob.indexOf("*");
  return wildcard === -1 ? glob.length : wildcard;
}

function laneSpecificityFor(lane, binding) {
  const classified = classifyTestRef(binding.test_ref);
  const candidates = [binding.test_ref];
  if (classified.system === "source-path") candidates.push(classified.path, `${classified.path}/`);
  let best = -1;
  for (const glob of lane.package_globs ?? []) {
    if (candidates.some((c) => matchesGlob(c, glob))) best = Math.max(best, globSpecificity(glob));
  }
  if (typeof lane.command === "string" && classified.system === "source-path" && lane.command.includes(classified.path)) {
    best = Math.max(best, classified.path.length);
  }
  return best;
}

function laneCommand(lane) {
  // Prefer the lane's own `just` recipe(s) — a literally runnable command —
  // over its `command` field, which for several lanes (ci.check) is a
  // prose SUMMARY of several recipes rather than a single invocation.
  // `command` remains the fallback for a lane with no just recipe at all
  // (ci.forbid-skip, chdb.roundtrip-* run a bare `node .github/scripts/
  // *.mjs`, itself already runnable).
  return lane.recipes?.length ? lane.recipes.map((r) => `just ${r}`).join(" && ") : (lane.command ?? null);
}

/**
 * The canonical execution recipe for a contract's active bindings: for the
 * lowest-sorted binding with any lane obligation, the obligation whose lane
 * runs that binding's evidence system (preferredLaneIds above); when none
 * of the preferred lanes is among them, the obligation whose lane declared
 * the binding's path most narrowly (laneSpecificityFor), lowest lane ID on
 * a tie — resolved to that lane's OWN command/recipe text via the registry,
 * never a second copy of the command. A contract with no active binding,
 * or whose bindings resolve to no lane (a surface-parity symbol, or a
 * config file only a tree-wide lane covers), reports `null` rather than a
 * placeholder.
 */
function canonicalExecution(contractRecord, registry) {
  const lanesById = new Map((registry.lanes ?? []).map((l) => [l.id, l]));
  for (const binding of contractRecord.bindings) {
    const runnable = binding.obligations.filter((o) => {
      const lane = lanesById.get(o.lane_id);
      return lane && laneCommand(lane);
    });
    if (runnable.length === 0) continue;
    const preferred = preferredLaneIds(binding)
      .map((id) => runnable.find((o) => o.lane_id === id))
      .find(Boolean);
    const mostSpecific = [...runnable].sort((a, b) => {
      const diff = laneSpecificityFor(lanesById.get(b.lane_id), binding) - laneSpecificityFor(lanesById.get(a.lane_id), binding);
      if (diff !== 0) return diff;
      if (a.lane_id === WHOLE_TREE_COVERAGE_LANE) return -1;
      if (b.lane_id === WHOLE_TREE_COVERAGE_LANE) return 1;
      return a.lane_id < b.lane_id ? -1 : a.lane_id > b.lane_id ? 1 : 0;
    })[0];
    const obligation = preferred ?? mostSpecific;
    const lane = lanesById.get(obligation.lane_id);
    return {
      binding_id: binding.id,
      lane_id: lane.id,
      command: laneCommand(lane),
      merge_required: obligation.merge_required,
      release_required: obligation.release_required,
    };
  }
  return null;
}

/** The compact per-contract index row: id, scope label, canonical execution, obligations. */
function indexRow(contractRecord, registry) {
  const scopeLabel =
    contractRecord.scope === "head"
      ? contractRecord.applicable_heads.join(", ")
      : contractRecord.scope === "signal"
        ? `signal (${contractRecord.applicable_heads.join(", ")})`
        : "architecture (universal)";
  const execution = canonicalExecution(contractRecord, registry);
  return {
    id: contractRecord.id,
    status: contractRecord.status,
    scope_label: scopeLabel,
    authority: contractRecord.authority,
    assured: contractRecord.bound_evidence.assured,
    execution,
  };
}

/**
 * Extracts an explicit "CLAUDE.md invariant N" citation from a contract's
 * own `statement`, if the source model already names one — DERIVED, never a
 * second, hand-maintained contract-to-invariant mapping table. Most
 * architecture-scope contracts describe a design decision CLAUDE.md's
 * numbered hard invariants never enumerated (there are more architectural
 * rules than there are hard invariants), so most report `null` here, which
 * is the correct, honest answer rather than a guessed pairing.
 */
function citedInvariant(statement) {
  const match = /CLAUDE\.md invariant (\d+)/.exec(statement);
  return match ? Number(match[1]) : null;
}

/** The selected architectural-rule block: every active, architecture-scope contract. */
function architecturalRules(report) {
  return report.contracts
    .filter((c) => c.scope === "architecture" && c.status === "active")
    .map((c) => ({
      id: c.id,
      statement: c.statement,
      authority: c.authority,
      claude_md_invariant: citedInvariant(c.statement),
      anchor: anchor(c.id),
    }))
    .sort(byId);
}

function findContract(report, id) {
  return report.contracts.find((c) => c.id === id) ?? null;
}

/**
 * The adversarial-evidence projection: which contracts the real
 * (non-synthetic) mutants of the semantic mutation pilot target, each with
 * its RESOLVED disposition and bucket — read straight from
 * report.mutation_cohort (lib/semantic-mutation-report.mjs), never
 * recomputed. `mutant_count` is the size of the semantic cohort, so an
 * empty corpus reads as a derived zero rather than a hand-written claim.
 */
function adversarialEvidence(cohort) {
  const records = new Map((cohort?.records ?? []).map((r) => [r.id, r]));
  const byContract = Object.entries(cohort?.by_contract ?? {})
    .map(([contract, ids]) => ({
      contract,
      mutants: [...ids].sort().map((id) => {
        const record = records.get(id);
        return { id, disposition: record?.disposition?.status ?? null, bucket: record?.bucket ?? null };
      }),
    }))
    .sort((a, b) => (a.contract < b.contract ? -1 : a.contract > b.contract ? 1 : 0));
  return {
    mutant_count: cohort?.semantic_cohort?.record_ids?.length ?? 0,
    by_contract: byContract,
  };
}

function workedExample(report, key, contractId) {
  const contractRecord = findContract(report, contractId);
  if (!contractRecord) return null;
  return {
    key,
    contract: {
      id: contractRecord.id,
      statement: contractRecord.statement,
      authority: contractRecord.authority,
      scope: contractRecord.scope,
      applicable_heads: contractRecord.applicable_heads,
      blind_spots: contractRecord.blind_spots,
      anchor: anchor(contractRecord.id),
    },
    bindings: contractRecord.bindings.map((b) => ({
      id: b.id,
      test_ref: b.test_ref,
      evidence_class: b.evidence_class,
      evidence_system: b.evidence_system,
      obligations: b.obligations,
      verifier: b.verifier && {
        id: b.verifier.id,
        substrate: b.verifier.substrate,
        detects: b.verifier.detects,
        cannot_detect: b.verifier.cannot_detect,
      },
    })),
    complement_gaps: contractRecord.complement_gaps,
  };
}

/**
 * Builds the guide's data model from an already-built semantic-report.mjs
 * `buildReport()` result plus the CI lane registry (for canonical
 * execution's command text, which the report itself does not carry).
 */
export function buildGuide(report, registry) {
  const contractIndex = report.contracts
    .filter((c) => c.status === "active")
    .map((c) => indexRow(c, registry))
    .sort(byId);

  const examples = Object.entries(WORKED_EXAMPLES)
    .map(([key, contractId]) => workedExample(report, key, contractId))
    .filter(Boolean);

  return {
    schema_version: GUIDE_SCHEMA_VERSION,
    generator: "node .github/scripts/semantic-guide.mjs",
    source: "the semantic-report.mjs buildReport() output over test/semantic/*.json",
    how_to_use: HOW_TO_USE_STEPS,
    metadata_integrity_note: METADATA_INTEGRITY_NOTE,
    adversarial_evidence: adversarialEvidence(report.mutation_cohort),
    merge_release_caveat: MERGE_RELEASE_CAVEAT,
    contract_index: contractIndex,
    architectural_rules: architecturalRules(report),
    worked_examples: examples,
  };
}

// --- Rendering ---------------------------------------------------------------

function renderExecutionCell(execution) {
  if (!execution) return "(no resolvable lane — see full report)";
  const flags = [execution.merge_required ? "merge" : null, execution.release_required ? "release" : null]
    .filter(Boolean)
    .join("+");
  const req = flags ? `required: ${flags}` : "advisory";
  return `\`${execution.command}\` (${req})`;
}

function renderIndexTable(rows) {
  const lines = [
    "| Contract | Scope | Authority | Assured | Canonical execution |",
    "| --- | --- | --- | --- | --- |",
  ];
  for (const row of rows) {
    lines.push(
      `| [\`${row.id}\`](semantic-conformance.md#${anchor(row.id)}) | ${row.scope_label} | ${row.authority} | ${row.assured} | ${renderExecutionCell(row.execution)} |`,
    );
  }
  return lines.join("\n");
}

// Every string below rendered from test/semantic/*.json (a contract's own
// `statement`, `blind_spots`, or a verifier's `cannot_detect`) is free-text
// DATA, not Markdown source, and goes through mdEscapeProse before
// interpolation — see that function's own header comment (lib/semantic-
// report.mjs) for why a bare "internal/chsql/**" or "__name__" silently
// corrupts under the house-style autofixer otherwise. Hand-authored prose
// this module itself writes (HOW_TO_USE_STEPS, METADATA_INTEGRITY_NOTE, the
// imported MERGE_RELEASE_CAVEAT) is real Markdown source and is never
// escaped.
function renderArchitecturalRules(rules) {
  const lines = [];
  for (const rule of rules) {
    const invariant = rule.claude_md_invariant
      ? ` (CLAUDE.md invariant ${rule.claude_md_invariant})`
      : "";
    lines.push(
      `- [\`${rule.id}\`](semantic-conformance.md#${rule.anchor})${invariant} — ${mdEscapeProse(rule.statement)}`,
    );
  }
  return lines.join("\n");
}

function renderWorkedExample(example) {
  const lines = [];
  const c = example.contract;
  lines.push(`### ${example.key}: [\`${c.id}\`](semantic-conformance.md#${c.anchor})`);
  lines.push("");
  lines.push(mdEscapeProse(c.statement));
  lines.push("");
  lines.push(`- **authority**: ${c.authority}`);
  if (c.blind_spots.length > 0) {
    lines.push(`- **blind spots**: ${c.blind_spots.map(mdEscapeProse).join("; ")}`);
  }
  lines.push("- **canonical execution and counterexample search**:");
  for (const b of example.bindings) {
    const obligations = b.obligations.length
      ? b.obligations
          .map((o) => `${o.lane_id}${o.merge_required ? " [merge-required]" : ""}${o.release_required ? " [release-required]" : ""}`)
          .join(", ")
      : "no owning CI lane resolved";
    lines.push(`  - \`${b.id}\` (${b.evidence_class}/${b.evidence_system ?? "unclassified"}): \`${b.test_ref}\` — ${obligations}`);
    if (b.verifier?.cannot_detect?.length) {
      lines.push(`    - cannot detect: ${b.verifier.cannot_detect.map(mdEscapeProse).join("; ")}`);
    }
  }
  if (example.complement_gaps.length > 0) {
    lines.push(
      `- **required complement not yet supplied**: ${example.complement_gaps
        .map((g) => `${g.verifier} needs ${g.missing_complements.join(", ")}`)
        .join("; ")}`,
    );
  }
  return lines.join("\n");
}

function renderAdversarialEvidence(evidence) {
  const lines = [];
  lines.push(
    `${evidence.mutant_count} real (non-synthetic) mutant record(s) in the semantic mutation pilot ` +
      "(`test/semantic/mutants/`; the full report's \"Semantic mutation pilot\" section carries " +
      "the kill/escape rates and every detector). Per targeted contract, each mutant's resolved " +
      "disposition and bucket; a contract absent from this table has no adversarial evidence.",
  );
  lines.push("");
  if (evidence.by_contract.length === 0) {
    lines.push("No committed mutant targets any contract.");
    return lines.join("\n");
  }
  lines.push("| Contract | Mutant | Disposition | Bucket |");
  lines.push("| --- | --- | --- | --- |");
  for (const row of evidence.by_contract) {
    for (const m of row.mutants) {
      lines.push(
        `| [\`${row.contract}\`](semantic-conformance.md#${anchor(row.contract)}) | \`${m.id}\` | ${m.disposition} | ${m.bucket} |`,
      );
    }
  }
  return lines.join("\n");
}

export function renderMarkdown(guide) {
  const parts = [];
  parts.push(
    "<!-- Code-generated by node .github/scripts/semantic-guide.mjs from the\n" +
      "     validated semantic contract model (test/semantic/*.json). DO NOT EDIT\n" +
      "     this file by hand: edit the model and run \"just semantic-guide\". CI\n" +
      "     gates on drift, so a hand-edit will fail the build. -->\n",
  );
  parts.push("# Semantic guidance\n");
  parts.push(
    "A compact, single entry point from **what task you are doing** to **what " +
      "semantic evidence it requires** — indexes the same validated contract " +
      "model [`docs/semantic-conformance.md`](semantic-conformance.md) reports " +
      "on exhaustively, without repeating its per-contract detail. Not a merge " +
      "gate: this document describes the process a change is verified through, " +
      "it does not itself certify one.\n",
  );

  parts.push("## How to use this guide\n");
  HOW_TO_USE_STEPS.forEach((step, i) => {
    parts.push(`${i + 1}. **${step.title}.** ${step.body}\n`);
  });

  parts.push("## Metadata integrity\n");
  parts.push(`${guide.metadata_integrity_note}\n`);

  parts.push("## Contract index\n");
  parts.push(
    "Every ACTIVE enrolled contract, one row each. `Canonical execution` is the " +
      "recipe of the CI lane that runs the first active binding's evidence — the " +
      "property lane for a property shape, the head's chDB round trip (or `check`) " +
      "for a TXTAR fixture, the head's compat lane for a compatibility harness, " +
      "`check` for any other Go test — deterministically chosen; tree-wide lanes " +
      "(lint, governance) are never an obligation of a binding. A contract may " +
      "bind more than one verifier; see its full card for the rest.\n",
  );
  parts.push(renderIndexTable(guide.contract_index));
  parts.push("");

  parts.push("## Selected architectural rules\n");
  parts.push(
    "Every ACTIVE contract whose `scope` is `architecture` — cross-cutting " +
      "rules that bind the shared engine rather than one head, several of " +
      "which correspond to a numbered hard invariant in [`CLAUDE.md`](../CLAUDE.md) " +
      "(named where the contract's own statement cites one; a contract with no " +
      "citation is still a real architectural rule, just one CLAUDE.md's " +
      "numbered list does not separately enumerate).\n",
  );
  parts.push(renderArchitecturalRules(guide.architectural_rules));
  parts.push("");

  parts.push("## Worked examples\n");
  parts.push(
    "One per head, plus one shared-engine (architecture-scope) change — real " +
      "contracts from the committed model, not synthetic placeholders, chosen " +
      "so the four examples show different comparator authorities rather than " +
      "four copies of the same shape.\n",
  );
  for (const example of guide.worked_examples) {
    parts.push(renderWorkedExample(example));
    parts.push("");
  }

  parts.push("## Adversarial evidence\n");
  parts.push(renderAdversarialEvidence(guide.adversarial_evidence));
  parts.push("");

  parts.push("## See also\n");
  parts.push(
    "- [`docs/semantic-conformance.md`](semantic-conformance.md) — the full " +
      "report: every contract's bound vs observed evidence, blind spots and " +
      "complement gaps.\n" +
      "- `just semantic-impact <base> <head>` — this guide's step 1, live " +
      "against any two revisions.\n" +
      "- [`docs/test-strategy.md`](test-strategy.md) — the 14-layer test map " +
      "and CI-gate inventory these contracts annotate.\n" +
      "- [`docs/agent-workflow.md`](agent-workflow.md) — the review ritual " +
      "this guide's metadata-integrity note refers to.\n",
  );

  return `${parts.join("\n")}\n`;
}

export function renderJSON(guide) {
  return `${JSON.stringify(guide, null, 2)}\n`;
}
