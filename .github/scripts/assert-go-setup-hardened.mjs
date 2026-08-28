// assert-go-setup-hardened — every job that sets up Go must do it through
// `./.github/actions/setup-go`, never through `actions/setup-go` directly.
//
// WHY THIS EXISTS. `actions/setup-go` saves its module cache keyed on go.sum,
// only on a primary-key MISS, and without ever checking that there is anything
// to save. A job that sets it up with `cache: true` and runs no Go command
// therefore archives an EMPTY GOMODCACHE, and if it wins that race on
// `refs/heads/main` after a go.sum bump, every later job — on main and on every
// PR ref falling back to main's cache scope — restores nothing and then
// declines to save, because from then on the key hits. Run 32705099037 did
// exactly that: `Cache folder path is retrieved but doesn't exist on disk`
// followed by `Cache saved with the key: setup-go-…-29abba13…`, a 7,616-byte
// archive that made 51 call sites run cold until the next go.sum change.
//
// The composite action fixes that by construction — it always warms GOMODCACHE
// before the post-job save can look at it. This gate is what keeps the fix from
// decaying: the cheapest way to add a new Go job is to copy an
// `actions/setup-go@v7` block out of some other repo, and that one step
// silently re-arms the whole failure. "Does this workflow set Go up through the
// hardened path" is a question about the shape of a workflow, so a gate can
// answer it, and a question a gate can answer should not have to be
// rediscovered from a poisoned cache.
//
// WHAT IT ASSERTS.
//
//   R1  No workflow step, and no composite action other than the wrapper
//       itself, `uses:` `actions/setup-go` directly.
//   R2  The wrapper exists and does delegate to `actions/setup-go` — otherwise
//       R1 is satisfiable by an action that sets up nothing.
//   R3  The wrapper's warm step runs `go-module-fetch.mjs` and carries no
//       `if:`. An `if:` on that step is precisely the hole this closes: a
//       conditional warm is a job that can still reach the post-save with an
//       empty GOMODCACHE.
//   R4  At least one workflow step actually uses the wrapper. A gate that
//       passes because nothing sets Go up at all reports the same green as a
//       gate that is satisfied.
//
// THERE IS NO ALLOW-LIST. The single file permitted to name `actions/setup-go`
// is permitted by IDENTITY, not by name: it is the wrapper's own action.yml —
// the definition site of the thing the rule is about, which cannot be a call
// site of itself. That is the same structural distinction
// `assert-image-jobs-authenticate.mjs` draws between declaring
// `pullImageWithRetry` and calling it. Anything else that ever has to be
// excluded must be excluded by a fact of that kind and not by a list of names.
//
// The gate deliberately says NOTHING about the `cache:` input. `false` is a
// legitimate, load-bearing choice — update-golden.yml's three jobs run
// target-branch code and must not persist bytes into later workflows — and the
// warm step runs either way, so the poisoning this prevents is unrelated to the
// value. Forcing `cache: true` would break that isolation for no gain.
//
// ENV CONTRACT
//   REPO_ROOT     — repository root. Default `process.cwd()`.
//   WORKFLOW_DIR  — workflows to scan. Default `.github/workflows`.
//   ACTIONS_DIR   — composite actions to scan. Default `.github/actions`.
//
// Exit: 0 when every rule holds, 1 on any violation.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { isAbsolute, join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice } from './lib/gh.mjs';

// The Action this repo must never name directly, and the local wrapper that is
// the one sanctioned way to reach it.
const upstreamSetupGo = 'actions/setup-go';
const wrapperDir = '.github/actions/setup-go';
const wrapperRef = './.github/actions/setup-go';

// The module the warm step has to run. Named rather than described, so a rename
// fails this gate instead of quietly turning R3 vacuous.
const warmScript = 'go-module-fetch.mjs';

// ---------------------------------------------------------------------------
// The narrow slice of YAML these files use. Only `node:` builtins are available
// (see the repo rule on CI scripts), so structure is read by indentation.
// ---------------------------------------------------------------------------

