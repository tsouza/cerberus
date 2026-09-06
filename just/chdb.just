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
#
# The platform-detection (case "$os"/case "$arch"), URL construction,
# curl/tar/sudo install, and idempotency short-circuit all live in
# .github/scripts/chdb-install.mjs (CLAUDE.md invariant 15, issue #3094) —
# self-contained, no shared lib/ dependency — so this recipe is a thin
# env-passing wrapper around it.

# Install libchdb.so, the in-process ClickHouse engine every chdb-tagged lane needs.
chdb-install:
    @CHDB_VERSION="{{CHDB_VERSION}}" CHDB_INSTALL_PATH="{{CHDB_INSTALL_PATH}}" node .github/scripts/chdb-install.mjs

