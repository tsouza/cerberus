// perf-sentinel-obligation.mjs — the same-PR obligation gate for #2370's
// perf sentinel corpora.
//
// Repo policy, one level more specific than forbid-deferral.mjs's own
// "work becomes an issue, filed before merge" discipline: a change to
// `internal/engine/query_settings_rules.go` or `internal/engine/spill.go`
// — the two files defining which per-query settings the engine's memory-
// bounding mechanisms apply, and how — is exactly the shape of change
// #2364 traced back to. #2358 silently removed a CSE fold those settings
// relied on; production hit MEMORY_LIMIT_EXCEEDED 6h8m later (#2364),
// because nothing in CI measured real memory at real scale. test/perf/
// smoke's sentinel corpus (sentinels.go) and #2370's nightly one
// (test/perf/nightly/sentinels.go) exist specifically to catch this class
// before it reaches production — a change to the settings machinery that
// adds no new sentinel coverage risks shipping in exactly the state #2364
// did.
//
// So: a diff touching either settings file must ALSO touch one of the two
// sentinel files, or cite a waiver — `PERF-SENTINEL-WAIVER: #<issue>` —
// naming an OPEN issue tracking why new coverage genuinely isn't owed by
// this change (a pure refactor with no behavioural change, verified by a
// human reviewer, is the shape this exists for; this gate cannot make
// that judgement itself, only require someone did).
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
// GITHUB_EVENT_PATH, BASE_SHA, HEAD_SHA, GITHUB_API_URL.
//
// The pure halves of this module are exported and pinned by
// perf-sentinel-obligation.test.mjs; the scan itself runs only when this
// file is invoked as the program.
//
// Exit codes: 0 = the trigger files were not touched, or were touched
// alongside sentinel coverage or a valid open-issue waiver; 1 = touched
// with neither, or a malformed input.

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
} from './forbid-deferral.mjs';

// TRIGGER_FILES — touching any of these is what obligates new sentinel
// coverage (or a waiver). Repo-root-relative paths, exactly as `git diff`
// reports them.
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
export function waiverRefs(text) {
  const src = String(text ?? '');
  const numbers = new Set();
  for (const m of src.matchAll(WAIVER_PATTERN)) numbers.add(Number(m[1]));
  return [...numbers].sort((a, b) => a - b);
}

// needsObligation — does this changed-file set touch a trigger file?
export function needsObligation(files) {
  return (files ?? []).some((f) => TRIGGER_FILES.includes(f));
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
export function verdict({ files, waiverNumbers, resolved }) {
  if (!needsObligation(files)) {
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
  'This change touches a memory-bounding settings file '
  + `(${TRIGGER_FILES.join(' or ')}) without touching either sentinel corpus `
  + `(${SENTINEL_FILES.join(' or ')}). Remedy, whichever is true: (1) add a `
  + 'sentinel that would have caught this class of change (see test/perf/smoke/'
  + 'sentinels.go or test/perf/nightly/sentinels.go for the shape); or (2) if '
  + 'new coverage is genuinely not owed here — a pure refactor with no '
  + 'behavioural change, verified by review — open an issue saying so and cite '
  + 'it as `PERF-SENTINEL-WAIVER: #<issue>` in the PR description or a commit '
  + 'message.';

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

  if (!needsObligation(files)) {
    notice(
      `perf-sentinel-obligation: neither ${TRIGGER_FILES.join(' nor ')} was touched — no obligation.`,
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

  const v = verdict({ files, waiverNumbers, resolved });

  const inspected = [
    `- trigger files touched: ${TRIGGER_FILES.filter((f) => files.includes(f)).join(', ')}`,
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
