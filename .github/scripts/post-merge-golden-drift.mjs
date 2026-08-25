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
// post-merge-drift.yml fans the regeneration out across one CI job per implied
// shard — the same `plan` → per-shard-matrix structure update-golden.yml uses
// for the identical regeneration work on the manual path, including its own
// further `cardinality-leg`/`cardinality-seal` split for the one shard whose
// regeneration is itself CI-matrix-sharded (see that workflow's comments).
// This script plays two roles in that structure, selected by `MODE`:
//
//   MODE=plan   run once, in the workflow's `plan` job. Diffs the push's
//               `before`/`after` commits, asks lib/golden-shards.mjs's
//               `impliedShards` which shards that diff could have staled, and
//               emits the per-shard matrix (plus the `cardinality` fan-out
//               flag) the rest of the workflow fans out over. Regenerating
//               everything unconditionally would take long enough to get
//               ignored, which is the same as not having the check.
//   (unset)     run once per matrix job, scoped to ONE shard via
//               POST_MERGE_SHARDS. Runs that shard's real regeneration
//               command — skipped when POST_MERGE_SKIP_REGENERATE=1, which
//               is how the `cardinality-seal` job reuses this same reporting
//               path after its own `cardinality-leg` matrix has already
//               regenerated the shard — then fails if the tree moved: the
//               committed artefact is not what the generator writes against
//               this merge base.
//
// # The failure message is the product
//
// Whoever responds to this is the person who merged LAST, not the author of the
// artefact and not necessarily the author of either change. So a failure names
// the artefact, the exact command that regenerates it, and BOTH commits whose
// interleaving produced the drift: the one that just landed, and the most
// recent commit before it that touched the same shard's inputs. Each shard's
// own matrix job reports this independently — with `fail-fast: false`, one
// shard's drift does not cancel its siblings' still-running checks, so a push
// that drifted two artefacts reports both rather than whichever job happened
// to fail first.
//
// CLI: `node post-merge-golden-drift.mjs dump` prints the full `regenerate`
// matrix row catalogue (every non-`cardinality` shard) as JSON, for
// test/regression/release_required_checks_test.go's `generatedMatrices` to
// expand post-merge-drift.yml#regenerate's plan-derived `strategy.matrix`.
//
// Environment inputs:
//
//   MODE                   (optional) `plan` to derive the matrix instead of
//                          regenerating; unset for the per-shard job. See above.
//   POST_MERGE_BEFORE      (required) the commit `main` pointed at before this
//                          push. GitHub supplies it as `github.event.before`.
//   POST_MERGE_AFTER       (optional) the commit that just landed. Default HEAD.
//   POST_MERGE_SHARDS      (optional) space/comma separated shard names that
//                          REPLACE the derived set. Lets the pin drive the
//                          regenerate-and-diff half over a synthetic tree
//                          without depending on the derivation's inputs; also
//                          how a `regenerate`/`cardinality-seal` matrix job
//                          scopes this script to its own single shard.
//   POST_MERGE_SKIP_REGENERATE  (optional) `1` skips the regeneration commands
//                          and goes straight to diffing the tree — the
//                          `cardinality-seal` job's own leg matrix already
//                          regenerated the shard by the time this runs.
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

import { error, log, notice, setOutput } from './lib/gh.mjs';
import {
  SHARD_NAMES,
  SHARDS,
  commandsFor,
  corpusRootsFor,
  flattenSteps,
  generatorPackageDirs,
  impliedShards,
  needsChdb,
  orderShards,
  resolveRequested,
} from './lib/golden-shards.mjs';

const repoRoot = process.cwd();

/**
 * The one shard whose own regeneration is CI-matrix-sharded further, into
 * `cardinality-leg` + `cardinality-seal` (post-merge-drift.yml), mirroring
 * update-golden.yml's identical split — see that workflow's comments.
 */
