// agpl-clean.mjs — the provably-clean-build licence gate.
//
// Cerberus ships under Apache-2.0. Its LogQL and TraceQL parsers are
// in-house, independent Apache reimplementations (internal/logql/lsyntax,
// internal/logql/logpattern, internal/drain, internal/traceql/ast); a small
// set of error-message strings is deliberately matched to upstream for wire
// compatibility (documented at each site). PromQL uses the upstream Apache
// prometheus parser. The Tempo wire types are an in-house copy of Tempo's
// Apache-licensed pkg/tempopb (internal/thirdparty/tempopb). The AGPLv3
// grafana/loki and grafana/tempo modules survive only as test-only oracles
// (the agpl_oracle-tagged tests and the compatibility harnesses), never in
// the shipped binary or its module graph.
//
// What it scans: ./cmd/cerberus, plus every Go package a tracked Dockerfile
// builds — read from each Dockerfile's `go build` / `go list` / `go install`
// RUN steps — because building one of those images (the quickstart's
// `docker compose build` among them) fetches that package's modules onto the
// builder's machine. A Dockerfile that runs a bare `go mod download` fetches
// every requirement in go.mod, the AGPL test-only oracles' modules included,
// so the gate fails on that too: an image build fetches its packages' graphs
// and nothing more.
//
// It scans every build configuration those binaries ship or are built in:
// each GOOS/GOARCH pair the cerberus build of .goreleaser.yml targets, crossed
// with each build-tag set the tree builds them with — the untagged build, the
// `-tags` literals of a Dockerfile's Go commands, and, for a `-tags "${ARG}"`
// Dockerfile variable, the ARG's default plus every value a just recipe passes
// it through `--build-arg` (a `{{VAR}}` read from an env var with
// env_var_or_default, and each value a workflow or recipe assigns that env
// var). A file-specific import (`_linux.go`, `//go:build chaos_sleep`) is
// thereby scanned under the configuration that compiles it. A Go command
// argument the scan cannot classify — a package that is not a `./` path, an
// unknown flag, a `--build-arg` or tag source of another shape — fails the
// gate instead of being skipped.
//
// What it does: `go list -deps <packages>` under each configuration gives the
// transitive package closure the linker pulls into those binaries (build deps
// only; test imports are not included), and with it every module that
// provides one of those packages — exactly the modules an empty-cache build
// downloads. Each such module must carry a licence file at its root, and the
// gate FAILS if any of them is the GNU Affero GPL. The licence is read from
// the module's own files, so a module is caught by what it is licensed under,
// not by its name: an Apache file inside an AGPL module (Tempo's
// pkg/tempopb/LICENSE_APACHE2) does not make the module Apache. The module
// roster and each module's licence are printed on every run.
//
// Env contract:
//   AGPL_CLEAN_PACKAGE     overrides the package set with one `go list`
//                          target, and skips the Dockerfile scan. Default
//                          unset: ./cmd/cerberus plus the Dockerfile-built
//                          packages.
//
// Exit 0 = clean; exit 1 = an AGPL module is in the binary's graph, a module's licence cannot be read, or `go list` fails. Offenders are
// always printed. This gate is ENFORCING and is a required status check on
// `main`.

