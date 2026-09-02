// forbid-contradicted-mutants.mjs — one mutant, one adjudication. A mutant
// documented as EQUIVALENT may not also be claimed as KILLED.
//
// WHY THIS EXISTS (#2958). `docs/test-strategy.md` § "Surviving-mutant policy"
// gives a surviving mutant exactly two remedies: prove it equivalent and
// document it (remedy 1, preferred), or add a test that genuinely
// distinguishes original from mutant (remedy 2). The two are mutually
// exclusive verdicts on one mutant. When both are recorded, one of them is
// false — and the false one is almost always the kill, because a `kills`
// header costs nothing to write and nothing checks it.
//
// Two such pairs were on `main` in a single file, both adjudicating mutants
// that the mutation lane records as LIVED:
//
//   - exemplars.go:`len(groupAliases)*2+6` — a slice CAPACITY hint. The
//     "kill" conceded in its own prose that it gave the tool "a path to call
//     it dead anyway". The whole internal/chsql package passes with the
//     mutation applied.
//   - exemplars.go:`if maxPerSeries > 0` — the "kill" asserted the `>= 0`
//     mutant "would emit `LIMIT 0 BY ...`". It does not: Limit(0) leaves
//     QueryBuilder's hasLimit false, so the mutant is byte-identical.
//
// A PROSE gate cannot find these. The first conceded in words and the second
// did not — its prose was confident and simply wrong — so any grep for
// hedging phrasings ("plausibly equivalent", "gives gremlins a path") finds
// one and misses the other. Every such phrasing on `main` occurs exactly
// once, in that one comment. The signature is STRUCTURAL: the same mutant
// carrying two opposite verdicts. That is what this gate checks.
//
// WHAT MAKES IT PRECISE is #2953's construct citations. Both verdicts already
// name the mutant they adjudicate as `<file>.go:`<construct>``, which
// verify-code-citations.mjs resolves to exactly one code line. Resolving both
// sides through that same resolver turns "do these two notes mean the same
// mutant" from a judgement call into a set intersection.
//
// THE UNIT OF ADJUDICATION IS A PARAGRAPH, not a comment block. A footer
// routinely adjudicates several mutants in one unbroken `//` run — one names
// three mutators across nine citations — so attributing a run's whole
// vocabulary to every citation in it would smear the evidence and defeat the
// discriminations below exactly where notes are densest.
//
// ONE CONSTRUCT IS NOT ONE MUTANT, and three refinements follow.
//
//   POSITION. `best < 0 || r < best` hosts an independent
//   CONDITIONALS_BOUNDARY per operand, one killed and one proven equivalent,
//   so a shared LINE is not a contradiction. Verdicts conflict only when their
//   constructs OVERLAP by containment — `maxPerSeries > 0` inside
//   `if maxPerSeries > 0` is one mutant at two widths, while `best < 0` and
//   `r < best` are disjoint siblings.
//
//   MUTATOR. `err != nil && srcErr == nil` carries CONDITIONALS_NEGATION
//   mutants a test kills AND an INVERT_LOGICAL mutant a footer proves
//   equivalent. Two notes naming disjoint mutators describe different mutants.
//
// A paragraph that names NO mutator is treated as matching ANY of them. That
// is the fail-closed reading — no evidence is not a defence — and it is worth
// stating plainly, because it means the discrimination above is only as good
// as the notes' own precision, and a note that names nothing gets reported.
//
// WHICH MUTATOR A NOTE IS ABOUT IS A PROPERTY OF THE NOTE, NOT OF ONE
// PARAGRAPH. A well-written footer states the rewrite once and then
// enumerates the sites it applies to:
//
//     // The same `||` -> `&&` INVERT_LOGICAL rewrite of … is EQUIVALENT at
//     // every COMPOSITE recognizer, and ten of the mutants are of that kind:
//     //
//     //     histogram_native_scalar_binop.go:expHistogramScalarBinop:`…`
//     //     …
//
// Reading the citation list in isolation says "equivalent, mutator unknown",
// which then cannot be told apart from a genuine CONDITIONALS_NEGATION kill on
// the same guard — and that is a real pair in internal/promql, both verdicts
// true. So a citation paragraph that names no mutator INHERITS its comment
// run's, and only when the run names exactly one: an ambiguous preamble lends
// nothing. This widens the evidence, never the verdict — whether a paragraph
// is a kill claim or an equivalence verdict stays strictly per paragraph, so a
// disclaimer or a neighbouring note still cannot cast one.
//
// Measured over the tree: 10 equivalence paragraphs carry citations without
// naming a mutator, and all 10 sit under a run naming exactly one, with no run
// naming more. On the kill side no paragraph qualifies at all, so the rule is
// symmetric at zero cost rather than a special case for footers.
//
// ONE MUTATOR CAN ALSO STRIKE ONE CONSTRUCT TWICE, and the gate deliberately
// does NOT try to tell those apart from prose. `len(groupAliases)*2+6` carries
// the ARITHMETIC_BASE at `*2` (equivalent) and the one at `+6` (killed — it
// panics on a negative capacity), so a kill of the second alongside a
// verdict on the first is reported even though both are true. Discriminating
// on the `` `X` -> `Y` `` rewrite a note spells was tried and rejected: two
// notes about the SAME mutant routinely spell it differently — one writes
// `` `*2` -> `/2` ``, the other quotes the whole `make(...)` line — so the
// rule bought a narrow false-positive fix at the price of silently MISSING
// real contradictions, which is the one failure this gate must not have.
//
// The remedy is the same one the POSITION refinement asks for, and it is a
// real improvement to the note rather than a suppression: narrow BOTH
// citations to the operand each is about, so `len(groupAliases)*2` and `2+6`
// name one mutant apiece. A citation covering the whole expression adjudicates
// neither.
//
// The mutator vocabulary is READ FROM `.gremlins.yaml` rather than hard-coded,
// so enabling or retiring a mutator cannot leave a stale list here silently
// mis-adjudicating. An unreadable or empty mutants block is a hard error — a
// vocabulary that quietly became empty would make every mutator set empty, and
// an empty set names nothing, which would silently WIDEN the gate.
//
// DIVISION OF LABOUR: a citation that does not resolve to exactly one line is
// not re-reported here — it is already a hard error in
// verify-code-citations.mjs, which runs immediately before this gate in the
// same job. Duplicating that failure would print every citation defect twice.
// There is no tolerance file and no allow-list.
//
// ENV CONTRACT
//   REPO_ROOT   — repository root. Default `process.cwd()`.
//   PATHSPECS   — whitespace-separated git pathspecs to scan.
//                 Default `*_test.go` (every tracked Go test file).
//
// Exit: 0 when no mutant carries two verdicts, 1 otherwise — including when
// the pathspec matched no file at all, or the vocabulary could not be read.

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import {
  units,
  parseCitations,
  resolveCited,
  matchLines,
  funcRanges,
  normalise,
  unescape,
} from './verify-code-citations.mjs';
import { error, log, notice, lsFiles } from './lib/gh.mjs';

