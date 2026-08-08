// forbid-deferral.mjs — the no-deferrals gate.
//
// Repo policy: work that surfaces mid-change and genuinely belongs outside the
// change's scope becomes a GitHub Issue, filed before the pull request merges.
// A sentence in a PR body, a commit message, or a code comment is not a resting
// place — the moment the PR merges that sentence appears in no list, no gate
// reports on it, and nobody is assigned. An Issue stays open until someone
// closes it, which is the whole point. Until this module existed the rule was
// discipline only; nothing checked it.
//
// WHAT IS SCANNED — the change's OWN additions, three surfaces, nothing else:
//
//   1. the pull request description,
//   2. the commit messages in the range base..head,
//   3. the `+` lines of `git diff base...head`.
//
// The whole tree is deliberately NOT scanned. `origin/main` carries hundreds of
// legitimate architecture sentences describing system boundaries ("out of scope
// for the unknown-name fan-out"), and a gate that fired on those would be
// unusable — an unusable gate gets routed around, which is worse than no gate.
// This is SCOPING, not an allow-list: there is no tolerance file, no exemption
// list, and no way to park a violation.
//
// Surface 1 carries ONE narrowing, on the same grounds — see
// GENERATED_DESCRIPTION_AUTHOR and scansDescription() below. Surfaces 2 and 3
// are read on every change without exception, and that is what keeps the
// narrowing scoping rather than an exemption: nothing reaches the tree behind
// it.
//
// WHAT COUNTS — see DEFERRAL_MARKERS below, the single exported table. Each row
// matches "work this change is postponing", never "a system boundary being
// described". The distinction is why the table's last row carries a qualifier:
// the bare phrase for an architectural exclusion must NOT match, only the
// variant that names this pull request.
//
// WHAT IS REQUIRED — every match must be accompanied by a reference to an OPEN
// GitHub Issue (`#<n>`, or the full issues URL for this repository) close enough
// to be about it. "Close enough" is the author's own structure, not a fixed
// radius:
//
//   * a marker ON A MARKDOWN HEADING is satisfied anywhere in the SECTION that
//     heading introduces — up to the next heading of the same or higher level.
//     A heading is a label for the block beneath it, so the canonical correct
//     shape is a heading naming the category followed by a list whose every
//     item leads with its issue. Paragraph scope rejected exactly that shape,
//     and told an author who had filed the issues that they had cited nothing.
//   * any other prose marker is satisfied within its own paragraph — an issue
//     named three paragraphs away tracks something else.
//   * a marker in a diff is satisfied within CITATION_WINDOW_LINES lines.
//
// Each cited number is resolved through the API and must be an Issue (GitHub's
// issues endpoint serves pull requests too, distinguished by the `pull_request`
// key) and must be open. A pointer at a merged PR or a closed issue is
// untracked work wearing a citation.
//
// WHY A CAPABILITY PROBE GUARDS THAT LOOKUP — GitHub answers a resource the
// caller may not read with 404, never 403: confirming a 403 would confirm the
// resource exists, which is precisely what it declines to do. So "#1431 names
// nothing in this repository" and "this token may not read issues at all"
// arrive at the issue endpoint as the same status code, and reading the second
// as the first accuses the author of parking work they in fact filed. That is
// not hypothetical: a run whose workflow lacked the `issues: read` grant
// resolved zero of two genuinely open citations and reported an untracked
// deferral, while the same module over the same range and body passed locally.
// So the first unresolvable number triggers issuesReadability() below, which
// separates the two by asking the API a question whose answer cannot be
// ambiguous, and a token that cannot read issues FAILS the run naming itself as
// the cause. A gate that reports the wrong reason is worse than one that
// reports none: the author corrects prose that was already correct while the
// real fault stays hidden.
//
// ANTI-VACUITY — a gate that silently inspects nothing is worse than none, and
// this repo has been bitten by exactly that class. Every run asserts positively
// that it did work: the marker table is non-empty and every pattern compiles,
// the PR-description surface was fetched (or is explicitly, provably empty),
// the commit range resolved to at least one commit, and the diff parsed to a
// non-empty file set. Any missing or malformed input FAILS the run rather than
// passing green.
//
// Env contract:
//   GITHUB_REPOSITORY  REQUIRED. `owner/repo` (runner-provided). Also fixes
//                      which issues-URL host/slug counts as a citation.
//   GITHUB_TOKEN       REQUIRED. Needs `issues: read` to resolve a cited
//                      number's kind + state, and `pull-requests: read` to
//                      resolve the description surface on a push event. A token
//                      without `issues: read` fails the run with a permission
//                      diagnostic rather than reporting the citations it could
//                      not read as untracked work — see issuesReadability().
//   GITHUB_EVENT_NAME  REQUIRED. `pull_request` reads PR_BODY; anything else
//                      resolves the description from the API by head commit.
//   PR_BODY            REQUIRED on a pull_request run — the description, passed
//                      via env and never interpolated into a shell `run:` line
//                      (bodies are attacker-controlled). May be empty; may not
//                      be unset, which would mean the surface went uninspected.
//   GITHUB_EVENT_PATH  Runner-provided path to the event payload JSON. Read for
//                      ONE field, `pull_request.user.login`, which decides
//                      whether the description surface is the author's own
//                      prose (see scansDescription). Deliberately NOT a
//                      workflow-interpolated author string: an input the caller
//                      supplies is an input the caller can get wrong, and this
//                      one decides whether a surface is read at all. Unset,
//                      unreadable or malformed yields no author, and no author
//                      means the description IS scanned — the fail-closed
//                      direction.
//   BASE_SHA           Range start. Falls back to `HEAD_SHA^` when unset or
//                      unresolvable (a branch-creation push sends all-zeroes).
//   HEAD_SHA           Range end. Falls back to `HEAD`.
//   GITHUB_API_URL     API base (runner-provided; default https://api.github.com).
//
// The pure halves of this module (the marker table, the scanners, the verdict)
// are exported and pinned by the `node --test` guard forbid-deferral.test.mjs;
// the scan itself runs only when this file is invoked as the program.
//
// Exit codes: 0 = every marker is tracked by an open issue (or none were
// found), 1 = an untracked deferral or a malformed input.
//
// A NOTE ON THIS FILE'S OWN TEXT: the gate scans the added lines of the very
// pull request that introduces it, so every pattern below is written so that
// its own source text cannot match it — each bare-word alternative is prefixed
// by `\b`, whose literal `b` denies the word boundary the pattern needs, and
// every phrase is joined by `\s+` rather than a literal space. This is the same
// move forbid-skip.mjs makes when it writes its t.Skip pattern as an escaped
// regex rather than a literal call. Prose in this file, its test, and the docs
// therefore names marker CLASSES rather than reproducing marker text.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { appendStepSummary, error, git, log, notice } from './lib/gh.mjs';

