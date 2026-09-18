# Security policy

## Supported versions

Security fixes land on the current minor release line only — see [`docs/operations.md` → Release support window / EOL policy](docs/operations.md#release-support-window--eol-policy). "Latest release (or `main`)" is the supported surface; an older minor line is end-of-life the moment a new minor ships.

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
- ReDoS or super-linear parser pathologies in any of the three QL parsers. PromQL is parsed by the upstream `prometheus/prometheus` parser (a pathology there is in scope for cerberus only where cerberus fails to apply the same rejection Prometheus applies); LogQL (`internal/logql/lsyntax`) and TraceQL (`internal/traceql/ast`) are cerberus's own clean-room parsers, so any pathology in them is a cerberus bug — report it here, not to Grafana.
- Information disclosure via error messages (CH-side schema details leaking into Prometheus-shaped error responses).

## Out of scope

- Bugs in the upstream libraries cerberus links — the `prometheus/prometheus` PromQL parser and `clickhouse-go` — please file upstream. (`grafana/loki` and `grafana/tempo` are not linked into the binary; their query languages are reimplemented in-house, and a bug in those reimplementations belongs here.)
- Misconfigurations in deployments (e.g. exposing CH without auth in front of cerberus). Cerberus itself ships no authentication, authorization, or tenant isolation — [`docs/operations.md` → Security posture](docs/operations.md#security-posture) documents the boundary an operator has to provide.
- Denial of service via legitimately expensive queries; cerberus relies on ClickHouse's query-time controls. We'll happily document mitigations.

## Cryptographic hardening

cerberus doesn't ship cryptographic primitives. The HTTP server delegates TLS to a reverse proxy (the example manifests under `test/e2e/k3s/` show one wiring). Bugs in upstream TLS / authn libraries should go to those projects.

## Hall of fame

We'll list responsible reporters here once we have any. Add yourself in the same PR that fixes the issue if you'd like attribution.
