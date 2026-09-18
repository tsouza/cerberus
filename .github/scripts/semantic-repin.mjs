#!/usr/bin/env node
// semantic-repin.mjs — regenerates the region fingerprints the semantic
// evidence family pins its hand-authored records with
// (lib/semantic-fingerprint.mjs). The ONE sanctioned way those values ever
// change: they are generated artefacts (CLAUDE.md invariant 9), never
// hand-edited.
//
// Usage:
//   node .github/scripts/semantic-repin.mjs mutant <mutant-id>
//       Rewrites test/semantic/mutants/<mutant-id>.json's
//       transformation.pre_image_fingerprint / post_image_fingerprint from
//       the record's own patch file. `just semantic-mutant-repin <id>`
//       wraps this and then re-runs `just semantic-mutate <id>`, so a
//       re-pinned record is immediately re-verified against its declared
//       expected_detection.
//   node .github/scripts/semantic-repin.mjs replay
//       Rewrites test/semantic/replay-fingerprints.json for every
//       counterexample contract entry (lib/semantic-replay.mjs's
//       writeFingerprints). `just semantic-replay-repin` wraps this.
//
// Env: SEMANTIC_MUTANTS_DIR (optional; default test/semantic/mutants).
// Exit: 0 on a rewrite (or when nothing moved); 1 on an unknown subcommand
// or mutant id, an unparseable patch, or a record whose fingerprint fields
// are not exactly where the schema puts them.

import { readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { error, notice } from "./lib/gh.mjs";
import { patchFingerprints } from "./lib/semantic-fingerprint.mjs";
import { DEFAULT_MUTANTS_DIR, loadMutants } from "./lib/semantic-mutation.mjs";
import { DEFAULT_FINGERPRINTS_PATH, writeFingerprints } from "./lib/semantic-replay.mjs";

const FINGERPRINT_FIELDS = ["pre_image_fingerprint", "post_image_fingerprint"];

// repinMutantRecord rewrites the two fingerprint values in place — a
// targeted substitution of each `"<field>": "<hex>"` pair, never a
// re-serialisation of the whole record, so the committed file's own
// formatting (and every other field) is left byte-identical. Returns the
// before/after values and whether anything moved.
export function repinMutantRecord(id, { root = process.cwd(), dir = DEFAULT_MUTANTS_DIR } = {}) {
  const records = loadMutants(dir, { root });
  const record = records.get(id);
  if (!record) {
    throw new Error(`no mutant record named ${id} under ${dir} (known: ${[...records.keys()].join(", ")})`);
  }
  const recordPath = join(resolve(root, dir), `${id}.json`);
  const patchText = readFileSync(resolve(root, record.transformation.patch_path), "utf8");
  const fresh = patchFingerprints(patchText);

  let text = readFileSync(recordPath, "utf8");
  const before = {};
  for (const field of FINGERPRINT_FIELDS) {
    const re = new RegExp(`("${field}":\\s*")([0-9a-f]{64})(")`, "g");
    const matches = [...text.matchAll(re)];
    if (matches.length !== 1) {
      throw new Error(`${recordPath}: expected exactly one "${field}" value, found ${matches.length}`);
    }
    before[field] = matches[0][2];
    text = text.replace(re, `$1${fresh[field]}$3`);
  }
  const changed = FINGERPRINT_FIELDS.some((field) => before[field] !== fresh[field]);
  if (changed) writeFileSync(recordPath, text);
  return { id, path: recordPath, before, after: fresh, changed };
}

function main() {
  const [subcommand, ...rest] = process.argv.slice(2);
  const root = process.cwd();
  if (subcommand === "mutant") {
    const [id] = rest;
    if (!id) throw new Error("usage: semantic-repin.mjs mutant <mutant-id>");
    const dir = process.env.SEMANTIC_MUTANTS_DIR || DEFAULT_MUTANTS_DIR;
    const result = repinMutantRecord(id, { root, dir });
    if (result.changed) {
      notice(
        `semantic-repin: ${id} re-pinned — pre_image ${result.before.pre_image_fingerprint} -> ` +
          `${result.after.pre_image_fingerprint}, post_image ${result.before.post_image_fingerprint} -> ` +
          `${result.after.post_image_fingerprint}`,
      );
    } else {
      notice(`semantic-repin: ${id} already pins its patch's current pre/post images — nothing to rewrite`);
    }
    return;
  }
  if (subcommand === "replay") {
    const snapshot = writeFingerprints(undefined, { root });
    notice(
      `semantic-repin: wrote ${DEFAULT_FINGERPRINTS_PATH} (${Object.keys(snapshot.fingerprints).length} contract-entry fingerprint(s))`,
    );
    return;
  }
  throw new Error(`usage: semantic-repin.mjs mutant <mutant-id> | replay (got ${JSON.stringify(subcommand)})`);
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (cause) {
    error(`semantic-repin: ${cause instanceof Error ? cause.message : String(cause)}`);
    process.exit(1);
  }
}
