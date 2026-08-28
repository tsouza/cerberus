// assert-go-setup-hardened — no job may reach the point where setup-go's
// post-job step archives the module cache with an EMPTY GOMODCACHE.
//
// WHY THIS EXISTS. `actions/setup-go` saves its module cache keyed on go.sum,
// only on a primary-key MISS, and without ever checking that there is anything
// to save. A job that sets it up with `cache: true` and runs no Go command
// therefore archives an empty GOMODCACHE, and if it wins that race on
// `refs/heads/main` after a go.sum bump, every later job — on main and on every
// PR ref falling back to main's cache scope — restores nothing and then
// declines to save, because from then on the key hits. Run 32705099037 did
// exactly that: `Cache folder path is retrieved but doesn't exist on disk`
// followed by `Cache saved with the key: setup-go-…-29abba13…`, a 7,616-byte
// archive that made 51 call sites run cold until the next go.sum change.
//
// `./.github/actions/setup-go` fixes it by construction — it always warms
// GOMODCACHE before the post-job save can look at it. This gate is what keeps
// the fix from decaying: the cheapest way to add a new Go job is to copy an
// `actions/setup-go@v7` block out of some other repo, and that one step
// silently re-arms the whole failure. "Does this workflow set Go up through the
// hardened path" is a question about the shape of a workflow, so a gate can
// answer it, and a question a gate can answer should not have to be
// rediscovered from a poisoned cache.
//
// WHAT IT ASSERTS.
//
//   R1  A workflow step reaching `actions/setup-go` directly must set
//       `cache: false` as a LITERAL in its own `with:`. The rule is keyed on
//       the MECHANISM, not on the Action's name: a step that never saves the
//       shared cache cannot poison it, so it is outside this rule's subject
//       rather than exempted from it. Everything that CAN write that cache
//       goes through the composite, where the warm step is unconditional.
//   R2  A job holding such a step must also run `go-module-fetch.mjs`. Opting
//       out of the shared cache is not opting out of fetching modules, and
//       that fetch is the one the Go resolver will not retry.
//   R3  No composite action other than the wrapper uses `actions/setup-go` at
//       all. R1's escape has no meaning there: a composite has no job scope in
//       which R2 could be satisfied, and it is reached from arbitrary callers.
//   R4  The wrapper exists, delegates to `actions/setup-go`, runs
//       `go-module-fetch.mjs`, and that warm step carries no `if:`. A
//       conditional warm leaves a path on which a job reaches the post-job save
//       with an empty GOMODCACHE, which is the hole the composite closes.
//   R5  At least one workflow step actually uses the wrapper. A gate that
//       passes because nothing sets Go up at all reports the same green as a
//       gate that is satisfied.
//   R6  A job that BUILDS a nested module must also WARM it, via the wrapper's
//       `also-warm` input or its own warm step in that directory. A nested
//       go.mod is a separate module graph: the root warm step does not fetch
//       it and the root `go test ./...` stops at it, so the job reaches the
//       proxy for it cold and unretried — the one fetch this action exists to
//       wrap, left uncovered. Keyed on `working-directory:` naming a directory
//       that actually contains a go.mod, so a Node directory or a scratch
//       checkout is outside the rule rather than exempted from it.
//
// WHY R1 IS A LITERAL AND NOT AN INPUT. `update-golden.yml`'s three
// regenerating jobs check out TARGET-BRANCH code under the default branch's
// privileges, and `cache: false` is what keeps that code from persisting bytes
// into later workflows. Routed through the composite the value reaches the
// Action as `${{ inputs.cache }}`, which no static analysis can resolve — and
// CodeQL's `actions/cache-poisoning/poisonable-step` query then reports this
// repository's most privileged workflow as poisonable in all three jobs. A
// trust boundary has to be legible to a reader and to a scanner, so those three
// spell the value out and pair it with their own warm step, which is exactly
// what R1 and R2 together require.
//
// THERE IS NO ALLOW-LIST. The single file permitted to name `actions/setup-go`
// unconditionally is permitted by IDENTITY, not by name: it is the wrapper's
// own action.yml — the definition site of the thing the rule is about, which
// cannot be a call site of itself. That is the same structural distinction
// `assert-image-jobs-authenticate.mjs` draws between declaring
// `pullImageWithRetry` and calling it. Anything else that ever has to be
// excluded must be excluded by a fact of that kind and not by a list of names.
//
// The gate never demands `cache: true`. Forcing it would break update-golden's
// read-only isolation for no gain, because the warm step — not the cache — is
// what makes an empty archive impossible.
//
// ENV CONTRACT
//   REPO_ROOT     — repository root. Default `process.cwd()`.
//   WORKFLOW_DIR  — workflows to scan. Default `.github/workflows`.
//   ACTIONS_DIR   — composite actions to scan. Default `.github/actions`.
//
// Exit: 0 when every rule holds, 1 on any violation.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { basename, isAbsolute, join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice } from './lib/gh.mjs';

