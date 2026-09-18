<!--
PR title: Conventional Commits subject (e.g. `feat(promql): support offset modifier`).
commitlint runs on the PR commits + the squash-merge subject.
-->

## Summary

<!-- 2–4 bullets, focused on the WHY. Reviewers should be able to skim this and know the change's intent. -->

-
-

## Test plan

<!-- Check every line you actually ran. Add lines specific to the change. Whole-tree `just lint` / `just test` are CI's job, not a pre-flight ritual. -->

- [ ] Narrowed local run for the change: `<command>`
- [ ] New / updated TXTAR fixture(s) reviewed: <paths>
- [ ] Compatibility pass rate moved (if QL-touching): <before> → <after>
- [ ] CI green

## Notes for reviewers

<!-- Anything non-obvious: trade-offs, a deliberately narrow scope (with the issue that tracks the rest), etc. -->
