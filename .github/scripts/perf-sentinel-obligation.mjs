// perf-sentinel-obligation.mjs — the same-PR obligation gate for #2370's
// perf sentinel corpora.
//
// Repo policy, one level more specific than forbid-deferral.mjs's own
// "work becomes an issue, filed before merge" discipline: a change to the
// engine's MEMORY-BOUNDING MECHANISM — the settings whose stamped value
// caps how much RAM a query may build before it spills or how many
// buffered read lanes it may open — is exactly the shape of change #2364
// traced back to. #2358 silently removed a CSE fold those settings relied
// on; production hit MEMORY_LIMIT_EXCEEDED 6h8m later (#2364), because
// nothing in CI measured real memory at real scale. test/perf/smoke's
// sentinel corpus (sentinels.go) and #2370's nightly one
// (test/perf/nightly/sentinels.go) exist specifically to catch this class
// before it reaches production — a change to that machinery that adds no
// new sentinel coverage risks shipping in exactly the state #2364 did.
//
// So: a diff that adds or alters a memory-bounding mechanism must ALSO
// touch one of the two sentinel files, or cite a waiver —
// `PERF-SENTINEL-WAIVER: #<issue>` — naming an OPEN issue tracking why
// new coverage genuinely isn't owed by this change (a pure refactor with
// no behavioural change, verified by a human reviewer, is the shape this
// exists for; this gate cannot make that judgement itself, only require
// someone did).
//
// WHAT "A MEMORY-BOUNDING MECHANISM" MEANS, and why it is not a filename
// (#2893). This gate originally obligated on whole-FILE membership: any
// edit to internal/engine/query_settings_rules.go or spill.go owed a
// sentinel or a waiver. Those two files also carry every RESULT-EQUIVALENT
// optimizer knob the engine stamps — the condition cache, lazy
// materialisation, the projection-index threshold, the log_comment shape
// id, the workload tag — none of which can move a query's peak memory at
// all. A comment fix, a rename, or a new query-tag setting therefore owed
// a waiver it could never honestly earn, and the only way to clear the
// gate was to open an issue whose entire content was the receipt. Three
// such issues were minted in one week (#2832, #2833, #2849) for zero
// engineering content, which is the per-leg pattern this repository's own
// discipline forbids.
//
// The obligation is now scoped by MECHANISM CLASS, derived from the trigger
// files' own source rather than declared in this script:
//
//   1. Every ClickHouse setting the engine stamps from those files is a
//      `setting<Name>` const, and each carries a `perf-sentinel:` doc-comment
//      classification — `memory-bounding` or `neutral` — stating which class
//      it belongs to and why. That comment is the single source of truth; a
//      const with no classification FAILS this gate rather than being assumed
//      harmless, so a new setting cannot slip in unclassified.
//   2. `memoryBoundingSurface` closes over the file's own reference graph
//      from those memory-bounding consts: a declaration that STAMPS one is
//      part of the mechanism, and so is every declaration it reaches to
//      compute or gate that stamp (`spillThreshold`, its two byte constants,
//      the plan predicates). Nothing here is a hand-maintained list — repoint
//      the code and the surface follows it.
//   3. A change obligates when one of its own changed lines — added OR
//      removed, code only, comments stripped — names something in that
//      surface. A comment-only edit, a neutral-setting edit and a rename of
//      unrelated machinery all leave the surface untouched and owe nothing.
//
// Two structural guards keep that derivation from going quietly vacuous:
// the gate fails if the surface resolves to nothing at all, and it fails if
// a `WithQuerySetting` call in a trigger file passes a bare string literal
// instead of a classified `setting<Name>` const — which would otherwise be
// a memory bound the classification never saw.
//
// WHAT IS SCANNED for the waiver citation — the same two surfaces
// forbid-deferral.mjs reads for a marker's citation, minus the diff
// itself: the pull request description and the commit messages in the
// range base..head. Not the diff: `PERF-SENTINEL-WAIVER:` is a statement
// ABOUT the change, not code, so requiring it in a source comment would
// force an otherwise-unnecessary edit into files this gate is trying to
// keep additions-only.
//
// Citation resolution reuses forbid-deferral.mjs's own machinery
// verbatim — resolveRefs, issuesReadability's 404-ambiguity guard,
// descriptionSurface's per-event-type resolution, commitsIn, diffOf —
// rather than re-deriving any of it. See forbid-deferral.mjs's own header
// for the full "why" behind each piece (the capability probe, the
// per-part description attribution, the 404-means-two-things problem).
//
// Env contract: identical to forbid-deferral.mjs's (see its header) —
// GITHUB_REPOSITORY, GITHUB_TOKEN, GITHUB_EVENT_NAME, PR_BODY,
// GITHUB_EVENT_PATH, BASE_SHA, HEAD_SHA, GITHUB_API_URL. The trigger files
// themselves are read from the checkout at HEAD, which is the tree the
// classification must be judged against.
//
// The pure halves of this module are exported and pinned by
// perf-sentinel-obligation.test.mjs; the scan itself runs only when this
// file is invoked as the program.
//
// Exit codes: 0 = no memory-bounding mechanism was added or altered, or it
// was alongside sentinel coverage or a valid open-issue waiver; 1 = altered
// with neither, or a malformed/unclassifiable input.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { appendStepSummary, error, log, notice } from './lib/gh.mjs';
import {
  commitsIn,
  descriptionSurface,
  diffOf,
  parseDiff,
  resolveRange,
  resolveRefs,
  stripFencedBlocks,
} from './forbid-deferral.mjs';

