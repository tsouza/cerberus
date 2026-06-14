#!/usr/bin/env bash
# port_forward_supervisor.sh — reconnecting `kubectl port-forward` supervisor
# for the rolling seeder (just e2e-seed-rolling).
#
# The chaos lane's `ch-pod-kill` scenario deletes the ClickHouse pod. A bare
# `kubectl port-forward` is bound to a single backing pod, so when that pod
# dies the tunnel breaks and `kubectl` exits — it NEVER re-establishes itself.
# The rolling seeder behind it then writes into a dead `127.0.0.1:<port>`
# socket (`connection refused`) for the rest of the run, leaving ClickHouse
# starved of fresh data and every downstream chaos scenario asserting against
# an empty/stale table.
#
# This supervisor wraps the forward in a respawn loop: whenever `kubectl
# port-forward` exits (pod recreated, connection reset, EOF), it waits a short
# backoff and re-runs it against the Service, which now resolves to the freshly
# recreated pod. The seeder's clickhouse-go pool re-dials a fresh connection on
# the next tick once the local socket is listening again, so the 30 s rolling
# feed resumes on its own after CH recovery.
#
# Run under `setsid` so the supervisor is its own process-group leader; the
# caller kills the whole group (`kill -TERM -<pgid>`) to take down the
# supervisor AND its current `kubectl` child atomically on teardown.
#
# Args:
#   $1  Kubernetes namespace          (e.g. cerberus)
#   $2  Service to forward            (e.g. svc/clickhouse)
#   $3  local:remote port mapping     (e.g. 19000:9000)
#
# Env (optional, named so the cadence is self-documenting):
#   PF_RESPAWN_BACKOFF_SECONDS  pause between a forward dying and respawning
#                               (default 1). Short so the tunnel re-establishes
#                               well within one 30 s seeder tick.
set -u

namespace="${1:?namespace required}"
service="${2:?service required (e.g. svc/clickhouse)}"
ports="${3:?port mapping required (e.g. 19000:9000)}"

respawn_backoff_seconds="${PF_RESPAWN_BACKOFF_SECONDS:-1}"

# Forward any termination signal to the active kubectl child so teardown is
# immediate rather than waiting out the current forward's lifetime.
child_pid=""
terminate() {
  if [ -n "$child_pid" ]; then
    kill -TERM "$child_pid" 2>/dev/null || true
  fi
  exit 0
}
trap terminate TERM INT

echo "supervisor: starting reconnecting port-forward ${service} ${ports} (ns=${namespace})"
while true; do
  kubectl -n "$namespace" port-forward "$service" "$ports" &
  child_pid=$!
  wait "$child_pid"
  child_pid=""
  echo "supervisor: port-forward ${service} ${ports} exited; respawning in ${respawn_backoff_seconds}s"
  sleep "$respawn_backoff_seconds"
done
