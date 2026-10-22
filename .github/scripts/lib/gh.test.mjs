// gh.test.mjs — node:test guard for setOutput() / exportEnv(), the two writers
// of the runner's file-command files ($GITHUB_OUTPUT, $GITHUB_ENV).
//
// Each call runs in a child process because a refused entry ends the process
// with exit 1. The assertions read the file back the way the runner does
// (parseFileCommands below), so "one call, one entry, the value intact" is
// checked against the runner's reading, not against a string shape.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const GH_MODULE = new URL('./gh.mjs', import.meta.url).href;

// The runner's reading of a file-command file (actions/runner
// FileCommandManager): lines split on \r\n, \r or \n; a line whose first `=`
// precedes any `<<` is `name=value`; otherwise `name<<DELIM` takes the
// following lines, joined with \n, up to a line equal to DELIM.
function parseFileCommands(text) {
  const lines = text.split(/\r\n|\r|\n/);
  if (lines.at(-1) === '') lines.pop();
  const entries = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line === '') continue;
    const eq = line.indexOf('=');
    const heredoc = line.indexOf('<<');
    if (eq >= 0 && (heredoc < 0 || eq < heredoc)) {
      entries.push([line.slice(0, eq), line.slice(eq + 1)]);
      continue;
    }
    assert.ok(heredoc > 0, `unparseable line ${JSON.stringify(line)}`);
    const delimiter = line.slice(heredoc + 2);
    const body = [];
    for (i++; i < lines.length && lines[i] !== delimiter; i++) body.push(lines[i]);
    assert.ok(i < lines.length, `unterminated heredoc ${JSON.stringify(delimiter)}`);
    entries.push([line.slice(0, heredoc), body.join('\n')]);
  }
  return entries;
}