// TRIGGER_FILES — the files the memory-bounding surface is derived FROM.
// Touching one is necessary but no longer sufficient (#2893): the changed
// lines must also name something in the surface those files declare.
// Repo-root-relative paths, exactly as `git diff` reports them.
export const TRIGGER_FILES = Object.freeze([
  'internal/engine/query_settings_rules.go',
  'internal/engine/spill.go',
]);

// SENTINEL_FILES — touching either satisfies the obligation directly, no
// waiver needed. Both corpora are named (not just one) because either is
// real, current coverage: a change that adds a smoke-tier sentinel is
// exactly as much new coverage as one that adds a nightly one.
export const SENTINEL_FILES = Object.freeze([
  'test/perf/smoke/sentinels.go',
  'test/perf/nightly/sentinels.go',
]);

// WAIVER_PATTERN matches the single accepted waiver shape: the label,
// immediately followed by its own citation — unlike forbid-deferral.mjs's
// markers, which are found first and matched to a NEARBY citation, this
// marker self-contains its citation by construction, so there is nothing
// to search a window around.
export const WAIVER_PATTERN = /PERF-SENTINEL-WAIVER:\s*#(\d+)\b/g;

// waiverRefs — every #<n> a PERF-SENTINEL-WAIVER: label cites in `text`.
//
// Strips fenced code blocks first, for the exact reason
// forbid-deferral.mjs's own scanProse does before it scans for a deferral
// marker: a fenced block in a description or commit message is QUOTED
// material (a paste of a revert's original message, a template, this very
// gate's own remedy text) rather than the author's own commitment, so a
// waiver label sitting inside one must not satisfy the gate.
export function waiverRefs(text) {
  const src = stripFencedBlocks(text);
  const numbers = new Set();
  for (const m of src.matchAll(WAIVER_PATTERN)) numbers.add(Number(m[1]));
  return [...numbers].sort((a, b) => a - b);
}

// touchesTriggerFile — does this changed-file set touch a trigger file at
// all? The cheap pre-filter: when it is false there is nothing to parse and
// nothing to classify, which is the overwhelmingly common case.
export function touchesTriggerFile(files) {
  return (files ?? []).some((f) => TRIGGER_FILES.includes(f));
}

// --- the memory-bounding surface --------------------------------------------