// How far a citation may sit from its marker in a diff. A marker and the issue
// that tracks it belong to the same comment block; three lines spans a short
// block without letting an unrelated citation elsewhere in the file adopt it.
// The same number is the diff's context width, so a citation on an unchanged
// line inside the block still counts.
export const CITATION_WINDOW_LINES = 3;

// GitHub's "not found" — for a cited number that names nothing AND for one the
// token may not read. The two are indistinguishable at the issue endpoint by
// design, which is why issuesReadability() exists.
const HTTP_NOT_FOUND = 404;

// The capability probe reads the smallest page the list endpoint will serve: it
// wants the status line, never the payload, and a repository with thousands of
// issues must not cost a page of them to answer one yes/no question.
const CAPABILITY_PROBE_PAGE_SIZE = 1;

// DEFERRAL_MARKERS — the whole marker table, in one place so it is reviewable
// and testable in isolation. Every pattern is applied case-insensitively.
//
// `id` names the shape rather than quoting it, for the self-match reason in the
// file header. `description` is what the failure message tells the author they
// wrote.
export const DEFERRAL_MARKERS = [
  {
    id: 'code-work-marker',
    description: 'a conventional source-comment work marker',
    pattern: String.raw`\bTODO\b|\bFIXME\b|\bXXX\b`,
  },
  {
    id: 'unfixed-here',
    description: 'a finding named as unresolved by this change',
    pattern: String.raw`\bnot\s+(?:fixed|addressed|handled|done)\s+(?:here|in\s+this\s+PR)\b`,
  },
  {
    // The qualifier is load-bearing here too. A sweep of all 1326 commits on
    // `main` found the single commonest false positive to be the phrase used to
    // RECORD work already done ("Follow-up to #1234"), which is the opposite of
    // a deferral. Only the LABEL forms — a heading, or the colon that
    // introduces a list of things nobody has taken — match.
    // The heading alternative starts at a zero-width line boundary rather than
    // consuming the preceding newline, so the reported line is the heading's
    // own — a match that swallowed the `\n` would report the line above it and
    // would not be recognised as a heading when its citation scope is computed.
    id: 'followup-label',
    description: 'a heading or label introducing work nobody has taken',
    pattern: String.raw`(?<![^\n])[ \t]*#{1,6}[ \t]*follow-?ups?\b|\bfollow-?ups?[ \t]*:`,
  },
  {
    // Same shape as `unfixed-here`, kept separate because the sweep counted it
    // separately (23 commits). Bare "not yet" is ordinary prose about state and
    // must not match — only the variant that names this change as the place it
    // did not happen.
    id: 'not-yet-here',
    description: 'a capability named as still missing after this change',
    pattern: String.raw`\bnot\s+yet\s+(?:implemented|supported|handled|addressed|fixed|done)\s+(?:here|in\s+this\s+PR)\b`,
  },
  {
    // Word-boundaried on purpose: Go's `defer` statement is the single largest
    // source of raw regex hits in this repository, and an unanchored
    // `defer(red|ring)?` would fire on `defer rows.Close()` in every file it
    // appears in. `defer` / `defers` / `defer func()` must not match; only the
    // English participle does, and only in PREDICATE position: right after a
    // linking or passive-voice verb (a form of "to be", "remain", "stay",
    // "get", or "left"), which is the grammar of a sentence whose whole point
    // is that something did not happen. A bare, unconditional match on the
    // participle is broader than that — it also matches ATTRIBUTIVE use,
    // where the identical word modifies a noun to name an existing,
    // already-shipped stage of a pipeline rather than postponed work. "the
    // deferred label-shaping (-State/-Merge) shape" describes a feature that
    // is fully built, not one whose work was put off, and reached a real diff
    // (#1955) as exactly that false positive: a correct comment had to be
    // reworded to get a clean run. Requiring the verb strips the attributive
    // shape without losing the predicate one, which is the only shape the
    // calibration corpus and this file's own test pin. A colon-suffixed label
    // is kept as its own arm — the heading style commit messages on this repo
    // used for a genuinely postponed test case before this gate existed —
    // mirroring `followup-label` below.
    id: 'deferral-to-later',
    description: 'work postponed instead of done',
    pattern: String.raw`\b(?:is|was|were|remains?|stays?|gets?|being|left)\s+deferred\b|\bdeferred[ \t]*:|\bdefer(?:ring|red)\s+to\s+(?:a\s+)?(?:later|future|separate)\b`,
  },
  {
    id: 'left-for-later',
    description: 'work handed to some later change',
    pattern: String.raw`\bleft\s+(?:for|to)\s+(?:later|a\s+follow-?up)\b`,
  },
  {
    id: 'later-pr',
    description: 'work assigned to a pull request that does not exist yet',
    pattern: String.raw`\bin\s+a\s+(?:later|future|separate)\s+PR\b`,
  },
  {
    id: 'punted',
    description: 'a decision put off rather than taken',
    pattern: String.raw`\bpunt(?:ed)?\s+(?:on|to)\b`,
  },
  {
    id: 're-visit',
    description: 'a promise to look at something again',
    pattern: String.raw`\brevisit\b`,
  },
  {
    // The qualifier is load-bearing. The unqualified phrase is architecture
    // prose ("out of scope for the unknown-name fan-out") and occurs across the
    // tree in the hundreds; only the variant that names THIS pull request is a
    // deferral.
    id: 'pr-scoped-exclusion',
    description: "work excluded by naming this pull request's scope",
    pattern: String.raw`\bout\s+of\s+scope\s+for\s+this\s+PR\b|\boutside\s+this\s+PR['’]?s\s+scope\b`,
  },
];

