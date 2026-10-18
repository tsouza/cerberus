// hook-payload.mjs — the one way every Claude Code hook here reads its payload.
//
// The harness writes the hook's JSON payload to stdin. A synchronous
// `readFileSync(0)` is not a safe read of it: when the inherited stdin is in
// non-blocking mode and the payload has not been written yet, the read throws
// EAGAIN, and a hook that maps that to "no payload" exits 0 with nothing on
// stderr — a guard that is silently off for that command. Reading
// `process.stdin` as a stream waits on the event loop instead, whatever the
// pipe's mode and however late the writer is.

import process from 'node:process';

// readPayload resolves to the parsed payload, or null when stdin is not one
// JSON document.
export async function readPayload() {
  let raw = '';
  try {
    process.stdin.setEncoding('utf8');
    for await (const chunk of process.stdin) raw += chunk;
  } catch {
    return null;
  }
  try {
    return JSON.parse(raw);
  } catch {
    return null;
  }
}
