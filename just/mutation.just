# === Mutation testing ===

# Run gremlins across internal/. Slow; expect minutes. Honors .gremlins.yaml.
mutate:
    gremlins unleash ./internal/...

# Quick mutation pass on a single package: `just mutate-pkg internal/chsql`.
mutate-pkg PATH:
    gremlins unleash ./{{PATH}}

# Run gremlins on internal/optimizer/ + internal/chsql/ with the `chdb`
# build tag enabled so the chDB-backed property test (R8.3) and the
# TXTAR round-trip suite (R8.1) participate in the kill criterion.
#
# `-i` is the integration flag: per mutation, gremlins runs the
# complete `go test -tags chdb ./...` instead of just the mutated
# package's local test file. That brings test/spec/<head>/ round-trip
# tests into scope, so a mutation that changes SQL text but not the
# rendered row set is correctly NOT killed (semantically equivalent),
# which sharpens the score over the default lane.
#
# Slow: hundreds of mutants, each spinning up an ephemeral chDB
# session. Expect tens of minutes. Requires libchdb.so (see
# `just chdb-install`). Not on the PR critical path — informational.

# Run gremlins over the optimizer and emitter under the chdb tag. Slow; needs libchdb.so.
mutate-chdb:
    gremlins unleash -t chdb -i ./internal/optimizer/... ./internal/chsql/...