// SETTING_CONST_PATTERN — the identifier shape every ClickHouse setting name
// in the trigger files is bound to. The `setting` prefix is the convention
// those files already follow for all eleven of them; requiring it is what
// makes "did anyone add a setting without classifying it?" answerable.
export const SETTING_CONST_PATTERN = /^setting[A-Z]/;

// CLASSIFICATION_PATTERN — the doc-comment tag that classifies a setting
// const. Written on its own comment line immediately above (or inside) the
// const's doc block, e.g.
//
//   // perf-sentinel: memory-bounding — caps the aggregator's in-RAM hash
//   // table so it spills instead of aborting at max_memory_usage.
//
// The prose after the class is for the human reader; only the class word is
// read here.
export const CLASSIFICATION_PATTERN = /^\s*\/\/\s*perf-sentinel:\s*(memory-bounding|neutral)\b/;

export const CLASS_MEMORY_BOUNDING = 'memory-bounding';
export const CLASS_NEUTRAL = 'neutral';

// TOP_LEVEL_DECL_PATTERN — a Go top-level declaration opener at column 0.
// The tree is gofumpt-formatted (`just fmt`), so every top-level decl starts
// flush left and every block-form decl closes with a flush-left `}` or `)`;
// that is the whole grammar this needs.
const TOP_LEVEL_DECL_PATTERN = /^(?:func|type|const|var)\b/;

// GROUPED_DECL_OPEN_PATTERN — `const (` / `var (`, whose members are each a
// declaration in their own right.
const GROUPED_DECL_OPEN_PATTERN = /^(?:const|var)\s*\($/;

// A Go identifier, for the reference scan. Package-qualified names such as
// `chplan.WalkDeep` still yield `chplan` and `WalkDeep` as separate words;
// neither is a local declaration, so neither joins the graph.
const IDENTIFIER_PATTERN = /[A-Za-z_][A-Za-z0-9_]*/g;

// stripComments — the code half of a Go line. Everything from `//` onward is
// prose: it can mention `spillThreshold` all it likes without being part of
// the mechanism, which is precisely the misfire #2893 is about.
export function stripComments(line) {
  const at = String(line ?? '').indexOf('//');
  return at === -1 ? String(line ?? '') : String(line).slice(0, at);
}

function declName(header) {
  // `func (r SettingsRules) apply(` -> apply; `func spillThreshold(` -> spillThreshold
  const method = /^func\s*\([^)]*\)\s*([A-Za-z_][A-Za-z0-9_]*)/.exec(header);
  if (method) return method[1];
  const fn = /^func\s+([A-Za-z_][A-Za-z0-9_]*)/.exec(header);
  if (fn) return fn[1];
  const other = /^(?:type|const|var)\s+([A-Za-z_][A-Za-z0-9_]*)/.exec(header);
  if (other) return other[1];
  return null;
}

/**
 * parseGoDecls — every top-level declaration in one gofumpt-formatted Go
 * source, as `{ name, doc, body }`. `doc` is the contiguous run of `//` lines
 * immediately above the declaration; `body` is its code text with comments
 * stripped. Members of a `const (` / `var (` group are returned individually,
 * each with the doc comment written directly above it inside the group.
 *
 * This is deliberately a line grammar rather than a Go parser: the two files
 * it reads are formatted by `just fmt` on every commit, and a full parser
 * would be a dependency this gate's "node: builtins only" contract does not
 * allow. `memoryBoundingSurface` fails loudly rather than silently narrowing
 * if the grammar ever stops matching (see `surfaceViolations`).
 */
