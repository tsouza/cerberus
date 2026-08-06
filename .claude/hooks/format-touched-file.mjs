#!/usr/bin/env node
// PostToolUse hook for Edit / Write / MultiEdit.
//
// Formats the SINGLE file the tool just touched, and nothing else. Two
// commands, matching what the CI `lint` gate enforces:
//
//   gofumpt -extra -w <file>
//   goimports -w -local github.com/tsouza/cerberus <file>
//
// `-extra` is not decorative. `.golangci.yml` sets `gofumpt.extra-rules: true`,
// so the required `lint` gate applies the extra rule set; a plain `gofumpt -w`
// leaves formatting that CI rejects, which is the worst kind of local
// formatter — one that reports success over a file that will fail.
//
// `-local` mirrors `.golangci.yml`'s `goimports.local-prefixes`, so cerberus's
// own imports land in their own block rather than being shuffled into the
// third-party group on every save.
//
// Scope is deliberately one file: no `./...`, no repo-wide linter. A hook that
// runs on every edit has to stay in the tens of milliseconds, and repo-wide
// validation already belongs to lefthook's pre-push stage and to CI.
//
// Non-Go files are a silent no-op. Markdown is handled by lefthook's
// `pre-commit` stage, which pairs `markdownlint-cli2 --fix` with the MD060
// table-padding pass those two need to run together.
//
// Failure posture: this hook never blocks. A missing formatter binary or a file
// that does not parse is reported on stderr and exits 0, because a syntax error
// mid-edit is an expected transient state, not a reason to refuse the edit that
// is about to fix it.
//
// Input: the PostToolUse JSON payload on stdin (`tool_name`, `tool_input`).
// Exit codes: always 0.

import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';

const LOCAL_IMPORT_PREFIX = 'github.com/tsouza/cerberus';
const FORMATTED_EXTENSION = '.go';

function readPayload() {
  let raw = '';
  try {
    raw = readFileSync(0, 'utf8');
  } catch {
    return null;
  }
  try {
    return JSON.parse(raw);
  } catch {
    return null;
  }
}

// run — execute a formatter over one file. Returns true when it ran cleanly.
// A missing binary (ENOENT) and a non-zero exit are reported the same way and
// neither is fatal; see the failure posture note above.
function run(bin, args) {
  try {
    execFileSync(bin, args, { stdio: ['ignore', 'ignore', 'pipe'] });
    return true;
  } catch (e) {
    const detail = e.code === 'ENOENT' ? `${bin} not on PATH` : String(e.stderr ?? e.message).trim();
    process.stderr.write(`format-touched-file: ${bin} skipped — ${detail}\n`);
    return false;
  }
}

function main() {
  const payload = readPayload();
  if (!payload) return 0;

  const file = payload?.tool_input?.file_path;
  if (typeof file !== 'string' || !file.endsWith(FORMATTED_EXTENSION)) return 0;
  if (!existsSync(file)) return 0;

  run('gofumpt', ['-extra', '-w', file]);
  run('goimports', ['-w', '-local', LOCAL_IMPORT_PREFIX, file]);
  return 0;
}

process.exit(main());
