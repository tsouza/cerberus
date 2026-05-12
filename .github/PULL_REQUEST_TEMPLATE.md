<!--
PR title: Conventional Commits subject (e.g. `feat(promql): support offset modifier`).
commitlint runs on the PR commits + the squash-merge subject.

Project tracker: https://github.com/users/tsouza/projects/1
Roadmap: docs/roadmap.md
-->

## Summary

<!-- 2–4 bullets, focused on the WHY. Reviewers should be able to skim this and know the change's intent. -->

-
-

## Roadmap link

<!-- Which milestone does this PR close or move forward? e.g. M1.2 — BinaryExpr lowering. -->

-

## Test plan

<!-- Check every line you actually ran. Add lines specific to the change. -->

- [ ] `just lint` clean
- [ ] `just test` passes (race + spec)
- [ ] New / updated TXTAR fixture(s) reviewed: <paths>
- [ ] Compatibility pass rate moved (if PromQL-touching): <before> → <after>
- [ ] CI green

## Notes for reviewers

<!-- Anything non-obvious: trade-offs, deferrals, rationale for an allowlist entry, etc. -->
