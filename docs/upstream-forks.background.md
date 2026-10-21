# Upstream tracking — background

This is the reasoning behind [`upstream-forks.md`](upstream-forks.md).

## Why the LogQL and TraceQL parsers are in-house

Grafana's LogQL and TraceQL parsers are AGPLv3, and cerberus ships under
Apache-2.0. Linking either would put the binary under the AGPL, so both
grammars were reimplemented clean-room from their published specifications.
The AGPL parsers stay useful as differential oracles, which is why they
survive in test-only code behind a build tag and in the compatibility
harnesses.

The licence rests on reachability rather than on a module boundary because
the root module has to require `grafana/loki/v3` for those harnesses, and
`compatibility/` has no `go.mod` of its own. A module boundary alone would
not keep an AGPL package out of the binary; `go list -deps ./cmd/cerberus` is
the check that does.

## Why forks at all

A fork gives cerberus a place it controls between an upstream and its own
`go.mod`. Two different needs use that place:

- **A place to patch.** The collector-contrib fork hoists `sqltemplates` out
  of `internal/`, and the tempo fork exports accessors. Without a fork the
  only alternatives are copying the code wholesale, which loses upstream
  tracking, or reaching into unexported state with `unsafe.Pointer` or
  `reflect`, which invariant 11 forbids.
- **A place to choose the base.** The prometheus fork carries no patch. It
  exists so that the parser cerberus compiles is pinned to a commit cerberus
  chose, and so that a future accessor has somewhere to land without a new
  `replace` and a new review of the dependency graph.

## Why the prometheus fork follows releases, not upstream main

Cerberus's PromQL claims are graded against a real Prometheus server: the
compatibility lane runs `prom/prometheus`, and the spec lane's in-process
oracle runs the same module's `promql.Engine`. A parser built from upstream
`main` would accept or reject grammar that no released server does, and the
oracle would evaluate semantics that differ from the image the compatibility
lane diffs against. Parity certified across that gap is unsound for whatever
moved between the two.

Upstream `main` also reports the last release in its `VERSION` file, so a
fork tag cut on `main` cannot be told apart by name from one cut on the
release. Naming the tag after the release's library version
(`v0.315.0-cerberus-parser` for `v3.15.0`) and cutting it only on the release
commit lets the version-skew gate check, from committed text alone, that the
parser, the oracle and the server are one release.

For the same reason the monitor's relevance filter does not apply to
prometheus. The filter answers "did anything cerberus imports change on
upstream main?", which is the right trigger for a fork that tracks main. For
a fork that tracks releases, every release is relevant, because the
reference server cerberus is graded against moves with it.

## Why the relevance filter exists for the collector-contrib fork

Collector-contrib is a monorepo that releases often, and cerberus consumes
one directory of templates from it. Rebasing and retagging on every upstream
commit would produce a stream of tags that change nothing cerberus reads.
The monitor's `relevant_paths` filter mints a tag only when the templates
themselves (or their pre-hoist location) changed.

## Why the metrics tables lead with MetricName

