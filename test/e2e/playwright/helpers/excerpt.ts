/**
 * Failure-line excerpts: one `truncate` and the named widths every spec
 * clips its diagnostic tails to. A failure line must carry enough of a
 * body or message to act on and no more — a 50 KB dashboard JSON pasted
 * into a summary buries the one line that names the panel.
 *
 * Pure; safe to import at Playwright config-load time.
 */

/** A request/response body quoted as the evidence of a failure. */
export const BODY_EXCERPT_CHARS = 600;

/**
 * A raw non-2xx body from the catch-net HTTP sweep, where the body IS the
 * only evidence and the surface is otherwise unnamed.
 */
export const WIDE_BODY_EXCERPT_CHARS = 800;

/** A captured non-2xx response line (method, URL and body) listed as context under a console error. */
export const RESPONSE_LINE_EXCERPT_CHARS = 700;

/** A banner text, console message or tunneled error message. */
export const MESSAGE_EXCERPT_CHARS = 400;

/**
 * A body quoted as secondary context beside a message that already
 * names the failure (a parse-error's input, a probe's diagnostic).
 */
export const CONTEXT_EXCERPT_CHARS = 300;

/** A single query expression or one serialised record. */
export const EXPR_EXCERPT_CHARS = 200;

/** Clip `s` to `max` characters, noting how much was dropped. */
export function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return `${s.slice(0, max)}…<truncated, ${s.length - max} more char(s)>`;
}
