// Self-test for agpl-clean.mjs's classification and parsing. The live gate
// runs in ci.yml's `agpl-clean` job over ./cmd/cerberus.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  classifyLicence,
  licenceSummary,
  parseModuleRows,
  readModuleLicence,
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
