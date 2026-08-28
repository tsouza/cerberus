// go-module-fetch — run one FETCH-ONLY Go command under the repository's
// single transient-failure retry policy.
//
// WHY THIS EXISTS. Every Go job in this repo pulls its dependency graph from
// proxy.golang.org, and the Go module resolver does not retry past a dropped
// HTTP/2 frame: one `stream error: stream ID 523; INTERNAL_ERROR; received from
// peer` anywhere in a multi-minute, ~340-module download stream kills the job.
// That killed seven jobs in one afternoon (runs 33080257221, 33084966918,
// 33093118010), and the reason the ambient flakiness became visible at all was
// a POISONED module cache: a Go-less job saved a 7,616-byte archive under the
// go.sum-keyed primary key on refs/heads/main, so every job in the repo
// restored nothing and re-fetched the whole graph. The cache half is fixed
// structurally by `.github/actions/setup-go`, which calls this module as its
// unconditional warm step; this module is the residual cover for the run that
// is GENUINELY cold — the first one after every go.sum bump, which is exactly
// when the whole graph has to come down the wire.
//
// WHY IT CANNOT MASK A REAL FAILURE. Three independent layers, each asserted by
// go-module-fetch.test.mjs against the ATTEMPT COUNT rather than the exit code
// — a wrapper that retried a real error and then failed would look identical on
// the exit code alone.
//
//   (a) SCOPE. Only a fetch-only command may be wrapped at all, and that is
//       enforced here rather than trusted to the call sites: `isFetchOnly()`
//       accepts `go mod download` and `go install <pkg>@<version>` and refuses
//       everything else. `go test`, `go list`, `go build`, `just <recipe>` and
//       `gremlins unleash` are therefore unwrappable by construction and still
//       fail on their first attempt — they simply stop needing the network,
//       because the warm step has already fetched what they read.
//   (b) CLASSIFY. An attempt is retried only when the merged output matches
//       lib/registry.mjs's `isTransientRegistryFailure()`, whose pattern list
//       already carries the exact Go-proxy signatures (`http2: stream error`,
//       `INTERNAL_ERROR; received from peer`, `connection reset by peer`, `i/o
//       timeout`, `TLS handshake timeout`, a proxy.golang.org-scoped 403). A Go
//       compile error — the one thing `go install` can produce that
//       `go mod download` cannot — matches nothing on that list and exits after
//       one attempt.
//   (c) NEVER-RETRY, evaluated FIRST and winning outright even when the output
//       also names a transport fault. This mirrors the rate-limit rule in
//       lib/registry.mjs, and for the same reason: retrying a `SECURITY ERROR`
//       or a checksum mismatch is the one behaviour that would be actively
//       dangerous, because a second attempt against a different proxy replica
//       is precisely how a poisoned module would eventually be accepted.
//
// The policy itself is IMPORTED, not re-derived. lib/registry.mjs owns the
// attempt budget, the linear backoff and the transport classification for every
// network fetch this CI performs; a second helper would be a second place for
// the answer to drift.
//
// USAGE
//   node .github/scripts/go-module-fetch.mjs
//       Warm mode: `go mod download` in the working directory. No arguments,
//       because for a `go 1.17`-or-later module that downloads exactly the
//       modules go.mod explicitly requires — which includes every `// indirect`
//       requirement, so the three modules whose fetch failed in the runs above
//       (go-sqlbuilder, autoexport, stdoutlog) are all covered. `all` would add
//       the test dependencies OF dependencies, which nothing here builds.
//
//   node .github/scripts/go-module-fetch.mjs -- go install <pkg>@<version>
//       Passthrough mode, for a tool that is not in go.mod and so is not
//       covered by `go mod download` at all.
//
// ENV CONTRACT
//   GO_FETCH_BACKOFF_STEP_SECONDS — linear backoff step in seconds; attempt N
//                                   sleeps N × this. Default below.
//
// Exit: the wrapped command's status once it has succeeded or run out of
// attempts; 1 when the command handed in is not fetch-only.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { capture, error, log, notice } from './lib/gh.mjs';
import {
  isTransientRegistryFailure,
  readBackoffStepSeconds,
  registryAttempts,
  sleepSeconds,
} from './lib/registry.mjs';

// Attempt N sleeps N × this. Shorter than the image-build wrapper's step
// because a module fetch has no `FROM`-resolution tail behind it: a proxy blip
// clears in seconds or is not a blip. Overridable so a lane that wants to ride
// out a longer incident can, without a second policy.
export const goFetchBackoffStepSeconds = 3;
const goFetchBackoffStepEnvVar = 'GO_FETCH_BACKOFF_STEP_SECONDS';

// The command prefixes this wrapper will run — layer (a) above, written as a
// property of the wrapper's INPUT rather than as trust in its call sites.
//
// This is not an exemption list: it is the opposite of one. It does not excuse
// any failure; it narrows what is eligible to be retried at all, so that a
// command whose failure carries real information can never reach the loop.
const fetchOnlyCommandPrefixes = [
  ['go', 'mod', 'download'],
  ['go', 'install'],
];

