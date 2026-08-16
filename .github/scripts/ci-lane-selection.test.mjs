// Negative controls for the merge-fence selector. These tests pin both safety
// directions: uncertainty cannot omit work, and a successful precise routing
// decision cannot silently keep unrelated expensive lanes selected.

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  ContractError,
  matchesGlob,
  validateSelection,
} from "./ci-lane-contract.mjs";
import {
  checkoutEvidence,
  createSelection,
  deriveChangedPaths,
  parseRunAttempt,
  selectionPolicy,
  writeSelection,
} from "./ci-lane-selection.mjs";

const sha = (character) => character.repeat(40);

function lane(id, mergePosture, packageGlobs) {
  return {
    id,
    executions: ["default"],
    package_globs: packageGlobs,
    merge_posture: mergePosture,
    main_posture: "always",
    release_posture: "advisory",
    applicability: { source: true, artifact: false },
  };
}

function registryFixture() {
  return {
    schema_version: 1,
    selection_schema_version: 1,
    report_schema_version: 1,
    impact_selection: {
      known_nonimpact_globs: ["**/*.md", "docs/**"],
    },
    lanes: [
      lane("always", "always", ["internal/**"]),
      lane("impact-a", "impact", ["internal/a/**"]),
      lane("impact-b", "impact", ["internal/b/**"]),
      lane("never", "never", ["release/**"]),
      lane("quickstart", "non_documentation", ["README.md", "runtime/**"]),
    ],
  };
}

function selectedIDs(policy) {
  return policy.lanes
    .filter((laneSelection) => laneSelection.disposition === "selected")
    .map((laneSelection) => laneSelection.lane_id);
}

function laneSelection(policy, id) {
  return policy.lanes.find((candidate) => candidate.lane_id === id);
}

function selection(changedPaths, fallbackReason = "none") {
  return createSelection({
    registry: registryFixture(),
    baseSHA: sha("a"),
    source: { sha: sha("b"), tree: sha("c") },
    runID: "123",
    runAttempt: 2,
    changedPaths,
    fallbackReason,
  });
}

test("documentation-only changes omit conditional work with exact reasons", () => {
  const result = selection(["docs/engine.md"]);
  assert.equal(result.document.selector.conclusion, "success");
  assert.deepEqual(selectedIDs(result.document), ["always"]);
  for (const id of ["impact-a", "impact-b", "quickstart"]) {
    assert.deepEqual(laneSelection(result.document, id), {
      lane_id: id,
      disposition: "omitted",
      executions: [],
      reason: "docs_only",
    });
  }
});

test("a direct README contract hit selects quickstart despite Markdown", () => {
  const result = selection(["README.md"]);
  assert.deepEqual(selectedIDs(result.document), ["always", "quickstart"]);
  assert.equal(laneSelection(result.document, "impact-a").reason, "docs_only");
});

test("one code impact selects exactly its lane plus non-documentation", () => {
  const result = selection(["internal/a/change.go"]);
  assert.deepEqual(selectedIDs(result.document), [
    "always",
    "impact-a",
    "quickstart",
  ]);
  assert.equal(laneSelection(result.document, "impact-b").reason, "not_impacted");
  assert.equal(laneSelection(result.document, "never").reason, "posture_excluded");
});

test("overlapping globs select every direct owner", () => {
  const registry = registryFixture();
  registry.lanes[2].package_globs.push("internal/a/shared/**");
  const result = selectionPolicy(registry, ["internal/a/shared/change.go"]);
  assert.deepEqual(selectedIDs(result), [
    "always",
    "impact-a",
    "impact-b",
    "quickstart",
  ]);
});

test("mixed documentation and code never manufactures docs-only omissions", () => {
  const result = selection(["docs/engine.md", "internal/a/change.go"]);
  assert.equal(laneSelection(result.document, "impact-b").reason, "not_impacted");
  assert.equal(laneSelection(result.document, "quickstart").disposition, "selected");
});

test("unknown and empty path sets select every conditional lane", () => {
  const unknown = selection(["new-surface/runtime.cfg"]);
  assert.equal(unknown.document.selector.conclusion, "fallback_full");
  assert.deepEqual(unknown.document.selector.unknown_paths, [
    "new-surface/runtime.cfg",
  ]);
  assert.deepEqual(selectedIDs(unknown.document), [
    "always",
    "impact-a",
    "impact-b",
    "quickstart",
  ]);

  const empty = selection([]);
  assert.equal(empty.document.selector.conclusion, "fallback_full");
  assert.equal(empty.fallbackReason, "empty_diff");
  assert.deepEqual(selectedIDs(empty.document), selectedIDs(unknown.document));
  assert.equal(laneSelection(empty.document, "never").disposition, "omitted");
});

test("explicit diff failures select full conditional work but preserve never", () => {
  for (const reason of [
    "merge_base_unavailable",
    "diff_unavailable",
    "unrepresentable_path",
  ]) {
    const result = selection([], reason);
    assert.equal(result.document.selector.conclusion, "fallback_full", reason);
    assert.deepEqual(selectedIDs(result.document), [
      "always",
      "impact-a",
      "impact-b",
      "quickstart",
    ]);
    assert.equal(laneSelection(result.document, "never").reason, "posture_excluded");
  }
});

