# === Dependencies ===

# Tidy go.mod and go.sum.
deps-tidy:
    go mod tidy


# Re-copy Tempo's Apache-2.0 wire types into internal/thirdparty/tempopb from
# the commit in compatibility/tempo/UPSTREAM_VERSION (see that package's README).

# Re-sync internal/thirdparty/tempopb from the pinned grafana/tempo commit.
tempopb-sync:
    node .github/scripts/tempopb-sync.mjs
