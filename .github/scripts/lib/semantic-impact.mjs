// semantic-impact.mjs — `just semantic-impact <base> <head>`: which semantic
// contracts a git range affects, why, what evidence they require, the exact
// existing verifier recipes to run, and the current merge/release
// obligations (cerberus issue #3460) — DERIVED entirely from machinery that
// already exists, never a second, hand-maintained dependency graph:
//
//   - the validated semantic contract model (./semantic-model.mjs,
//     test/semantic/*.json, issue #3426): which contracts exist, their
//     required evidence classes/independence groups, and their bound
//     (structural) assurance coverage (model.assurance.perContract).
//   - the CI lane registry's dependency closure (./lane-closure.mjs, issue
//     #2902): which package directories a lane's own declared globs
//     transitively reach, read out of the real `go list` import graph
//     rather than a hand-maintained list. merge-risk.mjs (issue #2899)
//     already consumes this the same way, for the same reason: a lane's OWN
//     `package_globs` names only its own directories, and the shared query
//     pipeline (chplan -> optimizer -> chsql -> chclient, driven by engine)
//     sits underneath every head's lane without appearing in any of their
//     declared globs.
//   - the binding-to-lane join and merge/release requiredness
//     classification (./semantic-lane-adapter.mjs, issue #3427) and the
//     lane-obligation resolution ./semantic-report.mjs (issue #3435)
//     already built on top of it.
//   - the registry's own `impact_selection.known_nonimpact_globs`
//     (ci-lane-contract.mjs), which quickstart-canary.mjs's `select` step
//     already reads to keep a docs-only diff from forcing a lane-relevant
//     verdict. Reused here for the same reason: a lane whose own declared
//     `package_globs` is the literal wildcard `**` (ci.lint,
//     governance.forbid-deferral) is technically "touched" by any path
//     string at all, and without this filter every docs-only diff would
//     read as affecting every contract those lanes own bindings for.
//
// This module adds exactly ONE new join on top of those four: which of a
// diff's changed files fall inside the DERIVED affected-path set of a lane
// that owns at least one active semantic binding. Everything downstream of
// that join — which contract, which evidence, which recipe, which
// obligation — is a lookup into data the three modules above already
// compute, never a second graph. Issues #2230/#2261 already tried and
// abandoned a from-scratch impact selector; #2902 already fixed the actual
// defect (declared globs alone under-report a lane's real dependency
// surface) once, and building a second graph here would silently reopen
// that exact hole one layer up.
//
// CONSERVATIVE ON UNKNOWN INPUT. A base/head ref this checkout cannot
// resolve, or an import graph `go list` cannot load, never collapses to
// "nothing affected" — that answer is indistinguishable from a clean diff
// and is the one this module must never give when it does not actually
// know. Both failure modes instead widen to "every lane this model has
// active evidence on is conservatively marked touched", with the reason
// naming exactly what could not be computed (buildImpactReport,
// resolveLaneClosures).
//
// ADVERSARIAL EVIDENCE. issue #3426's evidence_class vocabulary
// (execution/property/reference/static-analysis/manual-review) has no
// mutation/adversarial member yet — the M2 mutation-pilot binding class
// this command's own issue explicitly says it must not wait for. Every
// impacted contract therefore always reports an explicit (for now, always
// empty) `adversarial_bindings` list rather than omitting the field, so "no
// adversarial evidence exists yet" reads as a stated fact, not a silently
// absent one.
//
// SCOPE. Advisory only. This never selects, skips, or approves a CI
// workflow, and a targeted local green from a recipe this module names is
// never, by itself, a claim that the owning lane's merge or release
// obligation is satisfied — see MERGE_RELEASE_CAVEAT, printed with every
// report.

import { classifyBindingEvidenceSystem, resolveBindingObligations } from "./semantic-report.mjs";
import { resolveBindingLanes } from "./semantic-lane-adapter.mjs";
import { declaredGlobs, laneAffectedGlobs } from "./lane-closure.mjs";
import { matchesGlob } from "../ci-lane-contract.mjs";

export const IMPACT_SCHEMA_VERSION = 1;

export const ADVERSARIAL_NOTE =
  "no mutation/adversarial evidence class exists yet in the semantic model " +
  "(EVIDENCE_CLASSES, lib/semantic-model.mjs) — shown explicitly as none " +
  "rather than omitted, per this command's own scope: it must work before " +
  "the M2 mutation-pilot bindings land.";

export const MERGE_RELEASE_CAVEAT =
  "A green run of the recipe(s) named above is evidence for that binding's " +
  "own verifier only. It is never, by itself, a claim that the owning " +
  "lane's merge or release obligation is satisfied — that requires the " +
  "lane's own full, canonically-tagged run (see each binding's " +
  "merge_required/release_required below), not a narrowed local " +
  "reproduction of one test.";

