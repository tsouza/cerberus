# Toolchain — background

Why the toolchain contract in [`toolchain.md`](toolchain.md) is shaped the way it is: the
incidents, measurements and rejected alternatives behind each rule. Nothing here changes what a
reader does; it explains why the contract says what it says.

## Why the coverage-profile artifact uploads even when the floor gate is red

The enrollment check used to run inside `coverage-plan`, whose failure skipped every lane that
could have measured the remedy (tsouza/cerberus#2987): an unenrolled package turned the gate red,
and the red gate prevented the profile that would have produced the package's floor. Uploading the
artifact regardless of the gate's verdict is what makes an unenrolled package recoverable from CI
at all.

## Why a floor needs a profile carrying both lanes

A floor measured without the chdb-tagged lane under-records every package that lane reaches, and
because the ratchet only ever raises a floor, nothing corrects one written too low — it passes
enrollment and passes the gate indefinitely. That is why `just coverage-merge` stamps the merged
lane set onto the profile, bound to the profile's own SHA-256, and why `just update-coverage-floor`
refuses a record that is missing, narrower than `default+chdb`, or bound to different bytes, rather
than relying on the reader to have checked which lanes a run actually completed.

## Why the ratchet refuses to delete a floor

The recipe rewrites the whole ledger from the profile in hand, so a floored package the profile
never measured would simply lose its entry — the deepest lowering available, since a ratchet has
nothing that ever restores a floor that is gone. The two ways that happens (a package the module no
longer has, and a package an older artifact never measured) look identical from inside the profile,
which is why the recipe reads the tree instead of the profile to tell them apart.

## Why the slack is the wider of one point and one statement

A point alone means whatever the package's size makes it mean: on a 70-statement package it is
0.70 statements, so the floor tolerates no jitter at all and a single statement flipping reds a
required check, while on a 4600-statement package the same point is 46 statements. One statement is
the quantum the measurement moves in, so it is the narrowest honest margin; the statement term binds
only below 100 statements, which is exactly where a point is worth less than one.

## Why the coverage lanes pin the rapid seed

Slack absorbs jitter; it cannot absorb a random variable. Property tests draw through
`pgregory.net/rapid`, whose `-rapid.seed` defaults to a random value, and which branches of a
generator-driven test execute moves a package by whole statements between two runs of an identical
tree: `test/property/oracle/traceql` drew 242, 243, 244 and 245 of its 272 statements across
fifteen full-lane profiles (tsouza/cerberus#3000). No slack narrow enough to catch a real regression
is wide enough to cover that, and a ratchet fed a lucky draw never corrects itself. Pinning the seed
in the measuring lanes removes the largest source of variance while leaving `just property` — the
lane that searches for counterexamples — on a fresh sample every run.

## Why the markdownlint engine is pinned in exactly one place

The `lint-md` / `fmt-md` recipes, lefthook's hook and the `lint` job used to resolve to three
different engines — a Justfile pin, a bundled action, and whatever binary was on a developer's
`$PATH`. Because markdownlint IGNORES a config key naming a rule it does not implement rather than
rejecting it, the `MD060` key configured nothing under the older local pin: `just lint-md` reported
success on a table CI then failed on. The failure mode is silent and in the dangerous direction, and
it generalises past MD060 — any rule the repo configures ahead of the local pin is enforced in CI
and invisible locally, so invariant 5's "reproduce the red check locally" cannot be satisfied for
it. One literal, consulted by every caller, is the only shape in which that cannot recur.

## The gremlins fork: what each fix defends

The fixes the fork carries defend one thing between them: that the number a run reports is a number
the tests earned. A run that dies mid-flight reports nothing at all; a run that credits the compiler
reports something worse than nothing.

### `--on-shutdown-status`, for mutants cancelled in flight

Upstream's signal handler closes the channel that `os/signal` still writes to, so a second signal —
the typical CI runner sequence of SIGTERM then SIGKILL — panics with `send on closed channel` from
`signal.process`. Worse, a mutant whose `go test` subprocess is still running at cancellation time
falls through `runTests` to the default `return mutator.Lived` branch, because the per-test context
is rooted in `context.Background()` and only `DeadlineExceeded` is checked. Untested mutants recorded
as LIVED deflate `test_efficacy`. The fork stops the handler self-closing and threads the engine's
run context into the per-test context, so a cancelled-in-flight mutant is reported with the status
from the new flag. Upstream pull request: <https://github.com/go-gremlins/gremlins/pull/283>.

### `--timeout-max`, for runaway mutants that kill the runner

Upstream derives a mutant's test timeout as `timeout-coefficient × the package's baseline test
duration`, which scales the leash by how slow a package's tests are — a quantity unrelated to how
much damage a runaway mutant does in that time. A mutant that inverts a scanner's loop advance
(`i++` to `i--`) never terminates and allocates per iteration, so on a slow-baseline package it gets
minutes to exhaust the runner's memory; the OOM killer then reaps the runner and the job ends with
no verdict. Measured across 91 heavy runs, all 55 runner deaths were stalled on a lexer or scanner
mutant. `--timeout-max` bounds exposure absolutely, independent of the baseline. The regression test
pinning the flag and the fork tag together exists because the failure mode it prevents presents as
flake rather than as a missing bound.

### `--compile-allowance`, and why verdicts come from the output

Upstream bounds a mutant with one number, a context deadline wrapping the whole `go test` child, and
sets `go test`'s own run-only `-timeout` two seconds ABOVE it — so the run leash is structurally
unreachable and compile time is charged to the budget meant to bound execution. Measured on
cerberus: 12.7-15.8s of compile against a 15s budget while the test itself reaches a verdict in
0.3-2.1s, so mutants were recorded `TIMED OUT` having never run. Handing the bound to `go test
-timeout`, whose clock starts when the test binary starts, fixes the charge; the widened context
deadline stays as the backstop because no `-timeout` can bound a compile that has hung.

Letting Go's `-timeout` win that race is only safe with the second half. `go test` collapses a
failing test, a package that does not build and a test that ran past its `-timeout` into its own
exit status 1; only the test *binary* exits 2, and what gremlins spawns is `go`. Reading that 1 at
face value credits a timeout as a KILL and books a mutant that never compiled as one too. Scanning
the child's output for the markers is what separates them. The scan is streaming and bounded so a
mutant that prints without bound cannot exhaust memory.

### Why the two bounds report separately

Splitting the leash gave the run bound and the backstop different meanings, but with one status a
mutant that genuinely does not terminate stayed indistinguishable from a compile that hung. Reporting
them apart (`RUN TIMED OUT` vs `TIMED OUT`) lets the threshold script count the first as a detection
and leave the second crediting nobody, so a slow compiler cannot buy a score. The marker is read
before either deadline because a large goroutine dump can still be draining when the backstop
expires.

### Prefix operators

gremlins maps each `token.Token` to the mutations that make sense for it, and that table describes
the operator's *infix* meaning — but the same walk reads `*ast.UnaryExpr` too. Go spells four
operators identically in both positions and means something different by each, so two of them were
mutated against the wrong meaning: `&x` is address-of rather than bitwise AND, and `INVERT_BITWISE`
rewrote it to `|x`, which does not parse; `^x` is bitwise complement rather than XOR, and the same
rule rewrote it to `&x`, which no longer has the operand's type. On this tree, whose plan-building
code is largely `&chplan.Foo{...}` composite literals, that was most of a package's mutants — and
every one of them arrived as exit status 1 and was booked `KILLED`, so each leg was paid efficacy for
work the compiler did.

Removing those mutants rather than reclassifying them is what makes a mutant set mean something: a
leg's honest score is identical either way, since `NOT VIABLE` leaves both sides of the ratio, but a
set padded with entries no compiler accepts measures nothing.

### Type-checking a candidate before emitting it

Reading a prefix operator as a prefix operator is one instance of a wider gap: the mutation table
describes what a rewrite *means* for a token read on its own, and whether the result is a program
depends on the operand types, on the constant values around it, and on what statements the
enclosing function admits — none of which is in the token. Three shapes of that survived on this
tree. `hint.Name + "=" + hint.Value.String()` became `operator - not defined on ... (variable of
type string)`, since Go defines `+` on strings and nothing else; `const week = 7 * 24 * time.Hour`
became a legal constant expression whose `d / week` three lines down is a division by zero; and
`INVERT_LOOPCTRL` turned the `continue` that keeps a `for {}` from terminating into a `break`
(`missing return`), and a `break` inside a `switch` no loop encloses into a `continue` that is not in
a loop.

The check is of the whole package because the error a mutation causes need not appear where the
mutation is, as the constant case shows. One type-check (~100ms on `internal/promql`) buys back a
whole recompile-link-run cycle (~10s) whenever it rejects, and generation runs on its own goroutine
behind the executor pool, so the lane pays nothing for it. A package that cannot be type-checked as
it stands is left to the compiler rather than having its mutants dropped, because dropping a mutant
nobody proved illegal would shrink the set a score is measured against — the one direction a
mutation tool must not move in.

### Why mutated tests run with `-vet=off`

`go test` runs a subset of `go vet` before it builds anything and reports a finding as `FAIL pkg
[build failed]` — from the outside indistinguishable from source that does not compile. Its `bools`
analyzer rejects exactly what `INVERT_LOGICAL` produces from `name == a || name == b`: a conjunction
of equalities against distinct constants, which it calls "suspect and". Those mutants are legal Go
and a real change of behaviour — the predicate becomes unsatisfiable, and any test exercising either
operand kills it. This is the one fix in the list that can MOVE a leg's number rather than only
correct its meaning: mutants that used to leave the ratio as `NOT VIABLE` get a real verdict, and one
that nothing kills is a genuine gap in the suite rather than an artefact.

### The `doCopy` file-descriptor leak

Every worker's working-directory setup (`CachedDealer.Get`) walks the whole source tree once and
copies every regular file, but upstream's `doCopy` never closed either the source or destination
`os.File` it opened — two leaked file descriptors per copied file, for the process's lifetime, on
every run. Invisible until a consuming repo's tree size, times concurrent workers, times two, crosses
the runner's open-file ulimit: cerberus's own CI first hit `open ...: too many open files` panics
after a release added ~2,300 files to its source tree (cerberus #3154). Fixed with `defer s.Close()`
/ `defer d.Close()` in `doCopy`.

### Why two branches

The upstream pull requests need a diff against the upstream module path to stay reviewable, and
`go install` refuses a module whose `go.mod` declares a path other than the one being installed. The
only shape that satisfies both is a review branch on the upstream path and a consume branch that
adds exactly one renaming commit on top.
