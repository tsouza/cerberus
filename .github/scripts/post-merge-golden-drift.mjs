// post-merge-golden-drift.mjs — regenerate what a merge implied and fail when
// the bytes that just landed on `main` are not the bytes the generator writes.
//
// # Why a POST-merge check, when every artefact already has a ratchet
//
// Nearly every generated artefact in this tree carries a required, PR-blocking,
// content-exact ratchet that regenerates it from source and diffs it against
// the committed file (see generated-baseline-structural-guard.mjs's header for
// the inventory). Those ratchets are strong, and they all share one blind spot:
// they run against the tree AS THE PR SAW IT.
//
// Branch protection has `require branches to be up to date before merging` OFF:
//
//   $ gh api repos/tsouza/cerberus/branches/main/protection \
//       --jq '.required_status_checks.strict'
//   false
//
// so a PR's squash-merge computes its diff against a `main` that has since
// moved, and no ratchet ever runs against the result. Several artefacts here
// are regenerated from the WHOLE tree — the parity ledgers, the cardinality
// baseline, the solver decision baseline — so their correct content is a
// function of the merge base. Two PRs each green in isolation can therefore
// produce a `main` neither of them validated. .gitattributes' own header
// records this as the residual risk it is.
//
// `strict: true` would close the window by forcing every PR to update-branch
// and re-run the whole required set. It is the wrong trade here: a full run is
// ~19 checks and ~40 minutes, re-forced each time another PR lands first, so it
// serialises merges and multiplies CI spend worst exactly when the repo is
// busiest — the cost scales WITH the risk instead of against it. This script is
// the other half of the trade: let PRs merge unserialised, and validate the one
// place the merged content actually exists.
//
// # What it does
//
// 1. Takes the push's `before`/`after` commits and diffs them.
// 2. Asks lib/golden-shards.mjs's `impliedShards` which shards that diff could
//    have staled. Regenerating everything unconditionally would take long
//    enough to get ignored, which is the same as not having the check.
// 3. Runs each implied shard's real regeneration command.
// 4. Fails if the tree moved — the committed artefact is not what the generator
//    writes against this merge base.
//
// # The failure message is the product
//
// Whoever responds to this is the person who merged LAST, not the author of the
// artefact and not necessarily the author of either change. So a failure names
// the artefact, the exact command that regenerates it, and BOTH commits whose
// interleaving produced the drift: the one that just landed, and the most
// recent commit before it that touched the same shard's inputs.
//
// Environment inputs:
//
//   POST_MERGE_BEFORE      (required) the commit `main` pointed at before this
//                          push. GitHub supplies it as `github.event.before`.
//   POST_MERGE_AFTER       (optional) the commit that just landed. Default HEAD.
//   POST_MERGE_SHARDS      (optional) space/comma separated shard names that
//                          REPLACE the derived set. Lets the pin drive the
//                          regenerate-and-diff half over a synthetic tree
//                          without depending on the derivation's inputs.
//   JUST_EXECUTABLE        (optional) how to invoke `just` for recipe-backed
//                          shards. Default `just`.
//
// Exit codes:
//   0  nothing implied, or every implied shard regenerated to the committed
//      bytes.
//   1  an artefact drifted, a regeneration command failed, or the inputs are
//      unusable.

import { spawnSync } from 'node:child_process';
import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';
import {
  SHARDS,
  commandsFor,
  corpusRootsFor,
  generatorPackageDirs,
  impliedShards,
  orderShards,
  resolveRequested,
} from './lib/golden-shards.mjs';

const repoRoot = process.cwd();

/**
 * How many candidate culprit commits the failure message names per shard. One
 * is the honest answer to "which commit moved this shard's inputs last"; a
 * longer list stops being an accusation and starts being a log dump, and the
 * responder still has to open every one.
 */
const CULPRIT_COMMITS_SHOWN = 1;

/** The pretty-format the culprit and landed commits are rendered with. */
const COMMIT_FORMAT = '%h %s';

/**
 * The bucket a drifted path falls into when no regenerated shard declares it.
 * Not a shard name, so it can never collide with one.
 */
const UNCLAIMED = '(no shard declares this path)';

function git(args) {
  const r = spawnSync('git', args, { cwd: repoRoot, encoding: 'utf8' });
  if (r.status !== 0) return null;
  return r.stdout.split('\n').filter(Boolean);
}

function fail(lines) {
  for (const line of lines) log(line);
  error(lines[0].replace(/^error:\s*/, ''), { title: 'post-merge-golden-drift' });
  process.exit(1);
}

