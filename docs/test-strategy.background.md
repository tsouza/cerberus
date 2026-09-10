# Test strategy — background

The rationale behind design choices in [`test-strategy.md`](test-strategy.md)
that a reader does not need in order to use the test fence, only to
understand why it looks the way it does.

## Gremlins: why a mutant's compile and run bounds are measured, not fixed

The split between the run leash (`go test -timeout`) and the compile-and-run
backstop (a context deadline) is the fix for #2910. Before it the fork had
only the context deadline, and it was set BELOW the run leash, so the run
leash could never fire and compile time was charged to the budget meant to
bound execution. On a leg compiling in 12.7-15.8s against a 15s budget, a
mutant whose test reached a verdict in 0.3s was recorded `TIMED OUT` having
never run a line of test code.

The reason the resulting budget is measured per leg, per run, rather than a
flat constant, is that the dominant cost is a COMPILE, and a compile's cost
is a property of the package and the runner rather than of the test suite.
Measured per mutant on an 8-core machine, against the flat 15s the lane used
to declare:

| scope             | compile + link | test run |
| ----------------- | -------------- | -------- |
| `internal/chsql`  | 12.7-14.4s     | 0.31s    |
| `internal/promql` | 8.4-8.7s       | 2.07s    |

80-98% of the budget went to the compiler, so every contended runner pushed
ordinary mutants over it and gremlins recorded them as `TIMED OUT` — which
the gate then scored as detections (#2903, and the "Timed-out mutants"
section of `test-strategy.md`). Measuring sized the number to what it was
really bounding; splitting the bounds stopped it having to bound both.

