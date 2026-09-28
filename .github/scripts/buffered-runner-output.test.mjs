import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

const diagnosticBytes = 2 * 1024 * 1024;
const finalDiagnostic = 'FINAL_FAILURE_DIAGNOSTIC';

for (const script of ['go-test-fanout.mjs', 'perf-coverage-fanout.mjs']) {
  test(`${script} preserves the final diagnostic when a buffered child fails`, () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'buffered-runner-output-'));
    try {
      const go = path.join(dir, 'go');
      writeFileSync(go, `#!/usr/bin/env node
const fs = require('node:fs');
if (process.argv[2] === 'list') {
  console.log('example.test/package');
} else {
  fs.writeFileSync(1, 'x'.repeat(${diagnosticBytes}) + '\\n${finalDiagnostic}\\n');
  process.exitCode = 1;
}
`);
      chmodSync(go, 0o700);
      const args = script === 'go-test-fanout.mjs' ? [go, 'test', './example'] : [];
      const result = spawnSync(process.execPath, [new URL(script, import.meta.url).pathname, ...args], {
        encoding: 'utf8',
        maxBuffer: diagnosticBytes * 2,
        env: { ...process.env, GO: go, TAGS: 'chdb', COVERPKG: 'example.test/package', LEG_INDEX: '2' },
      });
      assert.equal(result.status, 1, result.stderr + result.stdout.slice(0, 250) + result.stdout.slice(-250));
      assert.ok(result.stdout.includes(finalDiagnostic), 'the failing child diagnostic was truncated');
      assert.match(result.stdout, /::error::.*failed/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
}
