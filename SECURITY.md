# Security policy

## Supported versions

cerberus is pre-1.0; only the latest tagged release (or `main` if no tag exists) is supported. Once v1.0 ships, this section will document an N-1 support window.

## Reporting a vulnerability

**Do not open a public PR, GitHub Issue, or GitHub Discussion** for a security report — public disclosure before a patch is ready helps attackers. The secure channels are:

1. **GitHub private vulnerability reporting** — preferred. Click the `Security` tab on <https://github.com/tsouza/cerberus> and use "Report a vulnerability". This routes through GitHub's [private advisory flow](https://docs.github.com/en/code-security/security-advisories) and stays confidential until a fix ships.
2. **Email** — `tcostasouza@gmail.com` with `[cerberus security]` in the subject if you can't use the GitHub flow.

Please include:

- A clear description of the issue.
- A minimal reproducer (a TXTAR fixture, a curl command against `/api/v1/query`, etc.).
- The version of cerberus + ClickHouse you tested against.
- Your assessment of impact (information disclosure, denial of service, unauthenticated execution, etc.).

## What to expect

- **Acknowledgement** within 72 hours.
- **Triage** (a public-facing patch plan, or "this is not a vulnerability, here's why") within 7 days.
- **Coordinated disclosure** — we'll work with you on a timeline. Default window is 90 days from the initial report unless severity dictates faster, in which case we'll publish the advisory + patch on the same day.

## Threat model in scope

- Query-injection / SQL-construction safety in `internal/chsql`. Arguments are bound positionally via `?` placeholders; bugs that allow literal interpolation are in scope.
- Authentication and tenant-isolation flaws once the multi-tenant header (`X-Scope-OrgID`) lands.
- ReDoS or super-linear parser pathologies in any of the three QL parsers (cerberus passes input verbatim to upstream parsers; if a query is rejected upstream as expensive, cerberus should reject it too).
- Information disclosure via error messages (CH-side schema details leaking into Prometheus-shaped error responses).

## Out of scope

- Bugs in upstream `prometheus/prometheus`, `grafana/loki`, `grafana/tempo`, or `clickhouse-go` — please file upstream.
- Misconfigurations in deployments (e.g. exposing CH without auth in front of cerberus).
- Denial of service via legitimately expensive queries; cerberus relies on ClickHouse's query-time controls. We'll happily document mitigations.

## Cryptographic hardening

cerberus doesn't ship cryptographic primitives. The HTTP server delegates TLS to a reverse proxy (per the deployment manifests in `deploy/k3s/`). Bugs in upstream TLS / authn libraries should go to those projects.

## Hall of fame

We'll list responsible reporters here once we have any. Add yourself in the same PR that fixes the issue if you'd like attribution.