// A kill claim, in the three shapes the tree uses: the canonical
// `// TestX kills …` header from `docs/test-strategy.md`, a sentence-initial
// `Kills …`, and `killed by TestX`. Keying only on the canonical header reads
// 224 of the tree's 269 kill-claim paragraphs and misses 45, including every
// `Kills INVERT_LOGICAL (…)` written as a second sentence.
//
// Two exclusions are deliberate. A NEGATED mention ("It does NOT kill the
// CONDITIONALS_BOUNDARY mutant …") is a disclaimer, and the correct pattern,
// so unanchored `kill`/`kills` is not a claim. And `Kills` requires the
// capital and the plural, which is what keeps the tree's `kill-switch`,
// `ch-pod-kill` and "tests that kill the LIVED mutants" out.
export const KILL_CLAIM = /\bTest[A-Za-z0-9_]+\s+kills\b|\bKills\b|\bkilled by\s+Test[A-Za-z0-9_]+/;

// An equivalence verdict. The `NOT KILLABLE` footer is the form
// `docs/test-strategy.md` prescribes, but it is not the only one in the tree:
// several packages keep a file-header ledger instead ("EQUIVALENCE verdicts
// (no killing test possible …)", "Genuinely equivalent", "provably
// EQUIVALENT"). Reading only the prescribed opener left ten verdicts
// invisible — among them a capacity-hint ARITHMETIC_BASE in internal/logql,
// the very class this gate exists for. A verdict is therefore recognised by
// what it SAYS, not by the heading it happens to sit under.
export const EQUIV_VERDICT = /\bNOT KILLABLE\b|\bequivalent\b|\bunkillable\b/i;

