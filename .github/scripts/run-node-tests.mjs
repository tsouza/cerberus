// run-node-tests.mjs — run `node --test` over the named suites with the
// Actions runner's workflow-command processing paused for the duration.
//
// A gate script reports through workflow commands (`::error::`,
// `::warning::`, `::notice::`), and its suite drives the script's failure
// paths in-process, so a passing suite prints the very commands the runner
// turns into annotations. Between `::stop-commands::<token>` and `::<token>::`
// the runner reads no workflow command, so those lines stay plain log text.
// The token is random per invocation, so no suite output can resume
// processing early. Pass or fail is the exit code of `node --test`, which this
// script returns unchanged.
//
// Usage: node .github/scripts/run-node-tests.mjs <suite.test.mjs>...
//
// ENV CONTRACT
//   GITHUB_ACTIONS — when `true`, the stop/resume markers are printed;
//                    elsewhere the suites run with no markers.
//
// Exit: the exit code of `node --test`; 1 when no suite is named or the
// child is killed by a signal.

import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { error } from './lib/gh.mjs';

// stopCommandsMarkers returns the line that pauses workflow-command processing
// and the line that resumes it, for one fresh token.
export function stopCommandsMarkers(token = randomUUID()) {
  return { stop: `::stop-commands::${token}`, resume: `::${token}::` };
}

// run executes `node --test <suites>` with inherited stdio, bracketed by the
// markers when it runs under Actions, and returns its exit code.
export function run(suites, { inActions = process.env.GITHUB_ACTIONS === 'true', write = (s) => process.stdout.write(s) } = {}) {
  if (suites.length === 0) {
    error('run-node-tests: no suite named');
    return 1;
  }
  const markers = inActions ? stopCommandsMarkers() : null;
  if (markers) write(`${markers.stop}\n`);
  // NODE_TEST_CONTEXT marks a process the test runner itself started; a
  // `node --test` that inherits it skips every file and exits 0, so the child
  // never sees it.
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT;
  const child = spawnSync(process.execPath, ['--test', ...suites], { stdio: 'inherit', env });
  if (markers) write(`${markers.resume}\n`);
  if (child.error) {
    error(`run-node-tests: could not start node --test: ${child.error.message}`);
    return 1;
  }
  return child.status ?? 1;
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(run(process.argv.slice(2)));
}
