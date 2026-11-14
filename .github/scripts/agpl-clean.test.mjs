// Self-test for agpl-clean.mjs's classification and parsing. The live gate
// runs in ci.yml's `agpl-clean` job over ./cmd/cerberus and every package a
// tracked Dockerfile builds.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import {
  buildArgValues,
  classifyLicence,
  goreleaserTargets,
  isDockerfile,
  licenceSummary,
  parseModuleRows,
  readModuleLicence,
  scanDockerfile,
} from './agpl-clean.mjs';

const AGPL_HEAD = '                    GNU AFFERO GENERAL PUBLIC LICENSE\n                       Version 3, 19 November 2007\n';
const APACHE_HEAD = '\n                                 Apache License\n                           Version 2.0, January 2004\n';

const moduleDir = (files) => {
  const dir = mkdtempSync(join(tmpdir(), 'agpl-clean-'));
  for (const [name, body] of Object.entries(files)) writeFileSync(join(dir, name), body);
  return dir;
};

test('a module is classified by its licence text', () => {
  assert.equal(classifyLicence(AGPL_HEAD), 'AGPL-3.0');
  assert.equal(classifyLicence(APACHE_HEAD), 'other');
  assert.equal(classifyLicence('GNU GENERAL PUBLIC LICENSE\nVersion 3'), 'other');
  // MPL-2.0 §1.12 names the AGPL as a Secondary License; that is not an AGPL module.
  assert.equal(
    classifyLicence('Mozilla Public License, version 2.0\n...the GNU Lesser General Public License, Version 2.1, the GNU Affero\nGeneral Public License, Version 3.0, or any later versions of those\nlicenses.\n... the GNU Affero General Public License, Version 3.0'),
    'other',
  );
  assert.equal(licenceSummary(APACHE_HEAD), 'Apache License');
});

test('the licence file at the module root decides, under any of its usual names', () => {
  assert.deepEqual(readModuleLicence(moduleDir({ LICENSE: AGPL_HEAD })), {
    file: 'LICENSE',
    kind: 'AGPL-3.0',
    summary: 'GNU AFFERO GENERAL PUBLIC LICENSE',
  });
  assert.equal(readModuleLicence(moduleDir({ 'LICENSE.md': APACHE_HEAD })).kind, 'other');
  assert.equal(readModuleLicence(moduleDir({ COPYING: AGPL_HEAD })).kind, 'AGPL-3.0');
  // A subtree licence (Tempo's pkg/tempopb/LICENSE_APACHE2) does not speak for
  // the module: only the root file is read.
  assert.equal(readModuleLicence(moduleDir({ 'LICENSE_APACHE2': APACHE_HEAD, README: 'x' })), null);
  assert.equal(readModuleLicence(moduleDir({ 'go.mod': 'module x\n' })), null);
  assert.equal(readModuleLicence(''), null);
});

test('module rows parse, deduplicate and sort by path', () => {
  const rows = 'b.example/m\tv1.0.0\t/mod/b\na.example/m\tv0.1.0\t/mod/a\nb.example/m\tv1.0.0\t/mod/b\n\n';
  assert.deepEqual(parseModuleRows(rows), [
    { path: 'a.example/m', version: 'v0.1.0', dir: '/mod/a' },
    { path: 'b.example/m', version: 'v1.0.0', dir: '/mod/b' },
  ]);
  assert.throws(() => parseModuleRows('only-a-path\n'), /malformed/);
});

test('tracked Dockerfiles are recognised by name, their ignore files are not', () => {
  assert.ok(isDockerfile('Dockerfile'));
  assert.ok(isDockerfile('Dockerfile.local'));
  assert.ok(isDockerfile('compatibility/tempo/driver/Dockerfile'));
  assert.ok(!isDockerfile('compatibility/tempo/driver/Dockerfile.dockerignore'));
  assert.ok(!isDockerfile('docs/Dockerfile-notes.md'));
});

test('a Dockerfile yields the packages its RUN steps build, across continuation lines', () => {
  const text = [
    'FROM golang AS build',
    'COPY . .',
    'RUN for a in 1 2; do \\',
    '      if go list -tags "${TAGS}" -deps ./cmd/cerberus ./test/e2e/seed/cmd/seed >/dev/null; then break; fi; \\',
    '    done',
    'RUN CGO_ENABLED=0 go build \\',
    '    -trimpath -o /out/driver \\',
    '    ./compatibility/tempo/driver',
    'RUN cp ./not/a/go/package /tmp',
  ].join('\n');
  assert.deepEqual(scanDockerfile(text), {
    packages: ['./cmd/cerberus', './compatibility/tempo/driver', './test/e2e/seed/cmd/seed'],
    bareDownload: false,
    tags: [],
    tagVariables: new Map([['TAGS', '']]),
    unknown: [],
  });
});

