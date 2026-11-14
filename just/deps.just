# === Dependencies ===

# Tidy go.mod and go.sum.
deps-tidy:
    go mod tidy


# Re-sync internal/thirdparty/tempopb from the pinned grafana/tempo commit.
tempopb-sync:
    node .github/scripts/tempopb-sync.mjs

# Fail if internal/thirdparty/tempopb differs from what tempopb-sync produces.
tempopb-sync-check:
    node .github/scripts/tempopb-sync.mjs --check
