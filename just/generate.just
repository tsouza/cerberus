# === Generate ===

# Regenerate the structural feature table (id / minVersion / stability) in
# docs/clickhouse-optimizations.md from internal/chopt/registry.go, the single
# source of truth. Adding a feature to the registry lands it in the doc here;
# CI fails any PR whose generated block drifts (git diff --exit-code). The rich
# hand-authored columns (experimental setting, effect prose) stay outside the
# markers and are untouched. Same as `go generate ./internal/chopt/`.

# Regenerate the structural feature table in docs/clickhouse-optimizations.md.
gen-opt-docs:
    go run ./cmd/cerberus optdocs -doc docs/clickhouse-optimizations.md

# Run the router-rules catalog against a corpus (offline analysis). Mines the
# cerberus_router_corpus table (or its per-pod JSONL fallback) and prints
# findings where the recorded A/B route is paying a cost the corpus shows the
# other route would avoid. Pass flags through, e.g.:
#   just route-rules --source jsonl --corpus-path /var/lib/cerberus/router-corpus
#   just route-rules --validate-only

# Run the router-rules catalog against a recorded corpus (offline analysis).
route-rules *ARGS:
    go run ./cmd/cerberus route-rules {{ARGS}}

# Pre-cutover migration preview. Renders the ClickHouse schema cerberus expects
# from the current CERBERUS_* environment — offline, no database connection —
# so you can review it before provisioning. Pipeable into clickhouse-client:
#   just migrate schema | clickhouse-client -h ...

# Render the ClickHouse schema cerberus expects, offline, for pre-cutover review.
migrate *ARGS:
    go run ./cmd/cerberus migrate {{ARGS}}