// footerRegion — the 1-based lines under a `NOT KILLABLE` opener, which runs
// to the end of its comment run. Everything under that heading is a verdict by
// declaration, whether or not the individual paragraph restates the word: the
// exemplars footer argues one mutant is byte-identical without ever writing
// "equivalent", and losing it would lose half of what this gate was built for.
// The opener is anchored at a line start, which is what separates a footer
// from a REFERENCE to one — "see the NOT KILLABLE note at the foot of this
// file" is a disclaimer, and the correct pattern.
export function footerRegion(lines) {
  const inFooter = new Set();
  for (const { start, end } of commentRuns(lines)) {
    for (let n = start; n <= end; n += 1) {
      const body = lines[n - 1].trim().replace(/^\/\/\s?/, '');
      if (!/^NOT KILLABLE\b/.test(body)) continue;
      for (let k = n; k <= end; k += 1) inFooter.add(k);
      break;
    }
  }
  return inFooter;
}

// commentRuns — the 1-based line bounds of every maximal run of `//` lines,
// bare separators included. A run is the whole note; `paragraphs()` splits it
// into the units a single verdict is written in.
export function commentRuns(lines) {
  const out = [];
  let start = null;
  lines.forEach((line, i) => {
    if (line.trim().startsWith('//')) {
      if (start === null) start = i + 1;
      return;
    }
    if (start !== null) out.push({ start, end: i });
    start = null;
  });
  if (start !== null) out.push({ start, end: lines.length });
  return out;
}

// paragraphs — the 1-based line ranges of every comment PARAGRAPH: a maximal
// run of `//` lines carrying text, bounded by a bare `//` or by a non-comment
// line. This is the unit a single adjudication is written in.
export function paragraphs(lines) {
  const out = [];
  let start = null;
  const flush = (end) => {
    if (start !== null) out.push({ start, end });
    start = null;
  };
  lines.forEach((line, i) => {
    const t = line.trim();
    const isText = t.startsWith('//') && t.replace(/^\/\/\s?/, '').trim() !== '';
    if (isText) {
      if (start === null) start = i + 1;
      return;
    }
    flush(i);
  });
  flush(lines.length);
  return out;
}

// paragraphText — one comment range as a single line of prose, with the `//`
// markers stripped, so a sentence that wraps reads as one string.
export function paragraphText(lines, { start, end }) {
  return lines
    .slice(start - 1, end)
    .map((l) => l.trim().replace(/^\/\/\s?/, ''))
    .join(' ');
}

// mutatorVocabulary — the enabled gremlins mutators, as the UPPER_SNAKE names
// adjudication prose spells them. `.gremlins.yaml` lists them kebab-cased under
// `mutants:`, which is `mt.String()` with `_` -> `-`, so the mapping back is
// exact rather than a guess.
export function mutatorVocabulary(root) {
  let text;
  try {
    text = readFileSync(resolve(root, '.gremlins.yaml'), 'utf8');
  } catch (cause) {
    throw new Error(`cannot read .gremlins.yaml: ${cause.message}`);
  }
  const block = /^mutants:\s*$/m.exec(text);
  if (!block) {
    throw new Error('.gremlins.yaml has no `mutants:` block — the vocabulary would be empty');
  }
  const names = new Set();
  for (const line of text.slice(block.index + block[0].length).split('\n')) {
    if (/^\S/.test(line)) break; // dedent ends the block
    const m = /^\s{2}([a-z][a-z0-9-]*):\s*$/.exec(line);
    if (m) names.add(m[1].toUpperCase().replaceAll('-', '_'));
  }
  if (names.size === 0) {
    throw new Error('.gremlins.yaml `mutants:` block named none — refusing to scan with an empty vocabulary');
  }
  return names;
}

