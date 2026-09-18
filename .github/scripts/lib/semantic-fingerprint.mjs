// semantic-fingerprint.mjs — the one region-scoped fingerprint scheme the
// semantic evidence family pins its hand-authored records with.
//
// A fingerprint is a SHA-256 over the REGION a record's claim actually
// depends on, never over the whole file that region lives in:
//
//   - a mutant record (lib/semantic-mutation.mjs) pins its patch's
//     PRE-IMAGE — every hunk's context and removed lines, exactly as they
//     must appear in the target — and the POST-IMAGE the same hunks leave
//     behind (context and added lines);
//   - a counterexample replay entry (lib/semantic-replay.mjs) pins the
//     region each resolved mechanism executes — a test function's own
//     source, a fixture file, a compat corpus file.
//
// Whole-file hashing was the previous scheme, and it made every unrelated
// edit to `internal/chsql/range_window.go` or a lowerer red the required
// `check` job (the corpus step re-runs every mutant on every PR) even
// though the mutated lines had not moved. `git apply` already refuses a
// hunk whose context no longer matches, so a whole-file pre-hash bought
// nothing over pinning the region itself. Re-pin with `just
// semantic-mutant-repin <id>` / `just semantic-replay-repin`
// (.github/scripts/semantic-repin.mjs) — never by hand (CLAUDE.md
// invariant 9).
//
// Node builtins only.

import { sha256Hex } from "./semantic-model.mjs";

// Separates the regions one fingerprint hashes. A plain ASCII marker, not a
// control byte: a NUL in the leading block of a committed .mjs source file
// trips repo-hygiene's binary-content classifier. Not a sequence any
// hashed region could plausibly contain verbatim at the seam, so it cannot
// be defeated by an edit to one region alone.
export const REGION_DELIMITER = "\n<<semantic-region-delimiter>>\n";

/**
 * SHA-256 hex over `regions` (strings or Buffers) in order, joined by
 * REGION_DELIMITER. One region hashes with no delimiter at all, so a
 * single-region fingerprint is the plain digest of that region.
 */
export function regionFingerprint(regions) {
  if (regions.length === 0) throw new Error("regionFingerprint: no regions to hash");
  const parts = [];
  regions.forEach((region, i) => {
    if (i > 0) parts.push(Buffer.from(REGION_DELIMITER, "utf8"));
    parts.push(Buffer.isBuffer(region) ? region : Buffer.from(region, "utf8"));
  });
  return sha256Hex(Buffer.concat(parts));
}

// --- unified-diff pre/post images ------------------------------------------

const HUNK_HEADER_RE = /^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/;
const NO_NEWLINE_MARKER = "\\ No newline at end of file";

/**
 * Parses a unified diff into its hunks' pre- and post-images. Each hunk's
 * `preImage` is its context (` `) and removed (`-`) lines in order — the
 * exact text the target must contain for the hunk to apply; `postImage`
 * is its context and added (`+`) lines — the text the target contains at
 * that place after the patch. Both are newline-joined line bodies without
 * the leading marker character. File headers (`diff --git`, `---`, `+++`,
 * `index`) and `\ No newline at end of file` markers are skipped. Throws on
 * a patch with no hunk at all, or on a line inside a hunk that carries no
 * recognised marker — a malformed patch must never fingerprint as if it
 * were an empty one.
 */
export function parsePatchHunks(patchText) {
  const hunks = [];
  let current = null;
  const lines = patchText.split("\n");
  if (lines.at(-1) === "") lines.pop(); // the file's own trailing newline, not a hunk line
  for (const line of lines) {
    if (HUNK_HEADER_RE.test(line)) {
      current = { preImage: [], postImage: [] };
      hunks.push(current);
      continue;
    }
    if (current === null) continue; // file header, before the first hunk
    if (line === NO_NEWLINE_MARKER) continue;
    if (line.startsWith("diff ") || line.startsWith("--- ") || line.startsWith("+++ ") || line.startsWith("index ")) {
      current = null; // a second file section's header: hunks resume at its next @@
      continue;
    }
    // A blank source line is rendered as a lone " " context marker; a
    // whitespace-stripped patch renders it as a fully empty line instead,
    // which `git apply` still reads as empty context — so do we.
    const marker = line === "" ? " " : line[0];
    const body = line.slice(1);
    if (marker === " ") {
      current.preImage.push(body);
      current.postImage.push(body);
    } else if (marker === "-") {
      current.preImage.push(body);
    } else if (marker === "+") {
      current.postImage.push(body);
    } else {
      throw new Error(`parsePatchHunks: unrecognised line inside a hunk: ${JSON.stringify(line)}`);
    }
  }
  if (hunks.length === 0) throw new Error("parsePatchHunks: the patch carries no @@ hunk");
  return hunks.map((h) => ({ preImage: h.preImage.join("\n"), postImage: h.postImage.join("\n") }));
}

/**
 * The two fingerprints a mutant record pins for its patch: the pre-image
 * (every hunk's context + removed lines, in hunk order) and the post-image
 * (context + added lines). Pure functions of the patch text.
 */
export function patchFingerprints(patchText) {
  const hunks = parsePatchHunks(patchText);
  return {
    pre_image_fingerprint: regionFingerprint(hunks.map((h) => h.preImage)),
    post_image_fingerprint: regionFingerprint(hunks.map((h) => h.postImage)),
  };
}

/**
 * Finds every hunk image inside `text` and returns the fingerprint of the
 * regions as they occur there, or `{ fingerprint: null, missing: [i, …] }`
 * naming the hunk indices whose image does not occur at all. A found region
 * is byte-identical to the image searched for, so a non-null result always
 * equals `regionFingerprint(images)` — the value of this function is the
 * MISSING list, which says which hunk a target has drifted under (and, with
 * `git apply`'s own offset tolerance, that an unrelated edit elsewhere in
 * the file is not drift).
 */
export function locateRegions(text, images) {
  const missing = [];
  images.forEach((image, i) => {
    if (!text.includes(image)) missing.push(i);
  });
  return { fingerprint: missing.length === 0 ? regionFingerprint(images) : null, missing };
}

// --- Go test-function regions ------------------------------------------------

const TOP_LEVEL_FUNC_RE = /^func /;

/**
 * The source region of one top-level Go function: from its `func <name>(`
 * declaration line (column 0) up to, not including, the next top-level
 * `func ` declaration or end of file. Deliberately over-inclusive rather
 * than under-inclusive — the region also carries whatever trails the
 * function (a following function's doc comment, say), so an edit there
 * re-pins rather than a real change inside the function ever going
 * unnoticed. gofmt puts every top-level declaration at column 0, which is
 * what makes the boundary structural rather than a brace count that a `{`
 * inside a query string literal would defeat. Returns null when no such
 * function is declared.
 */
export function goFuncRegion(source, funcName) {
  const lines = source.split("\n");
  const declRe = new RegExp(`^func ${funcName.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\(`);
  const start = lines.findIndex((line) => declRe.test(line));
  if (start === -1) return null;
  let end = lines.length;
  for (let i = start + 1; i < lines.length; i += 1) {
    if (TOP_LEVEL_FUNC_RE.test(lines[i])) {
      end = i;
      break;
    }
  }
  return lines.slice(start, end).join("\n");
}
