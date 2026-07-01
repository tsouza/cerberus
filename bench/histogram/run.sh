#!/usr/bin/env bash
# Histogram-quantile benchmark driver.
#
#   ./run.sh smoke   # small, fast — proves the harness end-to-end
#   ./run.sh bench    # larger dataset (24h, high cardinality)
#   ./run.sh down     # tear the stack down (removes volumes)
#
# Brings up ClickHouse + cerberus + Prometheus (required) and Mimir
# (best-effort — the run still produces cerberus-vs-Prometheus numbers if Mimir
# fails to start), seeds the identical fixture into all backends, then times the
# query battery and writes RESULTS.md.
#
# All host ports live in the 52xxx range and the compose project is
# `histbench-ab5`, so this stack is fully isolated from any other bench stack
# that may already be running on the machine.
set -euo pipefail
cd "$(dirname "$0")"

PROFILE="${1:-smoke}"
PROJECT=histbench-ab5

# Host ports — must match docker-compose.yml.
CH_NATIVE_PORT=52000
CERBERUS_PORT=52091
PROM_PORT=52090
MIMIR_PORT=52009

if [[ "$PROFILE" == "down" ]]; then
  docker compose -p "$PROJECT" down -v
  exit 0
fi

case "$PROFILE" in
  smoke) ROUTES=5   INSTANCES=2 BOUNDS=11 STEPS=240  INTERVAL=15 ITERS=30 ;;
  bench) ROUTES=100 INSTANCES=5 BOUNDS=11 STEPS=1440 INTERVAL=60 ITERS=50 ;;
  *) echo "usage: $0 [smoke|bench|down]"; exit 1 ;;
esac

echo "==> [1/5] building + starting core stack (clickhouse, cerberus, prometheus)"
docker compose -p "$PROJECT" up -d --build --wait clickhouse cerberus prometheus

echo "==> [2/5] starting mimir (best-effort)"
MIMIR_OK=1
docker compose -p "$PROJECT" up -d mimir || MIMIR_OK=0
if [[ "$MIMIR_OK" == "1" ]]; then
  MIMIR_OK=0
  for i in $(seq 1 40); do
    if curl -sf "http://localhost:${MIMIR_PORT}/ready" >/dev/null 2>&1; then MIMIR_OK=1; break; fi
    sleep 3
  done
  [[ "$MIMIR_OK" == "1" ]] && echo "    mimir ready" || echo "    mimir NOT ready — continuing with cerberus + prometheus only"
fi

MIMIR_RW=""
MIMIR_QURL=""
if [[ "$MIMIR_OK" == "1" ]]; then
  MIMIR_RW="http://localhost:${MIMIR_PORT}/api/v1/push"
  MIMIR_QURL="http://localhost:${MIMIR_PORT}/prometheus"
fi

echo "==> [3/5] resolving Go module (offline cache)"
go mod download

echo "==> [4/5] seeding identical fixture into ClickHouse + Prometheus${MIMIR_QURL:+ + Mimir}"
go run ./cmd/gen \
  -ch-addr "localhost:${CH_NATIVE_PORT}" \
  -routes "$ROUTES" -instances "$INSTANCES" -bounds "$BOUNDS" \
  -steps "$STEPS" -interval "$INTERVAL" \
  -prom-remote-write "http://localhost:${PROM_PORT}/api/v1/write" \
  -mimir-remote-write "$MIMIR_RW"

echo "    waiting for TSDB ingest to become queryable"
sleep 5

echo "==> [5/5] running query battery (profile=$PROFILE, iters=$ITERS)"
go run ./cmd/bench -profile "$PROFILE" -iters "$ITERS" \
  -cerberus-url "http://localhost:${CERBERUS_PORT}" \
  -prom-url "http://localhost:${PROM_PORT}" \
  -mimir-url "$MIMIR_QURL" \
  -cerberus-ctr "${PROJECT}-cerberus" \
  -prom-ctr "${PROJECT}-prometheus" \
  -mimir-ctr "${PROJECT}-mimir"

echo "==> done. See $(pwd)/RESULTS.md"