test("contract rejects both missing work and extra unimpacted work", () => {
  const { document } = selection(["internal/a/change.go"]);
  const missing = structuredClone(document);
  Object.assign(laneSelection(missing, "impact-a"), {
    disposition: "omitted",
    executions: [],
    reason: "not_impacted",
  });
  assert.throws(
    () => validateSelection(missing, registryFixture()),
    /must select impacted lane impact-a/,
  );

  const extra = structuredClone(document);
  Object.assign(laneSelection(extra, "impact-b"), {
    disposition: "selected",
    executions: ["default"],
    reason: null,
  });
  assert.throws(
    () => validateSelection(extra, registryFixture()),
    /must omit unimpacted lane impact-b/,
  );

  const noQuickstart = structuredClone(document);
  Object.assign(laneSelection(noQuickstart, "quickstart"), {
    disposition: "omitted",
    executions: [],
    reason: "docs_only",
  });
  assert.throws(
    () => validateSelection(noQuickstart, registryFixture()),
    /must select non-documentation lane quickstart/,
  );
});

test("glob matcher stays anchored with explicit star semantics", () => {
  assert.equal(matchesGlob("README.md", "**/*.md"), true);
  assert.equal(matchesGlob("docs/deep/file.md", "docs/**"), true);
  assert.equal(matchesGlob("internal/a/x.go", "internal/*/*.go"), true);
  assert.equal(matchesGlob("internal/a/deep/x.go", "internal/*/*.go"), false);
  assert.equal(matchesGlob("nested/Dockerfile.local", "Dockerfile*"), false);
  assert.equal(matchesGlob("Dockerfile.local", "Dockerfile*"), true);
});

test("manifest writer is canonical, atomic, and refuses stale reuse", (t) => {
  const root = mkdtempSync(join(tmpdir(), "ci-lane-selection-write-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const path = join(root, "selection.json");
  const document = selection(["internal/a/change.go"]).document;
  const written = writeSelection(path, document);
  assert.equal(written.body, `${JSON.stringify(document, null, 2)}\n`);
  assert.equal(readFileSync(path, "utf8"), written.body);
  assert.match(written.sha256, /^[0-9a-f]{64}$/);
  assert.throws(() => writeSelection(path, document), /refusing stale reuse/);
});

function git(root, args) {
  return execFileSync("git", args, { cwd: root, encoding: "utf8" }).trim();
}

function commit(root, message) {
  git(root, ["add", "--all"]);
  git(root, ["commit", "-m", message]);
  return git(root, ["rev-parse", "HEAD"]);
}

test("git-derived evidence sees additions, deletions, and both rename paths", (t) => {
  const root = mkdtempSync(join(tmpdir(), "ci-lane-selection-git-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  git(root, ["init", "--quiet"]);
  git(root, ["config", "user.name", "CI Test"]);
  git(root, ["config", "user.email", "ci@example.invalid"]);
  writeFileSync(join(root, "old.go"), "package old\n");
  writeFileSync(join(root, "delete.go"), "package deleted\n");
  const base = commit(root, "base");

  git(root, ["mv", "old.go", "new.go"]);
  rmSync(join(root, "delete.go"));
  writeFileSync(join(root, "added.go"), "package added\n");
  const head = commit(root, "change");
  const diff = deriveChangedPaths({ root, baseSHA: base, headSHA: head });
  assert.equal(diff.reason, "none");
  assert.deepEqual(diff.changedPaths, [
    "added.go",
    "delete.go",
    "new.go",
    "old.go",
  ]);

  assert.deepEqual(checkoutEvidence({ root, headSHA: head }), {
    sha: head,
    tree: git(root, ["rev-parse", "HEAD^{tree}"]),
  });
  writeFileSync(join(root, "dirty.txt"), "dirty\n");
  assert.throws(
    () => checkoutEvidence({ root, headSHA: head }),
    /checkout is not clean/,
  );
});

test("unavailable and empty git diffs become explicit full fallbacks", (t) => {
  const root = mkdtempSync(join(tmpdir(), "ci-lane-selection-fallback-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  git(root, ["init", "--quiet"]);
  git(root, ["config", "user.name", "CI Test"]);
  git(root, ["config", "user.email", "ci@example.invalid"]);
  writeFileSync(join(root, "one.txt"), "one\n");
  const head = commit(root, "one");
  assert.equal(
    deriveChangedPaths({ root, baseSHA: head, headSHA: head }).reason,
    "empty_diff",
  );
  assert.equal(
    deriveChangedPaths({ root, baseSHA: sha("f"), headSHA: head }).reason,
    "merge_base_unavailable",
  );
});

test("run identity is canonical and positive", () => {
  assert.equal(parseRunAttempt("1"), 1);
  for (const value of [undefined, "", "0", "01", "-1", "1.5", "word"]) {
    assert.throws(() => parseRunAttempt(value), ContractError);
  }
});
