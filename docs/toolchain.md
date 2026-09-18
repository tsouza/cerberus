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
  run's `coverage-profile` artifact into the repository root — all of it, not just the profile — and
  run `just update-coverage-floor` against the `cover-merged.out` it contains. The upload happens
  even when the floor gate itself is red, which is what makes an unenrolled package recoverable at
  all.

Either way, the floors come from a profile carrying BOTH lanes or they are not recorded at all.
`just coverage-merge` stamps the lane set it merged onto the profile as `cover-merged.out.lanes.json`,
bound to that profile's own SHA-256, and `just update-coverage-floor` refuses a profile whose record
is missing, narrower than `default+chdb`, or bound to different bytes. That record is part of the
`coverage-profile` artifact, which is why the whole artifact is what gets downloaded; a run whose
lane jobs did not all succeed produces no record naming both lanes, so the recipe declines it.

`just update-coverage-floor` only ratchets up. It refuses to lower a floor to match a coverage drop
and never records a `0`, so both of those stay hand-edited, reviewable lines in a diff. It also
refuses to DELETE one: the recipe rewrites the whole ledger from the profile in hand and reads the
tree to tell the two ways an entry can go missing apart — a package whose directory still holds
non-test Go files should have been measured, and its absence is a refusal naming it, while a package
whose directory is gone is one the module no longer has and its floor is dropped with a notice.
Removing a package therefore stays a plain `just update-coverage-floor` away, and enrolling from an
artifact older than a package in the tree fails loudly.

The margin a floor grants — the slack between the measurement and the floor it justifies — is the
wider of one percentage point and one statement. Widening a slack can only lower the floor a fresh
measurement justifies, and the ratchet keeps the greater of that and the committed value, so this
never moves an entry already in the ledger.

Both coverage lanes export `CERBERUS_RAPID_SEED` from the Justfile's `COVERAGE_RAPID_SEED`, and every
test binary that links `pgregory.net/rapid` honours it in an `init` that pins `-rapid.seed` before
`flag.Parse` — per binary, because rapid registers that flag from its own `init`, so a lane-wide
`go test -rapid.seed=N ./...` would abort every package that does not link it.
`test/regression/rapid_seed_pin_test.go` derives the set of packages owing a pin from the import
graph, so a new rapid-driven package cannot rejoin the unpinned set. `just property` and
`property.yml` deliberately leave the variable unset: that lane SEARCHES for counterexamples and
needs a fresh sample every run, where the coverage lane MEASURES and needs the same one. The seed's
value is arbitrary but permanent — changing it re-rolls every rapid-driven package's coverage
against floors derived from the old draw.

`actionlint` (`just lint-actions`) validates the workflow files. GitHub rejects an invalid workflow
file server-side as a zero-job failure run, which prevents required `pull_request` checks from ever
being scheduled — a PR then sits blocked on contexts that can never report.

`markdownlint-cli2` with `.markdownlint.yaml`. `MD060` pins table-column-style to `aligned`; the
`pre-commit` hook pads cell widths with `scripts/align-md-tables.py` before the auto-fixer runs,
because that rule has no auto-fixer of its own. `just fmt-md` therefore does not fix everything
`just lint-md` reports — when MD060 survives the fix pass it names the padding script rather than
leaving a clean-looking run that still fails the lint.

The engine version is declared exactly once, as `PINNED_CLI2_VERSION` in
`.github/scripts/markdownlint-run.mjs`, and all three callers route through that module: the
`lint-md` / `fmt-md` recipes, lefthook's `markdownlint` hook, and the `lint` job. Bumping the engine
is one literal, and the bump belongs in the same change as whatever the newer engine surfaces. The
hook is the one caller that cannot use `npm exec` — npm's startup alone is ~4s against a sub-second
`pre-commit` budget — so it prefers a `$PATH` binary, but only at exactly the pinned version, and
otherwise falls back to the pinned `npm exec`. `just install-tools` installs the matching binary so
that fast path is the default.

## Mutation testing — the gremlins fork

`mutation.yml` installs the `tsouza/gremlins` fork rather than upstream `go-gremlins/gremlins@v0.6.0`:

```bash
go install github.com/tsouza/gremlins/cmd/gremlins@v0.6.0-cerberus-workdir-fd-leak-consume
```

The fork carries these behaviours on top of upstream; each is what the lane relies on.

**`--on-shutdown-status`.** A mutant whose `go test` child is still running when the run is
cancelled (SIGTERM then SIGKILL, the CI runner sequence) is reported with the status named by this
flag instead of `LIVED`, and a second signal no longer panics the handler. Cerberus passes
`--on-shutdown-status=not-run`, which lands those mutants in `NOT_COVERED`, outside the
`KILLED / (KILLED + LIVED)` efficacy formula entirely.

**`--timeout-max`.** An absolute upper bound on a mutant's test timeout, independent of upstream's
`timeout-coefficient × baseline duration` derivation. Cerberus derives its value per leg and clamps
it into `[MUTANT_TIMEOUT_MIN, MUTANT_TIMEOUT_MAX]`, declared in `mutation.yml`. A runaway mutant is
bounded by this flag, never by excluding the file the log names: excluding a file relocates its
runaway mutants into whichever leg still owns it and burns real mutation coverage.
`test/regression/mutation_timeout_max_test.go` pins the flag and the fork tag together.

