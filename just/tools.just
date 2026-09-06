# === Tools ===

# Install dev tools into $GOBIN (one-time).
install-tools:
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{GOLANGCI_LINT_VERSION}}
    go install mvdan.cc/gofumpt@{{GOFUMPT_VERSION}}
    go install golang.org/x/tools/cmd/goimports@{{GOIMPORTS_VERSION}}
    go install github.com/go-gremlins/gremlins/cmd/gremlins@{{GREMLINS_VERSION}}
    go install github.com/rhysd/actionlint/cmd/actionlint@{{ACTIONLINT_VERSION}}
    # The `pre-commit` hook's Markdown fixer. The hook runs a $PATH binary
    # rather than `npm exec` because npm's own startup is ~4s, five times the
    # budget CLAUDE.md sets for that stage — but it accepts one ONLY at the
    # pinned version, and falls back to the (slow) pinned `npm exec` otherwise.
    # The version is read from the script so this stays a single declaration.
    #
    # Leading `-`: a global npm install needs a writable npm prefix, which not
    # every environment has. The hook is correct without it — just slower — so
    # a failure here must not block the rest of the toolchain.
    -npm install -g markdownlint-cli2@"$(node .github/scripts/markdownlint-run.mjs --print-version)"

# Install lefthook + activate git hooks. Idempotent; run once after clone.
# Hooks defined in lefthook.yml run gofumpt / goimports / markdownlint-cli2 --fix
# on staged files at commit time, and commitlint on the commit message.
# Heavy validation (go test / golangci-lint / go build) is NOT in the hook —
# CI owns that. See CLAUDE.md § "No local validation; lefthook + CI own it."

# Install lefthook and activate the git hooks. Idempotent; run once after clone.
hooks-install:
    go install github.com/evilmartians/lefthook@latest
    lefthook install