ClickHouse's `ORDER BY` (the table sort key) governs data skipping and
on-disk layout, never query results, so the MetricName-first key is
correctness-neutral: cerberus answers identically over the stock key or this
one. The common metric query filters on the metric name and has no
`service.name` matcher. With the name first, ClickHouse binary-searches the
primary key instead of falling back to a generic-exclusion granule scan,
measured at about 17× fewer granules read (see
[`benchmarks.md`](benchmarks.md#metricname-first-order-by)). The traces and
logs tables gain nothing from a change and stay stock.

## Why tempo is forked for `vparquet4.NewSpan`

TraceQL's structural operators (`>>`, `>`, `~`) are declared on the
`traceql.Span` interface but implemented in `tempodb/encoding/vparquet4` on an
unexported concrete type, which each of them type-asserts its arguments back
to. A caller that supplies its own `traceql.Span` cannot reach those methods,
so an oracle would have to reimplement the operators, turning it into a
mirror of the code under test. The three methods read nothing but the
nested-set integers (no parquet page, no row number), so a span built from
plain data drives them exactly as a decoded one does; upstream's own
`TestDescendantOf` builds spans as bare struct literals on that basis.
`NewSpan` is an exported spelling of that literal.

The tempo fork is moved by hand rather than by the forks-monitor. Its
`require` is a pseudo-version that the version-skew gate holds to the commit
the Tempo reference image is built from, so moving the fork moves the
reference too, which is a deliberate change rather than a daily sync.

## Why prometheus is not auto-queued

Dependabot bumps `require` lines against the upstream module and does not
rewrite `replace` targets. For prometheus that produces a pull request that
moves the `require` while the fork tag and the reference image stay on the
old release, and the version-skew gate rejects exactly that. Queueing it
would only park a pull request that cannot pass. Keeping prometheus in its
own Dependabot group means that bump never holds a collector-contrib or Loki
bump hostage in a shared grouped pull request. The pull request still has a
use: it is the notice that a release exists, and the companion changes turn
it into the release move.

## Why fork-tag drift lives in cerberus

Dependabot's Go updater bumps `require` lines against the upstream module
path and never rewrites a `replace` target, so it does not see a fork tag at
all. Before the drift job existed every move of a `tsouza/*` replace was made
by hand, and `v0.0.4-cerberus-ddl` sat on the collector-contrib fork for
months while `go.mod` pinned `v0.0.3-cerberus-ddl`.

The job runs in cerberus rather than in the forks-monitor because cerberus
owns the `replace` lines, the `go mod tidy` of each nested module, the
pull-request gates and the merge queue. A monitor that opened the pull
request would need write access to cerberus and a copy of its module layout,
and would still miss a tag minted by hand, as tempo's are. Polling every fork
daily from cerberus catches a tag however it was made.

It opens a pull request rather than failing a scheduled run, because a red
scheduled run reaches nobody, while a pull request runs every gate against the
bump and stays visible until someone merges or closes it. The pull request
needs `RELEASE_PAT`: one opened with the workflow's own token starts no
`pull_request` checks.

The branch is named per fork and left alone once it pins the target, so a
daily run neither restarts an open pull request's checks nor reopens one a
maintainer closed. A newer tag on the same fork rewrites the branch.

### Why the bump pull request is never queued automatically

A fork tag is a rebase onto upstream, and upstream changes things cerberus
depends on. `v0.0.4-cerberus-ddl` carried upstream's metrics schema rewrite
(open-telemetry/opentelemetry-collector-contrib#49438): the metrics
`TimeUnix` columns became second-precision `DateTime`, which cerberus's
emitted `toUnixTimestamp64Nano(TimeUnix)` rejects, and the mechanical rebase
turned the fork's MetricName-first sort-key patch into a different key. It
built cleanly, and the chDB suites passed because they seed their own
`DateTime64(9)` tables. `TestTimestampColumnsAreNanosDateTime64_InUpstreamDDL`
exists so that kind of bump fails on its own pull request. That decision
was made in `v0.0.5-cerberus-ddl`: the fork renders the metrics tables at
either precision, keeps the MetricName-first key for both, and cerberus
provisions nanoseconds unless told otherwise while reading whichever type a
deployment carries. A human reads each bump before it is queued.

### Why prometheus gets an issue instead

The prometheus fork's tag core equals its `require` version, because both
name the upstream release the reference image runs. A replace-only bump would
fail `TestForkVersionSkew`, so the job opens an issue that names the release
move. Detecting the coupling from `go.mod` itself, rather than from a list of
fork names, keeps the rule true for any future fork that tracks releases the
same way.

## Why the version-skew gate checks each head at its grain

- **Prometheus at the exact release.** The require, the fork tag and the
  image all name one release commit, so anything coarser would let them
  disagree on a patch release.
- **Loki at MAJOR.MINOR.** The reference image may lag the Go library by a
  bug-fix patch (go.mod `v3.7.1` against image `3.7.0`), and patch releases
  do not move the grammar or evaluation semantics parity rests on.
- **Tempo at the commit prefix.** It is pinned by commit on every side, so
  the commit is the only grain there is.

The tempo `replace` assertion keeps the `require` meaningful once a
`replace` supersedes it: without it, tempo could be repointed at an
arbitrary module while the skew table went on comparing a version nothing
compiles against.

## Why the transitive replaces and ignores exist

Without the `memberlist` replace, the root module resolves upstream
`hashicorp/memberlist`, which lacks the `NodeState` / `NodeSelection` /
`PushPullNodes` symbols `dskit/kv/memberlist` references, and the build fails
on undefined identifiers. Dependabot groups it with the parser modules
because those modules share state through `dskit` and `memberlist`; bumping
them one at a time produces partial-tree builds.

Without the `ignore` directives, the vendored upstream sources under
`compatibility/{tempo,loki}/upstream` would be root-module packages that
every `./...` pattern (`go build`, `go vet`, `go test`, `go list`) tries to
compile against the root module's dependency graph. The `agpl-clean` gate is
unaffected either way, because nothing under `compatibility/` is reachable
from the binary.
