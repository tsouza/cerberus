# Roadmap

This roadmap lists the directions cerberus is working in. Each theme links to the GitHub issues that
track it, and those issues carry the current status, the scope and the discussion. The roadmap is
reviewed with every minor release. To propose a direction, open an issue; to help with one, comment
on its issues, or start from [`good first issue`][gfi] and [`help wanted`][hw].

## Query-language parity

cerberus answers PromQL, LogQL and TraceQL exactly as Prometheus, Loki and Tempo do, measured by the
differential harnesses described in [docs/compatibility.md](docs/compatibility.md). Every difference
from a reference backend is a bug, fixed at its source.

- Close every open divergence on the three heads: [PromQL][promql], [LogQL][logql],
  [TraceQL][traceql].
- Widen the TraceQL corpus. No third-party TraceQL conformance suite exists, so its corpus is the
  project's own and its confidence the lowest of the three heads; every reported TraceQL divergence
  becomes a corpus case.
- Divergences that need a fix in an upstream dependency are tracked under [`upstream`][upstream]
  until that fix lands.

## A fourth head: continuous profiling

A head that serves the Grafana Pyroscope datasource from the OpenTelemetry profiles signal, which
the OpenTelemetry Collector's ClickHouse exporter writes to `otel_profiles`.

- **Query language.** FlameQL is a profile type plus Prometheus-style label matchers; grouping and
  limits are request fields, not syntax.
- **Wire protocol.** The datasource speaks Connect RPC with protobuf messages. The message
  definitions are the Apache-2.0 `github.com/grafana/pyroscope/api` module, so the binary stays free
  of AGPL code.
- **Server floor.** The exporter's profiles tables need ClickHouse 26.2 or later.
- **First stage:** the profile-type, label-name and label-value queries and `SelectSeries`, which
  the existing plan IR already expresses.
- **Second stage:** merged stack traces, flame graphs and profile diffs, which return a tree rather
  than a series and need a new plan node and result path.

The OpenTelemetry profiles signal is still in development and can change incompatibly; the head
follows it.

Tracked in [#4019](https://github.com/tsouza/cerberus/issues/4019).

## Authentication

cerberus ships no authentication and relies on the deployment boundary in front of it
([docs/operations.md](docs/operations.md#security-posture)). Built-in, opt-in authentication lets
Grafana connect to cerberus directly, using credentials the Grafana Prometheus, Loki and Tempo
datasources already send:

- **Basic auth and bearer tokens**, checked against hashed credentials in the configuration.
- **OpenID Connect.** The datasources' Forward OAuth identity option forwards the signed-in user's
  token; cerberus validates it against the identity provider's published keys, issuer, audience and
  expiry.
- **Mutual TLS.** Client certificates verified against a configured certificate authority.

Authentication is off by default, so a deployment behind an authenticating proxy is unchanged, and
the health and readiness probes stay unauthenticated.

Tracked in [#4020](https://github.com/tsouza/cerberus/issues/4020).

## Multi-tenancy

One deployment serving several isolated tenants over one ClickHouse cluster, opt-in.

- **Tenant identity.** The `X-Scope-OrgID` header that Loki, Mimir and Tempo use, so existing
  datasources work unchanged, and a request without a tenant is rejected. With authentication
  enabled, the tenant comes from the authenticated identity, and the header is trusted only from a
  configured proxy.
- **Isolation enforced by ClickHouse.** Each tenant maps to a ClickHouse user whose grants and row
  policies scope what it reads: a database per tenant, or row policies on shared tables. cerberus
  runs each tenant's queries as that user, so isolation does not depend on every lowering emitting
  the right predicate.
- **Per-tenant limits** through ClickHouse quotas and settings profiles, so one tenant's load cannot
  starve another.
- **Verification.** Cross-tenant leak tests on all three heads and every metadata endpoint, and a
  correctness check of every ClickHouse optimization cerberus enables when a row policy applies.
- **Later stage:** cross-tenant queries in Loki's `tenant-a|tenant-b` form.

Tracked in [#4021](https://github.com/tsouza/cerberus/issues/4021).

## ClickHouse releases

cerberus tracks each ClickHouse release for new functions and settings it can use, and for fixes and
regressions that change what it can safely enable. Each supported server line has a version floor
per optimization, recorded in [docs/clickhouse-optimizations.md](docs/clickhouse-optimizations.md).

- Release-by-release capability audits: [`ch-capability-audit`][audit].
- Keep the bundled ClickHouse in the Helm chart and quickstart on a current long-term-support line.

## Performance and resource safety

Every query is computed by ClickHouse, and cerberus's job is to lower it to SQL that ClickHouse runs
fast and within its memory limit.

- Cheaper lowerings and new optimizer rules: [`performance`][perf].
- Every unbounded operator has a memory bound or a spill-to-disk backstop, on every serving path.
- Performance baselines are measured per release and gate the next one; see
  [docs/performance.md](docs/performance.md).

## Test and CI health

The test suite is the project's specification, and a slow or flaky lane erodes it.

- Keep every CI lane well inside its time budget as the corpus grows: [`area/ci`][ci].
- Keep every local recipe runnable on a developer machine.

## Community

- Grow the contributor base: documented paths from first issue to maintainer
  ([GOVERNANCE.md](GOVERNANCE.md)), and issues labelled for newcomers.
- Apply to the CNCF as a Sandbox project.
- An adopters list, as deployments become public.

## Out of scope

These are boundaries of the project, not gaps in it:

- **Storage and ingestion.** cerberus reads the OpenTelemetry ClickHouse schema that the Collector's
  exporter writes; it does not ingest telemetry.
- **Caching query results.** cerberus never caches an answer; see invariant 12 in
  [CLAUDE.md](CLAUDE.md).
- **New query languages.** cerberus speaks the upstream languages as they are.
- **AGPL code in the binary.** The LogQL and TraceQL parsers are clean-room Apache-2.0
  reimplementations.
- **Identity management.** Users, groups and roles live in the identity provider; cerberus verifies
  identities and does not store them.

[gfi]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3A%22good+first+issue%22
[hw]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3A%22help+wanted%22
[promql]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3Aarea%2Fpromql
[logql]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3Aarea%2Flogql
[traceql]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3Aarea%2Ftraceql
[upstream]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3Aupstream
[audit]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3Ach-capability-audit
[perf]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3Aperformance
[ci]: https://github.com/tsouza/cerberus/issues?q=is%3Aopen+is%3Aissue+label%3Aarea%2Fci
