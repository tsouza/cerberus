// assert-image-jobs-authenticate — every CI job that acquires a container image
// must authenticate to the registries it can acquire from, BEFORE it pulls.
//
// WHY THIS EXISTS. An anonymous pull is not an error. It succeeds, on most
// runs, for as long as the shared per-runner-IP quota happens to have room in
// it — and then one day it is refused with a 429 and the job reads as flake.
// That is why #1572 could fix a whole class of these by construction and
// `e2e.yml`'s `startup-bench` could still be sitting on the anonymous path
// afterwards: nobody was wrong, the property simply was not asserted anywhere.
// "Does this job authenticate before it pulls" is a question about the shape of
// a workflow, so it can be answered by reading the workflow, and a question a
// gate can answer is not a question that should keep being rediscovered from an
// outage.
//
// WHAT IT ASSERTS. Three rules, each derived from the acquisition MECHANISM the
// job uses rather than from a list of job names:
//
//   R1  A job that acquires any image at all has at least one registry login.
//   R2  A job that acquires an image whose ref resolves to a Docker Hub ref
//       (no registry host in the name) has a Docker Hub login.
//   R3  A job that acquires through the shared mirror-first policy
//       (`pullImageWithRetry` in lib/registry.mjs, which reaches for the
//       PRIVATE GHCR mirror before Docker Hub) has a ghcr.io login. Without it
//       the mirror is unreadable and every acquisition silently degrades to the
//       anonymous Docker Hub quota the mirror exists to stop spending.
//
// THERE IS NO ALLOW-LIST, and that is the substance of the module rather than a
// stylistic preference. The obvious way to write this gate is a regex for
// `docker pull` over each job's text plus a list of the jobs known to be fine —
// and such a list is where a real regression goes to hide, because the cheapest
// way to make a new red job green is to append its name. So the one shape that
// looked like a violation and is not — `ci.yml`'s discipline lane running
// `node --test .github/scripts/compose-pull-images.test.mjs`, which mentions an
// image-pulling module and pulls nothing — is excluded by a STRUCTURAL fact
// about the command, not by its name: `node --test X` hands X to the node test
// runner, which imports the module rather than running its CLI (every module
// here guards `main` behind an `import.meta.url` check). Anything that has to be
// excluded has to be excluded by a fact of that kind.
//
// HOW IT READS A JOB. A step's text is not evidence on its own — most of the
// acquisitions in this repo are two or three hops from the workflow. So each
// step's `run:` is tokenised into commands and every command is resolved to a
// fixpoint: `just <recipe>` expands to the recipe body out of the Justfile (with
// `{{VAR}}` bound from the Justfile's own assignments and `{{PARAM}}` from the
// call site), `node <script.mjs>` expands to the script's spawned commands and
// its `pullImageWithRetry` calls, `uses: ./.github/actions/<x>` expands to the
// composite action's own steps, and a wrapper that takes a command on argv
// (`build-with-registry-retry.mjs docker compose up`) expands to the command it
// wraps. A reference this resolver cannot follow is reported as an error rather
// than passed over: a gate that cannot read a step must not report that step
// clean.
//
// ENV CONTRACT
//   WORKFLOW_DIR    — directory of workflows to scan. Default `.github/workflows`.
//   REPO_ROOT       — repository root the Justfile / scripts / actions resolve
//                     against. Default `process.cwd()`.
//
// Exit: 0 when every image-acquiring job authenticates, 1 on any violation or on
// any reference the resolver could not follow.

import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { basename, isAbsolute, join, resolve as resolvePath } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error, log, notice } from './lib/gh.mjs';

// The acquisition primitive R3 is about. A script that imports this name — at
// any depth through relative imports — acquires through the GHCR mirror first,
// so the job running it needs mirror credentials whatever ref it ends up
// naming. Spelled once here because it is the join between this gate and
// lib/registry.mjs; if the primitive is ever renamed, this gate goes quiet and
// the rename is the only place to notice.
const sharedPolicySymbol = 'pullImageWithRetry';

// Docker Hub under the two names the toolchain uses for it. `docker login` with
// no host argument targets exactly this, which is why the empty registry is
// spelled as a value here rather than handled as a missing one.
const dockerHubRegistries = new Set(['', 'docker.io', 'index.docker.io', 'registry-1.docker.io']);

// The mirror lives in GHCR, so this is the host R3 requires. Read off
// lib/mirror.mjs's default `mirrorRegistry` rather than restated, so a mirror
// that moves cannot leave the gate asserting a login to the old host.
const mirrorHost = 'ghcr.io';

// The two names a step uses for "this checkout". Both are the repository root
// on a runner, so binding them to it is what lets
// `node $GITHUB_WORKSPACE/.github/scripts/pull-buildkit-image.mjs` resolve to
// the file this scan is reading.
const repoRootVars = { GITHUB_WORKSPACE: '.', REPO_ROOT: '.' };

// How deep a command may resolve before the resolver calls it a cycle. Recipes
// call recipes and scripts spawn scripts, so some depth is normal; a chain
// longer than this is a loop, and a loop must fail loudly rather than hang.
const maxResolutionDepth = 12;

// ---------------------------------------------------------------------------
// YAML — the narrow slice of it these workflows use.
//
// Only `node:` builtins are available here (see the repo rule on CI scripts), so
// this reads the block structure by indentation rather than parsing YAML in
// general. That is sound for the shapes involved — a mapping of jobs, each with
// a sequence of step mappings — and every function below fails rather than
// guesses when the text does not have the shape it expects.
// ---------------------------------------------------------------------------

function indentOf(line) {
  return line.length - line.trimStart().length;
}

function isBlank(line) {
  return line.trim() === '' || line.trimStart().startsWith('#');
}

