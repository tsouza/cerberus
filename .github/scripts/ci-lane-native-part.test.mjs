import assert from "node:assert/strict";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { parseCaseJSON } from "./ci-lane-native-evidence.mjs";
import {
  nativeCase,
  writeNativeCase,
} from "./ci-lane-native-part.mjs";

test("completion records are closed parseable native evidence", () => {
  const document = nativeCase({
    id: "ci-check.source",
    seed: "seed-42",
    durationMS: 7,
  });
  assert.deepEqual(document, {
    schema_version: 1,
    seed: "seed-42",
    cases: [
      {
        id: "ci-check.source-completed",
        status: "passed",
        duration_ms: 7,
      },
    ],
  });
  assert.deepEqual(parseCaseJSON(JSON.stringify(document)), {
    executed: 1,
    passed: 1,
    failed: 0,
    skipped: 0,
    duration_ms: 7,
    seed: "seed-42",
    corpus_id: parseCaseJSON(JSON.stringify(document)).corpus_id,
  });
});

test("invalid identity, seed, and duration fail closed", () => {
  assert.throws(() => nativeCase({ id: "" }), /canonical native part id/);
  assert.throws(
    () => nativeCase({ id: "part", seed: "bad\nseed" }),
    /printable seed/,
  );
  assert.throws(
    () => nativeCase({ id: "part", durationMS: -1 }),
    /non-negative safe integer/,
  );
});

test("writer is atomic and refuses stale reuse", (t) => {
  const root = mkdtempSync(join(tmpdir(), "ci-native-part-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const path = join(root, "nested", "cases.json");
  const document = nativeCase({ id: "part" });
  assert.equal(writeNativeCase(path, document), path);
  assert.equal(readFileSync(path, "utf8"), `${JSON.stringify(document, null, 2)}\n`);
  assert.throws(() => writeNativeCase(path, document), /stale reuse/);
});