// The Action this repo may only reach under R1's conditions, and the local
// wrapper that is the sanctioned way to reach it otherwise.
const upstreamSetupGo = 'actions/setup-go';
const wrapperDir = '.github/actions/setup-go';
const wrapperRef = './.github/actions/setup-go';

// The module the warm step has to run. Named rather than described, so a rename
// fails this gate instead of quietly turning R2 and R4 vacuous.
const warmScript = 'go-module-fetch.mjs';

// The one `cache:` value that takes a step out of R1's subject. What matters is
// that the value is STATICALLY RESOLVABLE — a reader and CodeQL's
// cache-poisoning query must both be able to see that this step cannot write
// the shared cache. `cache: false` and `cache: 'false'` both qualify, because
// withEntry strips the quotes; a `${{ … }}` expression does not, and that is
// the shape this rule exists to reject.
const cacheDisabled = 'false';

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

export function isUpstreamSetupGo(ref) {
  return ref === upstreamSetupGo || String(ref).startsWith(`${upstreamSetupGo}@`);
}

// stepBlocks — every `steps:` sequence in a file, split into its steps.
//
// One block per job in a workflow and one per composite action, which is
// exactly the scope R2 asks about: "does the SAME job also warm the cache".
// Deriving it from the `steps:` key rather than from a job-name parse means the
// scope is the runtime one.
//
// A `steps:` line this reader cannot follow is a HARD ERROR, never a skip.
// Skipping is what made this gate hollow: `steps:` used to be matched as
// `/^\s*steps:\s*$/`, so `steps: &dashboard_shard_steps` (e2e.yml) matched
// nothing and BOTH dashboard-shard jobs — including their
// `uses: ./.github/actions/setup-go` — went unscanned on a job that runs on
// refs/heads/main. The gate reported "none able to archive an empty module
// cache" while being structurally unable to see one of the call sites it
// exists to police. A scanner that silently ignores what it cannot parse
// reports clean for the wrong reason, so unparsed shapes now fail loudly.
const stepsKey = /^\s*steps:\s*(?<tail>\S.*)?$/;
// A YAML anchor declares the block here; the body follows and is scanned
// normally. An alias re-uses that same body, so the anchor's own scan already
// covers it and re-scanning would double-count every call site inside it.
const stepsAnchor = /^&[A-Za-z0-9_-]+$/;
const stepsAlias = /^\*[A-Za-z0-9_-]+$/;

