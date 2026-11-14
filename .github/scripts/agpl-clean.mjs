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
// What it does: `go list -deps <packages>` gives the transitive package
// closure the linker pulls into those binaries (build deps only; test imports
// are not included), and with it every module that provides one of those
// packages — exactly the modules an empty-cache build downloads. Each such module must carry a licence file at its root, and the
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
// A Go command that fetches the modules of the packages it names.
const GO_PACKAGE_COMMAND = /\bgo\s+(build|list|install)\b/;
// A package argument: a relative path into the module.
const PACKAGE_ARG = /^\.\/[\w./-]+$/;
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

// scanDockerfile — { packages, bareDownload } for one Dockerfile: the Go
// package paths its RUN steps build, list or install, and whether any RUN step
// is a bare `go mod download`.
export function scanDockerfile(text) {
  const packages = new Set();
  let bareDownload = false;
  for (const step of runSteps(text)) {
    if (BARE_MOD_DOWNLOAD.test(step)) bareDownload = true;
    for (const command of step.split(/;|&&|\|\|/)) {
      if (!GO_PACKAGE_COMMAND.test(command)) continue;
      for (const token of command.split(/\s+/)) {
        const arg = token.replace(/^["']|["']$/g, '');
        if (PACKAGE_ARG.test(arg)) packages.add(arg);
      }
    }
  }
  return { packages: [...packages].sort(), bareDownload };
}

function goList(args) {
  const res = capture('go', ['list', ...args]);
  if (res.status !== 0) {
    error(`agpl-clean: \`go list ${args.join(' ')}\` failed`, { title: 'agpl-clean' });
    if (res.stderr) process.stderr.write(res.stderr);
    process.exit(1);
  }
  return res.stdout;
}

function main() {
  const problems = [];
  const override = process.env.AGPL_CLEAN_PACKAGE;
  const targets = new Set([assertSafeArg(override || BINARY_PACKAGE, 'AGPL_CLEAN_PACKAGE')]);
  if (!override) {
    const listed = capture('git', ['ls-files']);
    if (listed.status !== 0) {
      error('agpl-clean: `git ls-files` failed', { title: 'agpl-clean' });
      process.exit(1);
    }
    const dockerfiles = listed.stdout.split('\n').filter(isDockerfile);
    if (dockerfiles.length === 0) problems.push('no tracked Dockerfile found; the image scan inspected nothing');
    for (const file of dockerfiles) {
      const { packages, bareDownload } = scanDockerfile(readFileSync(file, 'utf8'));
      log(`${file}: builds ${packages.length === 0 ? '(no Go package)' : packages.join(' ')}`);
      if (bareDownload) {
        problems.push(
          `${file} runs a bare \`go mod download\`, which fetches every go.mod requirement (the AGPL ` +
            'test-only modules included); fetch with `go list -deps <the built packages>` or let `go build` fetch',
        );
      }
      for (const pkg of packages) targets.add(assertSafeArg(pkg, file));
    }
  }
  const target = [...targets].join(' ');

  const moduleTemplate =
    '{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t' +
    '{{if .Replace}}{{.Replace.Dir}}{{else}}{{.Dir}}{{end}}{{end}}{{end}}';
  const modules = parseModuleRows(goList(['-deps', '-f', moduleTemplate, ...targets]));
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
