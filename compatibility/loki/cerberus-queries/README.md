# Cerberus-owned additive LogQL compatibility corpus

This directory is a cerberus-owned, additive companion to the upstream
`grafana/loki` `pkg/logql/bench` query corpus, which the harness reads
from the pinned module in the Go module cache and which the repository
does not carry. The upstream corpus is fixed at that pin, so this
directory is where every new LogQL compatibility case goes. Its first
case was `| unpack`, which the upstream corpus never queries
([issue #1611](https://github.com/tsouza/cerberus/issues/1611)).

To add a case, append a query to the YAML file for its topic under the
suite it belongs to (`fast/`, `regression/` or `exhaustive/`), or create
a new `<topic>.yaml` there, then run `just compat-logql`.

## Why a second directory instead of extending the upstream corpus

The upstream corpus is AGPL-3.0 test material that cerberus reads at
run time and never redistributes; cerberus-side curation cannot go into
it. This directory is cerberus-licensed and holds only cerberus-authored
definitions.

## Why not the `cerberus-test-queries.yml` overlay

`compatibility/loki/cerberus-test-queries.yml` is a **different**
mechanism with a **different** contract: it is (by policy) an
always-empty schema placeholder for a `should_skip:`-shaped overlay
whose consumer code was deliberately removed — see that file's header
and `compatibility/loki/README.md`. It can only ever *annotate*
existing cases, never add new ones, and re-introducing a skip
mechanism there is explicitly forbidden. This directory is the
opposite shape: it only ever **adds** cases, and every case added here
runs and is graded exactly like an upstream one — no skip, no
tolerance, no allow-list semantics.

## Layout

Mirrors the upstream corpus's suite/file layout, so one loader
(`compatibility/loki/corpus`, `corpus.Load`) reads both roots —
`cmd/loki-compliance-tester/main.go`'s `loadCerberusQueries` loads this
directory and merges its definitions after the upstream corpus's, before
either set of cases is expanded and run.

```text
cerberus-queries/
  README.md              this file
  schema.json            JSON Schema of a query file
  fast/                  suite dir required by corpus.Load;
                          currently empty (see fast/README.md)
  regression/
    anchored-regex-matcher.yaml
                          anchored regex matcher differential coverage (issue #1741)
    unpack.yaml           | unpack differential coverage (issue #1611)
    structured-metadata-generic.yaml
                          generic (non-detected_level) structured-
                          metadata differential coverage (issue #1498)
  exhaustive/             suite dir required by corpus.Load;
                          currently empty (see exhaustive/README.md)
```

`corpus.Load` requires all three suite subdirectories
(`fast/`, `regression/`, `exhaustive/`) to exist — it is a hard error
if any is missing — even when a suite currently carries zero YAML
files, so `fast/` and `exhaustive/` exist as empty suite directories
(a `README.md` placeholder keeps git tracking them) ready for future
additions.

## Query YAML shape

Files here follow `schema.json` in this directory, which describes the
format `corpus.Load` reads (the loader rejects any key the schema does
not name, and `compatibility/loki/corpus/upstream_test.go` keeps the two
in step). Each file starts with a
`# yaml-language-server: $schema=../schema.json` line for editor
validation:
a top-level `queries:` list of `{description, query, kind, time_range,
directions, requires, skip, tags, notes}` objects. Unlike the upstream
corpus, queries here use **literal** LogQL selectors (e.g.
`{service_name="packed-source"}`) rather than the `${SELECTOR}` /
`${LABEL_NAME}` / `${LABEL_VALUE}` template placeholders, because they
target one specific seeded stream (`packed-source`,
`compatibility/loki/cmd/seed/main.go`) whose payload shapes only that
stream carries — the generic `${SELECTOR}` resolver picks a random
stream matching the `requires:` bounded sets and has no way to target
"the one stream with packed lines".
