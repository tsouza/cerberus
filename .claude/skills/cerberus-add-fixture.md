---
name: cerberus-add-fixture
description: Scaffold a new TXTAR fixture under test/spec/<ql>/. Use when adding spec coverage for a new PromQL/LogQL/TraceQL/optimizer/chsql construct. Prompts for the QL (or area) and fixture name, creates the file with the right section headers pre-populated.
tools: Read, Write, Bash
---

# /cerberus:add-fixture

Create a new TXTAR fixture under `test/spec/<area>/<name>.txtar` and remind the user how to fill it in.

## When to invoke

The user says any of:

- "add a fixture for `<query>`"
- "scaffold a TXTAR for `<rule>`"
- "new spec test"
- "/cerberus:add-fixture"

…and the active project is cerberus (`pwd` contains `cerberus`, repo origin is `tsouza/cerberus`).

## Inputs

Two positional arguments (prompt the user if missing):

1. **Area** — one of `promql`, `logql`, `traceql`, `chsql`, `optimizer`.
2. **Name** — snake_case, e.g. `rate_http_requests`, `filter_fusion_chain`. Must not already exist as a file under `test/spec/<area>/`.

## What to do

1. Confirm `pwd` is the repo root (the directory containing `go.mod`).
2. Check `test/spec/<area>/<name>.txtar` doesn't already exist; if it does, abort and tell the user.
3. Create the file with the right header per area:

   **promql / logql / traceql**:

   ```text
   -- query.<area> --
   <PASTE QUERY HERE>
   ```

   (only `query.<area>` — the `sql` and `args` sections get filled by `GOLDEN_UPDATE=1`.)

   **chsql**: this area's fixtures don't have an input section because the input plan is constructed in Go (`internal/chsql/emit_test.go` `plans` map). Create the fixture with **no content** (just `touch test/spec/chsql/<name>.txtar`) and remind the user to add the matching entry to the `plans` map in `internal/chsql/emit_test.go`.

   **optimizer**: same as `chsql` — input plans live in Go in `internal/optimizer/optimizer_test.go` `inputs` map. Create the empty file and remind the user to add the corresponding entry.

4. Tell the user the three follow-up steps:
   - For QL fixtures: paste the query into the `query.<area>` section.
   - For chsql/optimizer: add the corresponding entry to the `plans` / `inputs` map in the matching `_test.go` file.
   - Run `just update-golden` and review `git diff test/spec/` before committing.

## Tools

You only need:

- `Read` — to check whether the fixture already exists and to read the matching `_test.go` file for chsql/optimizer.
- `Write` — to create the file.
- `Bash` — to `mkdir -p` the area dir and `touch` empty fixtures.

Don't run `go test` — that's the user's job once the fixture is filled.