**`--compile-allowance`, and verdicts read from the output.** The run bound is handed to `go test
-timeout`, whose clock starts when the test binary starts; the context deadline over compile and run
— widened by `--compile-allowance` — stays as the backstop for a compile that has hung. The verdict
is read from the child's output rather than its exit status: the `panic: test timed out after` line
maps to `RUN TIMED OUT`, `[build failed]` and `[setup failed]` map to `NOT VIABLE`, and anything else
is left to the exit status. The scan is streaming and retains only enough bytes to recognise a marker
split across two writes.

**The two bounds report which of them claimed a mutant:**

| status          | what fired                                                         | what it proves                                               |
| --------------- | ------------------------------------------------------------------ | ------------------------------------------------------------ |
| `RUN TIMED OUT` | the test binary's own `-timeout` watchdog, which printed the panic | the suite did not finish inside a bound no compile can spend |
| `TIMED OUT`     | the context deadline over compile **and** run                      | nothing — a hung compile and a hung run reach it identically |

The marker is read before either deadline and is guarded on the child having failed, so a suite that
passes while printing those bytes stays `LIVED`. gremlins takes no position on which status is a
detection — neither appears in its own `test_efficacy` — so the policy lives one layer up, in
`.github/scripts/gremlins-threshold.mjs`, which counts `RUN TIMED OUT` as a detection and leaves
`TIMED OUT` in the denominator crediting nobody.

**Prefix operators read as prefix operators.** The mutation table is consulted for a
`*ast.UnaryExpr` only for the prefix operators whose infix mutations carry over unchanged, `+x` and
`-x`; `&x` (address-of) and `^x` (complement) are not mutated against their infix meanings. The
`NOT VIABLE` classification stays for a genuine build failure from any other source.

**A candidate mutant is type-checked before it is emitted.** The fork type-checks the candidate's
whole **package** and drops a candidate the checker rejects; generation runs on its own goroutine
behind the executor pool. A package that cannot be loaded and type-checked as it stands is not used
as an oracle: its mutants are generated and left to the compiler, and a log line names the package.

**Mutated tests run with `-vet=off`.** `go test`'s built-in vet subset would otherwise report a legal
`INVERT_LOGICAL` mutant as `[build failed]`; with vet off those mutants get a real verdict.

`.gremlins.yaml`'s `exclude-files` paths are interpreted relative to the run's scope, not the repo
root, and the matcher is RE2 with no lookahead. A path in the wrong form silently excludes nothing.

**`workdir.CachedDealer` closes both copy handles** in `doCopy`, so a run's open-file count does not
grow with the tree size times the worker count.

The fork ships two branches. `cerberus-sigterm-fix` at tag `v0.6.0-cerberus-workdir-fd-leak` keeps
the upstream module path `github.com/go-gremlins/gremlins` and is the branch the upstream pull
requests are built from. `cerberus-sigterm-fix-consume` at tag
`v0.6.0-cerberus-workdir-fd-leak-consume` is the branch `mutation.yml` installs; it adds one commit
renaming the `go.mod` module path to `github.com/tsouza/gremlins` and rewriting the internal imports,
because `go install` otherwise rejects the module with `module declares its path as:
github.com/go-gremlins/gremlins`. The fixes themselves are identical across the two. Both branches
carry every fix listed above — each new fix fast-forwards both branches and gets its own pair of
tags, so a tag name here always names the LATEST fix landed, not a snapshot frozen at that fix alone.

Unlike the module forks, this one sits outside the Dependabot watch flow: it is a build-time tool
rather than a Go module dependency.

## chDB

The chdb-tagged lanes — the `-- expected_rows --` roundtrip cells in `test/spec/`, the property
tests, and the cardinality baseline — link `libchdb.so`, installed by `just chdb-install`. Generated
artefacts are regenerated by dispatching `update-golden.yml` against the topic branch
(`docs/agent-workflow.md`); the workflow installs `libchdb.so` itself. A local `just update-golden`
is blocked by the `guard-heavy-local.mjs` hook and, when run with the escape hatch, refuses to run
any chdb-tagged shard without `libchdb.so` rather than regenerating a partial corpus.

chDB and a production ClickHouse server differ in scan strictness: chDB coerces some column types
that the server rejects outright. An emit-type bug can therefore pass every chDB lane and fail
against a real server, which is why three lanes run against a real ClickHouse: the required
`strict-scan` and `schema-ddl` contexts (testcontainers, one narrow seam each) and the release-gate
`compose-smoke` (the whole quickstart stack). `compose-smoke` does not scope to a diff's touched
paths: an ordinary PR omits it entirely (the required `quickstart` context covers the
published-startup contract with one stack instead), and it runs the full sweep only on `release/*`
PRs, `push`, and `schedule` — see `.github/scripts/compose-smoke-scope.mjs`.

---

For the rationale behind these choices — alternatives considered, incidents,
measurements — see [toolchain.background.md](toolchain.background.md).
