// semantic-report.mjs — builds the deterministic semantic conformance report
// (docs/semantic-conformance.md + docs/semantic-conformance.json, cerberus
// issue #3435) from the validated semantic contract model
// (lib/semantic-model.mjs, issue #3426), the CI lane registry
// (ci-lane-contract.mjs) and its captured policy snapshot
// (lib/semantic-lane-adapter.mjs, issue #3427).
//
// WHAT THIS REPORT ANSWERS. Not "how many tests exist" — a count alone
// cannot tell a reader whether evidence is independent, whether it was ever
// actually observed running, or which CI lane is even responsible for it.
// Three joins this module makes, each pulling from a DIFFERENT existing
// seam rather than re-deriving or hand-copying its answer:
//
//   1. BOUND vs OBSERVED. "Bound executable evidence" is a structural fact
//      from the model alone (an active contract has an active binding
//      covering a required evidence class/independence group — exactly
//      computeAssurance's own coverage rule, reused here via
//      model.assurance.perContract rather than re-walked). "Observed
//      passing on revision X" is a SEPARATE fact from executions.json: a
//      binding with bound evidence but zero recorded executions reports
//      observed status "unknown", never "pass" — a report that silently
//      treated "bound" as "passing" would be exactly the overclaim issue
//      #3435 exists to avoid.
//   2. CORRELATION-SAFE ROLLUP. Multiple executions of one binding, or
//      multiple bindings sharing one independence group, collapse to ONE
//      pass/fail/unknown verdict per required evidence class and per
//      required independence group (rollupObserved below) — never a count.
//      Five duplicate passing runs of the same correlated test report
//      exactly the same "pass" as one, which is the acceptance criterion
//      ("multiple correlated or stale/missing observations cannot inflate
//      assurance") made structural rather than a reviewer's judgment call.
//   3. CONTRACT-TO-LANE. resolveBindingLanes (lib/semantic-lane-adapter.mjs)
//      already joins a binding's test_ref to the CI lane(s) whose
//      package_globs cover it; classifyLaneRequiredness already classifies
//      whether that lane is a required merge/release check against the
//      captured policy snapshot. This module calls both rather than
//      re-declaring "which lane owns which binding" or "which check is
//      required" as its own table.
//
// DETERMINISM. buildReport/renderMarkdown/renderJSON are pure functions of
// their inputs: no Date.now(), no random iteration order (every array this
// module emits is explicitly sorted by ID), no environment read. Running
// the CLI twice against the same test/semantic/*.json + ci-lanes.json +
// policy-snapshot.json produces byte-identical output — issue #3435's own
// acceptance criterion — and a source change therefore produces the same
// diff no matter who or what regenerates it.
//
// NON-GOALS (issue #3435's own). This module never computes a global
// correctness percentage or an E0-E7 score, never claims a declared test
// "ran" (only a real recorded execution.result of pass/fail counts as
// observed — see classifyExecutionObservation), and never selects or
// recommends a CI workflow. It reports facts already true of the model; it
// does not grade them into one number.

import { classifyTestRef, EVIDENCE_SYSTEMS } from "./semantic-evidence-adapter.mjs";
import { classifyLaneRequiredness, resolveBindingLanes } from "./semantic-lane-adapter.mjs";
import { matchesGlob } from "../ci-lane-contract.mjs";

export const REPORT_SCHEMA_VERSION = 1;
export const DEFAULT_REPORT_MD_PATH = "docs/semantic-conformance.md";
export const DEFAULT_REPORT_JSON_PATH = "docs/semantic-conformance.json";

// The one worked example the generated Markdown walks end to end, chosen
// because its own data already exercises every join this report makes: a
// Tempo-dependent (reference-implementation authority) contract, evidence
// from two independence groups, a verifier whose own cannot_detect text
// documents a count-only limitation, and a documented required complement
// (VERIFIER-REFERENCE-DIFFERENTIAL) no active binding on the contract
// actually supplies. If a future model edit removes this ID, the worked
// example section is omitted rather than failing — see renderMarkdown.
export const WORKED_EXAMPLE_CONTRACT_ID = "TRACEQL-STRUCTURAL-RELATION-SEMANTICS";

