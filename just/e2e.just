# === E2E (k3d + ClickHouse + Grafana + cerberus) ===

K3D_CLUSTER := "cerberus-e2e"
CERBERUS_IMAGE := "cerberus:e2e"

# Chart deployment topology the e2e stack installs: `monolith` (default — one
# Deployment/Service serving all three heads) or `split` (three per-head
# Deployments + bare-named Services, each pinned via CERBERUS_ENABLED_HEADS).
# The dashboard e2e lane runs the SAME Grafana/Playwright smoke against both,
# driven by a CI matrix axis (E2E_MODE); the split-only isolation spec then
# proves one head can die without severing the others.
E2E_MODE := env_var_or_default("E2E_MODE", "monolith")

# Extra Go `-tags` baked into the cerberus image built by `e2e-up`. Empty
# by default, so the dashboard e2e lane and any local `just e2e-up` build a
# stock binary. ONLY the chaos lane sets CERBERUS_BUILD_TAGS=chaos_sleep
# (via the `chaos` job in .github/workflows/e2e.yml) to link the
# deterministic-sleep injection used by ch-slow-query-timeout. Threaded to
# Dockerfile.local's GO_BUILD_TAGS build-arg.
CERBERUS_BUILD_TAGS := env_var_or_default("CERBERUS_BUILD_TAGS", "")

# External images referenced by test/e2e/k3s/*.yaml. Kept in sync with the
# manifests by convention; a stale entry surfaces as a `Pending` /
# `ImagePullBackOff` pod once that image is no longer pre-loaded. When you
# bump a version in a manifest, bump it here too — both sides MUST agree.
#
# Why pre-pull: a fresh k3d cluster's containerd hits the registry directly
# (DockerHub + GHCR). DockerHub's anonymous-pull rate limit is shared across
# GHA-runner IP pools and intermittently fires at ~1/20 e2e runs, leaving
# ClickHouse stuck in `ImagePullBackOff` past the 180 s deployment wait. By
# pulling on the host docker daemon (which has its own auth + cache) and
# importing into k3d via the API, we never go through containerd's pull
# path. See run 26136032208 for the symptom.
# This list and the image pins under test/e2e/k3s*/ are one set seen twice: an
# image the manifests ask for and this list omits is pulled by containerd at pod
# start — anonymously, un-mirrored, the whole exposure the pre-pull removes.
# `TestE2EPrePullCoversEveryManifestImage` enforces the equality both ways,
# because the convention it replaces had already failed silently: #1263 moved the
# k3d ClickHouse to 26.6-alpine and left this line on 26.5-alpine, so for ten days
# the one image the pre-pull was written for was the one it did not cover (#1461).
E2E_EXTERNAL_IMAGES := "clickhouse/clickhouse-server:26.6-alpine ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.116.0 grafana/grafana:12.2.9 otel/opentelemetry-collector-contrib:0.152.1 busybox:1.37"

# Extra images the bundled-ClickHouse ("bwc") object-storage lane needs on top
# of E2E_EXTERNAL_IMAGES: the MinIO object store + its `mc` client (the
# bucket-create Job) and the bundled ClickHouse image the Helm chart deploys.
# Same pre-pull+import rationale as E2E_EXTERNAL_IMAGES, and the same gate holds
# it against the pins in test/e2e/k3s-bwc/*.yaml (minio + mc) and
# test/e2e/k3s/cerberus-values-bwc.yaml (clickhouse.bundled.image) — the chart
# maps the CH container's pullPolicy to .Values.image.pullPolicy (Never in the
# e2e values), so the exact bundled-CH tag MUST be imported here.
E2E_BWC_IMAGES := "minio/minio:RELEASE.2025-09-07T16-13-09Z minio/mc:RELEASE.2025-08-13T08-35-41Z clickhouse/clickhouse-server:26.3"

# The multi-data-shard lane (cerberus issue #3079) needs only the bundled
# ClickHouse image, at the SAME tag E2E_BWC_IMAGES already pins (gotcha #1's
# dedicated-user ConfigMap applies regardless of dataShards.count) — no MinIO
# (hot-only mode, see cerberus-values-datashard.yaml's own doc), so this is a
# standalone constant rather than reusing E2E_BWC_IMAGES wholesale, which
# would pull two MinIO images this lane never runs.
E2E_DATASHARD_IMAGES := "clickhouse/clickhouse-server:26.3"