// Runs `calls` — [['setOutput', name, value] | ['exportEnv', entries]] — in a
// child with fresh, empty GITHUB_OUTPUT / GITHUB_ENV files (or none, when
// `onRunner` is false) and returns the exit status, stdout and both files.
// `fixedUUID` replaces crypto.randomUUID, to reach the delimiter-collision path.
function runCalls(calls, { onRunner = true, fixedUUID = null } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'gh-file-command-'));
  try {
    const outputFile = join(dir, 'output');
    const envFile = join(dir, 'env');
    writeFileSync(outputFile, '');
    writeFileSync(envFile, '');
    const env = { ...process.env, CALLS: JSON.stringify(calls) };
    delete env.GITHUB_OUTPUT;
    delete env.GITHUB_ENV;
    if (onRunner) {
      env.GITHUB_OUTPUT = outputFile;
      env.GITHUB_ENV = envFile;
    }
    const stub =
      fixedUUID === null
        ? ''
        : "import crypto from 'node:crypto';\nimport { syncBuiltinESMExports } from 'node:module';\n" +
          `crypto.randomUUID = () => ${JSON.stringify(fixedUUID)};\nsyncBuiltinESMExports();\n`;
    const script =
      stub +
      `const gh = await import(${JSON.stringify(GH_MODULE)});\n` +
      'for (const [fn, ...args] of JSON.parse(process.env.CALLS)) gh[fn](...args);\n';
    const r = spawnSync(process.execPath, ['--input-type=module', '-e', script], { env, encoding: 'utf8' });
    return {
      status: r.status,
      stdout: r.stdout,
      output: readFileSync(outputFile, 'utf8'),
      envFile: readFileSync(envFile, 'utf8'),
    };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test('setOutput writes single-line values byte for byte', () => {
  const r = runCalls([
    ['setOutput', 'matrix', '{"include":[{"name":"a b","n":1}]}'],
    ['setOutput', 'commit_message', 'chore(release): cerberus v1.2.3 / chart 0.4.5'],
    ['setOutput', 'pair', 'k=v<<w'],
    ['setOutput', 'issue_number', ''],
    ['setOutput', 'dead_count', 3],
  ]);
  assert.equal(r.status, 0, r.stdout);
  assert.equal(
    r.output,
    'matrix={"include":[{"name":"a b","n":1}]}\n' +
      'commit_message=chore(release): cerberus v1.2.3 / chart 0.4.5\n' +
      'pair=k=v<<w\n' +
      'issue_number=\n' +
      'dead_count=3\n',
  );
});

test('exportEnv writes single-line pairs byte for byte', () => {
  const r = runCalls([
    [
      'exportEnv',
      [
        ['CERBERUS_BIN', '/tmp/bin/cerberus'],
        ['CERBERUS_EXPECT_VERSION', ''],
      ],
    ],
  ]);
  assert.equal(r.status, 0, r.stdout);
  assert.equal(r.envFile, 'CERBERUS_BIN=/tmp/bin/cerberus\nCERBERUS_EXPECT_VERSION=\n');
});

test('a line break in a setOutput value cannot add a second output', () => {
  for (const value of ['1\nis_latest=true', '1\r\nis_latest=true', '1\ris_latest=true', 'a\nb\n', 'x\nmatrix<<EOF\n{}\nEOF']) {
    const r = runCalls([['setOutput', 'issue_number', value]]);
    assert.equal(r.status, 0, r.stdout);
    assert.match(r.output, /^issue_number<<ghadelimiter_[0-9a-f-]{36}\n/);
    assert.deepEqual(parseFileCommands(r.output), [['issue_number', value.replace(/\r\n|\r/g, '\n')]]);
  }
});

test('a line break in an exportEnv value cannot add a second variable', () => {
  const r = runCalls([
    [
      'exportEnv',
      [
        ['RELEASE_IS_LATEST', 'true'],
        ['CERBERUS_IMAGE', 'img\nNODE_OPTIONS=--require=/tmp/x.js'],
      ],
    ],
  ]);
  assert.equal(r.status, 0, r.stdout);
  assert.deepEqual(parseFileCommands(r.envFile), [
    ['RELEASE_IS_LATEST', 'true'],
    ['CERBERUS_IMAGE', 'img\nNODE_OPTIONS=--require=/tmp/x.js'],
  ]);
});

test('each multi-line value gets its own delimiter', () => {
  const r = runCalls([
    ['setOutput', 'a', '1\n2'],
    ['setOutput', 'b', '3\n4'],
  ]);
  const delimiters = [...r.output.matchAll(/^[ab]<<(.*)$/gm)].map((m) => m[1]);
  assert.equal(delimiters.length, 2);
  assert.notEqual(delimiters[0], delimiters[1]);
});

test('a value containing its delimiter is refused, and nothing is written', () => {
  const uuid = '00000000-0000-4000-8000-000000000000';
  const value = `x\nghadelimiter_${uuid}\nNODE_OPTIONS=--require=/tmp/x.js`;
  const out = runCalls([['setOutput', 'reason', value]], { fixedUUID: uuid });
  assert.equal(out.status, 1);
  assert.equal(out.output, '');
  assert.match(out.stdout, /::error::file command: the value of reason contains its own heredoc delimiter/);
  const env = runCalls([['exportEnv', [['OK', 'v'], ['REASON', value]]]], { fixedUUID: uuid });
  assert.equal(env.status, 1);
  assert.equal(env.envFile, '', 'a refused pair leaves the earlier pairs of the call unwritten');
});

test('non-identifier names are refused by both writers', () => {
  for (const name of ['x<<EOF', 'a=b', 'a b', 'a\nb', '1abc', 'a-b', '', 'a.b']) {
    const out = runCalls([['setOutput', name, 'v']]);
    assert.equal(out.status, 1, `setOutput ${JSON.stringify(name)}`);
    assert.equal(out.output, '', `setOutput ${JSON.stringify(name)}`);
    assert.match(out.stdout, /::error::file command: refusing entry name/);
    const env = runCalls([['exportEnv', [['OK', 'v'], [name, 'v']]]]);
    assert.equal(env.status, 1, `exportEnv ${JSON.stringify(name)}`);
    assert.equal(env.envFile, '', `exportEnv ${JSON.stringify(name)}`);
    assert.match(env.stdout, /::error::file command: refusing entry name/);
  }
});

test('off the runner the logged entry has the same shape', () => {
  const ok = runCalls([['setOutput', 'tag', 'v1.2.3']], { onRunner: false });
  assert.equal(ok.status, 0);
  assert.match(ok.stdout, /^tag=v1\.2\.3$/m);
  const multi = runCalls([['setOutput', 'tag', 'v1\nx=y']], { onRunner: false });
  assert.equal(multi.status, 0);
  assert.doesNotMatch(multi.stdout, /^tag=/m);
  assert.match(multi.stdout, /^tag<<ghadelimiter_/m);
  const bad = runCalls([['exportEnv', [['A-B', 'c']]]], { onRunner: false });
  assert.equal(bad.status, 1);
});