const OBSERVED_EXECUTION_RESULTS = new Set(["pass", "fail"]);

function byId(a, b) {
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

function sortedValues(map) {
  return [...map.values()].sort(byId);
}

// --- Bindings, verifiers, evidence systems ---------------------------------

/** Every ACTIVE binding for `contractId`, sorted by ID. */
export function activeBindingsForContract(model, contractId) {
  return sortedValues(model.bindings).filter(
    (b) => b.contract === contractId && b.status === "active",
  );
}

/** The verifier record a binding references, or null if it dangles (schema already forbids this). */
export function bindingVerifier(model, binding) {
  return model.verifiers.get(binding.verifier) ?? null;
}

/** Classifies a binding's test_ref into one of the five known evidence systems, or null. */
export function classifyBindingEvidenceSystem(binding) {
  return classifyTestRef(binding.test_ref).system;
}

/** Active-binding counts per evidence system (the five known ones, plus "unclassified"). */
export function evidenceSystemCounts(model) {
  const counts = Object.fromEntries(EVIDENCE_SYSTEMS.map((s) => [s, 0]));
  counts.unclassified = 0;
  for (const [, binding] of model.bindings) {
    if (binding.status !== "active") continue;
    const system = classifyBindingEvidenceSystem(binding);
    counts[system ?? "unclassified"] += 1;
  }
  return counts;
}

// --- Observed evidence (executions.json) ------------------------------------

/**
 * The most recently observed execution for `bindingId`, or null if none was
 * ever recorded. Ties on observed_at (same timestamp) break on execution ID
 * descending — arbitrary but deterministic, since real data never collides.
 */
export function latestExecution(model, bindingId) {
  const candidates = sortedValues(model.executions).filter((e) => e.binding === bindingId);
  if (candidates.length === 0) return null;
  candidates.sort((a, b) => {
    if (a.observed_at !== b.observed_at) return a.observed_at < b.observed_at ? 1 : -1;
    return a.id < b.id ? 1 : -1;
  });
  return candidates[0];
}

/**
 * Classifies one execution record's observation verdict. Only a real
 * pass/fail counts — an "error" result means the run itself broke, not that
 * it exercised the contract, so it is "unknown" here exactly as
 * lib/semantic-lane-adapter.mjs's classifyObservedEvidence already treats a
 * non-pass/fail result: never evidence, in either direction.
 */
export function classifyExecutionObservation(execution) {
  if (!execution) return "unknown";
  return OBSERVED_EXECUTION_RESULTS.has(execution.result) ? execution.result : "unknown";
}

/** The observed-evidence summary for one binding: its latest execution, classified. */
export function bindingObservedStatus(model, binding) {
  const executions = sortedValues(model.executions).filter((e) => e.binding === binding.id);
  const latest = latestExecution(model, binding.id);
  return {
    status: classifyExecutionObservation(latest),
    observed_at: latest?.observed_at ?? null,
    run_ref: latest?.run_ref ?? null,
    execution_id: latest?.id ?? null,
    execution_count: executions.length,
  };
}

/**
 * Collapses a set of per-binding observed statuses into ONE verdict: "fail"
 * if any is fail, else "pass" if any is pass, else "unknown". This is the
 * correlation-safe rollup — it is a boolean OR over a set, so N correlated
 * bindings (same independence group) or N repeated executions of the same
 * binding contribute exactly the same verdict as one does. An empty input
 * (no active binding covers this requirement at all) is "unknown", never a
 * silent pass.
 */
export function rollupObserved(statuses) {
  if (statuses.includes("fail")) return "fail";
  if (statuses.includes("pass")) return "pass";
  return "unknown";
}

/**
 * The observed-evidence rollup for one contract, mirrored one-for-one
 * against computeAssurance's own BOUND coverage rule (required evidence
 * classes ∪ required independence groups, each satisfied by >=1 covering
 * active binding) so the two layers ("is evidence bound" vs "was it seen
 * passing") answer the exact same required set, never a looser or stricter
 * one of the report's own invention.
 */
export function contractObservedRollup(model, contract, activeBindings) {
  const observedByBinding = new Map(
    activeBindings.map((b) => [b.id, bindingObservedStatus(model, b).status]),
  );

  const byEvidenceClass = {};
  for (const cls of contract.required_evidence_classes ?? []) {
    const bindingIds = activeBindings.filter((b) => b.evidence_class === cls).map((b) => b.id);
    byEvidenceClass[cls] = {
      status: rollupObserved(bindingIds.map((id) => observedByBinding.get(id))),
      binding_ids: bindingIds,
    };
  }

  const byIndependenceGroup = {};
  for (const group of contract.required_independence_groups ?? []) {
    const bindingIds = activeBindings
      .filter((b) => b.independence_group === group)
      .map((b) => b.id);
    byIndependenceGroup[group] = {
      status: rollupObserved(bindingIds.map((id) => observedByBinding.get(id))),
      binding_ids: bindingIds,
    };
  }

  const requiredStatuses = [
    ...Object.values(byEvidenceClass).map((v) => v.status),
    ...Object.values(byIndependenceGroup).map((v) => v.status),
  ];
  // Unlike rollupObserved's OR-based collapse (any pass among correlated
  // evidence is enough), the CONTRACT's overall observed status must be
  // conjunctive: every required class AND every required group has to
  // report "pass" before the contract itself can. A single unseen or
  // failing requirement holds the whole contract back — an "unknown"
  // anywhere here is never overridden by a "pass" elsewhere, which is what
  // keeps a partial observation from being silently read as a full one.
  const overall = requiredStatuses.includes("fail")
    ? "fail"
    : requiredStatuses.includes("unknown")
      ? "unknown"
      : "pass";

  return { byEvidenceClass, byIndependenceGroup, overall };
}

// --- Verifier complement gaps -----------------------------------------------

/**
 * For each verifier an ACTIVE binding on this contract actually uses,
 * checks whether the verifier's own documented complement(s)
 * (verifiers.json's complemented_by) are ALSO in use on this same contract.
 * A verifier that names a complement nothing here supplies is a real,
 * derived gap — e.g. a property-oracle verifier whose complement is a live
 * reference-backend differential, used nowhere on this contract, means the
 * contract's evidence never actually ran against the real backend even
 * though its formal required_evidence_classes may not name "reference" at
 * all. This is informational, not a new assurance rule: it never changes
 * whether computeAssurance calls the contract assured.
 */
export function verifierComplementGaps(model, activeBindings) {
  const usedVerifierIds = new Set(activeBindings.map((b) => b.verifier));
  const gaps = [];
  for (const verifierId of [...usedVerifierIds].sort()) {
    const verifier = model.verifiers.get(verifierId);
    if (!verifier) continue;
    const missing = (verifier.complemented_by ?? []).filter((c) => !usedVerifierIds.has(c));
    if (missing.length > 0) {
      gaps.push({ verifier: verifierId, missing_complements: [...missing].sort() });
    }
  }
  return gaps;
}

// --- Lane / merge / release obligations -------------------------------------

/**
 * Resolves one binding to the CI lane(s) that own its test_ref
 * (resolveBindingLanes, lib/semantic-lane-adapter.mjs) and classifies each
 * against the captured policy snapshot for both the main-ruleset (merge)
 * and release-required-checks (release) authorities. A binding whose
 * test_ref names a non-file-shaped identity (a property-shape ID, for
 * instance) legitimately resolves to zero lanes — reported as such, never
 * treated as an error.
 */
export function resolveBindingObligations(binding, registry, snapshot) {
  const lanes = resolveBindingLanes(binding, registry, matchesGlob);
  const seen = new Set();
  const obligations = [];
  for (const lane of lanes) {
    if (seen.has(lane.id)) continue;
    seen.add(lane.id);
    const merge = classifyLaneRequiredness(lane, snapshot, "pull_request");
    const release = classifyLaneRequiredness(lane, snapshot, "push_main");
    obligations.push({
      lane_id: lane.id,
      context_name: lane.context?.name ?? null,
      merge_required: merge.required,
      release_required: release.required,
    });
  }
  return obligations.sort((a, b) => (a.lane_id < b.lane_id ? -1 : a.lane_id > b.lane_id ? 1 : 0));
}

/**
 * Groups every ACTIVE binding's resolved lane obligations by lane ID, for
 * the report's lane-inventory section. `unresolved` lists the active
 * binding IDs whose test_ref resolved to zero lanes.
 */
export function laneInventory(model, registry, snapshot) {
  const lanes = new Map();
  const unresolved = [];
  for (const binding of sortedValues(model.bindings)) {
    if (binding.status !== "active") continue;
    const obligations = resolveBindingObligations(binding, registry, snapshot);
    if (obligations.length === 0) {
      unresolved.push(binding.id);
      continue;
    }
    for (const ob of obligations) {
      if (!lanes.has(ob.lane_id)) {
        lanes.set(ob.lane_id, {
          lane_id: ob.lane_id,
          context_name: ob.context_name,
          merge_required: ob.merge_required,
          release_required: ob.release_required,
          binding_ids: [],
        });
      }
      lanes.get(ob.lane_id).binding_ids.push(binding.id);
    }
  }
  return { lanes: sortedValues(lanes).map((l) => ({ ...l, id: l.lane_id })), unresolved };
}

// --- Head / scope indexing --------------------------------------------------

/** contract IDs (scope === "head") owned by each canonical head, sorted. */
export function headContractIndex(model) {
  const byHead = {};
  for (const headId of model.heads.keys()) byHead[headId] = [];
  for (const [id, contract] of model.contracts) {
    if (contract.scope !== "head") continue;
    for (const headId of contract.applicableHeads) {
      if (byHead[headId]) byHead[headId].push(id);
    }
  }
  for (const headId of Object.keys(byHead)) byHead[headId].sort();
  return byHead;
}

/** contract IDs scoped across heads (signal) or universally (architecture), sorted. */
export function crossHeadContracts(model) {
  const signal = [];
  const architecture = [];
  for (const [id, contract] of model.contracts) {
    if (contract.scope === "signal") signal.push(id);
    else if (contract.scope === "architecture") architecture.push(id);
  }
  signal.sort();
  architecture.sort();
  return { signal, architecture };
}

// --- Counts / denominators --------------------------------------------------

function statusCounts(map, statuses) {
  const counts = Object.fromEntries(statuses.map((s) => [s, 0]));
  counts.total = 0;
  for (const [, record] of map) {
    counts.total += 1;
    if (record.status in counts) counts[record.status] += 1;
  }
  return counts;
}

// --- Top-level report assembly ----------------------------------------------

/**
 * Builds the full report as one plain, deterministic object — the single
 * source both renderMarkdown and renderJSON draw from, so the two output
 * formats can never disagree with each other about a fact.
 */
export function buildReport(model, { registry, snapshot }) {
  const counts = {
    contracts: statusCounts(model.contracts, ["draft", "active", "superseded", "explicit_deficit"]),
    verifiers: statusCounts(model.verifiers, ["draft", "active", "superseded"]),
    bindings: statusCounts(model.bindings, ["draft", "active"]),
    executions: { total: model.executions.size },
  };

  const contracts = sortedValues(model.contracts).map((contract) => {
    const activeBindings = activeBindingsForContract(model, contract.id);
    const coverage = model.assurance.perContract.get(contract.id) ?? {
      coveredClasses: new Set(),
      coveredGroups: new Set(),
      missingClasses: contract.required_evidence_classes ?? [],
      missingGroups: contract.required_independence_groups ?? [],
    };
    const bindings = activeBindings.map((binding) => {
      const verifier = bindingVerifier(model, binding);
      return {
        id: binding.id,
        verifier: verifier && {
          id: verifier.id,
          name: verifier.name,
          substrate: verifier.substrate,
          relative_cost: verifier.relative_cost,
          detects: verifier.detects,
          cannot_detect: verifier.cannot_detect,
          complemented_by: verifier.complemented_by,
        },
        evidence_class: binding.evidence_class,
        independence_group: binding.independence_group,
        test_ref: binding.test_ref,
        evidence_system: classifyBindingEvidenceSystem(binding),
        observed: bindingObservedStatus(model, binding),
        obligations: resolveBindingObligations(binding, registry, snapshot),
      };
    });

    return {
      id: contract.id,
      statement: contract.statement,
      scope: contract.scope,
      applicable_heads: [...contract.applicableHeads].sort(),
      applicable_capabilities: [...(contract.applicable_capabilities ?? [])].sort(),
      authority: contract.authority,
      status: contract.status,
      deficit_reason: contract.deficit_reason,
      owner: contract.owner,
      inherits_from: contract.inherits_from,
      related_contracts: [...(contract.related_contracts ?? [])].sort(),
      replaces: contract.replaces,
      replaced_by: contract.replaced_by,
      blind_spots: contract.blind_spots ?? [],
      required_evidence_classes: contract.required_evidence_classes ?? [],
      required_independence_groups: contract.required_independence_groups ?? [],
      bound_evidence: {
        covered_evidence_classes: [...coverage.coveredClasses].sort(),
        covered_independence_groups: [...coverage.coveredGroups].sort(),
        missing_evidence_classes: [...coverage.missingClasses].sort(),
        missing_independence_groups: [...coverage.missingGroups].sort(),
        assured: contract.status === "active" && coverage.missingClasses.length === 0 && coverage.missingGroups.length === 0,
      },
      observed_evidence: contractObservedRollup(model, contract, activeBindings),
      bindings,
      complement_gaps: verifierComplementGaps(model, activeBindings),
    };
  });

  return {
    schema_version: REPORT_SCHEMA_VERSION,
    generator: "node .github/scripts/semantic-report.mjs",
    source: "test/semantic/{heads,capabilities,contracts,verifiers,bindings,executions}.json",
    counts,
    assurance_summary: {
      assured_count: model.assurance.assured.length,
      excluded: {
        draft: [...model.assurance.excluded.draft].sort(),
        superseded: [...model.assurance.excluded.superseded].sort(),
        explicit_deficit: [...model.assurance.excluded.explicit_deficit].sort(),
      },
    },
    evidence_systems: evidenceSystemCounts(model),
    lane_inventory: laneInventory(model, registry, snapshot),
    heads: headContractIndex(model),
    cross_head: crossHeadContracts(model),
    contracts,
  };
}

// --- Rendering ---------------------------------------------------------------

const DISCLAIMER_SCOPE =
  "This report describes only the semantic contracts currently ENROLLED in " +
  "test/semantic/contracts.json. A behavior with no enrolled contract has no " +
  "representation here — it is neither assured nor a documented deficit, it " +
  "is simply outside this report's denominator. Every count below is relative " +
  "to the enrolled set named beside it; none of them is a claim about total " +
  "behavioral coverage of any head.";

const DISCLAIMER_GOLDEN_VS_REFERENCE =
  "A verifier's `substrate` is never collapsed into one meaning. `chdb` and " +
  "`real-clickhouse` are cerberus's own execution paths; `reference-stack` " +
  "means a live, separately-started reference backend (Prometheus, Loki, or " +
  "Tempo) actually ran. Agreeing with a golden fixture (chdb) is not the same " +
  "claim as agreeing with the reference implementation (reference-stack), and " +
  "this report keeps them in separate fields throughout rather than folding " +
  "both into one \"passing\" bit.";

const DISCLAIMER_BOUND_VS_OBSERVED =
  "\"Bound evidence\" (below) is a structural fact: the contract has an " +
  "active binding covering each required evidence class and independence " +
  "group. \"Observed evidence\" is a separate, weaker claim: whether that " +
  "binding's verifier was actually SEEN to run — pass or fail — on some " +
  "recorded revision. A binding can be bound with no observation on record " +
  "at all, which reports as `unknown`, never as an assumed pass.";

function mdEscape(text) {
  return String(text).replaceAll("|", "\\|");
}

function renderCountsTable(counts, columns) {
  const header = `| status | count |\n| --- | --- |\n`;
  const rows = columns.map((c) => `| ${c} | ${counts[c]} |`).join("\n");
  return `${header}${rows}\n| **total** | **${counts.total}** |\n`;
}

function renderBindingRow(binding) {
  const verifierId = binding.verifier?.id ?? "(dangling)";
  const obligations = binding.obligations.length
    ? binding.obligations
        .map((o) => `${o.lane_id}${o.merge_required ? " (merge-required)" : ""}${o.release_required ? " (release-required)" : ""}`)
        .join("; ")
    : "no owning CI lane resolved";
  return `| ${binding.id} | ${verifierId} | ${binding.evidence_class} | ${binding.independence_group} | \`${mdEscape(binding.test_ref)}\` | ${binding.observed.status} | ${obligations} |`;
}

function renderWorkedExample(report) {
  const contract = report.contracts.find((c) => c.id === WORKED_EXAMPLE_CONTRACT_ID);
  if (!contract) return "";

  const gapsText = contract.complement_gaps.length
    ? contract.complement_gaps
        .map(
          (g) =>
            `verifier \`${g.verifier}\` documents required complement(s) \`${g.missing_complements.join("`, `")}\` that no active binding on this contract supplies`,
        )
        .join("; ")
    : "no complement gap on this contract's currently active bindings";

  return `
## Worked example: looking up a contract

The JSON sibling of this document (\`${DEFAULT_REPORT_JSON_PATH}\`) is the report's
query interface — every fact below is one \`.contracts[]\` element, addressable
with \`jq\`:

\`\`\`sh
jq '.contracts[] | select(.id == "${WORKED_EXAMPLE_CONTRACT_ID}")' ${DEFAULT_REPORT_JSON_PATH}
\`\`\`

That query returns, for **${contract.id}**:

- **Its tests** — ${contract.bindings.length} active binding(s):
${contract.bindings.map((b) => `  - \`${b.id}\` (\`${b.test_ref}\`, verifier \`${b.verifier?.id}\`)`).join("\n")}
- **Its dependency on the reference implementation** — authority
  \`${contract.authority}\`; the statement itself names the reference system
  the contract's semantics are defined against.
- **Its count-only limitation** — the property-based verifier(s) bound here
  report only what their own \`cannot_detect\` documents:
${[...new Set(contract.bindings.map((b) => b.verifier?.cannot_detect?.join(" / ")).filter(Boolean))].map((c) => `  - ${c}`).join("\n")}
- **Its required complements** — ${gapsText}.

This is the general shape the whole report follows: every contract card below
carries the same bound-evidence / observed-evidence / blind-spots /
complement-gaps structure, not a one-off treatment for this example.
`;
}