# Extra args appended verbatim to `k3d cluster create` in `e2e-up`. Empty by
# default (CI uses none). Interpolated unquoted, so the value is shell-parsed:
# wrap any single arg containing shell metacharacters (`<`, `%`, `,`) in
# single quotes INSIDE the value.
#
# The motivating use case is dev hosts low on disk: k3s kubelet's default
# eviction thresholds (nodefs.available<10%, imagefs.available<15%) taint the
# node with disk-pressure and nothing schedules. For a throwaway local
# cluster, disable eviction:
#
#   K3D_EXTRA_ARGS="--k3s-arg '--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:0'" just e2e-up
K3D_EXTRA_ARGS := env_var_or_default("K3D_EXTRA_ARGS", "")

# `_pull-retry` / `_compose-pull-retry` protect the images the HOST daemon
# fetches. The images a BUILD fetches — the `FROM` refs BuildKit resolves while
# running `docker build` / `docker compose up` — are a separate exposure that no
# host-side pre-pull reaches (the `docker-container` driver resolves them from
# the registry regardless of what the daemon holds), and a Docker Hub 429 on
# `golang:1.26` took `e2e` + `migration-e2e` down on main. Every build-invoking
# command below therefore runs through
# `.github/scripts/build-with-registry-retry.mjs`, which retries the command on
# registry/network faults only and fails immediately on a real build error.

# Host ports map via the k3d loadbalancer to NodePorts on the k3s nodes:
#   host:8080 -> LB -> NodePort 30080 (cerberus svc)
#   host:3000 -> LB -> NodePort 30030 (grafana svc)
# The k3d loadbalancer publishes on 0.0.0.0, so both ports are reachable on
# every host interface (LAN IP included), not just localhost.

