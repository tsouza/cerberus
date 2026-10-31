# cerberus governance

cerberus is a drop-in Prometheus, Loki and Tempo HTTP gateway for ClickHouse. This document describes
how the project is run: who decides, how decisions are made, how people become maintainers, and what
happens if a maintainer can no longer continue.

## Values

- **Openness.** Discussion and decisions happen in public, in GitHub issues and pull requests, where
  they stay discoverable.
- **Fairness.** Anyone can propose a change, and every contribution is judged on its merits.
- **Community over product or company.** cerberus is not a part of any company's product. Each
  contributor participates as an individual.
- **Vendor neutrality.** Maintainer selection, roadmap priorities and release decisions are made on
  project merit, not on anyone's employer.

## Roles

- **Contributors** are anyone who opens an issue, reviews a pull request, or submits a change. No
  permission is needed to contribute; [CONTRIBUTING.md](CONTRIBUTING.md) describes how.
- **Maintainers** are listed in [MAINTAINERS.md](MAINTAINERS.md). They have write access to the
  repository, review and merge pull requests, triage issues, cut releases, and are responsible for
  the project's direction, its security response and its Code of Conduct enforcement.

## Decision making

Day-to-day decisions are made by lazy consensus on the issue or pull request concerned: a proposal
stands unless a maintainer objects. Any maintainer can call for a vote instead.

- A vote passes with a simple majority of all maintainers, except where this document says
  otherwise.
- A vote takes place on a public GitHub issue labelled `governance`, open for at least seven days, or
  until every maintainer has voted. Security and Code of Conduct matters are decided privately among
  the maintainers instead.
- While the project has a single maintainer, that maintainer's decision is the decision, and it is
  recorded on the issue or pull request concerned in the same way.

## Becoming a maintainer

A contributor becomes a maintainer by showing sustained, high-quality involvement:

- contributions over at least three months;
- at least five non-trivial pull requests merged, and regular reviews of other people's changes;
- an understanding of the project's architecture, its test layers, and the invariants in
  [CLAUDE.md](CLAUDE.md) and [CONTRIBUTING.md](CONTRIBUTING.md).

An existing maintainer nominates the contributor on a public `governance` issue, and a majority vote
of the maintainers approves the nomination. Nominations are judged without regard to employer, and
the maintainers weigh the organizational diversity of the maintainer group. A new maintainer is added
to [MAINTAINERS.md](MAINTAINERS.md) and granted write access.

## Stepping down and removal

A maintainer can step down at any time by opening a pull request that moves them to the emeritus
section of [MAINTAINERS.md](MAINTAINERS.md).

A maintainer who has been inactive for twelve months, or who has violated the Code of Conduct, can be
moved to emeritus status by a two-thirds vote of the other maintainers. An emeritus maintainer
returns to active status through the nomination process above.

## Continuity

The maintainers are committed to the project for the long term and keep maintaining it for as long as
they have the time and means to do so.

A maintainer who can no longer continue announces it on a public `governance` issue at least thirty
days before stepping down, wherever circumstances allow. If that would leave the project without a
maintainer, the departing maintainer first offers maintainership to the most active contributors who
meet the criteria above, and hands over repository administration, release signing and the project's
accounts to whoever accepts.

If no successor accepts, the repository is archived with a notice in the README, and its issues stay
readable. cerberus is licensed under Apache-2.0, so anyone can fork it and continue the work at any
time, with or without the maintainers.

## Code of Conduct

Everyone in the project's spaces follows the [Code of Conduct](CODE_OF_CONDUCT.md). Reports go to the
address it lists and are handled privately by the maintainers. A maintainer who is the subject of a
report takes no part in handling it.

## Security response

The maintainers handle vulnerability reports as described in [SECURITY.md](SECURITY.md).

## Changing this document

Changes to this document are made by pull request and need a two-thirds vote of the maintainers.