// blockUnder — the lines nested under the line at `start`, i.e. everything up to
// the next non-blank line at or above its indentation.
//
// Blank and comment lines are buffered rather than emitted on sight. A comment
// between two steps sits at the OUTER indentation, so emitting it immediately
// would splice it into the block that precedes it — and a `run: |` body that has
// swallowed the next step's explanatory comment contains whatever commands that
// prose names in backticks. That is not a hypothetical: it made this resolver
// report `just e2e-up`` (trailing backtick and all) as a recipe it could not
// find.
function blockUnder(lines, start) {
  const base = indentOf(lines[start]);
  const out = [];
  let pending = [];
  for (let i = start + 1; i < lines.length; i++) {
    if (isBlank(lines[i])) {
      pending.push(lines[i]);
      continue;
    }
    if (indentOf(lines[i]) <= base) break;
    out.push(...pending, lines[i]);
    pending = [];
  }
  return out;
}

function findKey(lines, key, atIndent = null) {
  const re = new RegExp(`^(\\s*)${key}:(\\s|$)`);
  for (let i = 0; i < lines.length; i++) {
    if (isBlank(lines[i])) continue;
    const m = re.exec(lines[i]);
    if (!m) continue;
    if (atIndent !== null && m[1].length !== atIndent) continue;
    return i;
  }
  return -1;
}

// scalarAt — the value of `key:` at the top level of `lines`, handling the three
// forms these workflows use: an inline scalar, a `|`/`>` block, and a quoted
// inline scalar.
function scalarAt(lines, key) {
  const i = findKey(lines, key, minIndent(lines));
  if (i === -1) return null;
  const inline = lines[i].slice(lines[i].indexOf(':') + 1).trim();
  if (inline !== '' && inline !== '|' && inline !== '>' && inline !== '|-' && inline !== '>-') {
    return unquote(inline);
  }
  return blockUnder(lines, i)
    .map((l) => l.slice(Math.min(indentOf(lines[i]) + 1, l.length)))
    .join('\n');
}

function minIndent(lines) {
  let min = null;
  for (const l of lines) {
    if (isBlank(l)) continue;
    const ind = indentOf(l);
    if (min === null || ind < min) min = ind;
  }
  return min ?? 0;
}

function unquote(value) {
  const t = value.trim();
  if ((t.startsWith('"') && t.endsWith('"')) || (t.startsWith("'") && t.endsWith("'"))) {
    return t.slice(1, -1);
  }
  return t;
}

// mappingAt — the flat `key: value` pairs directly under `key:`. Used for `env:`
// and `with:`, neither of which nests in these workflows.
function mappingAt(lines, key) {
  const i = findKey(lines, key, minIndent(lines));
  if (i === -1) return {};
  const out = {};
  for (const line of blockUnder(lines, i)) {
    if (isBlank(line)) continue;
    const m = /^\s*([A-Za-z0-9_.-]+):\s*(.*)$/.exec(line);
    if (m) out[m[1]] = unquote(m[2]);
  }
  return out;
}

// splitSteps — a `steps:` block into one line-array per step. A step begins at a
// `- ` item at the sequence's own indentation; everything until the next one
// belongs to it. The leading `- ` is replaced with spaces so each step's lines
// form a plain mapping the helpers above can read.
function splitSteps(stepsBlock) {
  const itemIndent = minIndent(stepsBlock);
  const steps = [];
  let current = null;
  for (const line of stepsBlock) {
    const isItem = !isBlank(line) && indentOf(line) === itemIndent && /^\s*-\s/.test(line);
    if (isItem) {
      if (current) steps.push(current);
      current = [line.replace(/^(\s*)-(\s)/, '$1 $2')];
      continue;
    }
    if (current) current.push(line);
  }
  if (current) steps.push(current);
  return steps;
}

// parseWorkflow — { env, jobs: [{ name, env, steps }] }.
export function parseWorkflow(text) {
  const lines = text.split('\n');
  const jobsIdx = findKey(lines, 'jobs', 0);
  if (jobsIdx === -1) return { env: {}, jobs: [] };

  const topEnvIdx = findKey(lines, 'env', 0);
  const workflowEnv = topEnvIdx === -1 ? {} : mappingAt(lines.slice(topEnvIdx), 'env');

  const jobsBlock = blockUnder(lines, jobsIdx);
  const jobIndent = minIndent(jobsBlock);
  const jobs = [];
  let current = null;
  for (const line of jobsBlock) {
    if (!isBlank(line) && indentOf(line) === jobIndent) {
      const m = /^\s*([A-Za-z0-9_-]+):\s*$/.exec(line);
      if (m) {
        if (current) jobs.push(current);
        current = { name: m[1], lines: [] };
        continue;
      }
    }
    if (current) current.lines.push(line);
  }
  if (current) jobs.push(current);

  return {
    env: workflowEnv,
    jobs: jobs.map((job) => {
      const stepsIdx = findKey(job.lines, 'steps', minIndent(job.lines));
      const steps = stepsIdx === -1 ? [] : splitSteps(blockUnder(job.lines, stepsIdx));
      return { name: job.name, env: mappingAt(job.lines, 'env'), steps };
    }),
  };
}

// ---------------------------------------------------------------------------
// Shell — splitting a `run:` body into commands, and a command into argv.
// ---------------------------------------------------------------------------

// tokenise — argv for one command. Quotes group, and nothing else is
// interpreted: the caller wants the words, not a shell.
export function tokenise(command) {
  const out = [];
  let token = '';
  let quote = null;
  let started = false;
  for (const ch of command) {
    if (quote) {
      if (ch === quote) quote = null;
      else token += ch;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      started = true;
      continue;
    }
    if (/\s/.test(ch)) {
      if (started) out.push(token);
      token = '';
      started = false;
      continue;
    }
    token += ch;
    started = true;
  }
  if (started) out.push(token);
  return out;
}