// namedMutators — the vocabulary members a paragraph names, which is what it
// says about WHICH mutant on the cited construct it adjudicates.
export function namedMutators(text, vocabulary) {
  const out = new Set();
  for (const name of vocabulary) if (text.includes(name)) out.add(name);
  return out;
}

// maskTo — the file's lines with every line outside [start, end] blanked.
// Blanking rather than slicing preserves each surviving line's index, so
// `units()` reports true source line numbers.
function maskTo(lines, { start, end }) {
  return lines.map((line, i) => (i + 1 >= start && i + 1 <= end ? line : ''));
}

// citationsIn — every construct citation of one paragraph, resolved to the
// single code line it names. A citation that resolves to zero or many lines is
// skipped; see the division-of-labour note in the file header.
function citationsIn({ root, file, lines, range, load }) {
  const out = [];
  for (const unit of units(maskTo(lines, range))) {
    if (!unit.lines.length) continue;
    for (const c of parseCitations(unit)) {
      if (c.kind !== 'construct' || c.unterminated || !c.construct) continue;
      const rel = resolveCited({ root, from: file, path: c.path });
      if (rel === null) continue;
      const target = load(rel);
      let ranges = null;
      if (c.scope !== null) {
        const found = funcRanges(target).get(c.scope);
        if (!found) continue;
        ranges = found;
      }
      const hits = matchLines({ lines: target, construct: c.construct, ranges });
      if (hits.length !== 1) continue;
      out.push({
        cited: `${rel}#${hits[0]}`,
        construct: normalise(unescape(c.construct)),
        where: `${file}:${c.line}`,
      });
    }
  }
  return out;
}

// collectFile — the kill claims and equivalence verdicts one file records, one
// paragraph at a time. A paragraph that claims a kill is never also read as an
// equivalence verdict: a kill claim that explains why a NEARBY mutant is
// equivalent is pointing at someone else's verdict, not casting one.
export function collectFile({ root, file, load, vocabulary }) {
  const lines = readFileSync(resolve(root, file), 'utf8').split('\n');
  const footer = footerRegion(lines);
  // The mutator vocabulary of each enclosing note, for the inheritance rule
  // described in the file header. Only an unambiguous run (exactly one
  // mutator) lends it to a paragraph that names none.
  const runMutators = commentRuns(lines).map((run) => ({
    ...run,
    mutators: namedMutators(paragraphText(lines, run), vocabulary),
  }));
  const inherited = (range) => {
    const run = runMutators.find((r) => range.start >= r.start && range.end <= r.end);
    return run && run.mutators.size === 1 ? run.mutators : new Set();
  };
  const kills = [];
  const equivalents = [];
  for (const range of paragraphs(lines)) {
    const text = paragraphText(lines, range);
    const isKill = KILL_CLAIM.test(text);
    const underFooter = footer.has(range.start);
    const isEquiv = !isKill && (underFooter || EQUIV_VERDICT.test(text));
    if (!isKill && !isEquiv) continue;
    const own = namedMutators(text, vocabulary);
    const mutators = own.size > 0 ? own : inherited(range);
    for (const c of citationsIn({ root, file, lines, range, load })) {
      (isKill ? kills : equivalents).push({ ...c, mutators });
    }
  }
  return { kills, equivalents };
}

// overlaps — whether two constructs describe the same expression. Containment
// either way means one note simply quoted more surrounding text than the
// other; disjoint constructs on a shared line are sibling mutants.
export function overlaps(a, b) {
  return a.includes(b) || b.includes(a);
}