`mutation-run.mjs` also neutralises gremlins' own run-bound formula
(`min(timeout-coefficient x max(coverage_elapsed, 1s), timeout-max)`) by
deriving `--timeout-coefficient` from the measured budget, because
`coverage_elapsed` is taken on whatever build-cache warmth the invocation
happened to inherit. Without that neutralisation, a second, cache-warmed
invocation of the same phase collapsed `coverage_elapsed` from ~50s to
~0.4s, clamping up to gremlins' 1s floor and dropping the derived budget to
5s — under `internal/promql`'s real ~6.6s recompile+link+run cost. Every
mutant then timed out, and because gremlins counts a timed-out mutant in
neither the efficacy ratio nor `mutants_total`, the leg reported `Timed out:
295 / Test efficacy: 0.00%` on pull requests while the identical leg on
push-to-main — one cold-cache invocation, the intended budget — reported
99.65% (#2692).

## Gremlins: why a mutant citation names the construct, not a line number

Replaying the 200 first-parent commits before the gate landed against the
citations they would have carried, a line-number citation was invalidated
**575 times, and in 573 of those the construct it named was never touched**
— the number rotted because unrelated lines moved above it. Two commits in
that window changed a cited line, which is exactly when a human must
re-read the adjudication. A third of the citations on `main` had already
drifted onto a comment or a blank line by the time this was measured
(cerberus issue #2953).

## Gremlins: the rejected mechanical check for an unnamed mutant in a kill claim

A mechanical version of "does every kill-claim citation actually name its
mutant" was built and measured before being rejected. Resolving every
kill-claim citation through the construct resolver and testing the resolved
line against the mutant inventory a completed `mutation` run reports flags
**16 of the 235 citations** that land in a file the lane measures, across
every phase of the lane — and all 16 are correct locator citations of the
four legitimate kinds (the guard whose body is the mutated statement, the
statement immediately above it, the assignment that opens the closure the
mutant sits in, and the `case` label of the arm that holds it). Precision is
zero, and the rule cannot be tightened into one a note could satisfy: the
two ways to clear it are to drop the citation out of the sentence that
claims the kill, which is the citation requirement it exists to impose, or
to invent a unique construct where the source deliberately repeats one. A
gate whose only compliant forms are worse than the violation is a
false-positive machine (cerberus issue #2966).

## Gremlins: the rejected digit-keyed checker for a note's stated numbers

A mechanical checker keyed on digits in adjudication notes was weighed and
rejected on measurement rather than taste. Across the 340 adjudication
blocks on `main`, the integers that restate a source-countable fact are
outnumbered roughly three to one by digits that are simply operands of a
quoted expression (`i == 1`, `Step=0`), and the same class of claim is
written as a spelled-out numeral ("four conjuncts", "three join keys") more
often than as a digit at all. A checker keyed on digits is therefore blind
to most of what it would need to model, which is the one thing a gate here
may not be: a set that silently shrinks is worse than no set.

## Gremlins: the timeout-scoring gate used to break its own monotonicity rule

The gremlins-efficacy gate used to count EVERY timeout as a detection, on the
premise that a mutant exhausting the budget broke termination rather than
lost a race with the compiler. Under one undifferentiated budget that
premise was false, and the per-mutant compile/run costs measured above
falsify it. Two signatures in the reports confirmed it, and every mutant in
both would be a backstop `TIMED OUT` today — the compile is what consumed
the budget, and the compile is what the backstop covers — so the corrected,
narrower reading credits none of them:

- Runs 33542904091 (red) and 33551271099 (green) recorded the SAME 486 kills
  on `phase4-promql-lower`. The whole 94.00% -> 97.01% swing was 16 mutants
  moving `LIVED` -> `TIMED OUT`, on a runner that was 44% slower (coverage
  53.2s vs 36.8s). Matching mutants across the two revisions by source text,
  18 of the red run's 31 survivors — short-circuit boolean guards and
  slice-capacity arithmetic, none of which can unbound a loop — were booked
  as detections.
- gremlins runs each mutant with `-failfast`, so a killed mutant exits at its
  first failing test while a survivor must run the suite to the end.
  Starvation therefore converts SURVIVORS preferentially, which makes the
  bias monotone rather than noisy. Run 33522074818 is the endpoint: all four
  `internal/chsql` legs reported 100.00% with `lived: 0`, over 1266 mutants
  of which 1230 timed out.

## The chdb race-detector crash

chdb-go's driver's `(*conn).Close` is a no-op, so `db.Close()` does not
shut the native chdb engine down. That asymmetry is invisible to a plain
`go test`, because Go's `os.Exit` reaches the `exit_group` syscall directly
and libchdb's C++ static destructors never run. It is NOT invisible under
`-race`: `os.Exit` first calls `runtime_beforeExit` → `runtime.racefini` →
`__tsan_fini`, which — per the Go runtime's own comment on `racefini` —
"will run C atexit functions and C++ destructors". Those destructors then
tore libchdb down while its engine was still live, and the process died
with

```text
SIGSEGV: segmentation violation
runtime.racefini()
os.runtime_beforeExit(0x0)
os.Exit(0x0)
```

at a constant offset inside `libchdb.so`, **after** every test in the binary
had already passed — turning a suite in which nothing failed into a failed
lane. `os.Exit(0x0)` in that trace is the tell: the exit code was zero.
`internal/chdbsession.CloseForExit` exists to close the cached session
before the binary exits, so those destructors run against an
already-shut-down engine instead.

The `chdb` CI job runs without `-race` for cost: measured over the four
`internal/api` packages, `-race` costs between 1.5x (`prom`, 131s → 201s —
it is already the long pole) and 5x (`tempo/grpc`, 0.4s → 2.1s) in wall
clock.

## Why `coverage` reports NOT MEASURED on a pull request instead of measuring something cheap

The obvious proposal — run the default-tag lane on PRs, since
`coverage-default` completes in about three minutes while `coverage-chdb` runs
35-50 — fails on soundness, not on cost. The committed floors in
`test/coverage-floor/` are measured from the MERGED `default+chdb` profile, and a
chdb-tagged run compiles and reaches code the default-tag run cannot. Comparing a
default-only profile against those floors therefore reports drops that are not
real, for every package whose coverage comes from chdb-tagged tests.
`coverage-summary.mjs` already refuses that comparison for this exact reason
(`resolveLanes` against `FULL_LANES`, and `COVERAGE_REQUIRE_LANES` to stop a
silently narrowed profile passing as a full one). Making a PR-time measurement
sound would need a second, default-only ledger carrying its own ratchet — a
doubling of the floor surface. Until someone wants to own that, the honest answer
on a pull request is to say plainly that nothing was measured, which is what the
verdict does.

## `-merge` only protects a LOCAL git merge — verified 2026-08-04

`-merge` is a built-in git merge driver, honoured by any git CLIENT that
reads `.gitattributes`: a local `git merge`, `git rebase`, or `git pull`.
Cerberus's actual merge paths on GitHub are mostly SERVER-SIDE — the "Update
branch" button, `gh pr merge --squash`, and the mergeability precomputation
that decides whether a PR shows conflict-free — and nothing had verified
those honour `.gitattributes` at all (issue #1568, spun out of #1567 while
adding the generated-baselines-never-auto-merge gate).

They do not. Verified empirically 2026-08-04: two throwaway branches pushed
to this repo, each inserting one new record at a different, non-overlapping
line offset into one `-merge`-guarded generated baseline off the same base
commit, produced a throwaway PR that `gh pr view --json mergeable` reported
as `"mergeable":"MERGEABLE"` — while a LOCAL `git merge` of the identical
branch pair, run in a scratch worktree, refused with "Cannot merge binary
files" / `CONFLICT (content)`, exactly as `.gitattributes` documents. The
throwaway PR was closed unmerged and both branches deleted immediately after.

Auditing every `-merge` path for what actually protects it turned up good
news: nearly all of them already carry a content-exact ratchet that
regenerates the artefact from source and diffs it against the committed file
— `TestCardinalityRatchet` / `TestSolverDecisionRatchet` / `TestScaleWallPin`
(`perf-guards`), `TestCatalogueIsRegenerable` and the surface-parity
inventory tests (`check`), `compat-ratchet.mjs` (`compatibility/*`),
`coverage-summary.mjs` (`coverage`), and the Tier-0 migration goldens via
`go test -tags=migration ./test/e2e/migration/tiers/tier0-offline/...`
(`lint`). Those are the strong "re-run the generator on the merge commit and
diff it" defence #1568 asked for, and they were already there for most of
the list. `check`, `coverage` and `lint` are REQUIRED and PR-blocking, so
those three ratchets stop a corrupted merge before it lands. `perf-guards`
and `compat-ratchet.mjs` (`compatibility/*`) are release-gate lanes (#2230):
they still run their real ratchet unconditionally on every push to `main`, so
a corrupted merge is still caught and reported, but no longer PRE-merge —
the defence is detection on the landed commit, not prevention of the landing.
That is an accepted, deliberate narrowing of this specific guarantee for
those two ratchets, traded for keeping them off the ordinary-PR critical
path; release.yml's preflight still refuses to publish past a red one.

The residual gap is procedural rather than a missing validator: branch
protection's "require branches to be up to date before merging" is OFF
(`strict: false` as of this writing), so a stale PR's squash-merge computes
its diff against whatever `main` has moved to WITHOUT re-running any of the
checks above against the resulting content. Turning `strict: true` on closes
that window — it forces the "Update branch" step, which re-runs every
required check (including all of the above) against the exact content that
will land — and is recommended to the maintainer as a follow-up; it is a
branch-protection admin setting, not a code change, so no PR flips it
unilaterally.