import { existsSync, readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { assertSafeArg, capture, error, notice, log } from './lib/gh.mjs';

// The binary the release ships, scanned whatever the Dockerfiles say.
export const BINARY_PACKAGE = './cmd/cerberus';
// A tracked Dockerfile: `Dockerfile` or `Dockerfile.<variant>`, never its
// `.dockerignore`.
const DOCKERFILE = /(^|\/)Dockerfile(\.[^/]+)?$/;
const DOCKERIGNORE = /\.dockerignore$/;
// The `go` subcommands that fetch the modules of the packages they name.
const GO_PACKAGE_SUBCOMMAND = /^(build|list|install)$/;
// A package argument: a relative path into the module.
const PACKAGE_ARG = /^\.\/[\w./-]+$/;
// The go build / list / install flags that take the next token as their value
// when written without `=`.
const VALUE_FLAGS = new Set([
  '-o', '-C', '-p', '-asmflags', '-buildmode', '-compiler', '-gccgoflags', '-gcflags', '-installsuffix',
  '-ldflags', '-mod', '-modfile', '-overlay', '-pgo', '-pkgdir', '-tags', '-toolexec', '-coverpkg',
  '-covermode', '-f', '-reuse',
]);
// The go build / list / install flags that take no value.
const BOOL_FLAGS = new Set([
  '-a', '-n', '-v', '-x', '-race', '-msan', '-asan', '-cover', '-trimpath', '-linkshared', '-work',
  '-buildvcs', '-modcacherw', '-deps', '-e', '-m', '-u', '-json', '-find', '-test', '-export', '-compiled',
  '-retracted', '-versions',
]);
// A shell redirection token, and one whose target is the next token.
const REDIRECTION = /^(\d*|&)(>>?|<)(&\d+)?/;
const BARE_REDIRECTION = /^(\d*|&)(>>?|<)$/;
// A Dockerfile `-tags` value that is one ARG, `${NAME}` or `$NAME`.
const TAG_VARIABLE = /^\$\{?(\w+)\}?$/;
// The untagged build every binary is also built as.
export const UNTAGGED = '';
// `go mod download` with no module argument (flags aside): every requirement.
const BARE_MOD_DOWNLOAD = /\bgo\s+mod\s+download((\s+-\S+)*)\s*($|;|&|\||\))/m;

export const LICENCE_FILE = /^(licen[cs]e|copying)(\.(md|txt))?$/i;
// The AGPL's own title line, upper case as the licence text prints it. Case
// matters: the MPL-2.0 names the "GNU Affero General Public License" among its
// Secondary Licenses, and an MPL module is not an AGPL one.
const AGPL_MARKER = /GNU AFFERO GENERAL PUBLIC LICENSE/;
const MODULE_ROW_FIELDS = 3;

// classifyLicence — 'AGPL-3.0' when the text is the GNU Affero GPL, else
// 'other'. Only the AGPL verdict gates; the rest is printed for the record.
export function classifyLicence(text) {
  return AGPL_MARKER.test(text) ? 'AGPL-3.0' : 'other';
}

// licenceSummary — the first non-empty line of a licence text, trimmed, for
// the printed roster.
export function licenceSummary(text) {
  return text
    .split('\n')
    .map((l) => l.trim())
    .find((l) => l !== '') ?? '';
}

// parseModuleRows — the `path<TAB>version<TAB>dir` rows main() asks go list
// for, deduplicated by module path, in path order.
export function parseModuleRows(text) {
  const modules = new Map();
  for (const raw of text.split('\n')) {
    if (raw.trim() === '') continue;
    const fields = raw.split('\t');
    if (fields.length !== MODULE_ROW_FIELDS) throw new Error(`malformed go list row: ${JSON.stringify(raw)}`);
    const [modPath, version, dir] = fields;
    modules.set(modPath, { path: modPath, version, dir });
  }
  return [...modules.values()].sort((a, b) => a.path.localeCompare(b.path));
}

// readModuleLicence — { file, kind, summary } for the licence at a module's
// root, or null when the module directory holds none.
export function readModuleLicence(dir) {
  if (!dir || !existsSync(dir)) return null;
  const file = readdirSync(dir)
    .filter((name) => LICENCE_FILE.test(name))
    .sort()[0];
  if (!file) return null;
  const text = readFileSync(path.join(dir, file), 'utf8');
  return { file, kind: classifyLicence(text), summary: licenceSummary(text) };
}

// isDockerfile — whether a tracked path is a Dockerfile.
export function isDockerfile(file) {
  return DOCKERFILE.test(file) && !DOCKERIGNORE.test(file);
}

// runSteps — the Dockerfile's RUN instructions, continuation lines joined.
function runSteps(text) {
  return text
    .replace(/\\\r?\n/g, ' ')
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => /^RUN\b/i.test(l));
}

