# Changelog

All notable changes to cerberus will be documented in this file. The format roughly follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one entry per tagged release.

## [Unreleased]

### Added

- Three-phase RC roadmap to v1.0.0 (`docs/roadmap.md`).
- AI-agent context (`CLAUDE.md`, `AGENTS.md`, three `.claude/skills/`).
- Engineering hygiene: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `.github/CODEOWNERS`, PR template.
- Release plumbing: `.goreleaser.yml`, `Dockerfile` (distroless), `.github/workflows/release.yml`.

### Changed

### Fixed

## [v0.1.0] — Seed

First tagged release. Closes the seed series (PR1–PR7 + admin + roadmap):

- Module `github.com/tsouza/cerberus` on `go 1.26.2` with the `replace github.com/hashicorp/memberlist => github.com/grafana/memberlist@…` hygiene fix.
- Shared plan IR (`internal/chplan`), ClickHouse SQL emitter (`internal/chsql`), TXTAR spec runner under `test/spec/`.
- Rule-based optimizer (`internal/optimizer`) with three rules: filter fusion, constant folding, projection pushdown.
- PromQL vertical slice (`internal/promql/lower.go`) covering instant vector selectors, label matchers (eq / ne / regex), range vectors (placeholder SQL), and aggregations (`sum`, `count` with `by(…)`).
- HTTP API surface (`internal/api/prom`) for `/api/v1/query` + `/api/v1/query_range` (range_range returns a single point until full `RangeWindow` lowering lands in M1.1).
- CH client wrapper (`internal/chclient`) over `clickhouse-go/v2` with a testcontainers integration test.
- CI: two-job workflow (`check` + `lint`), commitlint relaxed for Dependabot, markdownlint, mutation testing (gremlins) on a nightly cron.
- Branch protection on `main`: required checks, linear history, no force pushes / deletions.

[Unreleased]: https://github.com/tsouza/cerberus/compare/v0.1.0...HEAD
[v0.1.0]: https://github.com/tsouza/cerberus/releases/tag/v0.1.0
