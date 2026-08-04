# Cerberus-owned additive LogQL compatibility corpus

This directory is a cerberus-owned, additive companion to the vendored
`grafana/loki:pkg/logql/bench` corpus under
`compatibility/loki/upstream/loki-bench/queries/`. It exists for LogQL
behaviour that the vendored corpus has no coverage for at all — see
[issue #1611](https://github.com/tsouza/cerberus/issues/1611), which
found `| unpack` had zero corpus coverage (the vendored corpus never
queries it and the seeder never emitted packed-payload lines for it to
run against).

## Why a second directory instead of editing the vendor snapshot

`compatibility/loki/upstream/loki-bench/` is a verbatim AGPL-3.0
vendor snapshot (see its own `LICENSE` + `VERSION`); cerberus-side
curation must never leak into it — that boundary is what lets the
subtree be re-snapshotted mechanically from upstream. This directory
lives entirely outside `upstream/` and is cerberus-licensed.

## Why not the `cerberus-test-queries.yml` overlay

`compatibility/loki/cerberus-test-queries.yml` is a **different**
mechanism with a **different** contract: it is (by policy) an
always-empty schema placeholder for a `should_skip:`-shaped overlay
whose consumer code was deliberately removed — see that file's header
and `compatibility/loki/README.md`. It can only ever *annotate*
vendored cases, never add new ones, and re-introducing a skip
mechanism there is explicitly forbidden. This directory is the
opposite shape: it only ever **adds** cases, and every case added here
runs and is graded exactly like a vendored one — no skip, no
tolerance, no allow-list semantics.

## Layout

Mirrors the vendored corpus's suite/file layout so the same
`bench.QueryRegistry` loader (`query_registry.go`) can load both
directories interchangeably — `cmd/loki-compliance-tester/main.go`'s
`loadCerberusQueries` points a second `bench.NewQueryRegistry` instance
at this directory and merges its `GetQueries()` output into the
vendored corpus's, before either set of cases is expanded and run.

```text
cerberus-queries/
  README.md              this file
  fast/                  suite dir required by QueryRegistry.Load;
                          currently empty (see fast/README.md)
  regression/
    unpack.yaml           | unpack differential coverage (issue #1611)
    structured-metadata-generic.yaml
                          generic (non-detected_level) structured-
                          metadata differential coverage (issue #1498)
  exhaustive/             suite dir required by QueryRegistry.Load;
                          currently empty (see exhaustive/README.md)
```

`bench.QueryRegistry.Load` requires all three suite subdirectories
(`fast/`, `regression/`, `exhaustive/`) to exist — it is a hard error
if any is missing — even when a suite currently carries zero YAML
files, so `fast/` and `exhaustive/` exist as empty suite directories
(a `README.md` placeholder keeps git tracking them) ready for future
additions.

## Query YAML shape

Files here use the exact same schema as the vendored corpus
(`compatibility/loki/upstream/loki-bench/queries/schema.json`):
a top-level `queries:` list of `{description, query, kind, time_range,
directions, requires, tags, notes}` objects. Unlike the vendored
corpus, queries here use **literal** LogQL selectors (e.g.
`{service_name="packed-source"}`) rather than the `${SELECTOR}` /
`${LABEL_NAME}` / `${LABEL_VALUE}` template placeholders, because they
target one specific seeded stream (`packed-source`,
`compatibility/loki/cmd/seed/main.go`) whose payload shapes only that
stream carries — the generic `${SELECTOR}` resolver picks a random
stream matching the `requires:` bounded sets and has no way to target
"the one stream with packed lines".
