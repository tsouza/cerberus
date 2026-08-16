// Emit one closed case-json-v1 completion record after a registered producer
// job reaches the end of its real work. The step is intentionally placed after
// that work and is not marked always(): failed, cancelled, or skipped work
// produces no artifact, which makes native-bundle assembly fail closed.
//
// Env:
//   CI_NATIVE_PART_ID       registry native_evidence.parts[].id
//   CI_NATIVE_PART_OUTPUT   output path; must not already exist
//   CI_NATIVE_SEED          exact deterministic seed used by a seeded lane;
//                           empty for deterministic lanes
//   CI_NATIVE_DURATION_MS   optional non-negative integer; default 0 because
//                           Actions job provenance owns wall-clock duration

import {
  existsSync,
  mkdirSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import { dirname, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const partIDPattern = /^[a-z0-9][a-z0-9.-]*$/;
const seedPattern = /^[^\u0000-\u001f\u007f]{1,1024}$/;

export function nativeCase({ id, seed = "", durationMS = 0 }) {
  const partID = String(id ?? "").trim();
  if (!partIDPattern.test(partID)) {
    throw new Error("CI_NATIVE_PART_ID must be a canonical native part id");
  }
  const seedValue = String(seed ?? "");
  if (seedValue !== "" && !seedPattern.test(seedValue)) {
    throw new Error("CI_NATIVE_SEED must be empty or a printable seed");
  }
  const duration =
    typeof durationMS === "number" ? durationMS : Number(durationMS || 0);
  if (!Number.isSafeInteger(duration) || duration < 0) {
    throw new Error("CI_NATIVE_DURATION_MS must be a non-negative safe integer");
  }
  return Object.freeze({
    schema_version: 1,
    seed: seedValue === "" ? null : seedValue,
    cases: [
      {
        id: `${partID}-completed`,
        status: "passed",
        duration_ms: duration,
      },
    ],
  });
}

export function writeNativeCase(path, document) {
  const output = resolve(String(path ?? ""));
  if (existsSync(output)) {
    throw new Error(`native part output already exists; refusing stale reuse: ${output}`);
  }
  mkdirSync(dirname(output), { recursive: true });
  const temporary = `${output}.tmp-${process.pid}`;
  writeFileSync(temporary, `${JSON.stringify(document, null, 2)}\n`, {
    flag: "wx",
  });
  renameSync(temporary, output);
  return output;
}

function main() {
  try {
    const document = nativeCase({
      id: process.env.CI_NATIVE_PART_ID,
      seed: process.env.CI_NATIVE_SEED,
      durationMS: process.env.CI_NATIVE_DURATION_MS,
    });
    const output = writeNativeCase(process.env.CI_NATIVE_PART_OUTPUT, document);
    process.stdout.write(`native_part=${output}\n`);
  } catch (error) {
    process.stderr.write(`::error::${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}

if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  main();
}
