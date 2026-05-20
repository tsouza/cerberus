package prom

import (
	"net/http"
)

// handleRules implements `/api/v1/rules` and `/api/v1/alerts` — both are
// Prometheus alerting/recording-rules endpoints that Grafana polls on every
// page load to gate the "Alert Rules" UI affordance. cerberus does not
// evaluate alerting or recording rules (it is a query gateway, not a Prom
// server), so both return the canonical empty envelope an unconfigured
// upstream Prometheus would.
//
// Wire shape (matches upstream Prometheus exactly):
//
//	{
//	  "status": "success",
//	  "data": {"groups": []}    // rules
//	  "data": {"alerts": []}    // alerts
//	}
//
// Returning 404 here (the pre-handler behaviour) caused two visible
// regressions in the compose stack:
//
//   - Every Grafana page load logged a `404 page not found` on
//     `/api/datasources/uid/cerberus-prometheus/resources/api/v1/rules`,
//     polluting the browser console and the user's "Failed to load
//     resource" tally.
//   - The Grafana alerting UI degraded to a generic error banner because
//     the probe couldn't distinguish "datasource doesn't support rules"
//     from "datasource is broken".
//
// A 200-with-empty-groups response answers both — Grafana treats it as
// "no rules configured" and the page is quiet.
func (h *Handler) handleRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   map[string]any{"groups": []any{}},
	})
}

// handleAlerts implements `/api/v1/alerts` — the active-alerts companion
// to /rules. Same rationale: cerberus has no alerting state, so the
// canonical empty envelope keeps Grafana's poll quiet.
func (h *Handler) handleAlerts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   map[string]any{"alerts": []any{}},
	})
}
