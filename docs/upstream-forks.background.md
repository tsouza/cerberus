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
