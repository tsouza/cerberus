# === Migration lane — Tier-1 dual-backend substrate (Layer 14) ===
#
# The pinned reference stack `cerberus migrate verify` replays a harvested
# corpus against: reference Prometheus + Loki + Tempo alongside one ClickHouse,
# one OTel collector (the sole schema authority) and one cerberus serving all
# three heads. Explicit lifecycle verbs plus a composite, mirroring the e2e-*
# shape; the `tier1` prefix leaves room for the ruler tier without renaming.

# The image tag the migration stacks run when no released artifact is supplied.
# Held equal to the `${CERBERUS_IMAGE:-…}` default in
# tiers/tier1-dual/docker-compose.dual.yml by
# test/regression/migration_tier1_test.go, so the recipe below and the compose
# file cannot disagree about which tag "locally built" means. All three spell
# the suffix interpolation and expand it in their own runtime — the recipe's
# shell here, compose's interpolation there — so the tag stays one string.
#
# The suffix matters more here than anywhere else in the tree: this lane's `up`
# recipes carry no `--build` and the service declares `pull_policy: never`, so
# the tag is resolved a whole pull-retry loop after it is written. A tag shared
# with a second checkout would let that checkout's build land in between, and
# the lane would report on a binary from another tree.
MIGRATION_LOCAL_IMAGE := "cerberus:migration-tier1${COMPOSE_PROJECT_SUFFIX:-}"

# Put the cerberus image the migration stacks run into the local Docker daemon —
# the SINGLE acquisition point, shared by CI and local dev.
#
# Two paths, decided by CERBERUS_IMAGE alone: unset (every local run, every PR /
# dispatch / push CI run) it builds MIGRATION_LOCAL_IMAGE from the repo-root
# Dockerfile.local — whose final unnamed stage IS the cerberus image, so no
# `--target` is needed — and the stack proves this tree. Set (release.yml's
# artifact lane) it pulls that released tag and the stack proves the artifact.
#
# This exists because the compose up path deliberately cannot acquire the image
# itself: `pull_policy: never` and no `--build` on the up recipes are what keep
# a release run from recompiling the source tree over the released image.
#
# The build-vs-pull decision, the build command, and the pull command all live
# in .github/scripts/migration-cerberus-image.mjs (CLAUDE.md invariant 15,
# issue #3099) — this recipe is a thin env-passing wrapper around it.

# Put the cerberus image the migration stacks run into the local Docker daemon.
migration-cerberus-image:
    @MIGRATION_LOCAL_IMAGE="{{MIGRATION_LOCAL_IMAGE}}" node .github/scripts/migration-cerberus-image.mjs

# Bring the Tier-1 stack up and wait for every healthcheck to pass. The cerberus
# image is acquired first by migration-cerberus-image; there is deliberately no
# `--build` here, because `--build` forces a source build regardless of
# `pull_policy` and that is precisely how a released image gets silently
# replaced by a recompile of whatever tree the runner has checked out.
# The teardown is a sub-invocation rather than a `migration-tier1-up:
# migration-tier1-down` dependency on purpose: just runs a recipe at most once
# per invocation, so declaring it as a dependency would consume the trailing
# `migration-tier1-down` of the `migration-tier1` composite and leave the stack
# running after a clean lane. Seeding an already-seeded stack is the failure
# this closes — a run aborted by a failing assertion never reaches teardown,
# and a second seed into the same live window collides on every sample instant.
# migration-cerberus-image is a sub-invocation for the same reason.

# Bring the Tier-1 dual-backend stack up and wait for every healthcheck. Needs Docker.
migration-tier1-up:
    @just migration-tier1-down
    @just migration-cerberus-image
    @just _compose-pull-retry test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml
    @echo "==> migration tier-1 stack up"
    node .github/scripts/build-with-registry-retry.mjs \
        docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        up --wait --wait-timeout 300

