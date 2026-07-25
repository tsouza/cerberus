# Layer-14 migration scenarios

Each migration user-story in [`docs/migration-testing.md`](../../../docs/migration-testing.md)
is one executable Gherkin scenario driving the shipped `cerberus migrate` CLI.
The feature files **are** the manifest: metadata rides on tags — `@MIG-nn`
binds the story, `@tier0`/`@tier1`/`@tier2` the tier, `@archetype:<name>` the
archetype fixtures a scenario reads — so there is no second registry to keep in
step with them.

## Tree

| Path                    | What lives there                                                                    |
| ----------------------- | ----------------------------------------------------------------------------------- |
| `features/`             | one `.feature` per story, named `<MIG-id>.feature`, carrying the story as narrative |
| `steps/`                | the godog step definitions — the assertion library, untagged so it is linted        |
| `lib/`                  | repository-root discovery, binary build, offline process runs, golden comparison    |
| `tiers/tier0-offline/`  | the `migration`-tagged runner: the offline suite over committed fixtures            |
| `cmd/scenarios/`        | the enumerator projecting the features into the coverage ratchet's JSON             |
| `expected/`             | goldens the offline scenarios assert against, byte for byte                         |

## Running it

```sh
just migration-tier0        # the offline suite
just migration-scenarios    # the enumerator's JSON
just migration-golden       # regenerate the goldens (refused under CI)
```

`CERBERUS_BIN` hands the runner a prebuilt binary instead of compiling one.
`MIGRATION_TAGS` narrows the run to a single story, e.g. `@MIG-10 && @tier0`.

## What a scenario may assert

A `Then` reads the artifact a command emitted as a typed value and asserts on
its fields. "The command exited zero" and "a file was produced" are not
assertions; a scenario making only those claims asserts nothing. Every scenario
also asserts a positive cardinality — a corpus with queries, a render with
statements — so an empty fixture cannot produce a green run.

The arithmetic lives in Go and the relation is named in prose, which is why no
step text carries an operator or a number. The one place a number is legitimate
feature data is a `Scenario Outline` `Examples` table, where it is the varying
parameter; even there the Tier-0 scenarios pass case *names*, and the values
those names select live in the step definitions as named constants.

An unimplemented step is not a pending step: the suite runs under godog's
`Strict` option, so an undefined, pending or ambiguous step fails the run.

## Golden derivations

A golden regenerated from a first run asserts only that the code still does
what it did. Every golden here states what it is expected to contain, and the
statement is checked against the bytes by hand before the golden is committed.

- `expected/schema/default.sql` — the DDL `cerberus migrate schema` renders
  with no `CERBERUS_*` override set. It declares the `default` database, the
  five OTel metrics tables (gauge, sum, histogram, exponential histogram,
  summary), the logs table, the traces table, the trace-id timestamp index
  table and its materialised view, plus the idempotent
  `ADD PROJECTION IF NOT EXISTS` statements the metrics tables carry. No
  statement carries a TTL clause, because retention is unset by default and
  cerberus never invents one.
- `expected/schema/metrics-ttl-override.sql` — the same render with
  `CERBERUS_SCHEMA_TTL_METRICS` set. It differs from the default golden in
  exactly five places: one `TTL toDateTime(TimeUnix) + toIntervalDay(…)`
  clause on each of the five metrics tables. The logs and traces tables are
  byte-identical to the default render, because a per-signal metrics override
  must not reach another signal.