function renderContractCard(contract) {
  const lines = [];
  lines.push(`### ${contract.id}`);
  lines.push("");
  lines.push(contract.statement);
  lines.push("");
  lines.push(
    `- scope: \`${contract.scope}\` · applicable heads: ${contract.applicable_heads.map((h) => `\`${h}\``).join(", ") || "(none)"}`,
  );
  lines.push(`- authority: \`${contract.authority}\` · status: \`${contract.status}\` · owner: \`${contract.owner}\``);
  if (contract.status === "explicit_deficit") lines.push(`- deficit reason: ${contract.deficit_reason}`);
  if (contract.status === "superseded") lines.push(`- replaced by: \`${contract.replaced_by}\``);
  if (contract.inherits_from) lines.push(`- inherits from: \`${contract.inherits_from}\``);
  if (contract.related_contracts.length) {
    lines.push(`- related contracts: ${contract.related_contracts.map((c) => `\`${c}\``).join(", ")}`);
  }
  lines.push("");

  lines.push(
    `**Bound evidence** (structural — assured: **${contract.bound_evidence.assured}**): ` +
      `required classes \`${contract.required_evidence_classes.join(", ") || "(none)"}\`, ` +
      `required independence groups \`${contract.required_independence_groups.join(", ") || "(none)"}\`.`,
  );
  if (contract.bound_evidence.missing_evidence_classes.length || contract.bound_evidence.missing_independence_groups.length) {
    lines.push(
      `Missing: classes \`${contract.bound_evidence.missing_evidence_classes.join(", ") || "none"}\`, ` +
        `groups \`${contract.bound_evidence.missing_independence_groups.join(", ") || "none"}\`.`,
    );
  }
  lines.push("");

  lines.push(`**Observed evidence** (from executions.json, revision-scoped): overall **${contract.observed_evidence.overall}**.`);
  for (const [cls, v] of Object.entries(contract.observed_evidence.byEvidenceClass)) {
    lines.push(`  - class \`${cls}\`: **${v.status}** (${v.binding_ids.join(", ") || "no active binding"})`);
  }
  for (const [group, v] of Object.entries(contract.observed_evidence.byIndependenceGroup)) {
    lines.push(`  - group \`${group}\`: **${v.status}** (${v.binding_ids.join(", ") || "no active binding"})`);
  }
  lines.push("");

  if (contract.bindings.length > 0) {
    lines.push("| binding | verifier | evidence class | independence group | test_ref | observed | CI lane obligations |");
    lines.push("| --- | --- | --- | --- | --- | --- | --- |");
    for (const b of contract.bindings) lines.push(renderBindingRow(b));
    lines.push("");
  }

  if (contract.blind_spots.length > 0) {
    lines.push("**Blind spots:**");
    for (const b of contract.blind_spots) lines.push(`- ${b}`);
    lines.push("");
  }

  if (contract.complement_gaps.length > 0) {
    lines.push("**Complement gaps** (informational — does not change assurance above):");
    for (const g of contract.complement_gaps) {
      lines.push(`- verifier \`${g.verifier}\` documents complement(s) not in active use here: \`${g.missing_complements.join("`, `")}\``);
    }
    lines.push("");
  }

  return lines.join("\n");
}

