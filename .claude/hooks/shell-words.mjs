// shell-words.mjs — the one shell-line model both PreToolUse hooks share.
//
// A hook that splits a command line on `&&` / `;` / newline and greps the
// pieces sees the INSIDE of every quoted string as if it were a command: a
// commit-message heredoc that mentions `gremlins`, a grep for a script's own
// file name, an `echo "git push origin main is refused"` all read as the thing
// they merely talk about. And it misses the real command when it is quoted:
// `bash -c "just mutate"`, `echo pkg | xargs gremlins unleash`. Both hooks
// used exactly that model, and both had the false positives and the
// bypasses it implies.
//
// This module tokenises a line the way a shell does, far enough to answer
// "which commands run, with which words":
//
//   - single quotes, double quotes and backslashes are honoured, and the
//     quotes are stripped from the resulting words;
//   - a heredoc body (`<<EOF`, `<<-EOF`, `<<'EOF'`, `<<"EOF"`) is data and is
//     dropped entirely;
//   - `&&`, `||`, `;`, `|`, `&`, newline, `(` and `)` separate segments;
//   - `$(…)` and backticks START a segment even inside double quotes, because
//     a command substitution executes;
//   - `command`/`sudo`/`env`/`time`/`nice`/`timeout`/`rtk`/`rtk proxy`/
//     `systemd-run` and leading `VAR=value` assignments are stripped from the
//     front of a segment, with the argument each of them consumes;
//   - `bash -c '…'` / `sh -c '…'` and `xargs [opts] cmd …` are UNWRAPPED: the
//     payload is tokenised again and its segments are yielded too.
//
// Exports:
//   segments(command)  -> string[][]  every executed segment as its words,
//                                     wrappers stripped, quotes removed,
//                                     wrapped shells / xargs unwrapped.
//   splitWords(line)   -> string[][]  the raw tokenisation, one word list per
//                                     segment, wrappers NOT stripped.

const SHELLS = new Set(['bash', 'sh', 'zsh', 'dash', 'ksh']);

// Wrappers that consume no argument of their own.
const BARE_WRAPPERS = new Set(['command', 'time', 'rtk', 'proxy', 'env', 'exec', 'nohup']);

// xargs options that take a value; every other `-x` is a flag.
const XARGS_OPTS_WITH_VALUE = new Set(['-n', '-I', '-P', '-L', '-s', '-d', '-a', '-E', '--max-args', '--replace', '--max-procs', '--max-lines', '--max-chars', '--delimiter', '--arg-file', '--eof']);

// systemd-run / sudo / nice / timeout options that take a value.
const WRAPPER_OPTS_WITH_VALUE = new Set(['-p', '--property', '-u', '--unit', '-n', '--adjustment', '-k', '--kill-after', '-s', '--signal', '--user', '-g', '--group', '-C', '-D', '-h', '--host']);

const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/;

/**
 * splitWords — tokenise one command line into segments of words.
 */