test('a bare go mod download is caught, with or without flags or a retry wrapper', () => {
  for (const step of [
    'RUN go mod download',
    'RUN go mod download -x',
    'RUN for a in 1 2; do \\\n  if go mod download; then break; fi; \\\n done',
    'RUN go mod download && go build ./cmd/cerberus',
  ]) {
    assert.equal(scanDockerfile(`FROM golang\n${step}\n`).bareDownload, true, step);
  }
  assert.equal(scanDockerfile('FROM golang\nRUN go mod download github.com/x/y@v1.0.0\n').bareDownload, false);
  assert.equal(scanDockerfile('FROM golang\n# a comment naming go mod download\n').bareDownload, false);
});

test('a Go command argument the scan cannot classify is reported, not skipped', () => {
  for (const [step, word] of [
    ['RUN go build -o /out/x .', '.'],
    ['RUN go build github.com/tsouza/cerberus/cmd/cerberus', 'github.com/tsouza/cerberus/cmd/cerberus'],
    ['RUN go install golang.org/x/tools/cmd/goimports@v0.52.0', 'golang.org/x/tools/cmd/goimports@v0.52.0'],
    ['RUN go build -newflag ./cmd/cerberus', '-newflag'],
  ]) {
    const { unknown } = scanDockerfile(`FROM golang\n${step}\n`);
    assert.equal(unknown.length, 1, step);
    assert.ok(unknown[0].includes(`\`${word}\``), `${step}: ${unknown[0]}`);
  }
  // Flags, their values (quoted, inline or separate) and redirections are not packages.
  const ok = scanDockerfile(
    'FROM golang\nRUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=e2e" -o /out/c -mod=mod ./cmd/cerberus 2>&1 > /dev/null\n',
  );
  assert.deepEqual(ok.unknown, []);
  assert.deepEqual(ok.packages, ['./cmd/cerberus']);
});

test('a Dockerfile -tags value is a literal tag set or one ARG with its default', () => {
  const scan = scanDockerfile(
    [
      'FROM golang',
      'ARG GO_BUILD_TAGS=""',
      'ARG OTHER=netgo',
      'RUN go build -tags "${GO_BUILD_TAGS}" ./cmd/cerberus',
      'RUN go build -tags $OTHER ./cmd/cerberus && go build -tags=osusergo,netgo ./cmd/cerberus',
    ].join('\n'),
  );
  assert.deepEqual(scan.tags, ['osusergo,netgo']);
  assert.deepEqual([...scan.tagVariables], [['GO_BUILD_TAGS', ''], ['OTHER', 'netgo']]);
});

test('a --build-arg tag source resolves through env_var_or_default to every value the tree assigns', () => {
  const just = [
    'CERBERUS_BUILD_TAGS := env_var_or_default("CERBERUS_BUILD_TAGS", "")',
    'e2e-up:',
    '    docker build --build-arg GO_BUILD_TAGS="{{CERBERUS_BUILD_TAGS}}" -f Dockerfile.local .',
  ].join('\n');
  const workflow = 'jobs:\n  chaos:\n    env:\n      CERBERUS_BUILD_TAGS: chaos_sleep\n';
  assert.deepEqual(buildArgValues('GO_BUILD_TAGS', [just], [just, workflow]), { values: ['', 'chaos_sleep'], unknown: [] });
  assert.deepEqual(buildArgValues('GO_BUILD_TAGS', ['x:\n    docker build --build-arg GO_BUILD_TAGS=race .'], []), {
    values: ['race'],
    unknown: [],
  });
  const opaque = buildArgValues('GO_BUILD_TAGS', ['T := `cat tags`\nx:\n    docker build --build-arg GO_BUILD_TAGS="{{T}}" .'], []);
  assert.equal(opaque.unknown.length, 1);
});