const CARDINALITY_SHARD = 'cardinality';

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
  // Fan-outs are flattened and run one after another here. Each leg covers a
  // disjoint slice, so the union is identical either way and only the wall
  // clock differs — and this lane is a drift CHECK on `main` whose output must
  // stay a single readable stream (stdio is inherited, not line-tagged), which
  // concurrent children would interleave.
  for (const { argv, env } of flattenSteps(commandsFor(name, { justExecutable }))) {
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

/**
 * MODE=plan — derive the shard-level matrix post-merge-drift.yml's own
 * `plan` job fans the rest of the workflow out over, without regenerating
 * anything. The derivation is identical to the one `regenerateAndDiff` uses
 * to decide what to check (`shardsToCheck`, driven by the same `before`/
 * `after`/`POST_MERGE_SHARDS` inputs) — this mode only stops short of
 * running the regeneration commands themselves.
 *
 * `cardinality` is excluded from `matrix.include`: its own regeneration is
 * further CI-matrix-sharded by the workflow's `cardinality-leg` +
 * `cardinality-seal` jobs (mirroring update-golden.yml's identical split —
 * see that workflow's comments), so it is reported separately as the
 * `cardinality_selected` boolean rather than as a `regenerate` matrix row.
 *
 * Outputs (written to `$GITHUB_OUTPUT`, logged when that is unset):
 *
 *   nothing               `true` when there is no earlier commit to have
 *                          drifted from, or the diff implies no shard at all
 *                          — every downstream job should skip.
 *   matrix                JSON `{ include: [{ shard, needs_chdb }, ...] }`
 *                          for the `regenerate` matrix (cardinality excluded).
 *   shards                every implied shard, space-joined, stage-ordered —
 *                          for logging.
 *   has_regenerate_rows   `true` when `matrix.include` is non-empty. Tells an
 *                          EMPTY matrix (nothing beyond `cardinality` implied)
 *                          apart from a job that never ran, the same
 *                          ambiguity update-golden.yml's own output of the
 *                          same name exists to remove.
 *   cardinality_selected  `true` when `cardinality` is among the implied
 *                          shards.
 */
function planMode() {
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
    setOutput('nothing', 'true');
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
    setOutput('nothing', 'true');
    return;
  }

  const order = orderShards([...implied.keys()]);
  log(`Planned ${order.length} implied shard(s) to check in parallel: ${order.join(' ')}`);
  for (const name of order) log(`  ${name}: ${implied.get(name)}`);

  const cardinalitySelected = order.includes(CARDINALITY_SHARD);
  const matrix = {
    include: order
      .filter((name) => name !== CARDINALITY_SHARD)
      .map((name) => ({ shard: name, needs_chdb: needsChdb([name]) })),
  };

  setOutput('nothing', 'false');
  setOutput('matrix', JSON.stringify(matrix));
  setOutput('shards', order.join(' '));
  setOutput('has_regenerate_rows', String(matrix.include.length > 0));
  setOutput('cardinality_selected', String(cardinalitySelected));
}

function regenerateAndDiff() {
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

  if (process.env.POST_MERGE_SKIP_REGENERATE === '1') {
    log(
      `Skipping regeneration for ${order.join(' ')} — already regenerated by the caller ` +
        '(POST_MERGE_SKIP_REGENERATE=1, the cardinality-seal leg-merge path).',
    );
  } else {
    for (const name of order) regenerate(name);
  }

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

/**
 * `node post-merge-golden-drift.mjs dump` — prints the FULL `regenerate`
 * matrix row catalogue (every shard except `cardinality`, whichever a given
 * push happens to imply), mirroring manual-golden-update.mjs's identical
 * `dump` mode for `update-golden.yml#regenerate`.
 *
 * post-merge-drift.yml's `regenerate` job builds its `strategy.matrix` from
 * the `plan` job's output — an expression over another job, not a literal in
 * the workflow YAML — so a static reader of the workflow file alone cannot
 * enumerate it. `test/regression/release_required_checks_test.go`'s
 * `generatedMatrices` runs this instead, exactly as it already does for
 * `update-golden.yml#regenerate`, to index every job name post-merge-drift.yml
 * can ever post without the index going stale the moment a shard is added.
 */
function dumpMatrix() {
  const rows = orderShards(SHARD_NAMES)
    .filter((name) => name !== CARDINALITY_SHARD)
    .map((name) => ({ shard: name, needs_chdb: needsChdb([name]) }));
  process.stdout.write(`${JSON.stringify(rows)}\n`);
}

/** MODE=plan derives the matrix; unset MODE regenerates and diffs the shard(s)
 * POST_MERGE_SHARDS scopes this job to — see the file header for both. */
function main() {
  const mode = (process.env.MODE || '').trim();
  if (mode === 'plan') return planMode();
  if (mode !== '') {
    fail([`error: MODE must be "plan" or unset (got ${JSON.stringify(mode)}) — see this file's header.`]);
  }
  return regenerateAndDiff();
}

if (process.argv[2] === 'dump') dumpMatrix();
else main();