// --- marker + citation extraction ------------------------------------------

// markerRegex builds a fresh RegExp per call: a `g`-flagged regex carries
// lastIndex, and a shared instance would skip matches on alternate scans.
function markerRegex(pattern) {
  return new RegExp(pattern, 'gi');
}

// lineOf — 1-based line number of an index within text.
function lineOf(text, index) {
  let line = 1;
  for (let i = 0; i < index; i += 1) {
    if (text[i] === '\n') line += 1;
  }
  return line;
}

// findMarkers — every marker occurrence in `text`, with its 1-based line.
// Overlapping rows both report: a sentence can defer in two idioms at once and
// the author should see both.
export function findMarkers(text) {
  const found = [];
  const src = String(text ?? '');
  for (const marker of DEFERRAL_MARKERS) {
    const re = markerRegex(marker.pattern);
    for (const m of src.matchAll(re)) {
      found.push({
        id: marker.id,
        description: marker.description,
        text: m[0].trim(),
        index: m.index,
        line: lineOf(src, m.index),
      });
    }
  }
  return found.sort((a, b) => a.index - b.index || a.id.localeCompare(b.id));
}

// issueRefs — the issue numbers cited in `text`: `#<n>`, or the full issues URL
// for THIS repository. A URL naming another repository is not a citation this
// gate can resolve, so it is not accepted.
export function issueRefs(text, repoSlug) {
  const src = String(text ?? '');
  const numbers = new Set();
  for (const m of src.matchAll(/(?:^|[^\w/])#(\d+)\b/g)) numbers.add(Number(m[1]));
  if (repoSlug) {
    const escaped = repoSlug.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    const urlRe = new RegExp(String.raw`github\.com/${escaped}/issues/(\d+)\b`, 'gi');
    for (const m of src.matchAll(urlRe)) numbers.add(Number(m[1]));
  }
  return [...numbers].sort((a, b) => a - b);
}

// stripFencedBlocks — blank out fenced code blocks, preserving line count.
//
// Prose surfaces only. A fenced block in a description or a commit message is
// QUOTED material — a log excerpt, a diff hunk, a paste of this very gate's
// output, which the remedy message asks authors to read. Scanning it reports
// the quotation rather than the author's own commitment, and reporting the
// quotation of a violation as a violation is how a gate earns a reputation for
// crying wolf. The diff surface gets no such treatment: markers that reach the
// tree are caught wherever they sit, so nothing lands in the repository behind
// this.
export function stripFencedBlocks(text) {
  const lines = String(text ?? '').split('\n');
  let fence = null;
  return lines
    .map((line) => {
      const open = /^\s{0,3}(`{3,}|~{3,})/.exec(line);
      if (fence === null) {
        if (open) {
          fence = open[1][0];
          return '';
        }
        return line;
      }
      if (open && open[1][0] === fence) fence = null;
      return '';
    })
    .join('\n');
}

// GENERATED_DESCRIPTION_AUTHOR — the one account whose pull request DESCRIPTION
// this gate does not read.
//
// The reasoning is stripFencedBlocks's, one level up. Dependabot writes its own
// description, and what it writes is a paste of somebody else's release notes:
// a `<details>` block per bumped module, each holding that module's changelog
// and its upstream commit subjects verbatim. That is QUOTED material in exactly
// the sense the comment above means — a report of what other people wrote, not
// a commitment this change is making — and scanning it reports the quotation
// rather than the author's own promise. It is not hypothetical: a bump whose
// generated body quoted an upstream commit titled "remove obsolete <marker>"
// was reported four times, once per bumped module, as this change parking work.
// The upstream commit was DELETING a marker.
//
// WHAT THIS GIVES UP, stated plainly. A human who edits a bot pull request's
// description gets that prose unread — and that happens: the bump this rule was
// written for grew a hand-written repair section after a human took the branch
// over. Their commit messages and their diff are still read, so the loss is one
// surface on one class of pull request, and the surface a marker would have to
// survive to reach the repository is not it.
//
// WHY THIS IS SCOPING AND NOT AN ALLOW-LIST, against the header's own test at
// the top of this file: nothing here can park a violation. There is no tolerance
// file, no number to add, no per-change escape. The commit-message surface and
// the diff surface are read on every change from every author, and the diff
// surface is the one that decides what lands in the tree. What changes is WHICH
// SURFACE is read, on the ground that the text in it was written by neither the
// change nor its author.
const GENERATED_DESCRIPTION_AUTHOR = 'dependabot[bot]';

// scansDescription — is this pull request's description the author's own prose?
//
// Fail-closed by construction: only a login that positively matches the
// generated-description account suppresses the surface, so an absent, empty or
// unreadable author scans as normal. A gate that stopped reading a surface
// because it could not determine something is the failure this ordering avoids.
export function scansDescription(author) {
  const login = String(author ?? '').trim().toLowerCase();
  if (login === '') return true;
  return login !== GENERATED_DESCRIPTION_AUTHOR;
}

// eventPayloadAuthor — the login that opened this pull request, read out of the
// payload JSON the runner wrote for the event.
//
// Every failure mode collapses to null (no path, no file, unparseable JSON, a
// payload with no pull request, a login that is not a non-empty string), which
// scansDescription reads as "scan it". The read is therefore allowed to be
// total: there is no error state in which this function's answer widens what
// the gate accepts.
export function eventPayloadAuthor(eventPath) {
  if (!eventPath) return null;
  let payload;
  try {
    payload = JSON.parse(readFileSync(eventPath, 'utf8'));
  } catch {
    return null;
  }
  const login = payload?.pull_request?.user?.login;
  return typeof login === 'string' && login.trim() !== '' ? login.trim() : null;
}

// sharedAuthor — the single login behind a set of pull requests, or null when
// they do not agree on one.
//
// The push and merge-queue paths resolve the description surface from the head
// commit's associated pull requests, which is a LIST. Concatenating two bodies
// and attributing them to one author would be a guess; disagreement therefore
// yields null, and null scans. An empty list likewise: there is no author to
// read, so there is nothing to narrow.
export function sharedAuthor(pulls) {
  const logins = new Set(
    (Array.isArray(pulls) ? pulls : [])
      .map((p) => (typeof p?.user?.login === 'string' ? p.user.login.trim() : ''))
      .filter((l) => l !== ''),
  );
  return logins.size === 1 ? [...logins][0] : null;
}

// paragraphs — split prose into blank-line-separated blocks, each carrying the
// 1-based line at which it starts. The paragraph is the citation scope for
// prose: an issue named three paragraphs away tracks something else.
export function paragraphs(text) {
  const lines = String(text ?? '').split('\n');
  const out = [];
  let current = null;
  lines.forEach((line, i) => {
    if (line.trim() === '') {
      current = null;
      return;
    }
    if (current === null) {
      current = { startLine: i + 1, lines: [] };
      out.push(current);
    }
    current.lines.push(line);
  });
  return out.map((p) => ({ startLine: p.startLine, text: p.lines.join('\n') }));
}

// headingLevel — the ATX heading level of a line, or 0 when the line is not a
// heading. Setext headings are not recognised: no surface this gate reads uses
// them, and guessing would make an ordinary line of prose into a section head.
export function headingLevel(line) {
  const m = /^ {0,3}(#{1,6})\s/.exec(String(line ?? ''));
  return m ? m[1].length : 0;
}

// sectionAt — the text of the section a heading introduces: the heading itself
// through to just before the next heading of the same or higher level, or the
// end of the surface. Returns null when the line is not a heading.
//
// The boundary is deliberately structural rather than a line budget. A section
// is the unit of meaning the AUTHOR declared, so a heading with its issues
// listed forty lines below is still one thought; capping the distance would
// reintroduce an arbitrary number and reject bodies that are correct. The
// widening is confined to headings for the same reason: a heading is a label
// for the block beneath it, whereas a sentence buried mid-section is not, and
// keeps paragraph scope.
export function sectionAt(lines, headingIndex) {
  const level = headingLevel(lines[headingIndex]);
  if (level === 0) return null;
  let end = lines.length;
  for (let i = headingIndex + 1; i < lines.length; i += 1) {
    const l = headingLevel(lines[i]);
    if (l > 0 && l <= level) {
      end = i;
      break;
    }
  }
  return lines.slice(headingIndex, end).join('\n');
}

// scanProse — candidate violations in a prose surface (a description, a commit
// message). `locate(line)` renders a human-addressable location.
export function scanProse({ text, surface, locate, repoSlug }) {
  const stripped = stripFencedBlocks(text);
  const allLines = stripped.split('\n');
  const candidates = [];
  for (const para of paragraphs(stripped)) {
    const paragraphRefs = issueRefs(para.text, repoSlug);
    for (const marker of findMarkers(para.text)) {
      const line = para.startLine + marker.line - 1;
      const section = sectionAt(allLines, line - 1);
      candidates.push({
        surface,
        location: locate(line),
        markerId: marker.id,
        description: marker.description,
        markerText: marker.text,
        scope: section === null ? 'the same paragraph' : 'the section it introduces',
        refs: section === null ? paragraphRefs : issueRefs(section, repoSlug),
      });
    }
  }
  return candidates;
}

// --- diff surface -----------------------------------------------------------

// parseDiff — a unified diff into { files, lines }, where `lines` carries every
// line that exists in the NEW file (added and context) with its new-file number
// and whether the change added it. Removed lines have no new-file position and
// are dropped: this gate reads what the change INTRODUCES.
export function parseDiff(diffText) {
  const files = new Set();
  const lines = [];
  let file = null;
  let inHunk = false;
  let newLine = 0;

  for (const raw of String(diffText ?? '').split('\n')) {
    if (raw.startsWith('diff --git ')) {
      inHunk = false;
      file = null;
      continue;
    }
    if (!inHunk && raw.startsWith('+++ ')) {
      const path = raw.slice(4).trim();
      file = path === '/dev/null' ? null : path.replace(/^b\//, '');
      if (file) files.add(file);
      continue;
    }
    if (raw.startsWith('@@')) {
      const m = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(raw);
      if (!m) continue;
      inHunk = true;
      newLine = Number(m[1]);
      continue;
    }
    if (!inHunk || file === null) continue;
    if (raw.startsWith('\\')) continue; // "\ No newline at end of file"
    if (raw.startsWith('-')) continue;
    if (raw.startsWith('+')) {
      lines.push({ file, line: newLine, text: raw.slice(1), added: true });
      newLine += 1;
      continue;
    }
    if (raw.startsWith(' ') || raw === '') {
      lines.push({ file, line: newLine, text: raw.slice(1), added: false });
      newLine += 1;
      continue;
    }
    // Anything else ends the hunk (a mode line, a "Binary files ... differ").
    inHunk = false;
  }
  return { files: [...files], lines };
}

// scanDiff — candidate violations among the diff's ADDED lines. A citation may
// come from any line within CITATION_WINDOW_LINES in the same file, added or
// merely nearby, so an issue already cited in the comment block being extended
// still counts.
export function scanDiff(diffText, repoSlug) {
  const { lines } = parseDiff(diffText);
  const byFile = new Map();
  for (const l of lines) {
    if (!byFile.has(l.file)) byFile.set(l.file, []);
    byFile.get(l.file).push(l);
  }

  const candidates = [];
  for (const [file, fileLines] of byFile) {
    for (const l of fileLines) {
      if (!l.added) continue;
      for (const marker of findMarkers(l.text)) {
        const near = fileLines.filter(
          (o) => Math.abs(o.line - l.line) <= CITATION_WINDOW_LINES,
        );
        candidates.push({
          surface: 'diff',
          location: `${file}:${l.line}`,
          markerId: marker.id,
          description: marker.description,
          markerText: marker.text,
          scope: `±${CITATION_WINDOW_LINES} lines`,
          refs: issueRefs(near.map((o) => o.text).join('\n'), repoSlug),
        });
      }
    }
  }
  return candidates;
}

// --- surface composition -----------------------------------------------------

// candidatesFor — every candidate violation across all three surfaces.
//
// Composed here rather than inline in the runner so the surface scoping is a
// pure function a test can pin. The claim scansDescription makes is not "a
// description is skipped" but "a description is skipped AND the other two
// surfaces are not", and that is a statement about all three at once: a test
// that only exercised the description could not tell scoping apart from an
// exemption. The tests that matter are the ones asserting a marker in a bot
// pull request's COMMIT MESSAGE and in its DIFF is still reported.
export function candidatesFor({ description, author, commits, diffText, repoSlug }) {
  return [
    ...(scansDescription(author)
      ? scanProse({
          text: description,
          surface: 'pr-body',
          locate: (line) => `PR description line ${line}`,
          repoSlug,
        })
      : []),
    ...(Array.isArray(commits) ? commits : []).flatMap((c) =>
      scanProse({
        text: c.message,
        surface: 'commit',
        locate: (line) => `commit ${c.sha.slice(0, 12)} message line ${line}`,
        repoSlug,
      }),
    ),
    ...scanDiff(diffText, repoSlug),
  ];
}

// --- verdict ----------------------------------------------------------------

// citationVerdict — why a resolved reference does or does not track the work.
// `resolved` maps number -> { kind: 'missing' | 'pull-request' | 'issue',
// state }.
export function citationVerdict(refs, resolved) {
  if (refs.length === 0) {
    return { tracked: false, detail: 'it cites no issue at all' };
  }
  const reasons = [];
  for (const n of refs) {
    const r = resolved.get(n);
    if (!r || r.kind === 'missing') {
      reasons.push(`#${n} names nothing in this repository`);
      continue;
    }
    if (r.kind === 'pull-request') {
      reasons.push(`#${n} is a pull request, not an issue`);
      continue;
    }
    if (r.state !== 'open') {
      reasons.push(`#${n} is a ${r.state} issue, so the work it names is untracked`);
      continue;
    }
    return { tracked: true };
  }
  return { tracked: false, detail: reasons.join('; ') };
}

// evaluate — every candidate that no nearby citation rescues.
export function evaluate(candidates, resolved) {
  const violations = [];
  for (const c of candidates) {
    const verdict = citationVerdict(c.refs, resolved);
    if (!verdict.tracked) violations.push({ ...c, detail: verdict.detail });
  }
  return violations;
}

// --- issue-lookup capability -------------------------------------------------

// issuesReadability — can this token read this repository's issues at all?
//
// `get(url)` performs one GET and returns `{ ok, status, statusText }`; the
// probe never reads a body, only the status line. Two requests, in order:
//
//   1. `/repos/{owner}/{repo}` — the repository itself. Every token that can
//      see the repository can read this, whatever else it may not do; it needs
//      only the metadata permission, which is granted implicitly and cannot be
//      dropped. A failure HERE means the token cannot see the repository at
//      all (wrong slug, expired credential, a fork run with no access), which
//      is a different fault with a different fix, and it must not be reported
//      as a missing issues grant.
//   2. `/repos/{owner}/{repo}/issues` — the list endpoint, which requires
//      exactly the `issues: read` permission a cited number's lookup needs.
//
// Why this is SOUND and not merely plausible, on both visibilities:
//
//   * The two requests differ in one variable — the issues permission bit. They
//     use the same token, the same host and the same repository, so anything
//     that could make request 2 fail for a reason unrelated to issues (network,
//     auth, visibility, a wrong slug, an unreachable GHES base) makes request 1
//     fail first and is reported as itself.
//   * The list endpoint answers 200 with an EMPTY ARRAY for an authorised
//     caller in a repository with no issues, so a non-OK status is never "there
//     is nothing to list". There is no state of the repository that makes an
//     authorised list fail.
//   * It does not assume a public repository. An unauthenticated probe of a
//     "known-public" resource is the obvious alternative and it is unsound: on
//     a PRIVATE repository an anonymous GET answers 404 for everyone, so the
//     probe returns the same answer whether the token is fine or broken, and it
//     would report a permission fault on every private-repo run. This probe
//     compares two AUTHENTICATED reads that differ only in the permission under
//     test, so it behaves identically on public and private repositories.
//   * A repository with the Issues feature disabled also fails request 2. That
//     is honest: a gate whose only accepted resolution is filing an issue
//     cannot function there either, and it is a configuration fact rather than
//     something the author wrote.
export async function issuesReadability({ repo, apiBase, get }) {
  const repoUrl = `${apiBase}/repos/${repo}`;
  const repoRes = await get(repoUrl);
  if (!repoRes.ok) {
    return {
      readable: false,
      reason:
        `the token cannot read the repository ${repo} — GET ${repoUrl} answered HTTP ` +
        `${repoRes.status} ${repoRes.statusText}. Every issue lookup this run would make is ` +
        'therefore meaningless, so no citation verdict is reported: a number that cannot be ' +
        'resolved is not the same as a number that names nothing. Check GITHUB_REPOSITORY, ' +
        'GITHUB_API_URL and the job token before reading this run as a violation.',
    };
  }

  const issuesUrl = `${apiBase}/repos/${repo}/issues?per_page=${CAPABILITY_PROBE_PAGE_SIZE}`;
  const issuesRes = await get(issuesUrl);
  if (!issuesRes.ok) {
    return {
      readable: false,
      reason:
        `the token cannot read issues in ${repo} — GET ${repoUrl} answered 200 but GET ` +
        `${issuesUrl} answered HTTP ${issuesRes.status} ${issuesRes.statusText}. GitHub answers ` +
        'a resource the caller may not read with 404 rather than 403, so with this token every ' +
        'cited number looks like it names nothing and every correctly filed citation would be ' +
        'reported as untracked work. This is a permission fault in the job, not a fault in what ' +
        'the author wrote: the workflow must grant `issues: read` to this job ' +
        '(.github/workflows/forbid-deferral.yml does; a fork-scoped token, an expired ' +
        'credential, or a fine-grained PAT without the Issues scope does not).',
    };
  }

  return { readable: true };
}

// --- runner plumbing --------------------------------------------------------

function requireEnv(name, why) {
  const v = process.env[name];
  if (v === undefined || v === '') {
    throw new Error(`${name} is unset — ${why}`);
  }
  return v;
}

function verifyRev(rev) {
  if (!rev) return null;
  const res = git(['rev-parse', '--verify', '--quiet', `${rev}^{commit}`]);
  return res.status === 0 && res.stdout.trim() !== '' ? res.stdout.trim() : null;
}

// resolveRange — the commit range this run inspects. BASE_SHA is authoritative
// when it resolves; a branch-creation or force push sends an all-zeroes or
// unreachable value, and the head commit's first parent is the honest fallback.
// No base at all is a malformed input, not an empty scan.
export function resolveRange({ baseSha, headSha }) {
  const head = verifyRev(headSha) ?? verifyRev('HEAD');
  if (!head) {
    throw new Error(
      'neither HEAD_SHA nor HEAD resolves to a commit — the gate cannot tell what this change is',
    );
  }
  const base = verifyRev(baseSha) ?? verifyRev(`${head}^`);
  if (!base) {
    throw new Error(
      `no base commit: BASE_SHA does not resolve and ${head.slice(0, 12)} has no parent — ` +
        'the gate would scan an undefined range',
    );
  }
  return { base, head };
}

// commitsIn — every commit in base..head as { sha, message }. `git log -z`
// separates records with NUL, so a message containing blank lines survives.
function commitsIn(base, head) {
  const res = git(['log', '-z', '--format=%H%n%B', `${base}..${head}`]);
  if (res.status !== 0) {
    throw new Error(`git log ${base}..${head} failed: ${res.stderr.trim()}`);
  }
  return res.stdout
    .split('\0')
    .filter((r) => r.trim() !== '')
    .map((record) => {
      const nl = record.indexOf('\n');
      return {
        sha: record.slice(0, nl === -1 ? record.length : nl).trim(),
        message: nl === -1 ? '' : record.slice(nl + 1),
      };
    })
    .filter((c) => c.sha !== '');
}

function diffOf(base, head) {
  const res = git(['diff', `--unified=${CITATION_WINDOW_LINES}`, `${base}...${head}`]);
  if (res.status !== 0) {
    throw new Error(`git diff ${base}...${head} failed: ${res.stderr.trim()}`);
  }
  return res.stdout;
}

function apiHeaders(token) {
  return {
    Accept: 'application/vnd.github+json',
    Authorization: `Bearer ${token}`,
    'X-GitHub-Api-Version': '2022-11-28',
  };
}

// probeStatus — one GET reduced to its status line. The capability probe asks
// only whether a read was permitted, so it must not depend on a payload shape.
async function probeStatus(url, token) {
  const res = await fetch(url, { headers: apiHeaders(token) });
  return { ok: res.ok, status: res.status, statusText: res.statusText };
}

async function apiJson(url, token, what) {
  const res = await fetch(url, { headers: apiHeaders(token) });
  if (res.status === HTTP_NOT_FOUND) return null;
  if (!res.ok) {
    throw new Error(`${what}: HTTP ${res.status} ${res.statusText} for ${url}`);
  }
  return res.json();
}

// descriptionSurface — the PR description, however this event can reach it.
// On a pull_request run it is the event payload. On a push there is no payload
// body, so the associated pull request is resolved from the head commit; a
// commit with none (a maintenance cherry-pick) yields an EXPLICITLY empty
// surface, recorded as such rather than silently skipped.
export async function descriptionSurface({ eventName, repo, head, token, apiBase }) {
  if (eventName === 'pull_request' || eventName === 'pull_request_target') {
    if (process.env.PR_BODY === undefined) {
      throw new Error(
        'PR_BODY is unset on a pull_request run — the description surface would go uninspected. ' +
          'Wire `PR_BODY: ${{ github.event.pull_request.body }}` into the step env.',
      );
    }
    return {
      text: process.env.PR_BODY,
      origin: 'the pull_request event payload',
      author: eventPayloadAuthor(process.env.GITHUB_EVENT_PATH),
    };
  }
  const pulls = await apiJson(
    `${apiBase}/repos/${repo}/commits/${head}/pulls`,
    token,
    'list pull requests for the head commit',
  );
  if (pulls === null || !Array.isArray(pulls)) {
    // Same 404-means-two-things shape as the issue lookup, one layer up: a
    // commit with no associated pull request answers 200 with an empty array,
    // so a 404 here is the endpoint being unreadable rather than the commit
    // being unassociated, and the description surface must not silently
    // collapse to empty on a permission fault.
    throw new Error(
      `could not list the pull requests associated with ${head} — the endpoint answered 404, ` +
        'which for this call means the token may not read it rather than that the commit has ' +
        'no pull request (a commit with none answers 200 and an empty list). The job needs ' +
        '`pull-requests: read`.',
    );
  }
  if (pulls.length === 0) {
    return {
      text: '',
      origin: `explicitly empty: no pull request is associated with ${head.slice(0, 12)}`,
      author: null,
    };
  }
  return {
    text: pulls.map((p) => p.body ?? '').join('\n\n'),
    origin: `the description of ${pulls.map((p) => `#${p.number}`).join(', ')}`,
    author: sharedAuthor(pulls),
  };
}

async function resolveRefs(numbers, { repo, token, apiBase }) {
  const resolved = new Map();
  // The capability probe runs at most once per run, and only once a lookup has
  // actually come back empty: a run whose every citation resolves has nothing
  // to disambiguate and pays nothing, and the answer for the first unresolvable
  // number is the answer for all of them.
  let probed = false;
  for (const n of numbers) {
    const issue = await apiJson(`${apiBase}/repos/${repo}/issues/${n}`, token, `resolve #${n}`);
    if (issue === null) {
      if (!probed) {
        probed = true;
        const capability = await issuesReadability({
          repo,
          apiBase,
          get: (url) => probeStatus(url, token),
        });
        if (!capability.readable) throw new Error(capability.reason);
      }
      resolved.set(n, { kind: 'missing' });
      continue;
    }
    resolved.set(n, {
      kind: issue.pull_request ? 'pull-request' : 'issue',
      state: issue.state,
    });
  }
  return resolved;
}

// Three remedies, because the marker has three honest causes and only one of
// them is a deferral. Naming just the first taught authors to manufacture an
// issue for work that was already finished — a gate that inflates the backlog
// with fiction discredits the policy it exists to enforce.
const REMEDY =
  'Remedy, whichever is true. (1) The work is genuinely outstanding: open an ' +
  'issue for it (`gh issue create`) and cite it as #<n> — a description ' +
  'sentence is not a resting place, because once the pull request merges it ' +
  'appears in no list and nobody is assigned. (2) This change already DID the ' +
  'work: say so in the past tense ("Resolved in this change: ...") — do not ' +
  'file an issue for finished work. (3) Nothing is owed at all: delete the line.';

async function main() {
  if (DEFERRAL_MARKERS.length === 0) {
    throw new Error('DEFERRAL_MARKERS is empty — the scan would be vacuous');
  }
  for (const m of DEFERRAL_MARKERS) {
    markerRegex(m.pattern); // throws on a malformed pattern
  }

  const repo = requireEnv('GITHUB_REPOSITORY', 'the gate cannot resolve a cited issue without it');
  const token = requireEnv('GITHUB_TOKEN', 'resolving a cited issue needs issues:read');
  const eventName = requireEnv('GITHUB_EVENT_NAME', 'the description surface is resolved per event');
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';

  const { base, head } = resolveRange({
    baseSha: process.env.BASE_SHA,
    headSha: process.env.HEAD_SHA,
  });

  const commits = commitsIn(base, head);
  if (commits.length === 0) {
    throw new Error(
      `${base.slice(0, 12)}..${head.slice(0, 12)} resolved to zero commits — the gate would ` +
        'inspect no change at all',
    );
  }

  const diffText = diffOf(base, head);
  const { files } = parseDiff(diffText);
  if (files.length === 0) {
    throw new Error(
      `${base.slice(0, 12)}...${head.slice(0, 12)} parsed to an empty file set — either the ` +
        'range is wrong or the diff parser stopped understanding git output',
    );
  }

  const description = await descriptionSurface({ eventName, repo, head, token, apiBase });

  const candidates = candidatesFor({
    description: description.text,
    author: description.author,
    commits,
    diffText,
    repoSlug: repo,
  });

  const cited = [...new Set(candidates.flatMap((c) => c.refs))].sort((a, b) => a - b);
  const resolved = await resolveRefs(cited, { repo, token, apiBase });
  const violations = evaluate(candidates, resolved);

  // The anti-vacuity contract in the header says every run states what it
  // inspected. A surface that was resolved but deliberately not read has to say
  // so in the same breath, or the summary claims a scan that did not happen.
  const descriptionRead = scansDescription(description.author);
  const inspected = [
    `- description: ${description.origin} (${description.text.length} chars)` +
      (descriptionRead
        ? ''
        : ` — NOT scanned: written by ${GENERATED_DESCRIPTION_AUTHOR}, whose body quotes ` +
          'upstream release notes rather than stating this change\'s own commitment. Its ' +
          'commit messages and its diff were scanned as normal.'),
    `- commits: **${commits.length}** in \`${base.slice(0, 12)}..${head.slice(0, 12)}\``,
    `- diff: **${files.length}** file(s) at \`--unified=${CITATION_WINDOW_LINES}\``,
    `- marker table: **${DEFERRAL_MARKERS.length}** rows`,
    `- markers found: **${candidates.length}**, issue numbers resolved: **${cited.length}**`,
  ];
  appendStepSummary(
    ['## forbid-deferral', '', ...inspected, '', violations.length === 0 ? 'No untracked deferrals.' : violations.map((v) => `- ❌ ${v.location}: ${v.detail}`).join('\n')].join('\n'),
  );
  for (const line of inspected) log(line.replace(/[*`]/g, ''));

  if (violations.length > 0) {
    for (const v of violations) {
      error(
        `${v.location}: ${v.description} ("${v.markerText}") is not tracked by an open ` +
          `issue within ${v.scope} — ${v.detail}. ${REMEDY}`,
        { title: 'forbid-deferral' },
      );
    }
    error(
      `forbid-deferral: ${violations.length} untracked deferral(s) across ` +
        `${new Set(violations.map((v) => v.surface)).size} surface(s).`,
      { title: 'forbid-deferral' },
    );
    process.exit(1);
  }

  const inspectedScale =
    `${commits.length} commit(s), ${files.length} file(s) and the description`;
  notice(
    candidates.length === 0
      ? `forbid-deferral: no untracked work is named in this change — ${inspectedScale} inspected.`
      : `forbid-deferral: ${candidates.length} marker(s) across ${inspectedScale} — ` +
        'every one tracked by an open issue.',
    { title: 'forbid-deferral' },
  );
}

// --- entrypoint --------------------------------------------------------------

// The scan runs only when this file is the program. `forbid-deferral.test.mjs`
// imports the pure halves above and would otherwise trip the environment
// assertions in main() at import time.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`forbid-deferral: ${String(e?.message ?? e)}`, { title: 'forbid-deferral' });
    process.exit(1);
  });
}
