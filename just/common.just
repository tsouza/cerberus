# === Common (shared E2E/Test constants + retry helpers) ===

# ClickHouse images the `-tags=integration` Go tests start through
# testcontainers. testcontainers does the pull itself, single-attempt, so a slow
# Docker Hub fails those lanes the same way it failed the compose lanes — the
# `schema-ddl` job died on a `clickhouse-server:25.8-alpine` manifest HEAD during
# the v1.13.0 release window. Acquiring the images up front makes that pull
# retryable: testcontainers reuses an image already in the daemon.
#
# CH_TEST_IMAGE_PRIOR is the older server the replicated-DDL test pins to prove
# the DDL still applies on the previous supported line, so only that lane needs
# it. TestIntegrationImagePinsMatchTheJustfile holds both against the literals in
# the test sources.
CH_TEST_IMAGE := "clickhouse/clickhouse-server:25.9-alpine"
CH_TEST_IMAGE_PRIOR := "clickhouse/clickhouse-server:24.8-alpine"

# CH_STRICT_SCAN_IMAGE is the server the strict-scan differential boots. Unlike
# the other integration lanes (which pin 25.8 to reproduce a specific
# strict-scan divergence), this lane must EXECUTE every shape the emitter can
# produce — including the native timeSeries*ToGrid family whose chopt floor is
# 25.9, and chopt.FeatureTSGridLastOverTime's own 26.6 floor (the highest in
# the registry as of that feature). A pin below that floor does not go red;
# the server rejects the query before a row decodes and the shape silently
# drops out of scope. Kept at or above the highest chopt MinVersion by
# TestStrictScanImageClearsChoptFloors.
CH_STRICT_SCAN_IMAGE := "clickhouse/clickhouse-server:26.6-alpine"

# CH_QUANTILE_PROM_HISTOGRAM_IMAGE is the server the quantile_prom_histogram
# real-CH differential boots (internal/chsql's
# TestHistogramQuantile_RankWalkNative_DifferentialRealCH). Its own dedicated
# pin, like CH_TEST_IMAGE_PRIOR's: quantilePrometheusHistogram shipped in
# ClickHouse 25.10, above every other integration lane's pin (CH_TEST_IMAGE is
# 25.9), so reusing CH_TEST_IMAGE here would silently drop the aggregate out
# of scope the same way a stale CH_STRICT_SCAN_IMAGE would for the
# timeSeries*ToGrid family. TestIntegrationImagePinsMatchTheJustfile holds this
# against the literal in the test source.
CH_QUANTILE_PROM_HISTOGRAM_IMAGE := "clickhouse/clickhouse-server:25.10-alpine"

# CH_TAG_GROUPS_IMAGE is the server the ts_tag_groups real-CH differential
# boots (internal/api/prom's TestTagGroups_MatchesMapGrouping_RealClickHouse).
# Its own dedicated pin, like CH_QUANTILE_PROM_HISTOGRAM_IMAGE's:
# chopt.FeatureTSGridTagGroups's floor is 26.2 (timeSeriesThrowDuplicateSeriesIf,
# the higher of the two functions the feature needs) — above CH_TEST_IMAGE's
# 25.9 — so this lane boots its own server rather than reusing CH_TEST_IMAGE.
# TestIntegrationImagePinsMatchTheJustfile holds this against the literal in
# the test source.
CH_TAG_GROUPS_IMAGE := "clickhouse/clickhouse-server:26.2-alpine"

# k3s node image for the k3d clusters. Pinned (k3d otherwise picks a default tag
# per k3d version) so we pull ONE known tag with retry and hand it to
# `k3d cluster create --image` — k3d then boots from the host-cached copy
# instead of letting cluster creation pull k3s from Docker Hub mid-flight, which
# intermittently times out ("registry-1.docker.io ... context deadline
# exceeded") and fails the whole e2e. The Docker Hub dependency is already gone:
# `_pull-retry` resolves this ref through the GHCR mirror first and re-tags the
# result to the upstream name, so the host cache holds it under the name below
# without any file here naming a mirror an outside operator cannot read (#1514).
K3S_IMAGE := "rancher/k3s:v1.31.5-k3s1"

# Pull each image with retry + linear backoff. Docker Hub from CI runners
# intermittently times out the pull ("context deadline exceeded"); retrying
# clears the transient failure instead of failing k3d creation / image staging.
#
# The retry itself is `.github/scripts/pull-images.mjs` rather than a bash loop,
# because WHICH failures deserve another attempt is the whole question and it
# has exactly one right answer. A hand-rolled loop retries everything: a
# `manifest unknown` five times (slower red, same verdict), and — the failure
# that made this a rule — a Docker Hub rate-limit refusal five times, spending
# four more pulls out of the quota that is already exhausted. `lib/registry.mjs`
# owns that classification for every call site at once, and reaches the GHCR
# mirror ahead of Docker Hub, which a shell `docker pull` cannot do.
_pull-retry +IMAGES:
    @IMAGE_PULL_BACKOFF_SECONDS=3 node .github/scripts/pull-images.mjs {{IMAGES}}

# Acquire every image a compose stack needs, with retry, BEFORE `up` reaches for
# them. `docker compose up` pulls what it is missing but has no retry of its own,
# so one Docker Hub timeout ("context deadline exceeded" on a manifest HEAD)
# fails the whole lane — which is how the v1.13.0 release's Tier-1 leg died on a
# transient `prom/prometheus` fetch. `_pull-retry` already solved this for the
# k3d lanes; the compose lanes just weren't going through it.
#
# The pre-pull runs `docker pull`, not `docker compose pull`, because the two
# do not share a credential source: measured four seconds apart in one job, the
# CLI pull carried the runner's Docker Hub login and compose's was refused as
# UNAUTHENTICATED. Run through compose, the mechanism built to absorb Docker Hub
# failures was spending the anonymous quota rather than the authenticated one —
# which is why it never helped. `compose-pull-images.mjs` owns the resolution:
# it reads which services are fetchable off the compose model's `build:`
# sections (compose's own `--ignore-buildable` semantics) and pulls each one
# through the shared retry policy. See that module's header for the evidence.
_compose-pull-retry +FILES:
    @echo "==> pre-pulling compose images (retry, over the authenticated pull path)"
    @COMPOSE_PULL_BACKOFF_SECONDS=3 node .github/scripts/compose-pull-images.mjs {{FILES}}
