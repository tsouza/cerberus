# Contributor ladder

This ladder describes the roles in the cerberus project: what each role is responsible for, what it
takes to reach it, and what it grants. The process for adding and removing people is in
[GOVERNANCE.md](GOVERNANCE.md), and the current reviewers and maintainers are listed in
[MAINTAINERS.md](MAINTAINERS.md).

Every role is held by a person. Contributions written with AI tools count toward a role the same way
as any other contribution. Each role's requirements are met by the understanding and judgment a
person shows: in review, in design discussion, and in answering for their changes.

## Areas

Reviewers own the review of one area. An area is a set of paths in
[.github/CODEOWNERS](.github/CODEOWNERS).

| Area                       | Paths                                                                                              |
| -------------------------- | -------------------------------------------------------------------------------------------------- |
| PromQL head                | `internal/promql/`, `internal/api/prom/`, `compatibility/prometheus/`                              |
| LogQL head                 | `internal/logql/`, `internal/drain/`, `internal/api/loki/`, `compatibility/loki/`                  |
| TraceQL head               | `internal/traceql/`, `internal/api/tempo/`, `compatibility/tempo/`                                 |
| Plan IR and optimizer      | `internal/chplan/`, `internal/optimizer/`                                                          |
| SQL and ClickHouse         | `internal/chsql/`, `internal/engine/`, `internal/chopt/`, `internal/chclient/`, `internal/schema/` |
| Deployment and operations  | `deploy/`, `internal/config/`, `cmd/cerberus/`                                                     |
| CI and test infrastructure | `.github/`, `test/`, `Justfile`, `just/`                                                           |

## Contributor

Anyone who takes part in the project: opening an issue, commenting on a design, reviewing a pull
request, improving the documentation, or submitting a change.

- **Responsibilities:** follow the [Code of Conduct](CODE_OF_CONDUCT.md) and
  [CONTRIBUTING.md](CONTRIBUTING.md), including its
  [AI-assisted contributions](CONTRIBUTING.md#ai-assisted-contributions) policy.
- **Requirements:** none.
- **Privileges:** open issues and pull requests, take part in discussions, and be considered for the
  reviewer role.

## Reviewer

A contributor who owns the review of one [area](#areas).

- **Responsibilities:**
  - Review pull requests that touch the area, within five working days of being requested.
  - Check that a change keeps the area's behaviour, its tests and its documents consistent, and that
    each new test fails when the code under it is broken.
  - Triage the area's issues and keep them labelled.
  - Help new contributors to the area.
- **Requirements:**
  - Contributions to the area over at least two months.
  - At least five substantive reviews or merged pull requests in the area.
  - A working knowledge of the area's design, its test layers
    ([docs/test-strategy.md](docs/test-strategy.md)), and the hard invariants in
    [CLAUDE.md](CLAUDE.md).
  - Sponsorship by a maintainer.
- **Privileges:**
  - Listed in [MAINTAINERS.md](MAINTAINERS.md) and in [.github/CODEOWNERS](.github/CODEOWNERS) for
    the area, so that changes to it request their review.
  - Triage permission on the repository.

## Maintainer

A reviewer who shares responsibility for the whole project.

- **Responsibilities:**
  - Everything a reviewer does, across the project.
  - Decide which changes merge, and answer for every change they merge, whoever or whatever wrote it.
  - Set and keep the project's direction, its roadmap ([ROADMAP.md](ROADMAP.md)) and its invariants.
  - Cut releases, respond to security reports ([SECURITY.md](SECURITY.md)), and enforce the Code of
    Conduct.
  - Mentor reviewers toward the maintainer role.
- **Requirements:**
  - A reviewer for at least three months.
  - Reviews across more than one area, and the ownership of at least one.
  - A working knowledge of the whole architecture ([docs/engine.md](docs/engine.md)): able to explain
    and defend a design decision anywhere in the codebase.
  - Nomination and election as described in [GOVERNANCE.md](GOVERNANCE.md).
- **Privileges:**
  - Write access to the repository.
  - A vote in project decisions.
  - Administration of the repository and of the project's accounts, shared among the maintainers.

## Inactivity and stepping down

A reviewer or maintainer who expects to be away for more than a month says so on a public
`governance` issue. Stepping down, emeritus status and removal for inactivity are described in
[GOVERNANCE.md](GOVERNANCE.md#stepping-down-and-removal).
