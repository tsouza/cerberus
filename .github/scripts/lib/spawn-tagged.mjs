// spawn-tagged.mjs — run child processes concurrently with readable output.
//
// Shared by every fan-out runner that splits a corpus walk across concurrent
// `go test` processes: golden-update.mjs and cardinality-baseline-update.mjs
// tag each child's output and stream it line-by-line (`runTagged` /
// `runAllTagged`); perf-coverage-fanout.mjs, property-fanout.mjs and
// chdb-roundtrip.mjs instead buffer each child's output whole and print it
// after every leg finishes (`runLegBuffered`), because their failures are
// multi-line blocks — a fixture diff or a goroutine dump — that streaming
// would interleave into unreadable garbage across concurrent legs. All five
// callers need the same failure-propagation contract regardless of which
// output mode they use — let every leg finish before deciding whether the
// group failed — and a second copy of that per caller would be a second set
// of failure semantics to keep in step.
//
// No env inputs; it is a library, not a step.

import { spawn } from 'node:child_process';

/**
 * Runs one child process, streaming its stdout/stderr to this process's own
 * streams line-by-line, each line tagged `[name] `. Line-buffering (rather
 * than piping the raw stream through) is what keeps concurrent children's
 * output from interleaving mid-line into unreadable garbage; a whole line is
 * still the smallest unit two children can race to print.
 *
 * Resolves to the exit code rather than rejecting, so a caller running
 * several of these concurrently can let every child finish (and print its
 * output) before deciding whether the group as a whole failed.
 */
export function runTagged(name, argv, env, cwd, sink) {
  return new Promise((resolve) => {
    const child = spawn(argv[0], argv.slice(1), {
      cwd,
      env: { ...process.env, ...env },
    });

    const tag = (stream) => {
      let carry = '';
      stream.on('data', (chunk) => {
        carry += chunk.toString();
        const lines = carry.split('\n');
        carry = lines.pop();
        for (const l of lines) sink(`[${name}] ${l}`);
      });
      stream.on('end', () => {
        if (carry) sink(`[${name}] ${carry}`);
      });
    };
    tag(child.stdout);
    tag(child.stderr);

    child.on('close', (code) => resolve(code === null ? 1 : code));
  });
}

/**
 * Runs every command concurrently and resolves to the FIRST non-zero exit code,
 * or 0 when all succeeded.
 *
 * "First non-zero" is decided after every child has finished, never by aborting
 * the group: a fan-out failing on one leg usually means the same thing failed on
 * several, and killing the survivors would hide all but one of them. Each
 * command is `{ argv, env, label }`.
 */
export async function runAllTagged(commands, cwd, sink) {
  const codes = await Promise.all(
    commands.map(({ argv, env, label }) => runTagged(label, argv, env, cwd, sink)),
  );

  return codes.find((c) => c !== 0) ?? 0;
}

/**
 * Runs one leg (`{ argv, env? }`) to completion, buffering its stdout and
 * stderr rather than streaming them.
 *
 * Buffered rather than tagged-and-streamed because these callers' legs run
 * concurrently and a failure is a multi-line block — a `go test` fixture
 * diff or a goroutine dump — that becomes unreadable once several legs
 * shuffle their lines into each other. Callers print each leg's whole
 * buffer only after every leg has finished. Resolves rather than rejects
 * (including on a spawn error) so `Promise.all` over several legs always
 * waits for the group, the same contract `runTagged` holds for the
 * streaming callers above.
 */
export function runLegBuffered(leg) {
  return new Promise((resolve) => {
    const [cmd, ...args] = leg.argv;
    const child = spawn(cmd, args, {
      env: { ...process.env, ...leg.env },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    child.stdout.on('data', (d) => {
      out += d;
    });
    child.stderr.on('data', (d) => {
      out += d;
    });
    child.on('error', (err) => resolve({ leg, code: 1, out: `${out}\nspawn ${cmd}: ${err.message}` }));
    child.on('close', (code) => resolve({ leg, code: code ?? 1, out }));
  });
}