export function stepBlocks(text, file = '<text>') {
  const lines = text.split('\n');
  const blocks = [];
  for (let i = 0; i < lines.length; i++) {
    if (isBlank(lines[i])) continue;
    const key = stepsKey.exec(lines[i]);
    if (!key) continue;
    const tail = (key.groups.tail || '').replace(/\s+#.*$/, '').trim();
    if (tail) {
      if (stepsAlias.test(tail)) continue;
      if (!stepsAnchor.test(tail)) {
        throw new Error(
          `${file}:${i + 1}: cannot follow \`${lines[i].trim()}\` — this scanner understands ` +
            '`steps:`, `steps: &anchor` and `steps: *alias` only. Teach it this shape rather ' +
            'than letting the block go unscanned; an unreadable steps block is how a call site ' +
            'hides from the gate.',
        );
      }
    }

    const base = indentOf(lines[i]);
    const body = [];
    for (let j = i + 1; j < lines.length; j++) {
      if (isBlank(lines[j])) {
        body.push(lines[j]);
        continue;
      }
      if (indentOf(lines[j]) <= base) break;
      body.push(lines[j]);
    }
    blocks.push({ label: enclosingKey(lines, i, base), steps: splitSteps(body) });
  }
  return blocks;
}

// enclosingKey — the nearest preceding mapping key at a shallower indent, used
// only to name the job in an error message.
function enclosingKey(lines, at, indent) {
  for (let i = at - 1; i >= 0; i--) {
    if (isBlank(lines[i]) || indentOf(lines[i]) >= indent) continue;
    const m = /^\s*([A-Za-z0-9_-]+):\s*$/.exec(lines[i]);
    if (m) return m[1];
  }
  return 'steps';
}

// splitSteps — a `steps:` body into one line-array per step, with the leading
// `- ` blanked so each step's keys form a plain mapping.
function splitSteps(body) {
  const present = body.filter((l) => !isBlank(l));
  if (present.length === 0) return [];
  const itemIndent = Math.min(...present.map(indentOf));
  const steps = [];
  let current = null;
  for (const line of body) {
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

function stepIndent(stepLines) {
  const present = stepLines.filter((l) => !isBlank(l));
  return present.length === 0 ? 0 : Math.min(...present.map(indentOf));
}

// hasKeyAtStepLevel — does this step declare `key:` as one of its OWN keys?
export function hasKeyAtStepLevel(stepLines, key) {
  const indent = stepIndent(stepLines);
  const re = new RegExp(`^\\s*${key}:(\\s|$)`);
  return stepLines.some((l) => !isBlank(l) && indentOf(l) === indent && re.test(l));
}

// withEntry — the literal value a step passes for one `with:` input, or null
// when the step does not set it or sets it to something non-literal.
export function withEntry(stepLines, name) {
  const indent = stepIndent(stepLines);
  let inWith = false;
  for (const line of stepLines) {
    if (isBlank(line)) continue;
    if (indentOf(line) === indent) {
      inWith = /^\s*with:\s*$/.test(line);
      continue;
    }
    if (!inWith) continue;
    const m = new RegExp(`^\\s*${name}:\\s*(.*)$`).exec(line);
    if (m) return unquote(m[1].replace(/\s+#.*$/, ''));
  }
  return null;
}

export function stepUses(stepLines) {
  for (const line of stepLines) {
    const ref = usesValue(line);
    if (ref !== null && indentOf(line) === stepIndent(stepLines)) return ref;
  }
  return null;
}

function stepRunsWarm(stepLines) {
  return stepLines.some((l) => !isComment(l) && l.includes(warmScript));
}

// nestedBuildDirs — the nested-module directories a step block builds in, read
// from `working-directory:`. Returns a Set of directories, deduped, excluding
// the repository root (which the ordinary warm step already covers).
//
// A directory only counts when it actually CONTAINS a go.mod in this checkout.
// `working-directory:` is how a workflow says "run over there" for Node dirs
// and scratch checkouts too, and neither has a module graph to warm — keying on
// the declaration alone reported six false violations on this repository.
//
// Deliberately keyed on the workflow's own declaration rather than on a list of
// known nested modules: a nested module added later is caught by the same rule,
// and one that is never built needs no warming and is correctly silent.
export function nestedBuildDirs(block, root) {
  const dirs = new Set();
  for (const step of block.steps) {
    for (const line of step) {
      if (isComment(line)) continue;
      const m = /^\s*working-directory:\s*(\S.*?)\s*$/.exec(line);
      if (m === null) continue;
      const dir = unquote(m[1].replace(/\s+#.*$/, '')).replace(/\/+$/, '');
      if (dir === '' || dir === '.' || dir.includes('${{')) continue;
      if (!existsSync(join(root, dir, 'go.mod'))) continue;
      dirs.add(dir);
    }
  }
  return dirs;
}

// stepWarmsModule — whether this step warms the module rooted at dir, either by
// passing it to the composite's `also-warm` or by running the warm script with
// that directory as its own working-directory.
export function stepWarmsModule(stepLines, dir) {
  const also = withEntry(stepLines, 'also-warm');
  if (also !== null && also.split(/\s+/).includes(`${dir}/go.mod`)) return true;
  if (!stepRunsWarm(stepLines)) return false;
  return stepLines.some((l) => {
    if (isComment(l)) return false;
    const m = /^\s*working-directory:\s*(\S.*?)\s*$/.exec(l);
    return m !== null && unquote(m[1]).replace(/\/+$/, '') === dir;
  });
}

// expressionsOutsideRuns — every `${{ … }}` an action manifest writes ABOVE its
// `runs:` block, in a non-comment line.
//
// GitHub template-evaluates the manifest's metadata, and `inputs` is not a valid
// named-value there. An expression written into an input's DESCRIPTION as prose
// therefore does not render: the manifest fails to LOAD with `Unrecognized
// named-value: 'inputs'`, and every job using the action dies before its first
// command. Comments are excluded because `#` lines are not evaluated, which is
// exactly where such an explanation belongs.
export function expressionsOutsideRuns(text) {
  const lines = text.split('\n');
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    if (/^runs:\s*$/.test(lines[i])) break;
    if (isComment(lines[i])) continue;
    if (lines[i].includes('${{')) out.push({ line: i + 1, text: lines[i].trim() });
  }
  return out;
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
  let directCallSites = 0;
  let scanned = 0;

  const wrapperFile = ['action.yml', 'action.yaml']
    .map((n) => join(abs(wrapperPath), n))
    .find((p) => existsSync(p));

  // R1 / R2 — the workflows.
  for (const file of yamlFilesIn(abs(workflowDir))) {
    scanned++;
    const text = readFileSync(file, 'utf8');
    for (const block of stepBlocks(text, file)) {
      const warms = block.steps.some(stepRunsWarm);

      // R6 — a job that BUILDS a nested module must also WARM it. A nested
      // go.mod is its own module graph: the root warm step does not fetch it
      // and the root `go test ./...` stops at it, so the job reaches the proxy
      // for it cold and unretried — the one fetch this whole action exists to
      // wrap, left uncovered (#2700). Keyed on `working-directory:`, which is
      // how a workflow says "build over there".
      for (const dir of nestedBuildDirs(block, abs('.'))) {
        const warmed = block.steps.some((s) => stepWarmsModule(s, dir));
        if (!warmed) {
          violations.push(
            `${file}: job "${block.label}" builds the nested module in ${dir}/ ` +
              `(working-directory) but never warms it. Pass ` +
              `\`also-warm: ${dir}/go.mod\` to ./.github/actions/setup-go in this job — ` +
              `the root warm step covers the root module only, so this module's proxy ` +
              `fetch runs cold and unretried.`,
          );
        }
      }

      for (const step of block.steps) {
        const ref = stepUses(step);
        if (ref === wrapperRef) wrapperCallSites++;
        if (ref === null || !isUpstreamSetupGo(ref)) continue;
        directCallSites++;

        const cache = withEntry(step, 'cache');
        if (cache !== cacheDisabled) {
          violations.push(
            `${basename(file)}:${block.label}: uses \`${ref}\` with \`cache: ${cache ?? '(unset)'}\`. A ` +
              `step that can SAVE the shared module cache must go through \`${wrapperRef}\`, which warms ` +
              'GOMODCACHE before the post-job step can archive an empty one — the failure that made every ' +
              'Go job in the repo run cold until the next go.sum bump. Only a literal `cache: false`, which ' +
              'saves nothing and so cannot poison anything, may reach the Action directly.',
          );
        } else if (!warms) {
          violations.push(
            `${basename(file)}:${block.label}: opts out of the shared cache but never runs ` +
              `\`${warmScript}\`. Skipping the cache is not skipping the FETCH, and that fetch is the one ` +
              'the Go module resolver will not retry past a dropped HTTP/2 frame. Add the warm step to ' +
              'this job.',
          );
        }
      }
    }
  }

  // R3 — no other composite action may reach the Action at all.
  for (const file of actionFilesIn(abs(actionsDir))) {
    scanned++;
    const text = readFileSync(file, 'utf8');
    for (const block of stepBlocks(text, file)) {
      for (const step of block.steps) {
        const ref = stepUses(step);
        if (ref === wrapperRef) wrapperCallSites++;
        if (ref === null || !isUpstreamSetupGo(ref)) continue;
        // The definition site cannot be a call site of itself.
        if (wrapperFile !== undefined && file === wrapperFile) continue;
        violations.push(
          `${file}: uses \`${ref}\`. A composite action has no job scope in which the warm step could be ` +
            `required, and it is reached from arbitrary callers, so it must use \`${wrapperRef}\` instead.`,
        );
      }
    }
  }

  // R4 — the wrapper is the real thing, and its warm step is unconditional.
  if (wrapperFile === undefined) {
    violations.push(
      `${wrapperPath}/action.yml does not exist — every call site would resolve to nothing, so the rules ` +
        'above would be satisfied by a setup path that sets nothing up.',
    );
  } else {
    const wrapperText = readFileSync(wrapperFile, 'utf8');
    for (const hit of expressionsOutsideRuns(wrapperText)) {
      violations.push(
        `${wrapperFile}:${hit.line}: a \`\${{ … }}\` expression outside \`runs:\`. GitHub ` +
          'template-evaluates an action manifest\'s metadata — including every input DESCRIPTION — in a ' +
          'context where `inputs` is not a valid named-value, so the expression does not render as ' +
          'prose: it fails the manifest to LOAD, and every job using this action dies on ' +
          '"Unrecognized named-value: \'inputs\'" before its first Go command. 26 jobs went red that ' +
          'way. Explain the input in a `#` comment instead.',
      );
    }
    const steps = stepBlocks(wrapperText).flatMap((b) => b.steps);
    if (!steps.some((s) => isUpstreamSetupGo(stepUses(s) ?? ''))) {
      violations.push(
        `${wrapperFile}: does not delegate to \`${upstreamSetupGo}\` — the wrapper installs no Go ` +
          'toolchain, so every caller would be silently Go-less.',
      );
    }
    const warmSteps = steps.filter(stepRunsWarm);
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

  // R5 — non-vacuity.
  if (wrapperCallSites === 0) {
    violations.push(
      `no workflow or action uses \`${wrapperRef}\` at all — this gate is passing because nothing sets ` +
        'Go up through it, which is the same green a satisfied gate reports.',
    );
  }

  return { violations, wrapperCallSites, directCallSites, scanned };
}

function main() {
  const root = process.env.REPO_ROOT || process.cwd();
  const workflowDir = process.env.WORKFLOW_DIR || '.github/workflows';
  const actionsDir = process.env.ACTIONS_DIR || '.github/actions';
  const { violations, wrapperCallSites, directCallSites, scanned } = scan({ root, workflowDir, actionsDir });

  log(
    `scanned ${scanned} workflow/action files; ${wrapperCallSites} steps set Go up through ${wrapperRef}, ` +
      `${directCallSites} reach ${upstreamSetupGo} directly under a literal \`cache: ${cacheDisabled}\``,
  );
  for (const v of violations) error(v);
  if (violations.length > 0) process.exit(1);
  notice(`${wrapperCallSites + directCallSites} Go setup call sites, none able to archive an empty module cache`);
  process.exit(0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
