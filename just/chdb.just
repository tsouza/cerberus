# === chDB (in-process ClickHouse engine probe) ===

CHDB_VERSION := "v26.5.0"
CHDB_INSTALL_PATH := "/usr/local/lib/libchdb.so"

# Install libchdb.so (the in-process ClickHouse engine shared library)
# used by the chdb-go database/sql driver. Required only for tests that
# carry the `chdb` build tag — the engine-probe test under
# `internal/chclient/` and the native-family parity tests under
# `internal/chsql/`. Production builds never link against this; the
# release binary stays CGO_ENABLED=0.
#
# Pinned to chdb-core v26.5.0 (ClickHouse 26.5). The standalone
# `<platform>-libchdb.tar.gz` assets the chdb-go driver expects moved
# from the `chdb-io/chdb` repo (which stopped shipping them after v4.0.2,
# bundling libchdb inside Python wheels only) to `chdb-io/chdb-core`,
# whose release tags track the ClickHouse version directly. v26.5.0 is
# the newest stable that clears the 25.9 native-timeSeries*ToGrid floor
# while staying below the 26.7 aggregate-state serialization change.
# Mirror update_libchdb.sh shipped inside chdb-go.
#
# Idempotent: skips download if the install path already exists. Override
# CHDB_VERSION at the recipe call (`just chdb-install CHDB_VERSION=v26.5.0`).

# Install libchdb.so, the in-process ClickHouse engine every chdb-tagged lane needs.
chdb-install:
    @if [ -f "{{CHDB_INSTALL_PATH}}" ]; then \
        echo "==> libchdb already present at {{CHDB_INSTALL_PATH}} (delete to reinstall)"; \
        exit 0; \
    fi
    @os="$(uname -s)"; \
        arch="$(uname -m)"; \
        case "$os" in \
            Linux) \
                case "$arch" in \
                    aarch64|arm64) asset="linux-aarch64-libchdb.tar.gz" ;; \
                    *)             asset="linux-x86_64-libchdb.tar.gz" ;; \
                esac ;; \
            Darwin) \
                case "$arch" in \
                    arm64) asset="macos-arm64-libchdb.tar.gz" ;; \
                    *)     asset="macos-x86_64-libchdb.tar.gz" ;; \
                esac ;; \
            *) echo "unsupported platform: $os" >&2; exit 1 ;; \
        esac; \
        url="https://github.com/chdb-io/chdb-core/releases/download/{{CHDB_VERSION}}/$asset"; \
        echo "==> downloading $url"; \
        tmp="$(mktemp -d)"; \
        curl -fsSL -o "$tmp/libchdb.tar.gz" "$url"; \
        tar -C "$tmp" -xzf "$tmp/libchdb.tar.gz"; \
        echo "==> installing to {{CHDB_INSTALL_PATH}} (sudo may prompt)"; \
        sudo install -m 0755 "$tmp/libchdb.so" "{{CHDB_INSTALL_PATH}}"; \
        rm -rf "$tmp"; \
        echo "==> libchdb {{CHDB_VERSION}} installed"