export function isFetchOnly(argv) {
  return fetchOnlyCommandPrefixes.some((prefix) => prefix.every((token, i) => argv[i] === token));
}

// The failures a second attempt cannot fix and must not be given.
//
//   * `SECURITY ERROR` / `checksum mismatch` — the checksum database and the
//     bytes disagree. Retrying is how a poisoned module eventually gets
//     accepted, so this class wins over every transport signature.
//   * `404 Not Found` / `410 Gone` / `unknown revision` / `invalid version` /
//     `no matching versions` — the proxy answered, definitively, that the
//     version does not exist. A retyped tag is the fix; time is not.
//   * `missing go.sum entry` — the module graph in the tree is inconsistent
//     with go.sum. `go mod tidy` is the fix; the network is not involved.
const goFetchNeverRetryablePatterns = [
  /SECURITY ERROR/,
  /checksum mismatch/i,
  /\b404 Not Found\b/i,
  /\b410 Gone\b/i,
  /unknown revision/i,
  /invalid version/i,
  /no matching versions/i,
  /missing go\.sum entry/i,
];

export function goFetchNeverRetryable(text) {
  const haystack = String(text ?? '');
  return goFetchNeverRetryablePatterns.some((re) => re.test(haystack));
}

// runWithRetry — the loop, with every collaborator injectable so the tests can
// assert the ATTEMPT COUNT rather than only the exit status.
//
// Returns { status, attempts, verdict }:
//   verdict `succeeded`       the command exited 0.
//   verdict `never-retryable` layer (c) refused another attempt.
//   verdict `not-transient`   layer (b) found no transport signature.
//   verdict `exhausted`       every attempt hit a transport fault.
export function runWithRetry(options) {
  const {
    argv,
    attempts = registryAttempts,
    backoffStepSeconds = goFetchBackoffStepSeconds,
    spawn = (program, args) => capture(program, args),
    sleep = sleepSeconds,
    onLog = log,
  } = options;

  let used = 0;
  let status = 1;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    used = attempt;
    onLog(`    ${argv.join(' ')} (attempt ${attempt}/${attempts})`);
    const res = spawn(argv[0], argv.slice(1));
    status = res.status;
    if (status === 0) return { status, attempts: used, verdict: 'succeeded' };

    const output = `${res.stdout ?? ''}${res.stderr ?? ''}`;
    if (goFetchNeverRetryable(output)) {
      return { status, attempts: used, verdict: 'never-retryable' };
    }
    if (!isTransientRegistryFailure(output)) {
      return { status, attempts: used, verdict: 'not-transient' };
    }
    if (attempt < attempts) sleep(attempt * backoffStepSeconds);
  }
  return { status, attempts: used, verdict: 'exhausted' };
}

// The command a bare invocation runs. Spelled once so the warm step in
// .github/actions/setup-go and the tests below agree on what "warm" means.
export const warmCommand = ['go', 'mod', 'download'];

// commandFrom — the argv a CLI invocation asks for. Everything after a `--`
// separator, or the warm command when there is none.
export function commandFrom(argv) {
  const sep = argv.indexOf('--');
  if (sep === -1) return [...warmCommand];
  return argv.slice(sep + 1);
}

const verdictExplanations = {
  'never-retryable': (cmd) =>
    `${cmd} failed with an error no retry can fix — a checksum/security refusal, or a version the ` +
    'proxy definitively does not have. Retrying a checksum refusal is how a poisoned module gets ' +
    'accepted, so this class is refused another attempt by design. Fix the version or run `go mod tidy`.',
  'not-transient': (cmd) =>
    `${cmd} failed for a reason that is not a network fault, so it was not retried. The output above ` +
    'is the real error.',
  exhausted: (cmd, attempts) =>
    `${cmd} failed ${attempts} times on module-proxy transport faults. That is a genuinely unreachable ` +
    'proxy rather than a blip: check https://status.golang.org and the runner network before re-running.',
};

function main() {
  const argv = commandFrom(process.argv.slice(2));

  if (argv.length === 0 || !isFetchOnly(argv)) {
    error(
      `go-module-fetch refuses to run \`${argv.join(' ')}\`: it wraps FETCH-ONLY commands only ` +
        `(${fetchOnlyCommandPrefixes.map((p) => `\`${p.join(' ')}\``).join(', ')}). A command that ` +
        'compiles, tests or runs anything must fail on its first attempt, because retrying it would ' +
        'hide the failure it is reporting.',
    );
    process.exit(1);
  }

  const result = runWithRetry({
    argv,
    backoffStepSeconds: readBackoffStepSeconds(goFetchBackoffStepEnvVar, goFetchBackoffStepSeconds),
    spawn: (program, args) => {
      const res = capture(program, args);
      if (res.stdout) process.stdout.write(res.stdout);
      if (res.stderr) process.stderr.write(res.stderr);
      return res;
    },
  });

  const printable = argv.join(' ');
  if (result.status === 0) {
    notice(`${printable} succeeded on attempt ${result.attempts}/${registryAttempts}`);
    process.exit(0);
  }
  error(verdictExplanations[result.verdict](printable, result.attempts));
  process.exit(result.status);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
