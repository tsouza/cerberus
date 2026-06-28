// agpl-clean.mjs — the provably-clean-build licence gate.
//
// Cerberus ships under Apache-2.0. The grafana/loki and grafana/tempo
// parsers it lowers from are AGPLv3. Linking AGPL code into the Apache
// binary distributed as `cmd/cerberus` is a licence violation, so this
// gate asserts the EXACT set of packages the binary compiles in and
// fails if any AGPL package is reachable from `./cmd/cerberus`.
//
// What it does:
//   1. `go list -deps ./cmd/cerberus` — the transitive package closure
//      the linker actually pulls into the binary (build deps only; test
//      imports are NOT included, which is why the test-only AGPL
//      importers quarantined into the test/oracle nested module never
//      show up here).
//   2. FAIL if any dep import path matches:
//        - ^github.com/grafana/loki/        (all AGPL)
//        - ^github.com/grafana/tempo/  AND NOT ^github.com/grafana/tempo/pkg/tempopb
//          (tempopb is the Apache-2.0 protobuf package — transitively
//           clean — and is the one grafana/tempo subtree the binary may
//           legitimately link.)
//
// Env contract:
//   AGPL_CLEAN_PACKAGE     the `go list` target. Default `./cmd/cerberus`.
//   AGPL_CLEAN_WARN_ONLY   when '1', a violation is reported as a
//                          ::warning:: and the script exits 0 (tracking
//                          mode — non-blocking). Default: a violation is a
//                          ::error:: and the script exits 1 (enforcing mode).
//
// Exit 0 = clean (or warn-only tracking mode); exit 1 = violation in
// enforcing mode. Offending packages are always printed.
//
// NOTE (de-AGPL track): until the loki / tempo PARSER rewrites land,
// production code (internal/api/loki, internal/logql, internal/api/tempo,
// internal/traceql) still imports these AGPL packages, so this gate
// CURRENTLY REPORTS a violation. The CI job runs it with
// AGPL_CLEAN_WARN_ONLY=1 (+ continue-on-error) so it tracks the exposure
// as a warning without blocking. Drop the env (enforcing mode) and add it
// to branch protection once the parser PRs remove the prod imports.

import process from 'node:process';
import { capture, error, notice, warning, log } from './lib/gh.mjs';

const PACKAGE = process.env.AGPL_CLEAN_PACKAGE || './cmd/cerberus';
const WARN_ONLY = process.env.AGPL_CLEAN_WARN_ONLY === '1';

// An import path is an AGPL violation when it sits under grafana/loki,
// or under grafana/tempo EXCEPT the Apache-licensed tempopb subtree.
function isAGPL(pkg) {
  if (pkg.startsWith('github.com/grafana/loki/')) return true;
  if (pkg.startsWith('github.com/grafana/tempo/')) {
    return !pkg.startsWith('github.com/grafana/tempo/pkg/tempopb');
  }
  return false;
}

function main() {
  const res = capture('go', ['list', '-deps', PACKAGE]);
  if (res.status !== 0) {
    error(`agpl-clean: \`go list -deps ${PACKAGE}\` failed`, { title: 'agpl-clean' });
    if (res.stderr) process.stderr.write(res.stderr);
    process.exit(1);
  }

  const deps = res.stdout.split('\n').map((s) => s.trim()).filter(Boolean);
  const offenders = deps.filter(isAGPL).sort();

  if (offenders.length === 0) {
    notice(
      `agpl-clean: ${PACKAGE} links 0 AGPL packages across ${deps.length} build deps — binary is licence-clean`,
      { title: 'agpl-clean' },
    );
    process.exit(0);
  }

  log(`AGPL packages reachable from ${PACKAGE} (${offenders.length}):`);
  for (const pkg of offenders) log(`  ${pkg}`);
  const msg =
    `agpl-clean: ${PACKAGE} links ${offenders.length} AGPL package(s) — see the list above. ` +
    `The Apache-2.0 cerberus binary must not compile AGPLv3 grafana/loki or grafana/tempo code.`;
  if (WARN_ONLY) {
    warning(`${msg} (tracking only — not a merge gate until the parser rewrites land)`, {
      title: 'agpl-clean',
    });
    process.exit(0);
  }
  error(msg, { title: 'agpl-clean' });
  process.exit(1);
}

main();