// splitCommands — a shell script body into individual commands. Joins
// backslash-continued lines, drops comments, and splits on the separators that
// start a new command. Redirections and subshell punctuation are left in the
// token stream; they never occupy argv[0] in this tree, so they cost nothing.
export function splitCommands(script) {
  const joined = String(script ?? '')
    .replace(/\\\n/g, ' ')
    .split('\n')
    .map((l) => (l.trimStart().startsWith('#') ? '' : l))
    .join('\n');
  return joined
    .split(/\n|&&|\|\||;|(?<!\|)\|(?!\|)/)
    .map((c) => c.trim())
    .filter((c) => c !== '');
}

// stripPrefixes — leading noise that is not the program: a Justfile's `@`
// silencer, `sudo`, `env`, and inline `KEY=value` assignments. The assignments
// are returned, because a `run:` that sets an image ref inline is naming a ref
// the resolver wants.
function stripPrefixes(argv) {
  const assignments = {};
  let i = 0;
  if (argv[0]?.startsWith('@')) argv = [argv[0].slice(1), ...argv.slice(1)];
  while (i < argv.length) {
    const t = argv[i];
    if (t === 'sudo' || t === 'env' || t === 'command' || t === '-') {
      i++;
      continue;
    }
    const m = /^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/.exec(t);
    if (m) {
      assignments[m[1]] = m[2];
      i++;
      continue;
    }
    break;
  }
  return { argv: argv.slice(i).filter((t) => t !== ''), assignments };
}

// ---------------------------------------------------------------------------
// Refs.
// ---------------------------------------------------------------------------

// isDockerHubRef — a ref with no registry host. Docker's own rule: the first
// path segment is a host only when it contains a `.` or a `:`, or is
// `localhost`. Everything else is Docker Hub, which is why `golang:1.26` and
// `clickhouse/clickhouse-server:26.5` both spend that quota.
export function isDockerHubRef(ref) {
  const text = String(ref);
  // No slash at all is an official library image (`golang:1.26`). Testing the
  // first segment before checking for the slash reads the TAG's colon as a
  // registry port, which quietly classifies the single most-pulled image in
  // this repo as "not Docker Hub".
  const slash = text.indexOf('/');
  if (slash === -1) return true;
  const first = text.slice(0, slash);
  if (first === 'localhost') return false;
  return !(first.includes('.') || first.includes(':'));
}

// `docker compose`'s value-taking GLOBAL flags — the ones that may appear
// before the verb and swallow the token after them.
const composeValueFlags = new Set([
  '-f',
  '--file',
  '-p',
  '--project-name',
  '--project-directory',
  '--env-file',
  '--profile',
  '--parallel',
  '--progress',
  '--ansi',
]);

// `docker login`'s only value-taking flags. Needed because the registry is a
// trailing POSITIONAL, so "the first token that is not a flag" picks up a flag's
// value instead and reports a login to a registry named `tsouza`.
const dockerLoginValueFlags = new Set(['-u', '--username', '-p', '--password']);

// loginRegistry — which registry a `docker login` argv authenticates to.
//
// The empty string means Docker Hub, and it is the answer in two distinct
// cases that `docker` itself treats identically: no host argument at all, and a
// host argument that came from an environment variable the step never set.
// `docker login` with no host goes to Docker Hub, so a `${REGISTRY}` that is
// unset is a Docker Hub login rather than an unknown one — which is exactly how
// `registry-login.mjs` documents its own contract.
export function loginRegistry(argv, scope) {
  let host;
  for (let i = 0; i < argv.length; i++) {
    const t = argv[i];
    if (dockerLoginValueFlags.has(t)) {
      i++;
      continue;
    }
    if (t.startsWith('-')) continue;
    host = t;
    break;
  }
  if (host === undefined) return '';
  const expanded = expandVars(host, scope);
  if (expanded !== null) return expanded.trim();
  // A bare `$NAME` / `${NAME}` that the scope does not define is unset, not
  // unknowable. A GitHub `${{ … }}` expression is genuinely unknowable and is
  // NOT read as Docker Hub, because guessing there would invent a login.
  return /^\$\{?[A-Za-z_][A-Za-z0-9_]*\}?$/.test(host) ? '' : host.trim();
}

// looksLikeImageRef — used to tell an image argument from a file argument on a
// script's argv (`pull-images.mjs clickhouse/clickhouse-server:26.5` versus
// `compose-pull-images.mjs docker-compose.yml`). Deliberately conservative: a
// token it declines only costs R2 a ref it could have checked, never a false
// violation.
export function looksLikeImageRef(token) {
  const t = String(token);
  if (t === '' || t.startsWith('-')) return false;
  if (/\.(ya?ml|json|mjs|js|sh|txt|md)$/.test(t)) return false;
  if (t.includes('$') || t.includes('{')) return false;
  if (!/^[a-z0-9][a-z0-9._/-]*(:[\w][\w.-]*)?(@sha256:[a-f0-9]+)?$/.test(t)) return false;
  return t.includes(':') || t.includes('/');
}