// shellTokens — a command's words, quotes removed: enough of the shell's
// word splitting for the Go commands a Dockerfile RUN step writes.
export function shellTokens(command) {
  const tokens = [];
  let word = null;
  let quote = null;
  for (const ch of command) {
    if (quote) {
      if (ch === quote) quote = null;
      else word += ch;
    } else if (ch === '"' || ch === "'") {
      quote = ch;
      word ??= '';
    } else if (/\s/.test(ch)) {
      if (word !== null) tokens.push(word);
      word = null;
    } else {
      word = (word ?? '') + ch;
    }
  }
  if (word !== null) tokens.push(word);
  return tokens;
}

// goCommandArgs — the packages, `-tags` values and unclassifiable arguments
// of one Go command's words after `go build|list|install`.
export function goCommandArgs(words) {
  const packages = [];
  const tags = [];
  const unknown = [];
  for (let i = 0; i < words.length; i++) {
    const word = words[i];
    if (BARE_REDIRECTION.test(word)) {
      i++;
      continue;
    }
    if (REDIRECTION.test(word)) continue;
    if (word.startsWith('-')) {
      const [flag, inline] = word.replace(/^--/, '-').split(/=(.*)/s);
      if (VALUE_FLAGS.has(flag)) {
        const value = inline ?? words[++i];
        if (value === undefined) unknown.push(`${word} (no value)`);
        else if (flag === '-tags') tags.push(value);
      } else if (!BOOL_FLAGS.has(flag)) {
        unknown.push(word);
      }
      continue;
    }
    if (PACKAGE_ARG.test(word)) packages.push(word);
    else unknown.push(word);
  }
  return { packages, tags, unknown };
}

// scanDockerfile — for one Dockerfile: the Go package paths its RUN steps
// build, list or install; whether any RUN step is a bare `go mod download`;
// the `-tags` values of those commands, literal (`tags`) or one ARG
// (`tagVariables`, each with its ARG default); and the arguments the scan
// could not classify (`unknown`).
export function scanDockerfile(text) {
  const packages = new Set();
  const tags = new Set();
  const tagVariables = new Map();
  const unknown = [];
  const argDefaults = new Map();
  for (const line of text.split('\n')) {
    const arg = /^\s*ARG\s+(\w+)(?:=(.*))?$/i.exec(line);
    if (arg) argDefaults.set(arg[1], shellTokens(arg[2] ?? '').join(' '));
  }
  let bareDownload = false;
  for (const step of runSteps(text)) {
    if (BARE_MOD_DOWNLOAD.test(step)) bareDownload = true;
    for (const command of step.split(/;|&&|\|\|/)) {
      const words = shellTokens(command);
      const at = words.findIndex((w, i) => w === 'go' && GO_PACKAGE_SUBCOMMAND.test(words[i + 1] ?? ''));
      if (at === -1) continue;
      const found = goCommandArgs(words.slice(at + 2));
      for (const pkg of found.packages) packages.add(pkg);
      for (const value of found.tags) {
        const variable = TAG_VARIABLE.exec(value);
        if (!variable) tags.add(value);
        else tagVariables.set(variable[1], argDefaults.get(variable[1]) ?? UNTAGGED);
      }
      for (const word of found.unknown) unknown.push(`\`${word}\` in \`${words.slice(at).join(' ')}\``);
    }
  }
  return { packages: [...packages].sort(), bareDownload, tags: [...tags].sort(), tagVariables, unknown };
}

