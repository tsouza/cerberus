# Toolchain

Build-time environment, linters, and the tooling forks. Dependency policy and the `tsouza/*` fork
boundary live in `docs/upstream-forks.md`; this document covers the tools that build and check the
tree rather than the modules it links.

## Go

`go.mod` may pin a newer Go than the system-wide install. `GOTOOLCHAIN=auto`, the default, downloads
the pinned version into `~/go/pkg/mod/golang.org/toolchain@...` without further ceremony. The
`.envrc` — loaded by `direnv allow` — puts both the system Go and the downloaded toolchains on
`PATH`.

CGO is left at the platform default so `go test -race` works. Goreleaser pins `CGO_ENABLED=0` for
release builds independently of the development setting.

## Linters and formatters

`golangci-lint` v2. `.golangci.yml` uses the v2 schema, in which `gofumpt` and `goimports` are
configured under `formatters` rather than `linters`. The v2 install path carries the major-version
element: `github.com/golangci/golangci-lint/v2/cmd/golangci-lint`.

Two settings are load-bearing for anything that formats Go outside `just fmt`:

- `gofumpt.extra-rules: true` — the CI `lint` gate enforces gofumpt's extra rules, so any tool or
  hook that formats a Go file must pass `-extra`. Plain `gofumpt -w` leaves formatting that CI
  rejects.
- `goimports.local-prefixes: github.com/tsouza/cerberus` — import grouping puts cerberus's own
  packages in their own block, so `goimports` needs `-local github.com/tsouza/cerberus` to match.

`forbidigo` enforces the parser-internals rule across all of `internal/**`: no `unsafe.Pointer` and
no `reflect.Value.FieldByName` against upstream parser ASTs. The accessor belongs in the fork
instead.

`.go-arch-lint.yml` declares the allowed dependency graph between `internal/**` packages. The gate is
CI-only, so a new package that is missing from that file builds and tests clean locally and fails on
the PR. Declare the package in the same change that creates it.

A new package that carries statements needs a **second** registration in that same change: a positive
entry in the `test/coverage-floor/` ledger, which `coverage.yml`'s `coverage-enrollment` job checks on
every PR. A declaration-only package carries no statements and needs no entry. The ledger is
generated, so never hand-write a floor (invariant 9) — derive it from a merged coverage profile, one
of two ways, both ending in the same command:

- **Locally.** `just chdb-install` once, then `just coverage` (which writes `cover-merged.out`), then
  `just update-coverage-floor`.
- **From CI**, when the local sweep is not practical on the machine at hand.
  `gh workflow run coverage.yml --ref <branch>` runs the measuring lanes on that branch; download the
  run's `coverage-profile` artifact into the repository root and run `just update-coverage-floor`
  against the `cover-merged.out` it contains. Take the artifact from a run whose `coverage-default`,
  `coverage-chdb` and `coverage-chdb-ratchet` jobs all succeeded — that is what makes the merged
  profile the both-lane profile the floors are measured with. The upload happens even when the floor
  gate itself is red, which is what makes an unenrolled package recoverable at all
  (tsouza/cerberus#2987: the enrollment check used to run inside `coverage-plan`, whose failure
  skipped every lane that could have measured the remedy).

`just update-coverage-floor` only ratchets up. It refuses to lower a floor to match a coverage drop
and never records a `0`, so both of those stay hand-edited, reviewable lines in a diff.

`actionlint` (`just lint-actions`) validates the workflow files. GitHub rejects an invalid workflow
file server-side as a zero-job failure run, which prevents required `pull_request` checks from ever
being scheduled — a PR then sits blocked on contexts that can never report.

`markdownlint-cli2` with `.markdownlint.yaml`. `MD060` pins table-column-style to `aligned`; the
`pre-commit` hook pads cell widths with `scripts/align-md-tables.py` before the auto-fixer runs,
because that rule has no auto-fixer of its own.

## Mutation testing — the gremlins fork

`mutation.yml` installs the `tsouza/gremlins` fork rather than upstream `go-gremlins/gremlins@v0.6.0`:

```bash
go install github.com/tsouza/gremlins/cmd/gremlins@v0.6.0-cerberus-run-phase-timeout-consume
```

The fixes it carries defend one thing between them: that the number a run reports is a number the
tests earned. A run that dies mid-flight reports nothing at all; a run that credits the compiler
reports something worse than nothing.