// expandVars — `$VAR`, `${VAR}` and a Justfile's `{{VAR}}` against a scope.
// Returns null when a name is unresolvable, which is a real answer: an
// unresolved ref is not checked against R2 rather than guessed at.
export function expandVars(text, scope) {
  let out = String(text);
  let unresolved = false;
  out = out.replace(/\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g, (_, name) => {
    if (name in scope) return scope[name];
    unresolved = true;
    return '';
  });
  out = out.replace(/\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?/g, (_, name) => {
    if (name in scope) return scope[name];
    unresolved = true;
    return '';
  });
  // A GitHub expression is never statically known.
  if (/\$\{\{/.test(text)) unresolved = true;
  return unresolved ? null : out;
}

// ---------------------------------------------------------------------------
// Justfile.
// ---------------------------------------------------------------------------

function parseJustfile(text) {
  const vars = {};
  const recipes = new Map();
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const v = /^([A-Za-z_][A-Za-z0-9_]*)\s*:=\s*(.*)$/.exec(line);
    if (v) {
      vars[v[1]] = unquote(v[2].trim());
      continue;
    }
    // A recipe header sits at column 0 and its body is indented. Dependencies
    // after the colon are other recipes and are followed too.
    const r = /^([A-Za-z_][A-Za-z0-9_-]*)((?:\s+[+*]?[A-Za-z_][A-Za-z0-9_]*(?:="[^"]*")?)*)\s*:(.*)$/.exec(line);
    if (!r || line.startsWith(' ') || line.startsWith('\t')) continue;
    const params = (r[2].match(/[+*]?[A-Za-z_][A-Za-z0-9_]*(?:="[^"]*")?/g) ?? []).map((p) => {
      const [name, dflt] = p.replace(/^[+*]/, '').split('=');
      return { name, dflt: dflt === undefined ? null : unquote(dflt) };
    });
    const deps = r[3].trim().split(/\s+/).filter((d) => d !== '' && !d.startsWith('#'));
    const body = [];
    for (let j = i + 1; j < lines.length; j++) {
      if (lines[j].trim() === '') continue;
      if (!/^\s/.test(lines[j])) break;
      body.push(lines[j].trim());
    }
    recipes.set(r[1], { params, deps, body });
  }
  return { vars, recipes };
}

// ---------------------------------------------------------------------------
// Node scripts — the commands a `.mjs` module spawns, and whether it acquires
// through the shared mirror-first policy.
// ---------------------------------------------------------------------------

// stringLiterals — the quoted strings inside one bracketed argument list.
function stringLiterals(text) {
  return [...text.matchAll(/(['"])((?:\\.|(?!\1).)*)\1/g)].map((a) => a[2]);
}

// envReadInto — the environment variable a local name is assigned from, if any.
// `const registry = (process.env.REGISTRY ?? '').trim();` answers `REGISTRY`.
// The point is not the assignment: it is that an argv token sourced this way is
// knowable from the STEP's `env:` block, so a value the module cannot state
// statically is still resolvable where it is actually set.
function envReadInto(source, name) {
  const re = new RegExp(`\\b(?:const|let|var)\\s+${name}\\s*=[^;\\n]*process\\.env\\.([A-Za-z_][A-Za-z0-9_]*)`);
  const m = re.exec(source);
  return m === null ? null : m[1];
}

// resolveArgArray — the argv a module builds in a named variable before
// spawning it. Written separately from the inline-array case because the two
// shapes carry the same meaning and only one of them is greppable: a script
// that must append an argument conditionally cannot write its argv as a
// literal, and reading only literals would make the built-up form invisible.
// Tokens pushed from the environment come back as `${NAME}` so the caller
// resolves them against the step's own `env:`.
function resolveArgArray(source, name) {
  const decl = new RegExp(`\\b(?:const|let|var)\\s+${name}\\s*=\\s*\\[([^\\]]*)\\]`).exec(source);
  if (decl === null) return null;
  const args = stringLiterals(decl[1]);
  for (const m of source.matchAll(new RegExp(`\\b${name}\\.push\\(([^)]*)\\)`, 'g'))) {
    const arg = m[1].trim();
    const lit = /^(['"])((?:\\.|(?!\1).)*)\1$/.exec(arg);
    if (lit) {
      args.push(lit[2]);
      continue;
    }
    const env = /^[A-Za-z_$][\w$]*$/.test(arg) ? envReadInto(source, arg) : null;
    if (env !== null) args.push(`\${${env}}`);
  }
  return args;
}

// spawnedCommands — the argv shapes a module hands to the shared subprocess
// helpers. Every registry interaction in this tree is written this way, with the
// program and the verb as string literals, which is what makes it readable
// statically.
export function spawnedCommands(source) {
  const out = [];
  const re = /\b(?:capture|exec|spawnSync|spawn)\(\s*(['"])([\w./-]+)\1\s*,\s*(\[[^\]]*\]|[A-Za-z_$][\w$]*)/g;
  for (const m of source.matchAll(re)) {
    const args = m[3].startsWith('[') ? stringLiterals(m[3]) : resolveArgArray(source, m[3]);
    if (args === null) continue;
    out.push([m[2], ...args]);
  }
  return out;
}

// literalRefs — the image refs a module states as string literals, including the
// ones it states in a module it imports. `mirror-images.mjs` names none itself:
// its inventory is `mirroredImages` over in `lib/mirror.mjs`, and the refs reach
// `docker buildx imagetools` through a variable, so the argv alone says only
// THAT it copies images and never WHICH. Read only for a module already judged
// to acquire, so a ref mentioned by a module that acquires nothing stays
// invisible.
function literalRefs(source) {
  const out = [];
  for (const m of stripComments(source).matchAll(/\[([^\]]*)\]/g)) {
    for (const lit of stringLiterals(m[1])) if (looksLikeImageRef(lit)) out.push(lit);
  }
  return out;
}

// policyRefs — the image refs a module hands to `pullImageWithRetry`, resolved
// through its own top-level `const NAME = 'literal'` table so a ref named once
// and used once is still visible.
function policyRefs(source) {
  const consts = {};
  for (const m of source.matchAll(/^const\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(['"])([^'"]+)\2\s*;?\s*$/gm)) {
    consts[m[1]] = m[3];
  }
  const refs = [];
  for (const m of source.matchAll(new RegExp(`${sharedPolicySymbol}\\(\\s*([^,)]+)`, 'g'))) {
    const arg = m[1].trim();
    const lit = /^(['"])([^'"]+)\1$/.exec(arg);
    if (lit) refs.push(lit[2]);
    else if (arg in consts) refs.push(consts[arg]);
  }
  return refs;
}

// stripComments — so a module that DOCUMENTS the primitive is not read as one
// that calls it. lib/registry.mjs's header prints the signature
// (`pullImageWithRetry(image, options)`) in prose, which is indistinguishable
// from a call site to any regex that reads comments as code.
function stripComments(source) {
  return String(source ?? '')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .split('\n')
    .map((l) => l.replace(/(^|\s)\/\/.*$/, '$1'))
    .join('\n');
}

// callsSharedPolicy — the primitive is INVOKED here, as opposed to declared here
// (lib/registry.mjs) or named in a comment. The `function` lookbehind is what
// separates the declaration from every call of it.
export function callsSharedPolicy(source) {
  return new RegExp(`(?<!function\\s)\\b${sharedPolicySymbol}\\s*\\(`).test(stripComments(source));
}

function relativeImports(source) {
  return [...source.matchAll(/^import[^'"]*(['"])(\.[^'"]+)\1/gm)].map((m) => m[2]);
}

// namedImports — `{ a, b as c }` per relative specifier. Used to attribute an
// imported array to the module that actually reads it: every consumer of
// `lib/mirror.mjs` imports `mirroredRef`, but only the mirroring job imports the
// INVENTORY, and charging the inventory to all of them reports jobs as pulling
// two dozen images they never touch.
function namedImports(source) {
  const out = new Map();
  for (const m of stripComments(source).matchAll(/^import\s*\{([^}]*)\}\s*from\s*(['"])(\.[^'"]+)\2/gm)) {
    const names = m[1]
      .split(',')
      .map((n) => n.trim().split(/\s+as\s+/)[0].trim())
      .filter((n) => n !== '');
    out.set(m[3], [...(out.get(m[3]) ?? []), ...names]);
  }
  return out;
}

// arrayBinding — the string literals of `const NAME = [ … ]` in a module.
function arrayBinding(source, name) {
  const m = new RegExp(`\\b(?:export\\s+)?(?:const|let|var)\\s+${name}\\s*=\\s*\\[([^\\]]*)\\]`).exec(
    stripComments(source),
  );
  return m === null ? [] : stringLiterals(m[1]);
}

// ---------------------------------------------------------------------------
// The resolver.
// ---------------------------------------------------------------------------

// A `Findings` accumulator: what a job does, gathered across every hop.
function newFindings() {
  return {
    acquisitions: [],
    logins: [],
    viaSharedPolicy: false,
    refs: new Set(),
    unresolved: [],
    // R4's bookkeeping: which compose models the shared policy was applied to,
    // which ones were materialised, and how many acquisitions were writes INTO
    // the mirror rather than reads that the mirror could have served.
    composePolicyFiles: new Set(),
    composeMaterialised: [],
    mirrorWrites: 0,
  };
}

class Resolver {
  constructor(root) {
    this.root = root;
    this.just = null;
    this.sourceCache = new Map();
    this.policyCache = new Map();
  }

  read(path) {
    const abs = isAbsolute(path) ? path : join(this.root, path);
    if (!this.sourceCache.has(abs)) {
      this.sourceCache.set(abs, existsSync(abs) ? readFileSync(abs, 'utf8') : null);
    }
    return this.sourceCache.get(abs);
  }

  justfile() {
    if (this.just === null) {
      const text = this.read('Justfile');
      if (text === null) throw new Error('Justfile not found — `just` recipes cannot be resolved');
      this.just = parseJustfile(text);
    }
    return this.just;
  }

  // usesSharedPolicy — does this module, or anything it imports, acquire through
  // pullImageWithRetry? Memoised, and cycle-safe by marking before recursing.
  //
  // Following EVERY relative import rather than only the one the primitive
  // arrives on over-approximates, deliberately. The two errors are not
  // symmetric: over-approximating costs a job a login it did not strictly need,
  // while under-approximating is the anonymous pull this whole module exists to
  // catch.
  usesSharedPolicy(path, seen = new Set()) {
    if (this.policyCache.has(path)) return this.policyCache.get(path);
    if (seen.has(path)) return false;
    seen.add(path);
    const source = this.read(path);
    if (source === null) return false;
    // A CALL, not a mention. lib/registry.mjs declares the primitive and
    // chart-kubeconform.mjs imports a rate-limit classifier out of the same
    // module — matching the bare identifier reported that job as pulling images
    // it never pulls, and a gate that invents a violation gets an exemption
    // written for it, which is the failure mode this module exists to avoid.
    let answer = callsSharedPolicy(source);
    if (!answer) {
      const dir = join(this.root, path, '..');
      for (const spec of relativeImports(source)) {
        const child = resolvePath(dir, spec).slice(this.root.length + 1);
        if (this.usesSharedPolicy(child, seen)) {
          answer = true;
          break;
        }
      }
    }
    this.policyCache.set(path, answer);
    return answer;
  }

  // moduleLiteralRefs — the image refs this module states as literals, plus the
  // ones held by an array binding it IMPORTS BY NAME.
  //
  // The import hop is the load-bearing half: the mirroring job's inventory lives
  // in `lib/mirror.mjs` and reaches `docker buildx imagetools` through a loop
  // variable, so its argv says only THAT it copies images and never which — R2
  // would degrade to "this job logs in somewhere" for the one job whose whole
  // purpose is reading Docker Hub. Following the NAME rather than the module is
  // what stops that from charging the inventory to every job that imports
  // `mirroredRef` out of the same file.
  moduleLiteralRefs(path) {
    const source = this.read(path);
    if (source === null) return [];
    const out = literalRefs(source);
    const dir = join(this.root, path, '..');
    for (const [spec, names] of namedImports(source)) {
      const child = this.read(resolvePath(dir, spec).slice(this.root.length + 1));
      if (child === null) continue;
      for (const name of names) out.push(...arrayBinding(child, name).filter(looksLikeImageRef));
    }
    return out;
  }

  // command — classify one argv, following it wherever it goes.
  command(rawArgv, scope, findings, depth) {
    if (depth > maxResolutionDepth) {
      findings.unresolved.push(`resolution depth exceeded at \`${rawArgv.join(' ')}\``);
      return;
    }
    const { argv, assignments } = stripPrefixes(rawArgv);
    if (argv.length === 0) return;
    const inner = { ...scope, ...assignments };
    const program = argv[0];

    if (program === 'docker') return this.docker(argv, inner, findings, depth);
    if (program === 'k3d') return this.k3d(argv, inner, findings, depth);
    if (program === 'just') return this.justRecipe(argv, inner, findings, depth);
    if (program === 'node') return this.node(argv, inner, findings, depth);
    if (program === 'bash' || program === 'sh') {
      // `-c` takes the script itself rather than a path to one.
      const c = argv.indexOf('-c');
      if (c !== -1 && argv[c + 1] !== undefined) {
        for (const cmd of splitCommands(argv[c + 1])) {
          this.command(tokenise(cmd), inner, findings, depth + 1);
        }
        return;
      }
      const script = argv.slice(1).find((t) => !t.startsWith('-'));
      if (script) this.shellScript(script, inner, findings, depth);
      return;
    }
    if (/\.sh$/.test(program)) return this.shellScript(program, inner, findings, depth);
  }

  docker(argv, scope, findings, depth) {
    const rest = argv.slice(1).filter((t) => t !== '');
    const sub = rest.find((t) => !t.startsWith('-'));
    if (sub === 'login') {
      findings.logins.push({ registry: loginRegistry(rest.slice(rest.indexOf('login') + 1), scope) });
      return;
    }
    if (sub === 'pull') {
      findings.acquisitions.push(`docker pull`);
      for (const t of rest.slice(rest.indexOf('pull') + 1)) this.noteRef(t, scope, findings);
      return;
    }
    if (sub === 'run' || sub === 'create') {
      // The image argument sits after an arbitrary run of flags whose value
      // arity is not knowable from the token stream, so the ref is left
      // unresolved rather than guessed. R1 still applies; the ref that matters
      // is normally named by the pre-pull in the same job anyway.
      findings.acquisitions.push(`docker ${sub}`);
      return;
    }
    if (sub === 'compose') {
      const after = rest.slice(rest.indexOf('compose') + 1);
      // The verb sits after the global flags, and the value-taking ones must be
      // stepped over: `docker compose -f x.yml up` otherwise reads `x.yml` as
      // the verb, matches nothing, and the materialisation vanishes from the
      // scan entirely.
      let verb;
      for (let i = 0; i < after.length; i++) {
        if (composeValueFlags.has(after[i])) {
          i++;
          continue;
        }
        if (after[i].startsWith('-')) continue;
        verb = after[i];
        break;
      }
      if (['up', 'pull', 'create', 'run', 'build'].includes(verb)) {
        findings.acquisitions.push(`docker compose ${verb}`);
        // `-f` may repeat. No `-f` means compose's own default lookup in the
        // working directory, recorded as "the job's default model" — the cwd is
        // not tracked across a script's `cd`, so the models are matched by file
        // name. Looser than a path comparison, and deliberately so: the failure
        // that matters is a materialisation with NO pre-pull anywhere in the
        // job, and a gate that fires on a path it merely failed to follow would
        // earn itself an exemption.
        const files = [];
        for (let i = 0; i < after.length; i++) {
          if ((after[i] === '-f' || after[i] === '--file') && after[i + 1] !== undefined) {
            files.push(basename(unquote(expandVars(after[i + 1], scope) ?? after[i + 1])));
          }
        }
        findings.composeMaterialised.push(files);
      }
      return;
    }
    // `imagetools` reads a manifest straight out of the registry — `create`
    // copies one ref to another, `inspect` resolves one. Neither puts an image
    // in the local daemon, which is why it does not look like an acquisition,
    // but the READ still spends the source registry's quota exactly as a pull
    // does. The job that mirrors this repo's inventory acquires images this way
    // and no other way, so leaving it out excused the one job whose entire
    // purpose is reading Docker Hub.
    if (sub === 'buildx' && rest.includes('imagetools')) {
      const verb = rest.slice(rest.indexOf('imagetools') + 1).find((t) => !t.startsWith('-'));
      if (['create', 'inspect'].includes(verb)) {
        findings.acquisitions.push(`docker buildx imagetools ${verb}`);
        // A write INTO a registry, not a read the mirror could have served.
        // R4 asks that reads route through the mirror; asking it of the job
        // that POPULATES the mirror is circular, and this is the structural
        // fact that says so rather than the job's name.
        findings.mirrorWrites++;
        for (const t of rest.slice(rest.indexOf(verb) + 1)) this.noteRef(t, scope, findings);
      }
      return;
    }
    if (sub === 'build' || (sub === 'buildx' && rest.includes('build'))) {
      // A build resolves its `FROM` refs against the registry from inside
      // BuildKit, so it is an acquisition even though nothing is pulled by the
      // daemon.
      findings.acquisitions.push('docker build');
    }
  }

  k3d(argv, scope, findings, _depth) {
    if (argv[1] !== 'cluster' || argv[2] !== 'create') return;
    findings.acquisitions.push('k3d cluster create');
    const i = argv.indexOf('--image');
    if (i !== -1 && argv[i + 1]) this.noteRef(argv[i + 1], scope, findings);
  }

  justRecipe(argv, scope, findings, depth) {
    const { vars, recipes } = this.justfile();
    const name = argv[1];
    if (name === undefined) return;
    const recipe = recipes.get(name);
    if (!recipe) {
      findings.unresolved.push(`\`just ${name}\` names no recipe in the Justfile`);
      return;
    }
    const bound = { ...vars, ...scope };
    // Expanded AT THE CALL SITE. `just schema-ddl-test` runs
    // `just _pull-retry {{CH_TEST_IMAGE}}`, so binding the argument verbatim
    // would hand `_pull-retry` the literal text `{{CH_TEST_IMAGE}}` and the ref
    // would arrive at `pull-images.mjs` still unexpanded — which is exactly how
    // two jobs pulling a named Docker Hub image read as "ref unknown" and slipped
    // the Docker Hub rule.
    const args = argv.slice(2).map((a) => expandVars(a, bound) ?? a);
    recipe.params.forEach((p, i) => {
      // A variadic parameter swallows the rest; anything unsupplied falls back
      // to its default, and a required parameter with nothing behind it stays
      // unbound so `expandVars` reports it rather than inventing a value.
      const value = i === recipe.params.length - 1 ? args.slice(i).join(' ') : args[i];
      if (value !== undefined && value !== '') bound[p.name] = value;
      else if (p.dflt !== null) bound[p.name] = p.dflt;
    });
    for (const dep of recipe.deps) this.command(['just', dep], scope, findings, depth + 1);
    for (const line of recipe.body) {
      for (const cmd of splitCommands(line)) {
        this.command(tokenise(cmd), bound, findings, depth + 1);
      }
    }
  }

  node(argv, scope, findings, depth) {
    const rest = argv.slice(1);
    // `node --test X` runs X under the node test runner: it imports the module
    // and executes its tests, never its CLI main. This is the structural reason
    // ci.yml's discipline lane is not an image acquisition, and it holds for
    // every module rather than for one named one.
    if (rest.includes('--test')) return;
    const scriptIdx = rest.findIndex((t) => !t.startsWith('-'));
    if (scriptIdx === -1) return;
    const script = expandVars(rest[scriptIdx], scope) ?? rest[scriptIdx];
    if (/\.test\.mjs$/.test(script)) return;

    const source = this.read(script);
    if (source === null) {
      findings.unresolved.push(`\`node ${script}\` names no file in the repository`);
      return;
    }
    if (this.usesSharedPolicy(script)) {
      findings.viaSharedPolicy = true;
      findings.acquisitions.push(`${script} (shared mirror-first policy)`);
      for (const ref of policyRefs(source)) this.noteRef(ref, scope, findings);
      // Refs handed to the script on argv, e.g. `pull-images.mjs golang:1.26`.
      for (const t of rest.slice(scriptIdx + 1)) {
        this.noteRef(t, scope, findings);
        // A compose FILE handed to the shared policy is the pre-pull that puts
        // that model's images in the daemon ahead of `up`. Recorded structurally
        // — the policy was applied to this model — rather than by the name of
        // the module that applied it.
        const arg = unquote(expandVars(t, scope) ?? t);
        if (/\.ya?ml$/.test(arg)) findings.composePolicyFiles.add(basename(arg));
      }
    }
    const before = findings.acquisitions.length;
    for (const spawned of spawnedCommands(source)) {
      this.command(spawned, scope, findings, depth + 1);
    }
    // Only once the module has been judged to acquire. A ref stated by a module
    // that acquires nothing is a name, not a pull.
    if (findings.acquisitions.length > before) {
      for (const ref of this.moduleLiteralRefs(script)) this.noteRef(ref, scope, findings);
    }
    // A wrapper takes the command it wraps on its own argv.
    const wrapped = rest.slice(scriptIdx + 1);
    if (wrapped.length > 0) this.command(wrapped, scope, findings, depth + 1);
  }

  shellScript(rawPath, scope, findings, depth) {
    const path = expandVars(rawPath, scope) ?? rawPath;
    const source = this.read(path);
    if (source === null) {
      findings.unresolved.push(`\`${path}\` names no file in the repository`);
      return;
    }
    for (const cmd of splitCommands(source)) {
      this.command(tokenise(cmd), scope, findings, depth + 1);
    }
  }

  // noteRef — record every image ref a token names. Splits on whitespace because
  // a variadic Justfile parameter arrives as ONE token holding several refs
  // (`_pull-retry {{CH_TEST_IMAGE}} {{CH_TEST_IMAGE_PRIOR}}` binds `IMAGES` to
  // both), and a two-ref token matches no image-ref shape at all — so the whole
  // pair would vanish rather than half of it.
  noteRef(token, scope, findings) {
    const expanded = expandVars(token, scope);
    if (expanded === null) return;
    for (const part of unquote(expanded).trim().split(/\s+/)) {
      if (looksLikeImageRef(part)) findings.refs.add(part);
    }
  }

  // step — one workflow step, including a local composite action's own steps.
  step(stepLines, scope, findings, depth = 0) {
    const uses = scalarAt(stepLines, 'uses');
    const run = scalarAt(stepLines, 'run');
    const stepEnv = mappingAt(stepLines, 'env');
    const inner = { ...scope, ...stepEnv };

    if (uses !== null) {
      if (/^docker\/login-action(@|$)/.test(uses)) {
        const w = mappingAt(stepLines, 'with');
        findings.logins.push({ registry: (w.registry ?? '').trim() });
        return;
      }
      if (uses.startsWith('./')) {
        const actionPath = ['action.yml', 'action.yaml']
          .map((f) => join(uses.slice(2), f))
          .find((p) => this.read(p) !== null);
        if (actionPath === undefined) {
          findings.unresolved.push(`\`uses: ${uses}\` resolves to no action.yml`);
          return;
        }
        const lines = this.read(actionPath).split('\n');
        const runsIdx = findKey(lines, 'runs', 0);
        if (runsIdx === -1) return;
        const runsBlock = blockUnder(lines, runsIdx);
        const inputs = defaultInputs(lines);
        const withValues = mappingAt(stepLines, 'with');
        const actionScope = { ...inner, ...inputs, ...withValues };
        const stepsIdx = findKey(runsBlock, 'steps', minIndent(runsBlock));
        if (stepsIdx === -1) return;
        for (const s of splitSteps(blockUnder(runsBlock, stepsIdx))) {
          this.step(s, actionScope, findings, depth + 1);
        }
      }
      return;
    }

    if (run === null) return;
    for (const cmd of splitCommands(run)) {
      this.command(tokenise(cmd), inner, findings, depth + 1);
    }
  }
}

// defaultInputs — a composite action's `inputs.<name>.default` values, so a
// caller that does not override an input still resolves the ref it will use.
function defaultInputs(lines) {
  const i = findKey(lines, 'inputs', 0);
  if (i === -1) return {};
  const block = blockUnder(lines, i);
  const nameIndent = minIndent(block);
  const out = {};
  let name = null;
  for (const line of block) {
    if (isBlank(line)) continue;
    if (indentOf(line) === nameIndent) {
      const m = /^\s*([A-Za-z0-9_-]+):\s*$/.exec(line);
      name = m ? m[1] : null;
      continue;
    }
    const d = /^\s*default:\s*(.*)$/.exec(line);
    if (d && name) out[name] = unquote(d[1]);
  }
  return out;
}

// ---------------------------------------------------------------------------
// The rules.
// ---------------------------------------------------------------------------

export function violationsFor(jobId, findings) {
  const out = [];
  if (findings.acquisitions.length === 0) return out;

  const how = [...new Set(findings.acquisitions)].join(', ');
  const hasAny = findings.logins.length > 0;
  const hasHub = findings.logins.some((l) => dockerHubRegistries.has(l.registry));
  const hasMirror = findings.logins.some((l) => l.registry.startsWith(mirrorHost));

  if (!hasAny) {
    out.push(`${jobId}: acquires images (${how}) with no registry login step — the pull is anonymous`);
    return out;
  }
  const hubRefs = [...findings.refs].filter(isDockerHubRef);
  if (hubRefs.length > 0 && !hasHub) {
    out.push(
      `${jobId}: acquires the Docker Hub image(s) ${hubRefs.sort().join(', ')} but has no Docker Hub ` +
        'login — that pull spends the shared anonymous per-runner-IP quota',
    );
  }
  if (findings.viaSharedPolicy && !hasMirror) {
    out.push(
      `${jobId}: acquires through the shared mirror-first policy but has no ${mirrorHost} login — ` +
        'the mirror is unreadable, so every acquisition silently falls back to Docker Hub',
    );
  }

  // R4 — the acquisitions must run ON the authenticated, mirror-first path, not
  // merely alongside a login step.
  //
  // A login step is necessary and NOT sufficient, and there is a run that proves
  // it: 30701755787 logged in successfully and was refused as UNAUTHENTICATED
  // one second later, because `docker compose`'s own pull path does not carry
  // the credentials the CLI wrote. A gate that stopped at login-presence would
  // have passed that job — a hollow green on the exact failure it is named
  // after. So presence is asked above, and reachability is asked here.
  for (const files of findings.composeMaterialised) {
    const covered =
      files.length === 0
        ? findings.composePolicyFiles.size > 0
        : files.every((f) => findings.composePolicyFiles.has(f));
    if (covered) continue;
    const which = files.length === 0 ? 'its compose model' : files.join(', ');
    out.push(
      `${jobId}: materialises ${which} without pre-pulling it through the shared mirror-first ` +
        "policy — compose's own pull path carries neither the login nor the mirror, so those " +
        'images are fetched anonymously from Docker Hub however the job is authenticated',
    );
  }
  if (!findings.viaSharedPolicy && findings.acquisitions.length > findings.mirrorWrites) {
    out.push(
      `${jobId}: acquires images (${how}) without going through the shared mirror-first policy — ` +
        'a login raises the quota it spends, the mirror is what stops it spending that quota at all',
    );
  }
  return out;
}

// ---------------------------------------------------------------------------
// CLI.
// ---------------------------------------------------------------------------

export function scan({ root = process.cwd(), workflowDir = '.github/workflows' } = {}) {
  const resolver = new Resolver(root);
  // Absolute is honoured so a caller can point the scan at a doctored copy of
  // the workflows while the Justfile / scripts / actions still resolve against
  // the real tree. That is what lets the tests prove this gate FAILS, on the
  // real workflow text, rather than only that it passes.
  const dir = isAbsolute(workflowDir) ? workflowDir : join(root, workflowDir);
  const results = [];
  for (const file of readdirSync(dir).sort()) {
    if (!/\.ya?ml$/.test(file)) continue;
    const { env, jobs } = parseWorkflow(readFileSync(join(dir, file), 'utf8'));
    for (const job of jobs) {
      const findings = newFindings();
      const scope = { ...repoRootVars, ...env, ...job.env };
      for (const step of job.steps) resolver.step(step, scope, findings);
      results.push({ id: `${file}:${job.name}`, findings });
    }
  }
  return results;
}

function main() {
  const root = process.env.REPO_ROOT || process.cwd();
  const workflowDir = process.env.WORKFLOW_DIR || '.github/workflows';
  const results = scan({ root, workflowDir });

  const acquiring = results.filter((r) => r.findings.acquisitions.length > 0);
  const violations = [];
  const unresolved = [];
  for (const { id, findings } of results) {
    violations.push(...violationsFor(id, findings));
    for (const u of findings.unresolved) unresolved.push(`${id}: ${u}`);
  }

  log(`scanned ${results.length} jobs; ${acquiring.length} acquire at least one image`);
  for (const { id, findings } of acquiring) {
    const logins = findings.logins.map((l) => l.registry || 'docker.io').sort().join(' + ') || 'NONE';
    log(`  ${id} — ${[...new Set(findings.acquisitions)].join(', ')} | logins: ${logins}`);
  }

  // A job that no longer acquires anything would make this gate vacuous, and a
  // vacuous gate reports the same green as a satisfied one.
  if (acquiring.length === 0) {
    error('no image-acquiring job was found at all — the detector is not reading the workflows');
    process.exit(1);
  }
  for (const u of unresolved) error(`unreadable reference — ${u}`);
  for (const v of violations) error(v);
  if (unresolved.length > 0 || violations.length > 0) {
    error(
      'every job that acquires an image must acquire it over the authenticated, mirror-first path. ' +
        'Add the Docker Hub and ghcr.io logins to the job above ahead of its first acquisition, and ' +
        'route the acquisition itself through .github/scripts/lib/registry.mjs — for a compose ' +
        'stack that means pre-pulling the model with compose-pull-images.mjs before `up`, because ' +
        "compose's own pull path carries neither the login nor the mirror.",
    );
    process.exit(1);
  }
  notice(`${acquiring.length} image-acquiring jobs, all on the authenticated mirror-first path`);
  process.exit(0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
