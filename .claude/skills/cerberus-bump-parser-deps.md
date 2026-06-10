---
name: cerberus-bump-parser-deps
description: Bump the upstream PromQL/LogQL/TraceQL parser modules + dependent transitive packages, run the full test suite, and prepare a PR description summarising the upstream changelogs. Use when you want to pick up new parser features or security fixes.
tools: Read, Write, Bash, WebFetch
---

# /cerberus:bump-parser-deps

Bump `prometheus/prometheus`, `grafana/loki/v3`, `grafana/tempo`. Tame any new transitive-dep fallout (the memberlist swap is the recurring suspect). Capture changelog deltas for the PR description.

## When to invoke

User says:

- "bump parsers"
- "update PromQL/LogQL/TraceQL deps"
- "/cerberus:bump-parser-deps"

## What to do

1. **Branch off main**:

   ```sh
   git checkout main && git pull --ff-only && git checkout -b chore/bump-parser-deps-$(date +%Y%m%d)
   ```

2. **Record current versions** before bumping:

   ```sh
   go list -m github.com/prometheus/prometheus github.com/grafana/loki/v3 github.com/grafana/tempo
   ```

3. **Bump**:

   ```sh
   go get -u github.com/prometheus/prometheus
   go get github.com/grafana/loki/v3@latest
   go get github.com/grafana/tempo@main
   go mod tidy
   ```

   (Tempo uses `@main` because its v1.x tag is stale.)

4. **Try to build**:

   ```sh
   go build ./...
   ```

   **If it fails with `undefined: memberlist.NodeState` (or similar) from `dskit/kv/memberlist`:** the upstream memberlist pin has shifted. Find the new version by reading `~/go/pkg/mod/github.com/grafana/loki/v3@<new-version>/go.mod` for the `replace github.com/hashicorp/memberlist => github.com/grafana/memberlist@...` line and copy that version into cerberus's own `replace` in `go.mod`. Then `go mod tidy` again.

5. **Run the full suite**:

   ```sh
   just lint && just test
   ```

   If a smoke test in `internal/{promql,logql,traceql}/parser_smoke_test.go` fails because of an API change (e.g. `parser.ParseExpr` → `parser.NewParser().ParseExpr`), fix the smoke test inline; the lowering callers in `lower.go` may need adjustments too.

6. **Capture changelog deltas** for the PR description. For each bumped module, fetch the upstream release notes / commits and summarise the breaking changes + headline features. Use `WebFetch` against the project's GitHub releases page:
   - `https://github.com/prometheus/prometheus/releases`
   - `https://github.com/grafana/loki/releases`
   - `https://github.com/grafana/tempo/releases`

7. **Commit + open PR**:

   ```sh
   git add go.mod go.sum
   git commit -m "chore(deps): bump <projects> to <versions>"
   git push -u origin <branch>
   gh pr create --base main --title "..." --body "<changelog summary>"
   ```

   PR body template:

   ```markdown
   ## Summary
   - prometheus/prometheus: <old> → <new>
   - grafana/loki/v3:       <old> → <new>
   - grafana/tempo:         <old> → <new>

   ## Upstream highlights
   <one paragraph per project>

   ## Local adjustments
   - <e.g. memberlist replace version bumped from X to Y>
   - <e.g. smoke test updated to use new ParseExpr API>

   ## Test plan
   - [ ] CI green
   - [ ] Compatibility suite pass rate unchanged or improved
   ```

8. **Monitor CI**. If `compatibility.yml` shows new failures, the parser may have started accepting new queries that cerberus doesn't yet lower. There is no allow-list — implement the lowering (or split it into its own PR that lands first) so the suite goes green for real.

## Tools

- `Bash` — git, go, just.
- `WebFetch` — release notes.
- `Read` — `go.mod` after tidy, transitive `go.mod` files for memberlist.
- `Write` — only for the PR body if you stash it to a temp file before `gh pr create`.

## Don't

- Don't bump just one of the three; bump them together so transitive resolutions stabilise in one go.
- Don't add any allow-list / skip mechanism to make compatibility pass — none exists, by design. Investigate; if the failure is a genuine new feature cerberus doesn't yet lower, implement it in a separate PR and land that first.