**`--on-shutdown-status`, for mutants cancelled in flight.** Upstream's signal handler closes the
channel that `os/signal` still writes to, so a second signal — the typical CI runner sequence of
SIGTERM then SIGKILL — panics with `send on closed channel` from `signal.process`. Worse, a mutant
whose `go test` subprocess is still running at cancellation time falls through `runTests` to the
default `return mutator.Lived` branch, because the per-test context is rooted in `context.Background()`
and only `DeadlineExceeded` is checked. Untested mutants recorded as LIVED deflate `test_efficacy`.
The fork stops the handler self-closing and threads the engine's run context into the per-test
context, so a cancelled-in-flight mutant is reported with the status from the new flag. Cerberus
passes `--on-shutdown-status=not-run`, which lands those mutants in `NOT_COVERED`, outside the
`KILLED / (KILLED + LIVED)` efficacy formula entirely. Upstream pull request:
<https://github.com/go-gremlins/gremlins/pull/283>.

**`--timeout-max`, for runaway mutants that kill the runner.** Upstream derives a mutant's test
timeout as `timeout-coefficient × the package's baseline test duration`, which scales the leash by how
slow a package's tests are — a quantity unrelated to how much damage a runaway mutant does in that
time. A mutant that inverts a scanner's loop advance (`i++` to `i--`) never terminates and allocates
per iteration, so on a slow-baseline package it gets minutes to exhaust the runner's memory; the OOM
killer then reaps the runner and the job ends with no verdict. Measured across 91 heavy runs, all 55
runner deaths were stalled on a lexer or scanner mutant. `--timeout-max` bounds exposure absolutely,
independent of the baseline; cerberus derives its value per leg and clamps it into
`[MUTANT_TIMEOUT_MIN, MUTANT_TIMEOUT_MAX]`, declared in `mutation.yml`.

A recurrence is not fixed by excluding the file the log names. Excluding a file relocates its runaway
mutants into whichever leg still owns it and burns real mutation coverage at the same time.
`test/regression/mutation_timeout_max_test.go` pins the flag and the fork tag together, because the
failure mode it prevents presents as flake rather than as a missing bound.

**`--compile-allowance`, and verdicts read from the output rather than the exit status.** Upstream
bounds a mutant with one number, a context deadline wrapping the whole `go test` child, and sets
`go test`'s own run-only `-timeout` two seconds ABOVE it — so the run leash is structurally
unreachable and compile time is charged to the budget meant to bound execution. Measured on cerberus:
12.7-15.8s of compile against a 15s budget while the test itself reaches a verdict in 0.3-2.1s, so
mutants were recorded `TIMED OUT` having never run. The fork hands the bound to `go test -timeout`,
whose clock starts when the test binary starts, and keeps the context deadline — widened by
`--compile-allowance` — as the backstop for a compile that has hung, since no `-timeout` can bound
one.

Letting Go's `-timeout` win that race is only safe with the second half. `go test` collapses a
failing test, a package that does not build and a test that ran past its `-timeout` into its own exit
status 1; only the test *binary* exits 2, and what gremlins spawns is `go`. Reading that 1 at face
value credits a timeout as a KILL and books a mutant that never compiled as one too. The fork scans
the child's output instead — the `panic: test timed out after` line maps to `RUN TIMED OUT`,
`[build failed]` and `[setup failed]` map to `NOT VIABLE`, and anything else is left to the exit
status. The scan is streaming and retains only enough bytes to recognise a marker split across two
writes, so a mutant that prints without bound cannot exhaust memory.

**The two bounds report which of them claimed a mutant.** Splitting the leash gave the run bound and
the backstop different meanings, but both still produced one status, so a mutant that genuinely does
not terminate stayed indistinguishable from a compile that hung. The fork now reports them apart:

| status          | what fired                                                         | what it proves                                               |
| --------------- | ------------------------------------------------------------------ | ------------------------------------------------------------ |
| `RUN TIMED OUT` | the test binary's own `-timeout` watchdog, which printed the panic | the suite did not finish inside a bound no compile can spend |
| `TIMED OUT`     | the context deadline over compile **and** run                      | nothing — a hung compile and a hung run reach it identically |

The marker is read before either deadline, because a large goroutine dump can still be draining when
the backstop expires, and it is guarded on the child having failed, so a suite that passes while
printing those bytes stays `LIVED`. gremlins takes no position on which status is a detection —
neither appears in its own `test_efficacy` — so the policy lives one layer up, in
`.github/scripts/gremlins-threshold.mjs`, which counts `RUN TIMED OUT` as a detection and leaves
`TIMED OUT` in the denominator crediting nobody. A slow compiler still cannot buy a score.

**Prefix operators read as prefix operators.** gremlins maps each `token.Token` to the mutations
that make sense for it, and that table describes the operator's *infix* meaning — but the same walk
reads `*ast.UnaryExpr` too. Go spells four operators identically in both positions and means
something different by each, so two of them were mutated against the wrong meaning: `&x` is
address-of rather than bitwise AND, and `INVERT_BITWISE` rewrote it to `|x`, which does not parse;
`^x` is bitwise complement rather than XOR, and the same rule rewrote it to `&x`, which no longer has
the operand's type. On this tree, whose plan-building code is largely `&chplan.Foo{...}` composite
literals, that was most of a package's mutants — and every one of them arrived as exit status 1 and
was booked `KILLED`, so each leg was paid efficacy for work the compiler did. The fork consults the
table only for the prefix operators whose infix mutations carry over unchanged, `+x` and `-x`.

