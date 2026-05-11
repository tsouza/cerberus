---
name: cerberus-add-optimizer-rule
description: Scaffold a new optimizer rule under internal/optimizer/. Creates the rule file, a unit test, and one TXTAR before/after fixture. Use when adding a new chplan→chplan rewrite (filter pushdown, projection elision, constant fold variant, transpose, …).
tools: Read, Write, Bash
---

# /cerberus:add-optimizer-rule

Scaffold the four files that make up a new optimizer rule.

## When to invoke

The user says any of:

- "add an optimizer rule"
- "new pushdown rule for X"
- "/cerberus:add-optimizer-rule"

## Inputs

One positional argument (prompt if missing):

- **Name** — Go type name in PascalCase, e.g. `FilterProjectTranspose`, `LimitFusion`. The filename is the snake_case form (`filter_project_transpose.go`).

## Files to create

1. **`internal/optimizer/<snake_name>.go`** — the rule implementation:

   ```go
   package optimizer

   import "github.com/tsouza/cerberus/internal/chplan"

   // <PascalName> describes what the rule does and why it matters.
   // (Write this comment.)
   type <PascalName> struct{}

   func (<PascalName>) Name() string { return "<kebab-name>" }

   func (<PascalName>) Apply(n chplan.Node) (chplan.Node, bool) {
       // Pattern match + transform. Return (n, false) when no match.
       return n, false
   }
   ```

2. **`internal/optimizer/<snake_name>_test.go`** — Go-level unit test asserting the transformation:

   ```go
   package optimizer_test

   import (
       "testing"

       "github.com/tsouza/cerberus/internal/chplan"
       "github.com/tsouza/cerberus/internal/optimizer"
   )

   func Test<PascalName>_HappyPath(t *testing.T) {
       t.Parallel()
       // Build the matching tree, apply the rule, assert .Equal() against the expected tree.
   }

   func Test<PascalName>_NoMatch(t *testing.T) {
       t.Parallel()
       // Build a tree that shouldn't match, assert (n, false).
   }
   ```

3. **`test/spec/optimizer/<snake_name>.txtar`** — empty file. The matching entry in `internal/optimizer/optimizer_test.go` `inputs` map is what generates the `unoptimized` / `optimized` sections via `GOLDEN_UPDATE=1`. The skill creates the empty file and tells the user to add the input plan in Go.

4. **Register the rule** — add the rule to `optimizer.Default()` in `internal/optimizer/rule.go`. Find the existing call (e.g., `New(ConstantFold{}, FilterFusion{}, ProjectionPushdown{})`) and append the new rule **after** any rule it depends on (constant folding usually runs first; pushdowns later).

## What to do

1. Confirm `pwd` is the repo root.
2. Check the four target paths don't already exist; abort if any does.
3. Compute name forms: `PascalName` → `snake_name` (via simple regex) → `kebab-name`.
4. Write each file.
5. Tell the user the follow-up:
   - Implement the rule body in `<snake_name>.go`.
   - Add the matching entry to `optimizer_test.go` `inputs` map.
   - Run `just update-golden` to generate the TXTAR `unoptimized` / `optimized` sections.
   - Review the diff, then run `just test` + `just lint`.
   - Commit with a Conventional Commits message: `feat(optimizer): add <kebab-name> rule`.

## Don't

- Don't implement the rule body — that's the user's actual work.
- Don't run `just update-golden` automatically — the user wants to fill in the plan first.
- Don't modify `optimizer.Default()` registration silently — the user may want the rule disabled at first; tell them what to add and where.