export function parseGoDecls(src) {
  const lines = String(src ?? '').split('\n');
  const decls = [];
  let doc = [];
  let i = 0;

  const flushDoc = () => {
    const d = doc;
    doc = [];
    return d;
  };

  while (i < lines.length) {
    const line = lines[i];

    if (/^\s*\/\//.test(line)) {
      doc.push(line);
      i += 1;
      continue;
    }
    if (line.trim() === '') {
      doc = [];
      i += 1;
      continue;
    }

    if (GROUPED_DECL_OPEN_PATTERN.test(line)) {
      // The doc above `const (` documents every member that carries no doc of
      // its own — the Go convention the two trigger files already follow for
      // the group-by/sort spill pair, which shares one paragraph.
      const groupDoc = flushDoc();
      i += 1;
      let memberDoc = [];
      while (i < lines.length && lines[i] !== ')') {
        const member = lines[i];
        if (/^\s*\/\//.test(member)) {
          memberDoc.push(member);
        } else if (member.trim() === '') {
          memberDoc = [];
        } else {
          const name = /^\s*([A-Za-z_][A-Za-z0-9_]*)/.exec(member)?.[1] ?? null;
          if (name) {
            decls.push({
              name,
              doc: memberDoc.length > 0 ? memberDoc : groupDoc,
              body: stripComments(member),
            });
          }
          memberDoc = [];
        }
        i += 1;
      }
      i += 1;
      continue;
    }

    if (TOP_LEVEL_DECL_PATTERN.test(line)) {
      const header = line;
      const declDoc = flushDoc();
      const body = [stripComments(header)];
      // A block-form declaration closes at the next flush-left `}` or `)`.
      if (/[{(]\s*$/.test(header)) {
        i += 1;
        while (i < lines.length && lines[i] !== '}' && lines[i] !== ')') {
          body.push(stripComments(lines[i]));
          i += 1;
        }
      }
      i += 1;
      const name = declName(header);
      if (name) decls.push({ name, doc: declDoc, body: body.join('\n') });
      continue;
    }

    flushDoc();
    i += 1;
  }

  return decls;
}

/** classificationOf — the `perf-sentinel:` class a declaration's doc declares, or null. */
export function classificationOf(decl) {
  for (const line of decl?.doc ?? []) {
    const m = CLASSIFICATION_PATTERN.exec(line);
    if (m) return m[1];
  }
  return null;
}

/**
 * surfaceViolations — the structural defects that would make the derived
 * surface a lie, as printable strings. Any one of these fails the gate
 * outright: a surface that resolves to nothing, or a classification that was
 * never made, is a gate that passes everything.
 *
 * `sources` is `{ [path]: contents }` for the trigger files at HEAD.
 */
export function surfaceViolations(sources) {
  const problems = [];
  for (const path of TRIGGER_FILES) {
    const src = sources?.[path];
    if (typeof src !== 'string' || src.trim() === '') {
      problems.push(`${path} could not be read at HEAD, so its memory-bounding surface is unknown`);
      continue;
    }
    for (const decl of parseGoDecls(src)) {
      if (!SETTING_CONST_PATTERN.test(decl.name)) continue;
      if (classificationOf(decl) === null) {
        problems.push(
          `${path}: const ${decl.name} carries no \`// perf-sentinel: memory-bounding|neutral\` `
            + 'classification, so this gate cannot tell whether changing it bounds memory',
        );
      }
    }
    // A memory bound stamped through a bare string literal would never reach
    // the classification at all, so the literal form is rejected outright.
    for (const [, literal] of src.matchAll(/WithQuerySetting\([^,]+,\s*"([^"]+)"/g)) {
      problems.push(
        `${path}: WithQuerySetting stamps the bare literal "${literal}" — bind it to a classified `
          + '`setting<Name>` const so its memory class is declared',
      );
    }
  }
  return problems;
}

/**
 * memoryBoundingSurface — the set of identifiers that ARE the memory-bounding
 * mechanism, derived from the trigger files' own reference graph.
 *
 * Seeded with the setting consts classified `memory-bounding`, then closed in
 * BOTH directions until it stops growing:
 *
 *   - upward: a declaration whose body names something already in the surface
 *     STAMPS a memory bound, so it is part of the mechanism (`applySpillSettings`
 *     naming `settingMaxBytesBeforeExternalGroupBy`);
 *   - downward: a declaration named BY something in the surface computes or
 *     gates that bound, so it is part of the mechanism too (`spillThreshold`,
 *     reached from `applySpillSettings`, then `spillCapDenominator` and
 *     `spillThresholdBytes` reached from it).
 *
 * Both directions are needed and neither over-reaches: the neutral rules stamp
 * neutral consts and share no helper with the bounding ones, so the closure
 * terminates well short of the whole file. `surfaceIsNeutralOf` in the test
 * suite pins exactly that.
 */
export function memoryBoundingSurface(sources) {
  const decls = [];
  for (const path of TRIGGER_FILES) {
    for (const d of parseGoDecls(sources?.[path] ?? '')) decls.push(d);
  }
  const byName = new Map(decls.map((d) => [d.name, d]));
  const refs = new Map(
    decls.map((d) => [
      d.name,
      new Set([...String(d.body).matchAll(IDENTIFIER_PATTERN)]
        .map((m) => m[0])
        .filter((w) => w !== d.name && byName.has(w))),
    ]),
  );

  const surface = new Set(
    decls
      .filter((d) => SETTING_CONST_PATTERN.test(d.name) && classificationOf(d) === CLASS_MEMORY_BOUNDING)
      .map((d) => d.name),
  );

  for (let grew = true; grew; ) {
    grew = false;
    for (const d of decls) {
      if (surface.has(d.name)) {
        for (const r of refs.get(d.name)) {
          if (!surface.has(r)) {
            surface.add(r);
            grew = true;
          }
        }
        continue;
      }
      for (const r of refs.get(d.name)) {
        if (surface.has(r)) {
          surface.add(d.name);
          grew = true;
          break;
        }
      }
    }
  }

  return surface;
}

/**
 * changedCodeLines — every line a unified diff ADDS or REMOVES in one of
 * `paths`, with comments stripped.
 *
 * forbid-deferral.mjs's `parseDiff` deliberately drops removed lines (a
 * deletion cannot introduce a deferral marker) and is shared by that gate's
 * citation-window scan, so widening it there would weaken a different gate.
 * Here a DELETION is the most dangerous edit there is — #2364's postmortem is
 * a removal — so this walks the diff on its own terms.
 */
export function changedCodeLines(diffText, paths) {
  const want = new Set(paths ?? []);
  const out = [];
  let file = null;
  for (const raw of String(diffText ?? '').split('\n')) {
    if (raw.startsWith('diff --git ')) {
      file = null;
      continue;
    }
    if (raw.startsWith('+++ ')) {
      const p = raw.slice(4).trim();
      file = p === '/dev/null' ? null : p.replace(/^b\//, '');
      continue;
    }
    if (raw.startsWith('--- ') || raw.startsWith('@@') || raw.startsWith('\\')) continue;
    if (file === null || !want.has(file)) continue;
    if (raw.startsWith('+') || raw.startsWith('-')) {
      const code = stripComments(raw.slice(1)).trim();
      if (code !== '') out.push({ file, code, added: raw.startsWith('+') });
    }
  }
  return out;
}

/**
 * mechanismEdits — the changed lines that name something in the surface,
 * i.e. the evidence that this change actually touched a memory bound.
 */
export function mechanismEdits(changed, surface) {
  return (changed ?? []).filter((l) =>
    [...String(l.code).matchAll(IDENTIFIER_PATTERN)].some((m) => surface.has(m[0])),
  );
}

// needsObligation — does this change add or alter a memory-bounding
// mechanism? `files` is the changed-file set, `edits` the surface-naming
// changed lines within the trigger files.
export function needsObligation(files, edits) {
  if (!touchesTriggerFile(files)) return false;
  return (edits ?? []).length > 0;
}

// satisfiesViaSentinel — did the SAME changed-file set also touch a
// sentinel corpus?
export function satisfiesViaSentinel(files) {
  return (files ?? []).some((f) => SENTINEL_FILES.includes(f));
}

// verdict — the pure decision, given the changed files and every waiver
// number found across the scanned surfaces, each resolved to its
// kind/state via the SAME resolution forbid-deferral.mjs uses.
// `resolved` maps number -> { kind: 'missing' | 'pull-request' | 'issue',
// state }, exactly resolveRefs's own return shape.
export function verdict({ files, edits, waiverNumbers, resolved }) {
  if (!needsObligation(files, edits)) {
    return { obligated: false };
  }
  if (satisfiesViaSentinel(files)) {
    return { obligated: true, satisfied: true, via: 'sentinel' };
  }
  const reasons = [];
  for (const n of waiverNumbers ?? []) {
    const r = resolved?.get(n);
    if (!r || r.kind === 'missing') {
      reasons.push(`#${n} names nothing in this repository`);
      continue;
    }
    if (r.kind === 'pull-request') {
      reasons.push(`#${n} is a pull request, not an issue`);
      continue;
    }
    if (r.state !== 'open') {
      reasons.push(`#${n} is a ${r.state} issue, so it does not track an active waiver`);
      continue;
    }
    return { obligated: true, satisfied: true, via: 'waiver', number: n };
  }
  return {
    obligated: true,
    satisfied: false,
    reason:
      waiverNumbers && waiverNumbers.length > 0
        ? reasons.join('; ')
        : 'no PERF-SENTINEL-WAIVER: #<issue> citation was found in the description or commit messages',
  };
}

const REMEDY =
  'This change adds or alters a memory-bounding mechanism in '
  + `${TRIGGER_FILES.join(' or ')} without touching either sentinel corpus `
  + `(${SENTINEL_FILES.join(' or ')}). Remedy, whichever is true: (1) add a `
  + 'sentinel that would have caught this class of change (see test/perf/smoke/'
  + 'sentinels.go or test/perf/nightly/sentinels.go for the shape); (2) if the '
  + 'setting the changed lines name does NOT bound memory, say so on its const '
  + 'as `// perf-sentinel: neutral — <why>` and the obligation disappears at '
  + 'the root instead of needing a receipt; or (3) if new coverage is genuinely '
  + 'not owed for a real memory bound — a pure refactor with no behavioural '
  + 'change, verified by review — open an issue saying so and cite it as '
  + '`PERF-SENTINEL-WAIVER: #<issue>` in the PR description or a commit message.';

// MAX_REPORTED_EDITS caps how many surface-naming lines the step summary
// quotes back. The list is evidence for the reader ("this is why you were
// obligated"), not a transcript of the diff, and a wholesale rewrite of
// spill.go would otherwise bury the verdict under its own body.
const MAX_REPORTED_EDITS = 5;

async function main() {
  const repo = process.env.GITHUB_REPOSITORY;
  if (!repo) throw new Error('GITHUB_REPOSITORY is unset — the gate cannot resolve a cited waiver without it');
  const token = process.env.GITHUB_TOKEN;
  if (!token) throw new Error('GITHUB_TOKEN is unset — resolving a cited waiver needs issues:read');
  const eventName = process.env.GITHUB_EVENT_NAME;
  if (!eventName) throw new Error('GITHUB_EVENT_NAME is unset — the description surface is resolved per event');
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';

  const { base, head } = resolveRange({
    baseSha: process.env.BASE_SHA,
    headSha: process.env.HEAD_SHA,
  });

  const commits = commitsIn(base, head);
  if (commits.length === 0) {
    throw new Error(
      `${base.slice(0, 12)}..${head.slice(0, 12)} resolved to zero commits — the gate would `
        + 'inspect no change at all',
    );
  }

  const diffText = diffOf(base, head);
  const { files } = parseDiff(diffText);
  if (files.length === 0) {
    throw new Error(
      `${base.slice(0, 12)}...${head.slice(0, 12)} parsed to an empty file set — either the `
        + 'range is wrong or the diff parser stopped understanding git output',
    );
  }

  if (!touchesTriggerFile(files)) {
    notice(
      `perf-sentinel-obligation: neither ${TRIGGER_FILES.join(' nor ')} was touched — no obligation.`,
      { title: 'perf-sentinel-obligation' },
    );
    return;
  }

  // From here the classification is load-bearing, so it is validated before
  // it is trusted: an unclassified setting or an unreadable trigger file
  // makes the surface a guess, and a guessing gate is worse than none.
  const sources = Object.fromEntries(
    TRIGGER_FILES.map((p) => {
      try {
        return [p, readFileSync(p, 'utf8')];
      } catch {
        return [p, null];
      }
    }),
  );
  const problems = surfaceViolations(sources);
  if (problems.length > 0) {
    for (const p of problems) error(`perf-sentinel-obligation: ${p}`, { title: 'perf-sentinel-obligation' });
    process.exit(1);
  }

  const surface = memoryBoundingSurface(sources);
  if (surface.size === 0) {
    error(
      'perf-sentinel-obligation: the memory-bounding surface derived from '
        + `${TRIGGER_FILES.join(' and ')} is EMPTY, so nothing could ever obligate a sentinel. `
        + 'Either every setting is now classified neutral (say so deliberately) or the derivation '
        + 'stopped matching the source.',
      { title: 'perf-sentinel-obligation' },
    );
    process.exit(1);
  }

  const edits = mechanismEdits(changedCodeLines(diffText, TRIGGER_FILES), surface);
  if (edits.length === 0) {
    notice(
      `perf-sentinel-obligation: ${TRIGGER_FILES.filter((f) => files.includes(f)).join(', ')} changed, `
        + 'but no changed line names the memory-bounding surface — no obligation.',
      { title: 'perf-sentinel-obligation' },
    );
    return;
  }

  const description = await descriptionSurface({ eventName, repo, head, token, apiBase });
  const waiverText = [
    ...description.parts.map((p) => p.text),
    ...commits.map((c) => c.message),
  ].join('\n\n');
  const waiverNumbers = waiverRefs(waiverText);
  const resolved = waiverNumbers.length > 0
    ? await resolveRefs(waiverNumbers, { repo, token, apiBase })
    : new Map();

  const v = verdict({ files, edits, waiverNumbers, resolved });

  const inspected = [
    `- trigger files touched: ${TRIGGER_FILES.filter((f) => files.includes(f)).join(', ')}`,
    `- memory-bounding surface: ${[...surface].sort().join(', ')}`,
    `- changed lines naming it: ${edits.length} (${edits
      .slice(0, MAX_REPORTED_EDITS)
      .map((e) => `${e.added ? '+' : '-'}${e.code}`)
      .join(' | ')})`,
    `- sentinel files touched: ${SENTINEL_FILES.filter((f) => files.includes(f)).join(', ') || '(none)'}`,
    `- waiver citations found: ${waiverNumbers.length > 0 ? waiverNumbers.map((n) => `#${n}`).join(', ') : '(none)'}`,
  ];
  appendStepSummary(
    [
      '## perf-sentinel-obligation',
      '',
      ...inspected,
      '',
      v.satisfied ? `Satisfied via ${v.via}${v.number ? ` (#${v.number})` : ''}.` : `Not satisfied: ${v.reason}`,
    ].join('\n'),
  );
  for (const line of inspected) log(line.replace(/[*`]/g, ''));

  if (!v.satisfied) {
    error(`perf-sentinel-obligation: ${v.reason}. ${REMEDY}`, { title: 'perf-sentinel-obligation' });
    process.exit(1);
  }

  notice(
    `perf-sentinel-obligation: satisfied via ${v.via}${v.number ? ` (#${v.number})` : ''}.`,
    { title: 'perf-sentinel-obligation' },
  );
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`perf-sentinel-obligation: ${String(e?.message ?? e)}`, { title: 'perf-sentinel-obligation' });
    process.exit(1);
  });
}