export function splitWords(line) {
  const segments = [];
  let words = [];
  let word = '';
  let inWord = false;
  let quote = null; // null | "'" | '"'
  const quoteStack = []; // double-quote state to restore when a `$(` closes
  const pendingHeredocs = []; // delimiters whose bodies start at the next newline

  // An empty word (`""`, or the residue of a command substitution inside
  // double quotes) is dropped: nothing below classifies on an empty argument.
  const endWord = () => {
    if (inWord && word !== '') words.push(word);
    word = '';
    inWord = false;
  };
  const endSegment = () => {
    endWord();
    if (words.length > 0) segments.push(words);
    words = [];
  };

  let i = 0;
  const n = line.length;
  while (i < n) {
    const c = line[i];
    const next = line[i + 1];

    if (quote === "'") {
      if (c === "'") quote = null;
      else word += c;
      i++;
      continue;
    }

    // A command substitution executes wherever it appears outside single
    // quotes; its contents are a segment of their own.
    if (c === '$' && next === '(') {
      endSegment();
      quoteStack.push(quote);
      quote = null;
      i += 2;
      continue;
    }
    if (c === '`') {
      endSegment();
      if (quoteStack.length > 0 && quoteStack[quoteStack.length - 1] === '`') {
        quote = quoteStack.pop();
        quote = quote === '`' ? null : quote;
      } else {
        quoteStack.push('`');
        quote = null;
      }
      i++;
      continue;
    }

    if (quote === '"') {
      if (c === '"') {
        quote = null;
      } else if (c === '\\' && next !== undefined) {
        word += next;
        i++;
      } else {
        word += c;
        inWord = true;
      }
      i++;
      continue;
    }

    // quote === null
    if (c === "'" || c === '"') {
      quote = c;
      inWord = true; // an empty quoted string is still a word
      i++;
      continue;
    }
    if (c === '\\' && next !== undefined) {
      if (next !== '\n') {
        word += next;
        inWord = true;
      }
      i += 2;
      continue;
    }
    if (c === '<' && next === '<') {
      // heredoc: `<<`, `<<-`, then an optionally quoted delimiter
      endWord();
      i += 2;
      if (line[i] === '-') i++;
      while (line[i] === ' ' || line[i] === '\t') i++;
      let delim = '';
      if (line[i] === "'" || line[i] === '"') {
        const q = line[i++];
        while (i < n && line[i] !== q) delim += line[i++];
        i++;
      } else {
        while (i < n && !/[\s;&|<>()]/.test(line[i])) {
          if (line[i] === '\\') i++;
          delim += line[i++];
        }
      }
      if (delim) pendingHeredocs.push(delim);
      continue;
    }
    if (c === '\n') {
      endSegment();
      i++;
      // consume every pending heredoc body, in order
      while (pendingHeredocs.length > 0) {
        const delim = pendingHeredocs.shift();
        for (;;) {
          if (i >= n) break;
          let eol = line.indexOf('\n', i);
          if (eol === -1) eol = n;
          const bodyLine = line.slice(i, eol);
          i = eol + 1;
          if (bodyLine.replace(/^\t+/, '') === delim) break;
        }
      }
      continue;
    }
    if (c === ')') {
      endSegment();
      if (quoteStack.length > 0) {
        const restored = quoteStack.pop();
        quote = restored === '`' ? null : restored;
      }
      i++;
      continue;
    }
    if (c === '(') {
      endSegment();
      i++;
      continue;
    }
    if ((c === '&' && next === '&') || (c === '|' && next === '|')) {
      endSegment();
      i += 2;
      continue;
    }
    if (c === ';' || c === '|' || c === '&') {
      endSegment();
      i++;
      continue;
    }
    if (c === ' ' || c === '\t') {
      endWord();
      i++;
      continue;
    }
    word += c;
    inWord = true;
    i++;
  }
  endSegment();
  return segments;
}

/**
 * stripWrappers — drop leading `VAR=value` assignments and command wrappers
 * (with the argument each consumes) so the real command is at index 0.
 */
export function stripWrappers(words) {
  let w = words.slice();
  for (;;) {
    while (w.length > 0 && ASSIGNMENT.test(w[0])) w.shift();
    if (w.length === 0) return w;
    const head = w[0];
    if (BARE_WRAPPERS.has(head)) {
      w.shift();
      continue;
    }
    if (head === 'sudo' || head === 'nice' || head === 'systemd-run' || head === 'timeout') {
      w.shift();
      while (w.length > 0 && w[0].startsWith('-')) {
        const opt = w.shift();
        if (WRAPPER_OPTS_WITH_VALUE.has(opt) && !opt.includes('=')) w.shift();
      }
      // `timeout DURATION cmd`, `nice` without -n takes nothing more.
      if (head === 'timeout') w.shift();
      continue;
    }
    return w;
  }
}

/**
 * segments — every executed command of a line as its stripped word list,
 * including the commands inside `bash -c` payloads and behind `xargs`.
 */
export function segments(command) {
  const out = [];
  // MAX_UNWRAP_DEPTH bounds `bash -c "bash -c …"` nesting; what is seen by
  // then is enough to classify.
  const MAX_UNWRAP_DEPTH = 4;
  const visitWords = (raw, depth) => {
    const w = stripWrappers(raw);
    if (w.length === 0) return;
    out.push(w);
    if (depth >= MAX_UNWRAP_DEPTH) return;
    if (SHELLS.has(w[0]) || w[0].endsWith('/bash') || w[0].endsWith('/sh')) {
      const at = w.findIndex((a, k) => k > 0 && /^-[a-zA-Z]*c[a-zA-Z]*$/.test(a));
      if (at >= 0 && w[at + 1] !== undefined) visitLine(w[at + 1], depth + 1);
      return;
    }
    if (w[0] === 'xargs') {
      let k = 1;
      while (k < w.length && w[k].startsWith('-')) {
        const opt = w[k++];
        if (XARGS_OPTS_WITH_VALUE.has(opt)) k++;
      }
      if (k < w.length) visitWords(w.slice(k), depth + 1);
    }
  };
  const visitLine = (line, depth) => {
    for (const raw of splitWords(line)) visitWords(raw, depth);
  };
  visitLine(command, 0);
  return out;
}