// disjoint — both sides named something, and they named different things.
function disjoint(a, b) {
  if (a.size === 0 || b.size === 0) return false;
  for (const x of a) if (b.has(x)) return false;
  return true;
}

// sameMutant — whether a kill claim and an equivalence verdict can be about one
// mutant. Naming a DIFFERENT mutator is positive evidence of different mutants;
// naming none is no evidence at all, so the pair is still reported.
export function sameMutant(kill, equiv) {
  if (!overlaps(kill.construct, equiv.construct)) return false;
  if (disjoint(kill.mutators, equiv.mutators)) return false;
  return true;
}

// scan — every contradiction across the pathspec set.
export function scan({ root = process.cwd(), pathspecs = ['*_test.go'] } = {}) {
  const files = lsFiles(pathspecs, { cwd: root });
  // A scan that matched nothing is not a clean scan, it is no scan — the same
  // fail-closed rule verify-code-citations.mjs applies to its own pathspec. A
  // typo in PATHSPECS, or a rename that empties it, would otherwise report a
  // confident green over zero files.
  if (files.length === 0) {
    throw new Error(`no file matched ${JSON.stringify(pathspecs)} — refusing to report a vacuous pass`);
  }

  const cache = new Map();
  const load = (rel) => {
    if (!cache.has(rel)) cache.set(rel, readFileSync(resolve(root, rel), 'utf8').split('\n'));
    return cache.get(rel);
  };

  const vocabulary = mutatorVocabulary(root);
  const kills = [];
  const equivalents = [];
  for (const file of files) {
    const got = collectFile({ root, file, load, vocabulary });
    kills.push(...got.kills);
    equivalents.push(...got.equivalents);
  }

  // One contradiction is one finding, however many equivalence paragraphs
  // restate the verdict — a footer and the disclaiming test that points at it
  // both count as verdicts, and reporting the same kill claim twice would read
  // as two defects.
  const violations = [];
  const seen = new Set();
  for (const k of kills) {
    for (const e of equivalents) {
      if (k.cited !== e.cited) continue;
      if (!sameMutant(k, e)) continue;
      const key = `${k.where}|${k.cited}`;
      if (seen.has(key)) continue;
      seen.add(key);
      violations.push(
        `${k.where}: claims to KILL \`${k.construct}\`, which ${e.where} documents as equivalent ` +
          `(\`${e.construct}\`). One of the two verdicts is false. Apply the mutation by hand and ` +
          `run the claimed killer: if it passes, the kill claim is the one to retire; if it fails, ` +
          `the equivalence note is. If they are genuinely different mutants on one construct, say ` +
          `which: name the mutator on both sides, or narrow each citation to the operand it is ` +
          `about.`,
      );
    }
  }
  return { files: files.length, kills: kills.length, equivalents: equivalents.length, violations };
}

function main() {
  const root = process.env.REPO_ROOT || process.cwd();
  const pathspecs = (process.env.PATHSPECS || '*_test.go').split(/\s+/).filter(Boolean);

  let result;
  try {
    result = scan({ root, pathspecs });
  } catch (err) {
    error(`forbid-contradicted-mutants: ${err.message}`);
    process.exit(1);
  }
  const { files, kills, equivalents, violations } = result;

  if (violations.length > 0) {
    for (const v of violations) error(v);
    error(
      `forbid-contradicted-mutants: ${violations.length} mutant(s) carry two opposite verdicts. ` +
        `docs/test-strategy.md § "Surviving-mutant policy" allows exactly one per mutant.`,
    );
    process.exit(1);
  }
  log(
    `forbid-contradicted-mutants: ${files} file(s), ${kills} kill claim(s) and ` +
      `${equivalents} equivalence verdict(s); no mutant carries both.`,
  );
  notice(
    `forbid-contradicted-mutants: ${kills} kill claim(s) and ${equivalents} equivalence ` +
      `verdict(s) agree across ${files} file(s)`,
  );
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) main();