function renderLaneInventory(report) {
  const lines = [];
  lines.push("| CI lane | merge-required | release-required | active bindings bound |");
  lines.push("| --- | --- | --- | --- |");
  for (const lane of report.lane_inventory.lanes) {
    lines.push(`| ${lane.id} | ${lane.merge_required} | ${lane.release_required} | ${lane.binding_ids.length} |`);
  }
  lines.push("");
  lines.push(
    `${report.lane_inventory.unresolved.length} active binding(s) resolve to no CI lane from their test_ref alone ` +
      "(expected for non-file-shaped identities such as property-shape IDs): " +
      `${report.lane_inventory.unresolved.join(", ") || "(none)"}.`,
  );
  return lines.join("\n");
}

function renderHeadSection(report, headId, headName) {
  const ids = report.heads[headId] ?? [];
  const lines = [`## ${headName} (\`${headId}\`)`, ""];
  lines.push(`${ids.length} contract(s) scoped exclusively to this head.`);
  lines.push("");
  for (const id of ids) {
    const contract = report.contracts.find((c) => c.id === id);
    if (contract) lines.push(renderContractCard(contract));
  }
  return lines.join("\n");
}

/** Renders the full Markdown document from a buildReport() result. */
export function renderMarkdown(report) {
  const parts = [];
  parts.push(
    "<!-- Code-generated by node .github/scripts/semantic-report.mjs from the\n" +
      "     validated semantic contract model (test/semantic/*.json). DO NOT EDIT\n" +
      "     this file by hand: edit the model and run \"just semantic-report\". CI\n" +
      "     gates on drift, so a hand-edit will fail the build. -->\n",
  );
  parts.push("# Semantic conformance report\n");
  parts.push(
    "This document joins the semantic contract model's obligations " +
      "(`test/semantic/contracts.json`) to their actual verifier owners, " +
      "the evidence bound to them, whether that evidence has ever been " +
      "observed passing, and which CI lane (if any) is responsible for it. " +
      "See [`docs/test-strategy.md`](test-strategy.md) for the broader " +
      "14-layer test map this model annotates.\n",
  );
  parts.push("## Scope and denominator\n");
  parts.push(`${DISCLAIMER_SCOPE}\n`);
  parts.push(`${DISCLAIMER_BOUND_VS_OBSERVED}\n`);
  parts.push(`${DISCLAIMER_GOLDEN_VS_REFERENCE}\n`);

  parts.push("### Contracts\n");
  parts.push(renderCountsTable(report.counts.contracts, ["active", "draft", "superseded", "explicit_deficit"]));
  parts.push(
    `\nOf the ${report.counts.contracts.active} active contracts, **${report.assurance_summary.assured_count}** ` +
      "are structurally assured (bound evidence covers every required class and independence group).\n",
  );
  parts.push("### Verifiers\n");
  parts.push(renderCountsTable(report.counts.verifiers, ["active", "draft", "superseded"]));
  parts.push("### Bindings\n");
  parts.push(renderCountsTable(report.counts.bindings, ["active", "draft"]));
  parts.push(`\nExecutions on record: **${report.counts.executions.total}**.\n`);

  parts.push("## Evidence system inventory\n");
  parts.push(
    "Active bindings, classified by which existing identity system their " +
      "`test_ref` names (derived, never hand-counted — see " +
      "`.github/scripts/lib/semantic-evidence-adapter.mjs`):\n",
  );
  parts.push("| evidence system | active bindings |");
  parts.push("| --- | --- |");
  for (const [system, count] of Object.entries(report.evidence_systems)) {
    parts.push(`| ${system} | ${count} |`);
  }
  parts.push("");

  parts.push("## Lane inventory (merge / release obligations)\n");
  parts.push(renderLaneInventory(report));
  parts.push("");

  const worked = renderWorkedExample(report);
  if (worked) parts.push(worked);

  parts.push(renderHeadSection(report, "HEAD-PROMQL", "PromQL"));
  parts.push(renderHeadSection(report, "HEAD-LOGQL", "LogQL"));
  parts.push(renderHeadSection(report, "HEAD-TRACEQL", "TraceQL"));

  parts.push("## Cross-head contracts\n");
  parts.push(
    "Contracts scoped to a signal spanning more than one head, or to the " +
      "shared architecture (`applicable_heads: \"universal\"`) — never folded " +
      "into a single head's section above, since asymmetry between heads is " +
      "exactly what this report exists to preserve.\n",
  );
  for (const id of report.cross_head.signal) {
    const contract = report.contracts.find((c) => c.id === id);
    if (contract) parts.push(renderContractCard(contract));
  }
  for (const id of report.cross_head.architecture) {
    const contract = report.contracts.find((c) => c.id === id);
    if (contract) parts.push(renderContractCard(contract));
  }

  parts.push("## Deficits, drafts and superseded contracts\n");
  parts.push(
    "Excluded from the assured count above by design — a draft is not yet " +
      "load-bearing, a superseded contract is historical, and an explicit " +
      "deficit is a KNOWN, tracked gap rather than a silently missing one:\n",
  );
  parts.push(`- draft: ${report.assurance_summary.excluded.draft.map((i) => `\`${i}\``).join(", ") || "(none)"}`);
  parts.push(`- superseded: ${report.assurance_summary.excluded.superseded.map((i) => `\`${i}\``).join(", ") || "(none)"}`);
  parts.push(
    `- explicit deficit: ${report.assurance_summary.excluded.explicit_deficit.map((i) => `\`${i}\``).join(", ") || "(none)"}`,
  );
  parts.push("");

  return `${parts.join("\n")}\n`;
}

/** Renders the JSON sibling: the same report object, stable key order, trailing newline. */
export function renderJSON(report) {
  return `${JSON.stringify(report, null, 2)}\n`;
}