# Boot the k3d cluster, build and import the cerberus image, apply manifests, wait for pods.
e2e-up: e2e-down
    @echo "==> pre-pulling k3s node image (retry — Docker Hub flaky from CI)"
    @just _pull-retry {{K3S_IMAGE}}
    @echo "==> creating k3d cluster {{K3D_CLUSTER}}"
    k3d cluster create {{K3D_CLUSTER}} \
        --image {{K3S_IMAGE}} \
        --port "3000:30030@loadbalancer" \
        --port "8080:30080@loadbalancer" \
        --no-lb=false \
        --k3s-arg "--disable=traefik@server:0" \
        {{K3D_EXTRA_ARGS}} \
        --wait
    @echo "==> building cerberus image (build tags: '{{CERBERUS_BUILD_TAGS}}')"
    node .github/scripts/build-with-registry-retry.mjs \
        docker build -t {{CERBERUS_IMAGE}} --build-arg GO_BUILD_TAGS="{{CERBERUS_BUILD_TAGS}}" --build-arg GO_IMAGE -f Dockerfile.local .
    @echo "==> pre-pulling external images on host docker (retry)"
    @just _pull-retry {{E2E_EXTERNAL_IMAGES}}
    @echo "==> importing images into k3d ({{K3D_CLUSTER}}) — one at a time, with retry+verify"
    @# k3d bundles EVERY image into one tarball, mounts it into a transient
    @# tools node, and runs `ctr image import`. Two failure modes bite:
    @#   1. the bundled tarball intermittently vanishes mid-import
    @#      ("ctr: open /k3d/images/...tar: no such file or directory" —
    @#      run 27511641349), a transient image-volume race whose window
    @#      grows with the bundle size, so importing ONE image at a time
    @#      shrinks both the tarball and the race;
    @#   2. `k3d image import` prints "Successfully imported" and exits 0
    @#      even when a node-level import failed (run 27274975563 left
    @#      cerberus:e2e absent → ImagePullBackOff), so success has to be
    @#      VERIFIED against the node's containerd, not trusted.
    @# So: import each image on its own, then verify it actually landed,
    @# retrying the import until it's present or the attempt budget is
    @# spent. Normalise short names the way containerd stores them
    @# (docker.io/ + library/ prefixes).
    @for img in {{CERBERUS_IMAGE}} {{E2E_EXTERNAL_IMAGES}}; do \
        ref="$img"; \
        case "$ref" in \
            *.*/*|*:*/*) ;; \
            */*) ref="docker.io/$ref" ;; \
            *)   ref="docker.io/library/$ref" ;; \
        esac; \
        landed=0; \
        for attempt in 1 2 3 4 5; do \
            k3d image import "$img" -c {{K3D_CLUSTER}} || true; \
            if docker exec k3d-{{K3D_CLUSTER}}-server-0 ctr -n k8s.io images ls -q | grep -qF "$ref"; then \
                landed=1; break; \
            fi; \
            echo "    import attempt $attempt: $ref not in containerd yet, retrying after backoff" >&2; \
            sleep $((attempt * 2)); \
        done; \
        if [ "$landed" != "1" ]; then \
            echo "ERROR: $ref missing from k3d node containerd after 5 import attempts" >&2; \
            exit 1; \
        fi; \
        echo "    ok $ref"; \
    done
    @echo "==> applying fixture manifests (CH, Grafana, collector, sample apps)"
    kubectl apply -k test/e2e/k3s/
    @# In split mode the chart-managed datasource hostnames change (each head
    @# gets its own bare-named Service), so rewrite the Grafana datasource
    @# ConfigMap to point each datasource type at its head Service. In monolith
    @# mode the kustomize-applied ConfigMap (url: http://cerberus:8080) is
    @# already correct.
    @#
    @# Grafana provisions datasources into its DB ONCE, at boot, from the
    @# mounted ConfigMap file — and the kubelet's ConfigMap→volume sync lags a
    @# pod's startup by tens of seconds. So updating the ConfigMap object after
    @# `apply -k` has already created the Grafana Deployment does NOT change what
    @# Grafana provisioned: it boots from the original `http://cerberus:8080`
    @# and never re-reads. In split that URL resolves to the NodePort-alias
    @# Service, which selects ONLY the prometheus head, so loki/tempo datasource
    @# queries hit the prometheus process (no /loki/* or /api/* routes) and 404
    @# — the v1.4.0 split-mode dashboard-lane breakage.
    @#
    @# Fix: after updating the ConfigMap, BLOCK until the kubelet has synced the
    @# per-head URLs into the running Grafana pod's mounted file, THEN restart
    @# Grafana so it re-provisions from the corrected file. Polling the mounted
    @# file (not a fixed sleep) makes the restart race-free: Grafana only
    @# re-reads once the new content is actually on disk.
    @if [ "{{E2E_MODE}}" = "split" ]; then \
        echo "==> [split] rewriting Grafana datasource URLs to per-head Services"; \
        kubectl -n cerberus get configmap grafana-datasources -o jsonpath='{.data.datasources\.yaml}' \
            | awk '/type: prometheus/{t="prometheus"} /type: loki/{t="loki"} /type: tempo/{t="tempo"} \
                   /url: http:\/\/cerberus:8080/{sub(/cerberus/, t)} {print}' > /tmp/cerberus-e2e-ds-split.yaml; \
        kubectl -n cerberus create configmap grafana-datasources \
            --from-file=datasources.yaml=/tmp/cerberus-e2e-ds-split.yaml \
            --dry-run=client -o yaml | kubectl apply -f -; \
        echo "==> [split] waiting for the per-head datasource URLs to sync into the Grafana pod"; \
        kubectl -n cerberus rollout status deployment/grafana --timeout=120s; \
        synced=0; \
        for attempt in $(seq 1 60); do \
            if kubectl -n cerberus exec deploy/grafana -- grep -q 'url: http://loki:8080' /etc/grafana/provisioning/datasources/datasources.yaml 2>/dev/null; then \
                synced=1; break; \
            fi; \
            sleep 2; \
        done; \
        [ "$synced" = "1" ] || { echo "ERROR: per-head datasource URLs never synced into the Grafana pod after 120s" >&2; exit 1; }; \
        echo "==> [split] restarting Grafana so it re-provisions datasources from the corrected file"; \
        kubectl -n cerberus rollout restart deployment/grafana; \
        kubectl -n cerberus rollout status deployment/grafana --timeout=120s; \
    fi
    @# cerberus is deployed via its OWN Helm chart so the e2e cluster dogfoods
    @# the published chart (deploy/helm/cerberus) — a chart bug now fails the
    @# dashboard/chaos lanes, not just `chart-validate`'s static lint. The
    @# kustomize apply above already created the `cerberus` namespace.
    @echo "==> installing cerberus via Helm chart (deploy/helm/cerberus, mode={{E2E_MODE}})"
    @if [ "{{E2E_MODE}}" = "split" ]; then \
        helm upgrade --install cerberus deploy/helm/cerberus \
            --namespace cerberus \
            --values test/e2e/k3s/cerberus-values.yaml \
            --values test/e2e/k3s/cerberus-values-split.yaml \
            --wait --timeout 180s; \
        echo "==> [split] exposing the prometheus head as the host:8080 NodePort alias"; \
        kubectl -n cerberus apply -f test/e2e/k3s/cerberus-split-nodeport.yaml; \
    else \
        helm upgrade --install cerberus deploy/helm/cerberus \
            --namespace cerberus \
            --values test/e2e/k3s/cerberus-values.yaml \
            --wait --timeout 180s; \
    fi
    @echo "==> waiting for pods (up to 3 min)"
    kubectl -n cerberus wait --for=condition=Available deployment/clickhouse              --timeout=180s
    @if [ "{{E2E_MODE}}" = "split" ]; then \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus-prometheus --timeout=180s; \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus-loki       --timeout=180s; \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus-tempo      --timeout=180s; \
    else \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus            --timeout=180s; \
    fi
    kubectl -n cerberus wait --for=condition=Available deployment/grafana                 --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/otel-collector-gateway  --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-traces       --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-metrics      --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-logs         --timeout=180s
    kubectl -n cerberus rollout status daemonset/otel-collector-agent                     --timeout=180s
    @echo "==> e2e-up done (mode={{E2E_MODE}})"
    @echo "    grafana:    http://localhost:3000 (admin/admin)"
    @echo "    cerberus:   http://localhost:8080/healthz"

# Ingest sample OTel data into ClickHouse. Runs the Go seed program at
# test/e2e/seed/cmd/seed/ which (a) applies the upstream OTel-CH DDL via
# internal/schema/ddl.Apply and (b) inserts the deterministic fixture rows.
# The DDL is the source of truth — the schema can no longer drift from the
# upstream exporter, unlike the previous hand-maintained *.sql scripts.
#
# Connects from the host via a transient kubectl port-forward; CH listens on
# port 9000 inside the cluster.
#
# Dual-data-source model (see test/e2e/k3s/README.md):
#   - `e2e-seed` inserts deterministic synthetic rows used by spec tests
#     that need exact values (e.g. `up` metric with known labels).
#   - The OTel collector DaemonSet+gateway+sample-app trio populates real
#     OTel data continuously for realistic Grafana smoke + dashboard tests.
# Both share the same `otel.*` tables (schema cannot drift — both write
# via the upstream sqltemplates).

# Seed deterministic OTel fixture rows into the cluster's ClickHouse.
e2e-seed:
    @echo "==> seeding OTel data via Go seeder"
    @kubectl -n cerberus port-forward svc/clickhouse 19000:9000 > /tmp/cerberus-e2e-seed-pf.log 2>&1 & \
        pf_pid=$!; \
        trap "kill $pf_pid 2>/dev/null || true" EXIT; \
        for i in 1 2 3 4 5 6 7 8 9 10; do \
            if nc -z 127.0.0.1 19000 2>/dev/null; then break; fi; \
            sleep 1; \
        done; \
        CH_ADDR=127.0.0.1:19000 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
            go run ./test/e2e/seed/cmd/seed
    @echo "==> seed done"

# One-shot RE-seed through a fresh, self-contained port-forward — used by the
# chaos lane's heal step after a CH-recreating scenario kills + recreates the
# ClickHouse pod. CH's data dir is PVC-backed (test/e2e/k3s/clickhouse.yaml:
# `strategy: Recreate` + a ReadWriteOnce PersistentVolumeClaim on
# /var/lib/clickhouse), so the recreated pod comes back WITH its schema +
# historical data — this re-seed no longer RESTORES lost tables, it RE-ANCHORS
# the rolling metric/log window on now64(9) so the next scenario's time-windowed
# asserts see fresh data. (The rolling seeder's long-lived port-forward,
# `e2e-seed-rolling`, is bound to a single backing pod, so `ch-pod-kill` breaks
# that tunnel; the reconnecting supervisor respawns it once CH is recreated, and
# this one-shot closes the freshness gap until the next rolling tick lands.)
# This recipe stands up its OWN throwaway forward on a DISTINCT local port (so it
# never races the rolling forward's 19000 socket), re-applies the idempotent
# OTel-CH DDL, re-inserts every fixture, verifies the rowcounts, and tears the
# forward down. Idempotent + re-runnable: the seeder's DDL is CREATE … IF NOT
# EXISTS and the INSERTs re-anchor on now64(9), so running it against either an
# empty or an already-populated CH is safe.

# Re-anchor the seeded window through a one-shot, fresh port-forward. Idempotent.
e2e-reseed:
    @echo "==> re-seeding OTel data (one-shot, fresh port-forward) after CH recreation"
    @kubectl -n cerberus port-forward svc/clickhouse 19001:9000 > /tmp/cerberus-e2e-reseed-pf.log 2>&1 & \
        pf_pid=$!; \
        trap "kill $pf_pid 2>/dev/null || true" EXIT; \
        for i in 1 2 3 4 5 6 7 8 9 10; do \
            if nc -z 127.0.0.1 19001 2>/dev/null; then break; fi; \
            sleep 1; \
        done; \
        CH_ADDR=127.0.0.1:19001 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
            go run ./test/e2e/seed/cmd/seed
    @echo "==> re-seed done"

# Rolling seeder. Performs the same INSERTs as `e2e-seed` and then stays
# alive, re-anchoring the metric/log windows on now64(9) every 30 s until
# stopped via `just e2e-seed-stop` (or SIGTERM). Replaces the static-window
# arms-race that widened the seed envelope to ±15 min in PRs #590 / #615 /
# #617 / #693 just to survive the ~12 min Playwright suite drift — with
# fresh data arriving continuously the static window only has to cover
# the 30 s gap between two ticks plus the 5 m Prom/Loki staleness lookback.
#
# Background pattern: a single bash command builds + launches the seeder
# under nohup, dissociated from this just-recipe shell (so the recipe
# returns and the next CI step can run while the seeder keeps reseeding).
# PID files at /tmp/cerberus-e2e-seed-*.pid let `e2e-seed-stop` find the
# processes for clean teardown. Logs land at /tmp/cerberus-e2e-seed-rolling.log
# so a failing tick is visible in CI artefacts.
#
# Port-forward durability (chaos lane): the forward is run through a
# RECONNECTING supervisor (test/e2e/seed/port_forward_supervisor.sh) under
# `setsid` so it is its own process-group leader. A bare `kubectl
# port-forward` is bound to one backing pod, so the chaos `ch-pod-kill`
# scenario breaks the tunnel and it never reconnects — the seeder then writes
# into a dead socket for the rest of the run, so the rolling window goes stale
# (the PVC keeps CH's historical data, but no fresh tick re-anchors it at now).
# The supervisor respawns the forward when it dies, so once CH is recreated the
# tunnel re-establishes and the 30 s rolling ticks resume (the seeder's
# clickhouse-go pool re-dials a fresh connection per tick). We stash the
# supervisor's PGID so e2e-seed-stop can kill the whole group (supervisor +
# its current kubectl child) atomically. This complements the chaos heal
# step's one-shot `just e2e-reseed`: the one-shot re-anchors the window
# immediately, the supervised rolling feed keeps data anchored at wall-clock now
# for the time-windowed assertions of the scenarios that follow.

# Seed, then keep re-anchoring the window every 30s in the background until e2e-seed-stop.
e2e-seed-rolling:
    @echo "==> launching rolling seeder (30s tick) in background"
    @# 1) start the reconnecting port-forward supervisor in its own process
    @#    group (setsid) and stash its PGID (== leader PID under setsid) so
    @#    teardown can signal the whole group.
    @setsid bash test/e2e/seed/port_forward_supervisor.sh \
        cerberus svc/clickhouse 19000:9000 > /tmp/cerberus-e2e-seed-pf.log 2>&1 & \
        echo $! > /tmp/cerberus-e2e-seed-pf.pid
    @# 2) wait for the forward to come up.
    @for i in 1 2 3 4 5 6 7 8 9 10; do \
        if nc -z 127.0.0.1 19000 2>/dev/null; then break; fi; \
        sleep 1; \
    done
    @# 3) build the seeder once so `nohup` launches a real binary
    @#    (a stray `go run` keeps the toolchain attached to the shell that
    @#    spawned it; harder to detach cleanly across CI step boundaries).
    @go build -o /tmp/cerberus-e2e-seeder ./test/e2e/seed/cmd/seed
    @# 4) launch the seeder under nohup with the rolling flag.
    @CH_ADDR=127.0.0.1:19000 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
        nohup /tmp/cerberus-e2e-seeder --re-seed-interval=30s \
            > /tmp/cerberus-e2e-seed-rolling.log 2>&1 & \
        echo $! > /tmp/cerberus-e2e-seed-rolling.pid
    @echo "==> rolling seeder pid=$(cat /tmp/cerberus-e2e-seed-rolling.pid) pf-pid=$(cat /tmp/cerberus-e2e-seed-pf.pid)"
    @echo "    initial seed runs synchronously inside the seeder before the loop starts —"
    @echo "    tail /tmp/cerberus-e2e-seed-rolling.log to confirm 'seed: done' lands."
    @# 5) wait for the initial seed to land before returning, so the next
    @#    CI step (e2e-wait-otel / e2e-run) sees a populated database.
    @for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
        if grep -q '^.*seed: done' /tmp/cerberus-e2e-seed-rolling.log 2>/dev/null; then \
            echo "==> initial seed landed"; \
            exit 0; \
        fi; \
        sleep 2; \
    done; \
        echo "==> ERROR: initial seed did not complete within 30s. log:"; \
        cat /tmp/cerberus-e2e-seed-rolling.log; \
        exit 1

# Stop the rolling seeder + its port-forward supervisor (idempotent).
# Called from CI teardown so the dashboard job tears down cleanly even when
# the Playwright step failed before reaching `e2e-down`. SIGTERM gives the
# seeder a chance to log the exit reason; the port-forward never has any
# state to flush. The port-forward PID file holds the supervisor's setsid
# leader PID (== its PGID), so `kill -TERM -<pid>` signals the whole group —
# the supervisor AND its current kubectl child — instead of orphaning the
# kubectl child while the supervisor respawns it.

# Stop the rolling seeder and its port-forward supervisor. Idempotent.
e2e-seed-stop:
    @echo "==> stopping rolling seeder"
    @if [ -f /tmp/cerberus-e2e-seed-rolling.pid ]; then \
        pid=$(cat /tmp/cerberus-e2e-seed-rolling.pid); \
        kill -TERM "$pid" 2>/dev/null || true; \
        rm -f /tmp/cerberus-e2e-seed-rolling.pid; \
    fi
    @if [ -f /tmp/cerberus-e2e-seed-pf.pid ]; then \
        pid=$(cat /tmp/cerberus-e2e-seed-pf.pid); \
        kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true; \
        rm -f /tmp/cerberus-e2e-seed-pf.pid; \
    fi

# Wait until the OTel collector has populated real data in every signal
# table (logs / traces / one of the metrics tables) AND the metric table
# carries ≥60 s of history. Bootstraps the pipeline before tests rely on
# it — telemetrygen + kubeletstats take ~30-60s to flush a first batch
# through the gateway, and 1m-windowed queries (rate(x[1m]), up[1m:30s])
# need a 60s span of TimeUnix values before they return a vector.
#
# Polls every 5s for up to 3 min; fails the recipe if any signal stays
# empty or the metric stream never reaches 60 s of spread. Uses
# `kubectl exec` against the ClickHouse pod so it does not need a
# host-side port-forward. Spread is asserted on whichever metric table
# (sum or gauge) carries non-zero rows first.

# Block until the OTel collector has populated every signal table with >=60s of history.
e2e-wait-otel:
    @echo "==> waiting for real OTel data (incl. clickhouse query_log stream) + ≥60s metric history in ClickHouse"
    @deadline=$(($(date +%s) + 180)); \
        while [ $(date +%s) -lt $deadline ]; do \
            logs=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_logs" 2>/dev/null || echo 0); \
            chlogs=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_logs WHERE ServiceName = 'clickhouse'" 2>/dev/null || echo 0); \
            traces=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_traces" 2>/dev/null || echo 0); \
            sum=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_sum" 2>/dev/null || echo 0); \
            gauge=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_gauge" 2>/dev/null || echo 0); \
            histogram=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_histogram WHERE MetricName = 'http_server_request_duration'" 2>/dev/null || echo 0); \
            hist_spread=0; \
            if [ "$histogram" -gt 0 ]; then \
                hist_spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_histogram WHERE MetricName = 'http_server_request_duration'" 2>/dev/null || echo 0); \
            fi; \
            spread=0; \
            if [ "$sum" -gt 0 ]; then \
                spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_sum" 2>/dev/null || echo 0); \
            elif [ "$gauge" -gt 0 ]; then \
                spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_gauge" 2>/dev/null || echo 0); \
            fi; \
            echo "    logs=$logs chlogs=$chlogs traces=$traces metrics_sum=$sum metrics_gauge=$gauge metrics_histogram=$histogram spread=${spread}s hist_spread=${hist_spread}s"; \
            if [ "$logs" -gt 0 ] && [ "$chlogs" -gt 0 ] && [ "$traces" -gt 0 ] && { [ "$sum" -gt 0 ] || [ "$gauge" -gt 0 ]; } && [ "$spread" -ge 60 ] && [ "$histogram" -gt 0 ] && [ "$hist_spread" -ge 60 ]; then \
                echo "==> OTel pipeline is live with ≥60s of metric history (incl. histogram companion + clickhouse query_log stream)"; \
                exit 0; \
            fi; \
            sleep 5; \
        done; \
        echo "==> timeout waiting for OTel data / metric history span"; \
        exit 1

# Run Go E2E HTTP tests against the deployed stack. CH_ADDR/CH_DATABASE/
# CH_USERNAME/CH_PASSWORD point at the same port-forward `e2e-seed-rolling`
# already opened on 19000 — test/e2e/seed/cmd/seed/reseed_stability_test.go
# (issue #1527's row-count-stability assertion, living next to the
# re-seeder it tests) dials ClickHouse directly to drive its own seedAll
# ticks; every other Go E2E test only talks to CERBERUS_URL and ignores
# these.

# Run the Go E2E HTTP tests against the deployed stack.
e2e-run:
    @echo "==> running Go E2E tests"
    CH_ADDR=127.0.0.1:19000 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
        go test -timeout 35m -tags=e2e ./test/e2e/...

# A godog suite driving `cerberus migrate` over committed fixtures — no
# Docker, no backend, seconds.

# Run the Tier-0 offline migration scenarios.
migration-tier0:
    @echo "==> running the tier-0 migration scenarios"
    go test -tags=migration ./test/e2e/migration/tiers/tier0-offline/... -count=1 -v

# Enumerate the Gherkin features into the JSON the coverage ratchet reads.
migration-scenarios out="build/migration-scenarios.json":
    @echo "==> enumerating migration scenarios into {{out}}"
    mkdir -p build
    go run ./test/e2e/migration/cmd/scenarios --out {{out}}

# A golden is a reviewed artifact, so this never REWRITES one under CI; diff
# every regenerated byte against the fixture's stated derivation. Under CI the
# rewrite degrades to the comparison, so the recipe still answers "are the
# committed bytes what the generator writes" — the question post-merge-drift.yml
# runs it to ask — and drift there is a hard error from the harness naming the
# differing lines, never a silent skip: a lane that quietly regenerated nothing
# would look identical to one with nothing to regenerate.
# `update-golden` chains this recipe, so a plan-shape change repairs the
# explain reports in the same command that repairs the TXTAR goldens; run it
# directly when the migration corpus is the only thing that moved.

# Regenerate the Tier-0 migration goldens from the current tool behaviour.
migration-golden:
    @echo "==> regenerating the tier-0 migration goldens"
    MIGRATION_UPDATE_GOLDENS=1 go test -tags=migration ./test/e2e/migration/tiers/tier0-offline/... -count=1
    @echo
    @echo "Diff of regenerated goldens:"
    @git --no-pager diff --stat test/e2e/migration/archetypes/ || true

# Run the startup-speed benchmark: spawn cerberus and measure wall-clock
# time from process-start to first 200 OK on /healthz. Asserts < 2.5 s.
# Requires a reachable ClickHouse at $CH_ADDR (default 127.0.0.1:9000);
# the benchmark sets CERBERUS_AUTO_CREATE_SCHEMA=false so we measure
# pure HTTP-listener bootstrap and not DDL apply time.
#
# Override with CH_ADDR / CH_DATABASE / CH_USERNAME / CH_PASSWORD env
# vars; see test/e2e/startup_bench_test.go for the full list.

# Measure process-start to first 200 on /healthz. Needs a reachable ClickHouse.
startup-bench:
    @echo "==> startup-speed benchmark (target < 2 s to /healthz)"
    go test -tags=startup_bench -v -count=1 -run TestStartupSpeed ./test/e2e/...

# Run the Grafana playwright smoke (lands in M0.2).
e2e-playwright:
    @echo "==> playwright smoke (lands in M0.2)"
    @if [ -d test/e2e/playwright ]; then \
        cd test/e2e/playwright && npm ci && npx playwright test; \
    else \
        echo "    (no playwright suite yet — landing in M0.2)"; \
    fi

# Apply the chaos overlay onto the running cerberus Deployment: the
# resilience knobs in test/e2e/chaos/manifests/chaos-overlay.env (low
# breaker threshold + small CERBERUS_QUERY_TIMEOUT + small admit/pool
# caps) so the live-stack chaos faults trip FAST + DETERMINISTICALLY
# within budget. Patches the Deployment's pod env via `kubectl set env`
# (one rollout), then waits for it to roll out so every cerberus pod
# carries the overlay before fault injection. Idempotent — re-applying
# the same env values is a no-op rollout.

# Apply the chaos resilience knobs to the running cerberus Deployment.
e2e-chaos-overlay:
    @echo "==> applying chaos overlay (resilience knobs) to deploy/cerberus"
    @env_args=""; \
        while IFS= read -r line; do \
            case "$line" in ''|\#*) continue ;; esac; \
            env_args="$env_args $line"; \
        done < test/e2e/chaos/manifests/chaos-overlay.env; \
        echo "    kubectl set env deploy/cerberus$env_args"; \
        kubectl -n cerberus set env deployment/cerberus $env_args
    @echo "==> waiting for the overlay rollout"
    kubectl -n cerberus rollout status deployment/cerberus --timeout=120s

# Run the live-stack chaos lane: fault-inject against the running k3d
# stack and assert the gateway's resilience contracts (circuit breaker,
# per-query wall-clock timeout, admission control, replica resilience)
# hold under REAL faults. Drives .github/scripts/chaos-run.mjs (node ESM,
# kubectl + fetch). Assumes `e2e-up` + `e2e-seed-rolling` + `e2e-wait-otel`
# already ran AND the chaos overlay is applied (`e2e-chaos-overlay`).
# CHAOS_PHASE=phase-1 by default (ch-pod-kill, ch-slow/query-timeout,
# cerberus-pod-kill); CHAOS_PHASE=all adds the phase-2 scenarios.
# Mirrors the `e2e-run` / `e2e-playwright` shape — locally reproducible.

# Fault-inject against the running k3d stack and assert the resilience contracts hold.
e2e-chaos:
    @echo "==> running live-stack chaos lane (chaos-run.mjs)"
    CERBERUS_URL=http://localhost:8080 \
        CHAOS_PHASE="${CHAOS_PHASE:-phase-1}" \
        node .github/scripts/chaos-run.mjs

# Tear down the cluster. Also stops the rolling seeder + port-forward
# if either was started by `e2e-seed-rolling` — idempotent and silent
# when the PID files don't exist.

# Tear the k3d cluster down, stopping the rolling seeder first. Idempotent.
e2e-down:
    @if [ -f /tmp/cerberus-e2e-seed-rolling.pid ] || [ -f /tmp/cerberus-e2e-seed-pf.pid ]; then \
        just e2e-seed-stop; \
    fi
    @if k3d cluster list | grep -q "^{{K3D_CLUSTER}} "; then \
        echo "==> deleting k3d cluster {{K3D_CLUSTER}}"; \
        k3d cluster delete {{K3D_CLUSTER}}; \
    fi



# Full lifecycle. Seed first (deterministic rows, rolling so the window
# slides with wall-clock now), then wait for the collector to populate
# real OTel data, then run the test matrix. `e2e-down` stops the rolling
# seeder on teardown.

# Run the full e2e lifecycle: up, rolling seed, wait, Go tests, Playwright, down.
e2e: e2e-up e2e-seed-rolling e2e-wait-otel e2e-run e2e-playwright e2e-down

