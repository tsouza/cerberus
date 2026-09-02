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

`actionlint` (`just lint-actions`) validates the workflow files. GitHub rejects an invalid workflow
file server-side as a zero-job failure run, which prevents required `pull_request` checks from ever
being scheduled — a PR then sits blocked on contexts that can never report.

`markdownlint-cli2` with `.markdownlint.yaml`. `MD060` pins table-column-style to `aligned`; the
`pre-commit` hook pads cell widths with `scripts/align-md-tables.py` before the auto-fixer runs,
because that rule has no auto-fixer of its own.

## Mutation testing — the gremlins fork

`mutation.yml` installs the `tsouza/gremlins` fork rather than upstream `go-gremlins/gremlins@v0.6.0`:

```bash
go install github.com/tsouza/gremlins/cmd/gremlins@v0.6.0-cerberus-unary-operators-consume
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
the child's output instead — the `panic: test timed out after` line maps to `TIMED OUT`,
`[build failed]` and `[setup failed]` map to `NOT VIABLE`, and anything else is left to the exit
status. `TIMED OUT` stays in the efficacy denominator and credits nobody, so a slow test cannot buy
a score. The scan is streaming and retains only enough bytes to recognise a marker split across two
writes, so a mutant that prints without bound cannot exhaust memory.

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

`.gremlins.yaml`'s `exclude-files` paths are interpreted relative to the run's scope, not the repo
root, and the matcher is RE2 with no lookahead. A path in the wrong form silently excludes nothing.

The fork ships two branches on purpose. `cerberus-sigterm-fix` at tag
`v0.6.0-cerberus-unary-operators` is the branch the upstream pull request is built from, and keeps
the upstream module path `github.com/go-gremlins/gremlins` so the diff stays reviewable.
`cerberus-sigterm-fix-consume` at tag `v0.6.0-cerberus-unary-operators-consume` is the branch
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