// --- Owner lanes: bounding what closures are even worth computing ---------

/**
 * The lane IDs at least one ACTIVE binding resolves to via its DECLARED
 * package_globs (resolveBindingLanes — cheap, no `go list`). This bounds
 * which lanes' dependency closures are worth paying to compute: a lane no
 * binding ever resolves to cannot own any contract's evidence no matter
 * what its closure contains.
 */
export function ownerLaneIds(model, registry) {
  const ids = new Set();
  for (const [, binding] of model.bindings) {
    if (binding.status !== "active") continue;
    for (const lane of resolveBindingLanes(binding, registry, matchesGlob)) ids.add(lane.id);
  }
  return ids;
}

// --- Lane closures and their derived touch set -----------------------------

/**
 * Resolves the DERIVED affected-path globs (declared UNION dependency
 * closure) for `lanes`, exactly the lane-closure.mjs machinery merge-
 * risk.mjs already runs — never re-derived here. On a `go list` failure (no
 * toolchain, a broken module) returns `{ ok: false, error }` rather than a
 * narrower answer: falling back to the declared-only globs would silently
 * re-open the #2902 hole this whole join exists to avoid.
 */
export function resolveLaneClosures(lanes, { repoRoot, runGoList } = {}) {
  try {
    return { ok: true, affected: laneAffectedGlobs(lanes, { repoRoot, runGoList }), error: null };
  } catch (error) {
    return { ok: false, affected: null, error: error instanceof Error ? error.message : String(error) };
  }
}

/**
 * Which of `lanes` a changed-file set touches, and why. Returns
 * `Map<laneId, { touched, conservative, reasons }>`.
 *
 * Each reason is `{ file, glob, kind }` with `kind` "declared" when the
 * file matches one of the lane's own `package_globs` directly, "derived"
 * when it matches only through the dependency-closure union — the
 * distinction the shared-pipeline framing (chplan/chsql/engine/chclient)
 * exists to surface, not just "touched". A file that matches BOTH is
 * reported once, as "declared" — the narrower, more legible claim.
 *
 * `closures.ok === false` (the graph could not be loaded) marks EVERY lane
 * touched, `conservative: true`, with one reason naming the failure —
 * never silently narrows to whichever lanes happened to match on declared
 * globs alone.
 */
export function touchedLanes(files, lanes, declared, closures) {
  const out = new Map();
  const fileList = [...new Set(files ?? [])].sort();
  for (const lane of lanes) {
    if (!closures.ok) {
      out.set(lane.id, {
        touched: true,
        conservative: true,
        reasons: [
          {
            file: null,
            glob: null,
            kind: "conservative",
            detail: `import graph unavailable (${closures.error}) — every lane with active semantic evidence is conservatively marked touched`,
          },
        ],
      });
      continue;
    }
    const declGlobs = declared.get(lane.id) ?? [];
    const allGlobs = closures.affected.get(lane.id) ?? [];
    const reasons = [];
    for (const file of fileList) {
      const declaredHit = declGlobs.find((g) => matchesGlob(file, g));
      if (declaredHit) {
        reasons.push({ file, glob: declaredHit, kind: "declared" });
        continue;
      }
      const derivedHit = allGlobs.find((g) => matchesGlob(file, g));
      if (derivedHit) reasons.push({ file, glob: derivedHit, kind: "derived" });
    }
    out.set(lane.id, { touched: reasons.length > 0, conservative: false, reasons });
  }
  return out;
}

// --- Contract fan-out --------------------------------------------------------