// buildArgValues — the values the just recipes pass a Dockerfile ARG through
// `--build-arg NAME=<value>`: a literal, or a `{{VAR}}` just variable read
// from an env var with env_var_or_default, which contributes its default and
// every value `assignments` (workflow and recipe text) gives that env var.
// Any other form is returned in `unknown`.
export function buildArgValues(name, justTexts, assignmentTexts) {
  const values = new Set();
  const unknown = [];
  const justVariables = new Map();
  for (const text of justTexts) {
    for (const m of text.matchAll(/^(\w+)\s*:=\s*(.*)$/gm)) justVariables.set(m[1], m[2].trim());
  }
  for (const text of justTexts) {
    for (const m of text.matchAll(new RegExp(`--build-arg\\s+${name}=("[^"]*"|'[^']*'|\\S+)`, 'g'))) {
      const value = m[1].replace(/^["']|["']$/g, '');
      const ref = /^\{\{\s*(\w+)\s*\}\}$/.exec(value);
      if (!ref) {
        if (value.includes('{{') || value.includes('$')) unknown.push(`--build-arg ${name}=${m[1]}`);
        else values.add(value);
        continue;
      }
      const source = /^env_var_or_default\(\s*"(\w+)"\s*,\s*"([^"]*)"\s*\)$/.exec(justVariables.get(ref[1]) ?? '');
      if (!source) {
        unknown.push(`--build-arg ${name}=${m[1]} (just variable ${ref[1]} is not env_var_or_default("ENV", "default"))`);
        continue;
      }
      values.add(source[2]);
      for (const textOf of assignmentTexts) {
        for (const a of textOf.matchAll(new RegExp(`\\b${source[1]}\\s*[:=]\\s*(["']?)([^"'\\s]*)\\1\\s*$`, 'gm'))) {
          if (!a[2].includes('{{') && !a[2].includes('$')) values.add(a[2]);
        }
      }
    }
  }
  return { values: [...values].sort(), unknown };
}

// goreleaserTargets — the GOOS/GOARCH pairs and the `tags` of the
// .goreleaser.yml build that ships the cerberus binary. A build list the
// parser does not model (more than one build, `targets:` / `ignore:`
// overrides) is returned in `unknown` rather than guessed.
export function goreleaserTargets(text) {
  const unknown = [];
  const list = (key) => {
    const m = new RegExp(`^(\\s+)${key}:\\s*\\n((?:\\1\\s*- .*\\n?)+)`, 'm').exec(text);
    return m ? [...m[2].matchAll(/-\s*(\S+)/g)].map((x) => x[1].replace(/^["']|["']$/g, '')) : [];
  };
  const buildsBlock = /^builds:\s*\n((?:[ \t].*\n|\n)*)/m.exec(text)?.[1] ?? '';
  if ((buildsBlock.match(/^ {2}- /gm) ?? []).length !== 1) unknown.push('.goreleaser.yml must declare exactly one build');
  for (const key of ['targets', 'ignore', 'goarm', 'goamd64']) {
    if (new RegExp(`^\\s+${key}:`, 'm').test(buildsBlock)) unknown.push(`.goreleaser.yml build sets \`${key}:\`, which this scan does not model`);
  }
  const goos = list('goos');
  const goarch = list('goarch');
  if (goos.length === 0 || goarch.length === 0) unknown.push('.goreleaser.yml build names no goos/goarch list');
  const platforms = goos.flatMap((os) => goarch.map((arch) => ({ goos: os, goarch: arch })));
  const tags = /^\s+tags:/m.test(buildsBlock) ? [list('tags').join(',')] : [];
  return { platforms, tags, unknown };
}

function goList(args, env) {
  const res = capture('go', ['list', ...args], { env: { ...process.env, ...env } });
  if (res.status !== 0) {
    error(`agpl-clean: \`go list ${args.join(' ')}\` (${JSON.stringify(env)}) failed`, { title: 'agpl-clean' });
    if (res.stderr) process.stderr.write(res.stderr);
    process.exit(1);
  }
  return res.stdout;
}

function trackedFiles(pattern) {
  const listed = capture('git', ['ls-files']);
  if (listed.status !== 0) {
    error('agpl-clean: `git ls-files` failed', { title: 'agpl-clean' });
    process.exit(1);
  }
  return listed.stdout.split('\n').filter((f) => pattern(f));
}

function main() {
  const problems = [];
  const override = process.env.AGPL_CLEAN_PACKAGE;
  const targets = new Set([assertSafeArg(override || BINARY_PACKAGE, 'AGPL_CLEAN_PACKAGE')]);
  const tagSets = new Set([UNTAGGED]);
  const release = goreleaserTargets(readFileSync('.goreleaser.yml', 'utf8'));
  problems.push(...release.unknown);
  for (const t of release.tags) tagSets.add(t);
  if (!override) {
    const dockerfiles = trackedFiles(isDockerfile);
    if (dockerfiles.length === 0) problems.push('no tracked Dockerfile found; the image scan inspected nothing');
    const justTexts = ['Justfile', ...trackedFiles((f) => /^just\/.*\.just$/.test(f))].map((f) => readFileSync(f, 'utf8'));
    const assignmentTexts = [
      ...justTexts,
      ...trackedFiles((f) => /^\.github\/workflows\/.*\.ya?ml$/.test(f)).map((f) => readFileSync(f, 'utf8')),
    ];
    for (const file of dockerfiles) {
      const scan = scanDockerfile(readFileSync(file, 'utf8'));
      log(`${file}: builds ${scan.packages.length === 0 ? '(no Go package)' : scan.packages.join(' ')}`);
      if (scan.bareDownload) {
        problems.push(
          `${file} runs a bare \`go mod download\`, which fetches every go.mod requirement (the AGPL ` +
            'test-only modules included); fetch with `go list -deps <the built packages>` or let `go build` fetch',
        );
      }
      for (const word of scan.unknown) problems.push(`${file}: unrecognised Go command argument ${word}`);
      for (const pkg of scan.packages) targets.add(assertSafeArg(pkg, file));
      for (const t of scan.tags) tagSets.add(t);
      for (const [name, fallback] of scan.tagVariables) {
        tagSets.add(fallback);
        const passed = buildArgValues(name, justTexts, assignmentTexts);
        for (const v of passed.values) tagSets.add(v);
        for (const u of passed.unknown) problems.push(`${file}: unrecognised build-tag source ${u}`);
      }
    }
  }
  const target = [...targets].join(' ');
  const configs = release.platforms.flatMap((p) => [...tagSets].sort().map((tags) => ({ ...p, tags })));
  log(`Build configurations (${configs.length}): ${configs.map((c) => `${c.goos}/${c.goarch}[${c.tags}]`).join(' ')}`);

  const moduleTemplate =
    '{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t' +
    '{{if .Replace}}{{.Replace.Dir}}{{else}}{{.Dir}}{{end}}{{end}}{{end}}';
  const rows = configs
    .map((c) =>
      goList(['-deps', '-tags', c.tags, '-f', moduleTemplate, ...targets], { GOOS: c.goos, GOARCH: c.goarch, CGO_ENABLED: '0' }),
    )
    .join('\n');
  const modules = parseModuleRows(rows);
  if (modules.length === 0) problems.push(`no third-party module found in ${target}'s graph; the scan inspected nothing`);

  log(`Modules in ${target}'s build graph (${modules.length}):`);
  for (const mod of modules) {
    const licence = readModuleLicence(mod.dir);
    if (licence === null) {
      log(`  ${mod.path} ${mod.version}  (no licence file)`);
      problems.push(`module ${mod.path} ${mod.version} has no licence file at its root (${mod.dir})`);
      continue;
    }
    const flag = licence.kind === 'AGPL-3.0' ? '  [AGPL-3.0]' : '';
    log(`  ${mod.path} ${mod.version}  ${licence.file}: ${licence.summary}${flag}`);
    if (licence.kind === 'AGPL-3.0') problems.push(`AGPL-3.0 module ${mod.path} ${mod.version} (${licence.file})`);
  }

  if (problems.length === 0) {
    notice(
      `agpl-clean: ${target} builds from ${modules.length} third-party modules, none AGPL-licensed, and no ` +
        'Dockerfile fetches beyond its build graph',
      { title: 'agpl-clean' },
    );
    return;
  }
  log(`Problems (${problems.length}):`);
  for (const p of problems) log(`  ${p}`);
  error(
    `agpl-clean: ${target}'s build graph is not licence-clean (${problems.length} problem(s), listed above). ` +
      'Neither the Apache-2.0 binary nor an image build may compile AGPLv3 code or fetch an AGPLv3 module.',
    { title: 'agpl-clean' },
  );
  process.exit(1);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) main();