const indentOf = (line) => line.length - line.trimStart().length;
const isComment = (line) => line.trimStart().startsWith('#');
const isBlank = (line) => line.trim() === '' || isComment(line);

function unquote(value) {
  const t = value.trim();
  if ((t.startsWith('"') && t.endsWith('"')) || (t.startsWith("'") && t.endsWith("'"))) {
    return t.slice(1, -1);
  }
  return t;
}

// usesValue — the Action a line references, or null when the line is not a
// `uses:` key. Comment lines are excluded before matching, which is what keeps
// compatibility.yml's header — a prose inventory of the Actions it runs — from
// reading as a call site.
export function usesValue(line) {
  if (isComment(line)) return null;
  const m = /^\s*(?:-\s+)?uses:\s*(\S.*?)\s*$/.exec(line);
  if (m === null) return null;
  // A trailing `  # v2.86.5` version comment is common on pinned SHAs.
  return unquote(m[1].replace(/\s+#.*$/, ''));
}

// directSetupGoUses — every line in `text` that references the upstream Action.
// Matched on the reference itself rather than anywhere in the line, so a
// workflow that MENTIONS the name in a step title stays clean.
export function directSetupGoUses(text) {
  const out = [];
  text.split('\n').forEach((line, i) => {
    const ref = usesValue(line);
    if (ref === null) return;
    if (ref === upstreamSetupGo || ref.startsWith(`${upstreamSetupGo}@`)) {
      out.push({ line: i + 1, ref });
    }
  });
  return out;
}

export function wrapperUses(text) {
  return text.split('\n').filter((line) => usesValue(line) === wrapperRef).length;
}

// compositeSteps — the `runs.steps:` items of a composite action, each as a
// line array with the leading `- ` blanked so its keys form a plain mapping.
export function compositeSteps(text) {
  const lines = text.split('\n');
  let stepsAt = -1;
  for (let i = 0; i < lines.length; i++) {
    if (!isBlank(lines[i]) && /^\s*steps:\s*$/.test(lines[i])) {
      stepsAt = i;
      break;
    }
  }
  if (stepsAt === -1) return [];

  const base = indentOf(lines[stepsAt]);
  const block = [];
  for (let i = stepsAt + 1; i < lines.length; i++) {
    if (isBlank(lines[i])) {
      block.push(lines[i]);
      continue;
    }
    if (indentOf(lines[i]) <= base) break;
    block.push(lines[i]);
  }

  const itemIndent = Math.min(...block.filter((l) => !isBlank(l)).map(indentOf));
  const steps = [];
  let current = null;
  for (const line of block) {
    if (!isBlank(line) && indentOf(line) === itemIndent && /^\s*-\s/.test(line)) {
      if (current) steps.push(current);
      current = [line.replace(/^(\s*)-(\s)/, '$1 $2')];
      continue;
    }
    if (current) current.push(line);
  }
  if (current) steps.push(current);
  return steps;
}

// hasKeyAtStepLevel — does this step declare `key:` as one of its own keys?
// Used for `if:`, which must be absent from the warm step.
function hasKeyAtStepLevel(stepLines, key) {
  const keyIndent = Math.min(...stepLines.filter((l) => !isBlank(l)).map(indentOf));
  return stepLines.some((l) => !isBlank(l) && indentOf(l) === keyIndent && new RegExp(`^\\s*${key}:(\\s|$)`).test(l));
}

// ---------------------------------------------------------------------------
// The scan.
// ---------------------------------------------------------------------------

function yamlFilesIn(dir) {
  if (!existsSync(dir)) return [];
  return readdirSync(dir)
    .sort()
    .filter((f) => /\.ya?ml$/.test(f))
    .map((f) => join(dir, f));
}

function actionFilesIn(dir) {
  if (!existsSync(dir)) return [];
  const out = [];
  for (const entry of readdirSync(dir).sort()) {
    const full = join(dir, entry);
    if (!statSync(full).isDirectory()) continue;
    for (const name of ['action.yml', 'action.yaml']) {
      if (existsSync(join(full, name))) out.push(join(full, name));
    }
  }
  return out;
}

export function scan({
  root = process.cwd(),
  workflowDir = '.github/workflows',
  actionsDir = '.github/actions',
  wrapperPath = wrapperDir,
} = {}) {
  const abs = (p) => (isAbsolute(p) ? p : join(root, p));
  const violations = [];
  let wrapperCallSites = 0;
  let scanned = 0;

  const wrapperFile = ['action.yml', 'action.yaml']
    .map((n) => join(abs(wrapperPath), n))
    .find((p) => existsSync(p));

  // R1 — nothing but the wrapper names the upstream Action.
  for (const file of [...yamlFilesIn(abs(workflowDir)), ...actionFilesIn(abs(actionsDir))]) {
    scanned++;
    const text = readFileSync(file, 'utf8');
    wrapperCallSites += wrapperUses(text);
    // The definition site cannot be a call site of itself.
    if (wrapperFile !== undefined && file === wrapperFile) continue;
    for (const hit of directSetupGoUses(text)) {
      violations.push(
        `${file}:${hit.line}: uses \`${hit.ref}\` directly. Go setup must go through ` +
          `\`${wrapperRef}\`, which warms GOMODCACHE before setup-go's post-job step can archive an ` +
          'empty one — the failure that made every Go job in the repo run cold until the next go.sum ' +
          'bump. Pass `cache: false` to the wrapper if this job must stay off the shared cache.',
      );
    }
  }

  // R2 / R3 — the wrapper is the real thing, and its warm step is unconditional.
  if (wrapperFile === undefined) {
    violations.push(
      `${wrapperPath}/action.yml does not exist — every call site would resolve to nothing, so R1 ` +
        'would be satisfied by a setup path that sets nothing up.',
    );
  } else {
    const text = readFileSync(wrapperFile, 'utf8');
    if (directSetupGoUses(text).length === 0) {
      violations.push(
        `${wrapperFile}: does not delegate to \`${upstreamSetupGo}\` — the wrapper installs no Go ` +
          'toolchain, so every caller would be silently Go-less.',
      );
    }
    const warmSteps = compositeSteps(text).filter((step) => step.some((l) => !isComment(l) && l.includes(warmScript)));
    if (warmSteps.length === 0) {
      violations.push(
        `${wrapperFile}: runs no \`${warmScript}\` step. Without it GOMODCACHE can still be empty when ` +
          'setup-go archives it, which is the whole failure this path exists to make impossible.',
      );
    }
    for (const step of warmSteps) {
      if (hasKeyAtStepLevel(step, 'if')) {
        violations.push(
          `${wrapperFile}: the \`${warmScript}\` step carries an \`if:\`. A conditional warm leaves a ` +
            'path on which a job reaches the post-job cache save with an empty GOMODCACHE — exactly ' +
            'the hole the composite exists to close.',
        );
      }
    }
  }

  // R4 — non-vacuity.
  if (wrapperCallSites === 0) {
    violations.push(
      `no workflow or action uses \`${wrapperRef}\` at all — this gate is passing because nothing sets ` +
        'Go up, which is the same green a satisfied gate reports.',
    );
  }

  return { violations, wrapperCallSites, scanned };
}

function main() {
  const root = process.env.REPO_ROOT || process.cwd();
  const workflowDir = process.env.WORKFLOW_DIR || '.github/workflows';
  const actionsDir = process.env.ACTIONS_DIR || '.github/actions';
  const { violations, wrapperCallSites, scanned } = scan({ root, workflowDir, actionsDir });

  log(`scanned ${scanned} workflow/action files; ${wrapperCallSites} steps set Go up through ${wrapperRef}`);
  for (const v of violations) error(v);
  if (violations.length > 0) process.exit(1);
  notice(`${wrapperCallSites} Go setup call sites, all on the warming ${wrapperRef} path`);
  process.exit(0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
