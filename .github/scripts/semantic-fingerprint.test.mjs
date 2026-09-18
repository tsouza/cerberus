// semantic-fingerprint.test.mjs — node --test guard for
// lib/semantic-fingerprint.mjs, the region-scoped fingerprint scheme the
// mutant records (lib/semantic-mutation.mjs) and the counterexample replay
// snapshot (lib/semantic-replay.mjs) both pin with. The class each test
// pins: a fingerprint moves when, and only when, the region a record's
// claim depends on moves.

import assert from "node:assert/strict";
import { test } from "node:test";

import {
  REGION_DELIMITER,
  goFuncRegion,
  locateRegions,
  parsePatchHunks,
  patchFingerprints,
  regionFingerprint,
} from "./lib/semantic-fingerprint.mjs";
import { sha256Hex } from "./lib/semantic-model.mjs";

const ONE_HUNK = `diff --git a/x.go b/x.go
index 0000000..1111111 100644
--- a/x.go
+++ b/x.go
@@ -10,7 +10,7 @@ func f() {
 	a := 1
 	b := 2
 	if a > b {
-		return a
+		return b
 	}
 	return 0
 }
`;

const TWO_HUNKS = `--- a/x.go
+++ b/x.go
@@ -1,3 +1,3 @@
 package x
-// old
+// new

@@ -20,4 +20,5 @@ func g() {
 	x := 1
+	x++
 	return x
 }
\\ No newline at end of file
`;

test("parsePatchHunks: a hunk's pre-image is its context + removed lines, its post-image context + added lines", () => {
  const hunks = parsePatchHunks(ONE_HUNK);
  assert.equal(hunks.length, 1);
  assert.equal(hunks[0].preImage, "\ta := 1\n\tb := 2\n\tif a > b {\n\t\treturn a\n\t}\n\treturn 0\n}");
  assert.equal(hunks[0].postImage, "\ta := 1\n\tb := 2\n\tif a > b {\n\t\treturn b\n\t}\n\treturn 0\n}");
});

test("parsePatchHunks: several hunks stay separate, a bare blank line is empty context, and the no-newline marker is skipped", () => {
  const hunks = parsePatchHunks(TWO_HUNKS);
  assert.equal(hunks.length, 2);
  assert.equal(hunks[0].preImage, "package x\n// old\n");
  assert.equal(hunks[0].postImage, "package x\n// new\n");
  assert.equal(hunks[1].preImage, "\tx := 1\n\treturn x\n}");
  assert.equal(hunks[1].postImage, "\tx := 1\n\tx++\n\treturn x\n}");
});

test("parsePatchHunks: a patch with no hunk, or an unmarked line inside one, is an error — never an empty fingerprint", () => {
  assert.throws(() => parsePatchHunks("--- a/x\n+++ b/x\n"), /no @@ hunk/);
  assert.throws(() => parsePatchHunks("@@ -1 +1 @@\n?bogus\n"), /unrecognised line/);
});

test("patchFingerprints: the pre-image pin moves only with the pre-image, the post-image pin only with the post-image", () => {
  const base = patchFingerprints(ONE_HUNK);
  const addedLineChanged = patchFingerprints(ONE_HUNK.replace("+\t\treturn b", "+\t\treturn a + b"));
  assert.equal(addedLineChanged.pre_image_fingerprint, base.pre_image_fingerprint);
  assert.notEqual(addedLineChanged.post_image_fingerprint, base.post_image_fingerprint);
  const removedLineChanged = patchFingerprints(ONE_HUNK.replace("-\t\treturn a", "-\t\treturn a * 2"));
  assert.notEqual(removedLineChanged.pre_image_fingerprint, base.pre_image_fingerprint);
  assert.equal(removedLineChanged.post_image_fingerprint, base.post_image_fingerprint);
  const contextChanged = patchFingerprints(ONE_HUNK.replace(" \tb := 2", " \tb := 3"));
  assert.notEqual(contextChanged.pre_image_fingerprint, base.pre_image_fingerprint);
  assert.notEqual(contextChanged.post_image_fingerprint, base.post_image_fingerprint);
});

test("patchFingerprints: the @@ line numbers and file headers are NOT part of the pin — a hunk moving to another offset is not drift", () => {
  const base = patchFingerprints(ONE_HUNK);
  const moved = patchFingerprints(ONE_HUNK.replace("@@ -10,7 +10,7 @@", "@@ -42,7 +42,7 @@").replace("index 0000000..1111111", "index 2222222..3333333"));
  assert.deepEqual(moved, base);
});

test("regionFingerprint: one region is its plain digest; several are delimiter-joined, so region boundaries are part of the hash", () => {
  assert.equal(regionFingerprint(["abc"]), sha256Hex(Buffer.from("abc")));
  assert.equal(regionFingerprint(["ab", "c"]), sha256Hex(Buffer.from(`ab${REGION_DELIMITER}c`)));
  assert.notEqual(regionFingerprint(["ab", "c"]), regionFingerprint(["a", "bc"]));
  assert.throws(() => regionFingerprint([]), /no regions/);
});

test("locateRegions: reports the fingerprint when every image occurs in the text, else the 0-based indices of the missing ones", () => {
  const text = "line1\nREGION-A\nline3\nREGION-B\n";
  assert.deepEqual(locateRegions(text, ["REGION-A", "REGION-B"]), {
    fingerprint: regionFingerprint(["REGION-A", "REGION-B"]),
    missing: [],
  });
  assert.deepEqual(locateRegions(text, ["REGION-A", "REGION-C", "REGION-D"]), { fingerprint: null, missing: [1, 2] });
});

const GO_SOURCE = `package p

import "testing"

// TestA's doc comment.
func TestA(t *testing.T) {
	q := "sum by (x) ({job=\\"a\\"})" // braces inside a string literal
	if q == "" {
		t.Fatal("empty")
	}
}

// TestB's doc comment.
func TestB(t *testing.T) {
	t.Log("b")
}
`;

test("goFuncRegion: spans from the func line to the next top-level func, so a brace inside a string literal cannot truncate it", () => {
  const a = goFuncRegion(GO_SOURCE, "TestA");
  assert.ok(a.startsWith("func TestA(t *testing.T) {"));
  assert.ok(a.includes('t.Fatal("empty")'));
  assert.ok(a.endsWith("// TestB's doc comment."), "over-inclusive up to the next declaration, never under-inclusive");
  assert.ok(!a.includes("func TestB"));
  const b = goFuncRegion(GO_SOURCE, "TestB");
  assert.equal(b, "func TestB(t *testing.T) {\n\tt.Log(\"b\")\n}\n");
});

test("goFuncRegion: an edit in a sibling function does not move a function's region; an edit inside it does; an unknown name is null", () => {
  const before = goFuncRegion(GO_SOURCE, "TestB");
  assert.equal(goFuncRegion(GO_SOURCE.replace('t.Fatal("empty")', 't.Fatal("blank")'), "TestB"), before);
  assert.notEqual(goFuncRegion(GO_SOURCE.replace('t.Log("b")', 't.Log("B")'), "TestB"), before);
  assert.equal(goFuncRegion(GO_SOURCE, "TestC"), null);
  assert.equal(goFuncRegion(GO_SOURCE, "TestAB"), null, "a prefix match is not a declaration");
});