Removing those mutants rather than reclassifying them is what makes a mutant set mean something: a
leg's honest score is identical either way, since `NOT VIABLE` leaves both sides of the ratio, but a
set padded with entries no compiler accepts measures nothing. The `NOT VIABLE` classification stays
for a genuine build failure from any other source.

**A candidate mutant is type-checked before it is emitted.** Reading a prefix operator as a prefix
operator is one instance of a wider gap: the mutation table describes what a rewrite *means* for a
token read on its own, and whether the result is a program depends on the operand types, on the
constant values around it, and on what statements the enclosing function admits — none of which is
in the token. Three shapes of that survived on this tree. `hint.Name + "=" + hint.Value.String()`
became `operator - not defined on ... (variable of type string)`, since Go defines `+` on strings
and nothing else; `const week = 7 * 24 * time.Hour` became a legal constant expression whose `d /
week` three lines down is a division by zero; and `INVERT_LOOPCTRL` turned the `continue` that keeps
a `for {}` from terminating into a `break` (`missing return`), and a `break` inside a `switch` no
loop encloses into a `continue` that is not in a loop.

The fork type-checks the candidate's whole **package** before emitting it — the package, because the
error a mutation causes need not appear where the mutation is, as the constant case shows. One
type-check (~100ms on `internal/promql`) buys back a whole recompile-link-run cycle (~10s) whenever
it rejects, and generation runs on its own goroutine behind the executor pool, so the lane pays
nothing for it. A package that cannot be loaded and type-checked as it stands is not used as an
oracle at all: its mutants are generated and left to the compiler exactly as before, and a log line
names the package. Dropping a mutant nobody proved illegal would shrink the set a score is measured
against, which is the one direction a mutation tool must not move in.

**`go vet` does not decide whether a mutant may be adjudicated.** `go test` runs a subset of `go
vet` before it builds anything and reports a finding as `FAIL pkg [build failed]` — from the outside
indistinguishable from source that does not compile. Its `bools` analyzer rejects exactly what
`INVERT_LOGICAL` produces from `name == a || name == b`: a conjunction of equalities against
distinct constants, which it calls "suspect and". Those mutants are legal Go and a real change of
behaviour — the predicate becomes unsatisfiable, and any test exercising either operand kills it —
so the fork keeps generating them and runs the mutated tests with `-vet=off`. This is the one fix in
this list that can MOVE a leg's number rather than only correct its meaning: mutants that used to
leave the ratio as `NOT VIABLE` now get a real verdict, and one that nothing kills is a genuine gap
in the suite rather than an artefact.

`.gremlins.yaml`'s `exclude-files` paths are interpreted relative to the run's scope, not the repo
root, and the matcher is RE2 with no lookahead. A path in the wrong form silently excludes nothing.

The fork ships two branches on purpose. `cerberus-sigterm-fix` at tag
`v0.6.0-cerberus-run-phase-timeout` is the branch the upstream pull request is built from, and keeps
the upstream module path `github.com/go-gremlins/gremlins` so the diff stays reviewable.
`cerberus-sigterm-fix-consume` at tag `v0.6.0-cerberus-run-phase-timeout-consume` is the branch
`mutation.yml` installs; it adds one commit renaming the `go.mod` module path to
`github.com/tsouza/gremlins` and rewriting the internal imports, because `go install` otherwise
rejects the module with `module declares its path as: github.com/go-gremlins/gremlins`. The fixes
themselves are identical across the two.

Unlike the module forks, this one sits outside the Dependabot watch flow: it is a build-time tool
rather than a Go module dependency.

## chDB

The chdb-tagged lanes — the `-- expected_rows --` roundtrip cells in `test/spec/`, the property
tests, and the cardinality baseline — link `libchdb.so`, installed by `just chdb-install`. Without
it, `just update-golden` refuses to run any chdb-tagged shard rather than regenerating a partial
corpus.

chDB and a production ClickHouse server differ in scan strictness: chDB coerces some column types
that the server rejects outright. An emit-type bug can therefore pass every chDB lane and fail
against a real server, which is why `compose-smoke` runs against a server. It does not scope to a
diff's touched paths, though: an ordinary PR omits it entirely (the required `quickstart` context
covers the published-startup contract with one stack instead), and it runs the full sweep only on
`release/*` PRs, `push`, and `schedule` — see `.github/scripts/compose-smoke-scope.mjs`.
