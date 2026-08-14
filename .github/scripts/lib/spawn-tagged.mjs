// spawn-tagged.mjs — run child processes concurrently with readable output.
//
// Shared by the two regeneration runners that fan a corpus walk out across
// processes: golden-update.mjs (the TXTAR goldens, partitioned by
// test/spec/shard.go) and cardinality-baseline-update.mjs (the perf cardinality
// baseline, partitioned by test/perf/profile/shard.go). Both need exactly the
// same two things — tag each child's output so concurrent legs stay readable,
// and let every leg finish before deciding whether the group failed — and a
// second copy of that would be a second set of failure semantics to keep in
// step.
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