# Every archetype an `@tier1` scenario actually reads fixture data from:
# MIG-02/06/07/08 read kube-prometheus-stack, every other `@tier1` scenario
# reads three-signal (see the `@archetype:` tags in
# test/e2e/migration/features/*.feature). migration-tier1-seed's empty
# default seeds all of them into ONE compose-stack lifecycle — bringing the
# stack up twice in one CI run just to seed a second archetype costs another
# full rebuild + boot + healthcheck wait for no reason.
MIGRATION_TIER1_ARCHETYPES := "three-signal kube-prometheus-stack"

# The services each migration-tier*-logs recipe dumps, and how many trailing
# lines it quotes per service. The Tier-2 list is Tier-1's plus the ruler leg,
# spelled out rather than derived from `compose config --services` so a service
# silently dropped from a compose file shows up as a missing dump rather than a
# quietly shorter one. 200 lines carries a boot failure's stack or a Grafana
# provisioning rejection while keeping ten services readable in a job log.
MIGRATION_TIER1_SERVICES := "clickhouse otel-collector prometheus loki tempo cerberus"
MIGRATION_TIER2_SERVICES := MIGRATION_TIER1_SERVICES + " grafana relay-prom otel-collector-writeback dead-end-receiver" + " incumbent-ruler incumbent-alertmanager incumbent-dead-end-receiver"
MIGRATION_LOG_TAIL := "200"

# Archetypes sharing one live stack are kept apart by their IDENTITIES, not by
# their windows: every archetype in MIGRATION_TIER1_ARCHETYPES declares trace
# service names, metric names and a log job that no other one declares, so all
# of them can occupy the same fixture window without any search seeing another
# archetype's data. test/regression/migration_tier1_test.go enforces that
# pairwise disjointness, so adding a ninth archetype that reuses a name fails
# the required `check` lane rather than a 45-minute Docker job.
#
# Pushing each archetype into its own earlier window (cmd/seed's
# --window-offset) was the previous mechanism and is arithmetically impossible
# for more than one archetype: disjoint windows need an offset of at least
# seed.SeedWindow (30m), and reference Tempo's 1h wal.ingestion_time_range_slack
# rejects any offset where offset + SeedWindow >= 1h. Every value satisfying one
# constraint violates the other, so the second archetype could never seed. The
# flag remains on cmd/seed — it is a real capability with a correct guard — but
# nothing here uses it.

# Load the deterministic all-signal fixture into the running stack: one
# in-memory fixture per archetype, each written twice — directly into
# ClickHouse and into the reference Prometheus / Loki / Tempo — so both sides
# of a parity diff are comparable by construction. Publishes one manifest per
# archetype: three-signal's stays at the historical unsuffixed
# `manifest.json` path (the plain-Go substrate self-check in
# tier1_parity_test.go hardcodes that exact path), every other archetype gets
# `manifest-<archetype>.json`. A Tier-1 scenario tagged `@archetype:<name>`
# reads exactly that archetype's manifest (test/e2e/migration/lib/live.go's
# LoadManifest), so it always sees its own window and metric names.
#
# `archetype` seeds ONLY that one archetype — for iterating on a single story
# locally once the stack is already up, e.g.
# `just migration-tier1-seed kube-prometheus-stack`. Its empty default seeds
# every archetype MIGRATION_TIER1_ARCHETYPES lists, which is what the
# canonical `just migration-tier1` composite (and the CI migration-tier1 job)
# drives — the whole `@tier1` scenario set is green off one seed pass, not
# just whichever archetype happened to be seeded last.
#
# The archetype loop, the default-list fallback, and the manifest-path
# convention all live in .github/scripts/migration-tier-seed.mjs, folded
# behind a tier parameter (MIGRATION_SEED_TIER) so migration-tier2-seed below
# reuses the exact same implementation (CLAUDE.md invariant 15, issue #3099).

# Seed the Tier-1 archetype fixtures into the running stack (all of them by default).
migration-tier1-seed archetype="":
    @MIGRATION_SEED_TIER=tier1 node .github/scripts/migration-tier-seed.mjs {{archetype}}