/**
 * A shard's golden roots as git pathspecs: each declared golden truncated at
 * its first `*`.
 *
 * The truncation is not laziness, it is the only correct reading. A git
 * pathspec containing a wildcard is matched by wildmatch against the WHOLE
 * path and gets no leading-directory treatment, so the literal
 * `test/e2e/migration/archetypes/*​/expected` matches the directory and nothing
 * inside it — every file under it, which is the entire artefact, is invisible
 * to it. Truncating to `test/e2e/migration/archetypes` restores the
 * leading-directory match and can only ever widen the window, never narrow it;
 * this check runs on a clean tree where the generators are the only writers, so
 * a wider window costs nothing and catches a generator that writes somewhere
 * its own declaration did not predict.
 */
function goldenRoots(name) {
  return [
    ...new Set(
      SHARDS[name].goldens.map((g) => {
        const star = g.indexOf('*');
        return star === -1 ? g : g.slice(0, star).replace(/\/$/, '');
      }),
    ),
  ].sort();
}

/**
 * The paths whose change can move what a shard records: its data corpus and the
 * source directories of its generators, MINUS its own goldens. The exclusion is
 * what keeps the culprit search from indicting the last commit that regenerated
 * the artefact, which is never the commit that staled it.
 *
 * `go list` is how the generator package dirs are derived, and it needs a Go
 * toolchain and a module. When it cannot run the culprit search narrows to the
 * data corpus alone — this set only enriches the failure MESSAGE, and the
 * verdict above it never consults it.
 */
function shardInputPathspecs(name) {
  const paths = new Set(corpusRootsFor(repoRoot, name));
  try {
    for (const dir of generatorPackageDirs(repoRoot, SHARDS[name].generators)) paths.add(dir);
  } catch {
    // Narrowed, not skipped: see above.
  }
  return [...paths].sort().concat(goldenExclusions(name));
}

/**
 * A shard's goldens as EXCLUDE pathspecs, at full precision.
 *
 * Deliberately not `goldenRoots` — the truncation that is right for the drift
 * verdict is catastrophic here. `migration`'s corpus and its truncated golden
 * root are the same directory, so excluding the root would subtract the entire
 * corpus and leave the culprit search with nothing to look at, reporting "no
 * commit touched this shard's inputs" for every drift it ever finds. The glob
 * has to survive, and it needs a trailing `/*` to match the files UNDER the
 * directory it names rather than only the directory itself.
 */
