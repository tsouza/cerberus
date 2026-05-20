<!--
PR title: Conventional Commits subject (e.g. `feat(promql): support offset modifier`).
commitlint runs on the PR commits + the squash-merge subject.
-->

## Summary

<!-- 2–4 bullets, focused on the WHY. Reviewers should be able to skim this and know the change's intent. -->

-
-

## Test plan

<!-- Check every line you actually ran. Add lines specific to the change. -->

- [ ] `just lint` clean
- [ ] `just test` passes (race + spec)
- [ ] New / updated TXTAR fixture(s) reviewed: <paths>
- [ ] Compatibility pass rate moved (if QL-touching): <before> → <after>
- [ ] CI green

## Notes for reviewers

<!-- Anything non-obvious: trade-offs, rationale for an allowlist entry, etc. -->
