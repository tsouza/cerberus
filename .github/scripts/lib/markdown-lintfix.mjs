// markdown-lintfix.mjs — runs the repository's own Markdown house-style
// fixers (`scripts/align-md-tables.py` then `.github/scripts/markdownlint-
// run.mjs --staged`, in that order — MD060 table alignment has no fixer
// inside markdownlint itself and must be resolved first) against a
// throwaway temp copy of generated Markdown, never the real target.
//
// WHY THIS EXISTS AS ITS OWN MODULE (CLAUDE.md invariant "DRY"). Every
// generator that emits committed Markdown from a non-Markdown source —
// semantic-report.mjs (issue #3435) and semantic-guide.mjs (issue #3462)
// alike — needs its raw output run through the SAME two fixers lefthook's
// own pre-commit hooks already run on staged Markdown, in the SAME order,
// rather than re-implementing markdownlint-cli2's rule set (MD032
// blank-line-around-list, MD049/MD050 emphasis-marker choice, MD060
// table-column alignment, ...) a second time inside each generator. A
// second generator that skipped this and hand-rolled its own table
// rendering would either duplicate this exact logic or drift from the
// house style the checked-in Markdown files are held to everywhere else.
//
// Runs against a temp directory so `--check` callers stay read-only and
// safe inside a CI job that must not touch the working tree.
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { spawnSync } from "node:child_process";

/**
 * Runs `markdown` through align-md-tables.py then markdownlint-run.mjs
 * --staged, both against a temp copy, and returns the fixed text.
 * `root` is the repository root the two fixer scripts are invoked from.
 */
export function lintFixMarkdown(markdown, root) {
  const dir = mkdtempSync(join(tmpdir(), "md-lintfix-"));
  const tmpPath = join(dir, "generated.md");
  try {
    writeFileSync(tmpPath, markdown);
    const align = spawnSync("python3", ["scripts/align-md-tables.py", tmpPath], {
      cwd: root,
      encoding: "utf8",
    });
    if (align.status !== 0) {
      throw new Error(`align-md-tables.py exited ${align.status}: ${align.stderr || align.error}`);
    }
    const fix = spawnSync(
      "node",
      [join(root, ".github/scripts/markdownlint-run.mjs"), "--staged", tmpPath],
      { cwd: root, encoding: "utf8" },
    );
    if (fix.status !== 0) {
      throw new Error(
        `markdownlint-run.mjs --staged left unfixed findings on the generated Markdown:\n${fix.stdout}${fix.stderr}`,
      );
    }
    return readFileSync(tmpPath, "utf8");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
