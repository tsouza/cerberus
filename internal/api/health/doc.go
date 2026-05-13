// Package health implements the cerberus liveness (/healthz) and
// readiness (/readyz) HTTP probes.
//
// /healthz is intentionally minimal: it returns 200 as long as the
// process is alive. Kubernetes uses it as the livenessProbe and a
// failure means the orchestrator restarts the pod, so it must not
// depend on any downstream service.
//
// /readyz is the readinessProbe: it pings ClickHouse and reports the
// auto-create-schema invariant when CERBERUS_AUTO_CREATE_SCHEMA is on.
// Probes hit at multi-Hz rates, so the result is memoised behind a
// small TTL so the readiness handler does not hammer ClickHouse.
package health