test('the goreleaser build yields its GOOS x GOARCH pairs, and an unmodelled shape is reported', () => {
  const yml = 'builds:\n  - id: cerberus\n    goos:\n      - linux\n      - darwin\n    goarch:\n      - amd64\n      - arm64\n\narchives:\n  - id: x\n';
  const { platforms, tags, unknown } = goreleaserTargets(yml);
  assert.deepEqual(platforms, [
    { goos: 'linux', goarch: 'amd64' },
    { goos: 'linux', goarch: 'arm64' },
    { goos: 'darwin', goarch: 'amd64' },
    { goos: 'darwin', goarch: 'arm64' },
  ]);
  assert.deepEqual(tags, []);
  assert.deepEqual(unknown, []);
  assert.deepEqual(goreleaserTargets(yml.replace('    goos:', '    tags:\n      - netgo\n    goos:')).tags, ['netgo']);
  assert.equal(goreleaserTargets(yml.replace('    goos:', '    ignore:\n      - goos: darwin\n    goos:')).unknown.length, 1);
  assert.equal(goreleaserTargets(yml.replace('\narchives', '  - id: other\n    goos:\n      - linux\n\narchives')).unknown.length, 1);
});

// gateFixture builds a throwaway module whose binary imports an Apache module
// everywhere, an AGPL module only on darwin and another only under the
// chaos_sleep tag, with the
// release, Dockerfile, recipe and workflow files that name those
// configurations, and returns its directory.
function gateFixture() {
  const dir = mkdtempSync(join(tmpdir(), 'agpl-clean-gate-'));
  const write = (rel, body) => {
    mkdirSync(join(dir, dirname(rel)), { recursive: true });
    writeFileSync(join(dir, rel), body);
  };
  for (const [name, licence] of [['darwinonly', AGPL_HEAD], ['chaosonly', AGPL_HEAD], ['clean', APACHE_HEAD]]) {
    write(`agpl/${name}/go.mod`, `module example.test/${name}\n\ngo 1.22\n`);
    write(`agpl/${name}/${name}.go`, `package ${name}\n\nconst X = 1\n`);
    write(`agpl/${name}/LICENSE`, licence);
  }
  write(
    'go.mod',
    'module example.test/app\n\ngo 1.22\n\nrequire (\n\texample.test/darwinonly v0.0.0\n\texample.test/chaosonly v0.0.0\n\texample.test/clean v0.0.0\n)\n\n' +
      'replace (\n\texample.test/darwinonly => ./agpl/darwinonly\n\texample.test/chaosonly => ./agpl/chaosonly\n' +
      '\texample.test/clean => ./agpl/clean\n)\n',
  );
  write('cmd/cerberus/main.go', 'package main\n\nimport _ "example.test/clean"\n\nfunc main() {}\n');
  write('cmd/cerberus/darwin.go', '//go:build darwin\n\npackage main\n\nimport _ "example.test/darwinonly"\n');
  write('cmd/cerberus/chaos.go', '//go:build chaos_sleep\n\npackage main\n\nimport _ "example.test/chaosonly"\n');
  write('.goreleaser.yml', 'builds:\n  - id: cerberus\n    goos:\n      - linux\n      - darwin\n    goarch:\n      - amd64\n');
  write('Dockerfile.local', 'FROM golang\nARG GO_BUILD_TAGS=""\nRUN go build -tags "${GO_BUILD_TAGS}" -o /out/cerberus ./cmd/cerberus\n');
  write('Justfile', 'TAGS := env_var_or_default("CERBERUS_BUILD_TAGS", "")\nup:\n    docker build --build-arg GO_BUILD_TAGS="{{TAGS}}" .\n');
  write('.github/workflows/e2e.yml', 'jobs:\n  chaos:\n    env:\n      CERBERUS_BUILD_TAGS: chaos_sleep\n');
  const git = (args) => assert.equal(spawnSync('git', args, { cwd: dir }).status, 0, `git ${args.join(' ')}`);
  git(['init', '-q']);
  git(['add', '.']);
  return dir;
}

test('the gate scans every release platform and every build-tag set the tree builds with', () => {
  const dir = gateFixture();
  const res = spawnSync(process.execPath, [fileURLToPath(new URL('./agpl-clean.mjs', import.meta.url))], {
    cwd: dir,
    encoding: 'utf8',
    env: { ...process.env, GOWORK: 'off', GOFLAGS: '-mod=mod', GOPROXY: 'off', GOTOOLCHAIN: 'local' },
  });
  assert.equal(res.status, 1, res.stdout + res.stderr);
  assert.match(res.stdout, /AGPL-3\.0 module example\.test\/darwinonly/);
  assert.match(res.stdout, /AGPL-3\.0 module example\.test\/chaosonly/);
});