function goldenExclusions(name) {
  return [...new Set(SHARDS[name].goldens)]
    .sort()
    .map((g) => `:(exclude)${g.includes('*') ? `${g}/*` : g}`);
}

/** The most recent commits at or before `before` that touched a shard's inputs. */
function culpritCommits(name, before) {
  const inputs = shardInputPathspecs(name);
  if (inputs.every((p) => p.startsWith(':(exclude)'))) return [];
  return (
    git([
      'log',
      `-${CULPRIT_COMMITS_SHOWN}`,
      `--format=${COMMIT_FORMAT}`,
      before,
      '--',
      ...inputs,
    ]) ?? []
  );
}

/**
 * Width of `git status --porcelain`'s status field: two state columns and the
 * space separating them from the path.
 */
const PORCELAIN_PATH_OFFSET = 3;

/**
 * Every path in the tree that differs from the commit that just landed.
 *
 * Deliberately whole-tree rather than per-shard. This runs on a clean checkout
 * of `after` and the regeneration commands are the only things that write, so
 * anything git reports here IS generator output — including output landing
 * outside the golden roots its shard declares, which a per-shard pathspec would
 * define out of existence rather than report. Ignored paths stay ignored: a
 * build cache is not an artefact.
 */
function driftedPaths() {
  return (git(['status', '--porcelain', '--untracked-files=all']) ?? []).map((l) =>
    l.slice(PORCELAIN_PATH_OFFSET).trim(),
  );
}

/** The first shard in `order` whose golden roots contain `file`, else null. */
function owningShard(file, order) {
  for (const name of order) {
    for (const root of goldenRoots(name)) {
      if (file === root || file.startsWith(`${root}/`)) return name;
    }
  }
  return null;
}

/** The shards this merge could have staled, or the explicit override. */
function shardsToCheck(changed) {
  const override = process.env.POST_MERGE_SHARDS;
  if (override !== undefined && override.trim() !== '') {
    const { shards, error: parseError } = resolveRequested(override);
    if (parseError) fail(parseError.split('\n'));
    return new Map(shards.map((n) => [n, 'named by POST_MERGE_SHARDS']));
  }
  return impliedShards(changed, { repoRoot });
}

function regenerate(name) {
  const justExecutable = process.env.JUST_EXECUTABLE || 'just';
  for (const { argv, env } of commandsFor(name, { justExecutable })) {
    log(`==> ${name}: ${argv.join(' ')}`);
    const r = spawnSync(argv[0], argv.slice(1), {
      cwd: repoRoot,
      stdio: 'inherit',
      env: { ...process.env, ...env },
    });
    if (r.status !== 0) {
      fail([
        `error: the \`${name}\` shard could not be regenerated on \`main\`: ${argv.join(' ')}`,
        '       The merged content is not validated until this command runs clean, so read its',
        '       output above for what it found. A generator that refuses to WRITE under CI',
        '       compares instead and reports the drift there rather than through the tree diff',
        '       below, so this is the drift report for such a shard, not a broken toolchain.',
      ]);
    }
  }
}

function main() {
  const before = process.env.POST_MERGE_BEFORE;
  const after = process.env.POST_MERGE_AFTER || 'HEAD';
  if (!before) {
    fail([
      'error: POST_MERGE_BEFORE is unset — there is no merge to validate without the commit',
      '       `main` pointed at before this push (GitHub supplies it as github.event.before).',
    ]);
  }

  // GitHub sends an all-zero `before` when the push CREATED the ref. There is
  // no prior state to have drifted from, so there is nothing to validate — and
  // failing on it would make the gate's first-ever run a red herring.
  if (/^0+$/.test(before)) {
    notice(
      'post-merge-golden-drift: this push created the ref, so there is no earlier `main` to ' +
        'have drifted from — nothing to validate.',
    );
    return;
  }

  const changed = git(['diff', '--name-only', before, after]);
  if (changed === null) {
    fail([
      `error: cannot diff ${before}..${after} — one of them is not a commit in this checkout.`,
      '       A shallow clone is the usual cause; this check needs both sides of the merge.',
    ]);
  }

  const implied = shardsToCheck(changed);
  if (implied.size === 0) {
    notice(
      `post-merge-golden-drift: ${changed.length} changed file(s) imply no generated shard — ` +
        'nothing to regenerate.',
    );
    return;
  }

  const order = orderShards([...implied.keys()]);
  log(`Regenerating ${order.length} implied shard(s): ${order.join(' ')}`);
  for (const name of order) log(`  ${name}: ${implied.get(name)}`);

  for (const name of order) regenerate(name);

  const drifted = driftedPaths();
  if (drifted.length > 0) {
    const landed = (git(['log', '-1', `--format=${COMMIT_FORMAT}`, after]) ?? [after])[0];
    const byShard = new Map();
    for (const file of drifted) {
      const owner = owningShard(file, order) ?? UNCLAIMED;
      if (!byShard.has(owner)) byShard.set(owner, []);
      byShard.get(owner).push(file);
    }

    const failures = [];
    for (const [owner, files] of byShard) {
      if (owner === UNCLAIMED) {
        failures.push(
          `error: regenerating ${order.join(' ')} wrote ${files.length} path(s) no shard ` +
            'declares as its own:',
        );
        for (const f of files) failures.push(`         ${f}`);
        failures.push(
          '       A generator writing outside its declared goldens is a bug in the shard table ' +
            '(lib/golden-shards.mjs) or in the generator — the artefact it wrote is defended by ' +
            'nothing.',
        );
        continue;
      }

      failures.push(
        `error: the \`${owner}\` shard on \`main\` is not what its generator writes — ` +
          `${files.length} artefact(s) drifted:`,
      );
      for (const f of files) failures.push(`         ${f}`);
      failures.push(`       regenerate with:  just update-golden ${owner}`);
      failures.push(`       just landed:      ${landed}`);
      const culprits = culpritCommits(owner, before);
      if (culprits.length > 0) {
        for (const c of culprits) failures.push(`       merged before it: ${c}`);
      } else {
        failures.push(
          `       merged before it: nothing at or before ${before} touched this shard's inputs`,
        );
      }
    }

    failures.push('');
    failures.push(
      'Both commits were green on their own PR. Neither was ever checked against the other: ' +
        'branch protection does not require a PR to be up to date before merging, so the ' +
        'regenerate-and-diff ratchets ran on each branch\'s own base and never on this one. ' +
        'Regenerate on top of current `main` and open a PR with the result.',
    );
    fail(failures);
  }

  notice(
    `post-merge-golden-drift: ${order.length} implied shard(s) regenerate to the committed bytes ` +
      `(${order.join(' ')}).`,
  );
}

main();