function byId(a, b) {
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/**
 * Every contract with >=1 ACTIVE binding resolving (via resolveBindingLanes)
 * to a touched lane, sorted by contract ID. Never restricted to
 * `contract.status === "active"`: a draft or explicit_deficit contract is
 * still worth surfacing to a reader deciding what to verify, and its
 * status is reported plainly rather than silently filtered out.
 */
export function impactedContracts(model, registry, snapshot, touched) {
  const byContract = new Map();

  for (const [, binding] of model.bindings) {
    if (binding.status !== "active") continue;
    const lanes = resolveBindingLanes(binding, registry, matchesGlob);
    const hits = lanes
      .map((lane) => ({ lane, touch: touched.get(lane.id) }))
      .filter(({ touch }) => touch?.touched);
    if (hits.length === 0) continue;

    const contract = model.contracts.get(binding.contract);
    if (!contract) continue; // schema already forbids a dangling reference

    if (!byContract.has(contract.id)) byContract.set(contract.id, { contract, bindings: [] });
    const verifier = model.verifiers.get(binding.verifier) ?? null;
    byContract.get(contract.id).bindings.push({
      binding,
      verifier,
      evidenceSystem: classifyBindingEvidenceSystem(binding),
      obligations: resolveBindingObligations(binding, registry, snapshot),
      triggers: hits
        .map(({ lane, touch }) => ({
          lane_id: lane.id,
          recipes: lane.recipes ?? [],
          command: lane.command ?? null,
          conservative: touch.conservative,
          reasons: touch.reasons,
        }))
        .sort((a, b) => (a.lane_id < b.lane_id ? -1 : a.lane_id > b.lane_id ? 1 : 0)),
    });
  }

  const out = [];
  for (const [id, { contract, bindings }] of byContract) {
    const coverage = model.assurance.perContract.get(id) ?? {
      coveredClasses: new Set(),
      coveredGroups: new Set(),
      missingClasses: contract.required_evidence_classes ?? [],
      missingGroups: contract.required_independence_groups ?? [],
    };
    out.push({
      id,
      statement: contract.statement,
      status: contract.status,
      scope: contract.scope,
      applicable_heads: [...contract.applicableHeads].sort(),
      required_evidence_classes: contract.required_evidence_classes ?? [],
      required_independence_groups: contract.required_independence_groups ?? [],
      bound_evidence: {
        missing_evidence_classes: [...coverage.missingClasses].sort(),
        missing_independence_groups: [...coverage.missingGroups].sort(),
        assured:
          contract.status === "active" &&
          coverage.missingClasses.length === 0 &&
          coverage.missingGroups.length === 0,
      },
      adversarial_bindings: [],
      adversarial_note: ADVERSARIAL_NOTE,
      bindings: bindings
        .sort((a, b) => (a.binding.id < b.binding.id ? -1 : a.binding.id > b.binding.id ? 1 : 0))
        .map((b) => ({
          id: b.binding.id,
          verifier: b.verifier?.id ?? null,
          evidence_class: b.binding.evidence_class,
          independence_group: b.binding.independence_group,
          test_ref: b.binding.test_ref,
          evidence_system: b.evidenceSystem,
          obligations: b.obligations,
          triggers: b.triggers,
        })),
    });
  }
  return out.sort(byId);
}

// --- Top-level assembly ------------------------------------------------------

/**
 * Builds the full, deterministic impact report for the range `base...head`.
 *
 * `revExists`/`changedFiles` are injected (merge-risk.mjs's own exports,
 * normally) so this stays testable without a real checkout. Neither ref
 * resolving, or `changedFiles` throwing, sets `unknown_input` and engages
 * the conservative fallback described in touchedLanes' header — every lane
 * with active semantic evidence is reported touched, never nothing.
 */
export function buildImpactReport({
  model,
  registry,
  snapshot,
  repoRoot,
  base,
  head,
  revExists,
  changedFiles,
  runGoList,
}) {
  let files = [];
  let unknownInput = null;
  if (!revExists(base) || !revExists(head)) {
    unknownInput = `one or both of base=${JSON.stringify(base)}, head=${JSON.stringify(head)} do not resolve in this checkout`;
  } else {
    try {
      files = changedFiles(`${base}...${head}`);
    } catch (error) {
      unknownInput = `git diff ${base}...${head} failed: ${error instanceof Error ? error.message : String(error)}`;
    }
  }

  // A path in the registry's OWN `impact_selection.known_nonimpact_globs`
  // (docs, CLAUDE*, LICENSE*, ...) is never a trigger for ANY lane — the
  // same field quickstart-canary.mjs's own `select` step already reads to
  // decide whether a diff can skip the quickstart canary
  // (.github/scripts/README.md's own description: "names the paths ... that
  // a diff can touch without ever making a lane relevant"). Reused here
  // rather than declared again: without it, a lane whose own declared
  // package_globs is the literal wildcard "**" (ci.lint,
  // governance.forbid-deferral) would report every docs-only diff as
  // touching it, which is technically true of the glob but not the useful
  // reading of "impact" this command exists to give.
  const nonimpactGlobs = registry?.impact_selection?.known_nonimpact_globs ?? [];
  const relevantFiles = files.filter((f) => !nonimpactGlobs.some((g) => matchesGlob(f, g)));

  const owners = [...ownerLaneIds(model, registry)].sort();
  const lanesById = new Map(registry.lanes.map((l) => [l.id, l]));
  const ownerLanes = owners.map((id) => lanesById.get(id)).filter(Boolean);
  const declared = declaredGlobs(ownerLanes);

  let touched;
  if (unknownInput) {
    // Conservative: every owner lane is treated as touched — there is no
    // file set to test against, and an empty touched set would print as
    // "nothing affected" for a diff this command could not even read.
    touched = new Map(
      owners.map((id) => [
        id,
        {
          touched: true,
          conservative: true,
          reasons: [{ file: null, glob: null, kind: "conservative", detail: unknownInput }],
        },
      ]),
    );
  } else {
    const closures = resolveLaneClosures(ownerLanes, { repoRoot, runGoList });
    touched = touchedLanes(relevantFiles, ownerLanes, declared, closures);
  }

  const contracts = impactedContracts(model, registry, snapshot, touched);

  return {
    schema_version: IMPACT_SCHEMA_VERSION,
    base,
    head,
    changed_file_count: files.length,
    known_nonimpact_file_count: unknownInput ? 0 : files.length - relevantFiles.length,
    unknown_input: unknownInput,
    owner_lane_count: owners.length,
    touched_lane_count: [...touched.values()].filter((t) => t.touched).length,
    merge_release_caveat: MERGE_RELEASE_CAVEAT,
    adversarial_note: ADVERSARIAL_NOTE,
    contracts,
  };
}

// --- Rendering ---------------------------------------------------------------

function renderTrigger(t) {
  const recipe = t.command ?? (t.recipes.length ? t.recipes.join(", ") : "(no recipe declared)");
  const why = t.conservative
    ? t.reasons.map((r) => r.detail).join("; ")
    : t.reasons.map((r) => `${r.file} (${r.kind}: ${r.glob})`).join(", ");
  return `      lane ${t.lane_id}: run \`${recipe}\` — triggered by ${why}`;
}

function renderBinding(b) {
  const lines = [];
  const obligations = b.obligations.length
    ? b.obligations
        .map(
          (o) =>
            `${o.lane_id}${o.merge_required ? " [merge-required]" : ""}${o.release_required ? " [release-required]" : ""}`,
        )
        .join(", ")
    : "no owning CI lane resolved";
  lines.push(
    `  - ${b.id} (verifier ${b.verifier ?? "(dangling)"}, ${b.evidence_class}/${b.independence_group}): \`${b.test_ref}\``,
  );
  lines.push(`      obligations: ${obligations}`);
  for (const t of b.triggers) lines.push(renderTrigger(t));
  return lines.join("\n");
}

function renderContract(c) {
  const lines = [];
  lines.push(`### ${c.id} (status: ${c.status})`);
  lines.push(c.statement);
  lines.push(`- required evidence classes: ${c.required_evidence_classes.join(", ") || "(none)"}`);
  lines.push(`- required independence groups: ${c.required_independence_groups.join(", ") || "(none)"}`);
  const missing =
    c.bound_evidence.missing_evidence_classes.length || c.bound_evidence.missing_independence_groups.length
      ? ` (missing classes: ${c.bound_evidence.missing_evidence_classes.join(", ") || "none"}; missing groups: ${c.bound_evidence.missing_independence_groups.join(", ") || "none"})`
      : "";
  lines.push(`- structurally assured: ${c.bound_evidence.assured}${missing}`);
  lines.push(
    `- adversarial evidence: ${c.adversarial_bindings.length ? c.adversarial_bindings.join(", ") : "none"} — ${c.adversarial_note}`,
  );
  for (const b of c.bindings) lines.push(renderBinding(b));
  return lines.join("\n");
}

/** Renders the human-readable report — the CLI's default output. */
export function renderText(report) {
  const lines = [`semantic-impact: ${report.base}...${report.head}`];
  if (report.unknown_input) {
    lines.push(`  UNKNOWN INPUT: ${report.unknown_input}`);
    lines.push("  Conservative fallback engaged: every lane with active semantic evidence is treated as touched.");
  } else {
    const nonimpact =
      report.known_nonimpact_file_count > 0
        ? ` (${report.known_nonimpact_file_count} of them known-nonimpact — docs/CLAUDE*/LICENSE*-shaped — and never a trigger)`
        : "";
    lines.push(
      `  ${report.changed_file_count} changed file(s)${nonimpact}; ${report.touched_lane_count}/${report.owner_lane_count} evidence-owning lane(s) touched.`,
    );
  }
  lines.push("");
  if (report.contracts.length === 0) {
    lines.push("No enrolled semantic contract is affected by this range.");
    return `${lines.join("\n")}\n`;
  }
  lines.push(`${report.contracts.length} contract(s) affected:`, "");
  for (const c of report.contracts) lines.push(renderContract(c), "");
  lines.push(report.merge_release_caveat);
  return `${lines.join("\n")}\n`;
}

/** Renders the machine-readable report (`--json`). */
export function renderJSON(report) {
  return `${JSON.stringify(report, null, 2)}\n`;
}