# Assert the Tier-1 substrate contract against the seeded stack: the collector
# provisioned the OTel schema, cerberus serves all three heads off it, each
# reference backend returns exactly what was written to it, both sides hold the
# same telemetry over the manifest window, and a deliberately injected
# disagreement is observed.
#
# Two SEPARATE `go test` invocations, in this exact order, not one
# `./test/e2e/migration/...` sweep: the root package's negative control
# (`TestTier1Parity/negative_control_the_lane_can_see_disagreement`)
# deliberately and PERMANENTLY corrupts ClickHouse with an extra gauge series
# to prove drift is observable — see test/e2e/migration/seed/drift.go. Nothing
# undoes that injection, and go test gives no ordering guarantee across
# packages in one invocation, so a single sweep makes the Gherkin corpus
# replay's diverge-zero assertion order-dependent: it must read the seeded
# stack before the negative control ever touches it, never after.

# Assert the Tier-1 dual-backend substrate and parity contracts against the seeded stack.
migration-tier1-run:
    @echo "==> migration tier-1 dual-backend Gherkin scenarios"
    go test -tags=migration_tier1 -count=1 ./test/e2e/migration/tiers/tier1-dual/...
    @echo "==> migration tier-1 substrate + parity assertions"
    go test -tags=migration_tier1 -count=1 ./test/e2e/migration/

# Dump the Tier-1 stack's container state and per-service log tail on demand.
# The CI job runs this on failure BEFORE teardown, so a red run carries the
# evidence instead of a bare assertion message; teardown then deletes the
# containers this reads from, which is why that ordering matters. Every command
# runs the same way the replaced `|| true`-guarded bash did: a dump must never
# turn a lane red on its own, nor mask the real failure with a container that
# has already exited.
#
# The `docker compose ps` / per-service `logs` loop lives in
# .github/scripts/migration-tier-logs.mjs, parameterized by tier + compose
# file list + service list, so migration-tier2-logs below reuses the exact
# same implementation instead of duplicating the loop shape (CLAUDE.md
# invariant 15, issue #3099).

# Dump the Tier-1 stack's container state and per-service log tail. Never fails on its own.
migration-tier1-logs:
    @MIGRATION_LOG_TIER=tier1 MIGRATION_COMPOSE_FILES="test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml" MIGRATION_LOG_SERVICES="{{MIGRATION_TIER1_SERVICES}}" MIGRATION_LOG_TAIL="{{MIGRATION_LOG_TAIL}}" node .github/scripts/migration-tier-logs.mjs

# Tear the Tier-1 stack down. `-v` is mandatory, not cosmetic: the reference
# images declare their own VOLUMEs, and a surviving volume would carry one
# run's data into the next.

# Tear the Tier-1 migration stack down, volumes included.
migration-tier1-down:
    @echo "==> migration tier-1 stack down"
    docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        down -v --remove-orphans

# Full Tier-1 lifecycle. Fails fast on the first non-zero recipe.
migration-tier1: migration-tier1-up migration-tier1-seed migration-tier1-run migration-tier1-down

# === Migration lane — Tier-2 ruler substrate (Layer 14) ===
#
# The Tier-1 stack plus a real query-only external ruler (Grafana-managed
# alerting) pointed at cerberus, and the dead-end webhook receiver its one
# contact point routes to. Mirrors the Tier-1 lifecycle shape exactly — see
# the comments on the migration-tier1-* recipes above for the rationale each
# one shares with its tier2 counterpart.

# Bring the Tier-2 stack up: Tier-1's compose file plus tier2-ruler's, merged
# via the standard multi-`-f` invocation so both sets of services land in the
# SAME compose project (docker-compose.ruler.yml deliberately declares no
# `name:` of its own — see that file's header comment).
#
# Same image contract as migration-tier1-up: acquire once via
# migration-cerberus-image, never `--build`. The ruler tier adds services on top
# of Tier-1's `cerberus`, it does not declare a second one, so this stack runs
# exactly the image that recipe put in the daemon.

# Bring the Tier-2 ruler stack up: Tier-1's compose file plus the ruler leg. Needs Docker.
migration-tier2-up:
    @just migration-tier2-down
    @just migration-cerberus-image
    @just _compose-pull-retry test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml
    @echo "==> migration tier-2 stack up"
    node .github/scripts/build-with-registry-retry.mjs \
        docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        -f test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml \
        up --wait --wait-timeout 300

# Tier-2 runs against the same seeded window Tier-1 does — the ruler tier
# adds Grafana-managed alerting on top of the SAME cerberus/ClickHouse pair,
# not a second one — so this is the identical seed step, not a second corpus.
# Reuses migration-tier1-seed's own .github/scripts/migration-tier-seed.mjs
# (MIGRATION_SEED_TIER=tier2's default archetype list is three-signal alone —
# see that script's own DEFAULT_ARCHETYPES).

# Seed the Tier-2 stack — the same window Tier-1 uses, not a second corpus.
migration-tier2-seed:
    @MIGRATION_SEED_TIER=tier2 node .github/scripts/migration-tier-seed.mjs

# Assert the Tier-2 substrate contract: Grafana provisioned the rule fixture
# (kube-prometheus-stack's recording rule + NodeCPUSaturation alert, plus the
# MIG-18 fire/resolve probe) without error, its alert rules evaluate against
# cerberus for real, and a fired notification actually reaches the dead-end
# receiver.
#
# Two SEPARATE `go test` invocations, in this exact order, not one
# `./test/e2e/migration/...` sweep — the same reason migration-tier1-run is
# split, with a different shared resource. Both packages carry the
# migration_tier2 tag and both drive the ONE dead-end receiver, and each
# asserts a notification arrived by reading the receiver's count before its own
# trigger and polling for a rise. Run concurrently (go test's default is one
# process per package) those two deltas observe each other's notifications, so
# each can be satisfied by the other's traffic. Sequencing them makes each
# delta bracket only its own trigger. Gherkin first, substrate second, mirroring
# migration-tier1-run: the Gherkin corpus reads the receiver's captured stream
# for MIG-18's fire/resolve edges, and the substrate self-check's synthetic
# test notification is one more entry in that stream.

# Assert the Tier-2 ruler substrate contract: rules provision, evaluate, and notify.
migration-tier2-run:
    @echo "==> migration tier-2 ruler Gherkin scenarios"
    go test -tags=migration_tier2 -count=1 ./test/e2e/migration/tiers/tier2-ruler/...
    @echo "==> migration tier-2 substrate assertions"
    go test -tags=migration_tier2 -count=1 ./test/e2e/migration/

# Dump the Tier-2 stack's container state and per-service log tail — the ruler
# leg included, since a Tier-2 failure is usually Grafana rejecting the rule
# fixture or the write-back bridge dropping a sample. See
# migration-tier1-logs' comment for the ordering-before-teardown rationale and
# for where the loop itself now lives.

# Dump the Tier-2 stack's container state and per-service log tail, ruler leg included.
migration-tier2-logs:
    @MIGRATION_LOG_TIER=tier2 MIGRATION_COMPOSE_FILES="test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml" MIGRATION_LOG_SERVICES="{{MIGRATION_TIER2_SERVICES}}" MIGRATION_LOG_TAIL="{{MIGRATION_LOG_TAIL}}" node .github/scripts/migration-tier-logs.mjs

# Tear the Tier-2 stack down. `-v` is mandatory — see migration-tier1-down's
# comment; the same reference-image VOLUME concern applies here since this
# compose invocation includes the Tier-1 file too.

# Tear the Tier-2 migration stack down, volumes included.
migration-tier2-down:
    @echo "==> migration tier-2 stack down"
    docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        -f test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml \
        down -v --remove-orphans

# Full Tier-2 lifecycle. Fails fast on the first non-zero recipe.
migration-tier2: migration-tier2-up migration-tier2-seed migration-tier2-run migration-tier2-down

