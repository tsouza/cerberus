package loki

import "net/http"

// DrilldownLimitsResponse mirrors upstream Loki's
// `/loki/api/v1/drilldown-limits` body (pkg/loki.DrilldownConfigResponse,
// added by grafana/loki#19028): a flat top-level JSON object, NOT
// wrapped in the {status, data} envelope the rest of the v1 surface
// uses. Grafana's first-party Logs Drilldown app
// (grafana-lokiexplore-app, preinstalled since Grafana 12.x) fetches
// this resource on boot to decide which UI surfaces to enable.
type DrilldownLimitsResponse struct {
	// Limits carries the subset of upstream Loki's per-tenant limits
	// the backend chooses to publish (upstream filters through its
	// `tenant_limits_allow_publish` allowlist). Cerberus publishes
	// only the fields that describe behaviour it actually implements.
	Limits map[string]any `json:"limits"`
	// PatternIngesterEnabled gates the Drilldown app's "Patterns" tab.
	PatternIngesterEnabled bool `json:"pattern_ingester_enabled"`
	// Version is the backend build identifier (same value the
	// /status/buildinfo probe reports).
	Version string `json:"version"`
}

// handleDrilldownLimits implements `GET /loki/api/v1/drilldown-limits`.
// The published fields are cerberus's real contract, not a copy of
// upstream defaults:
//
//   - pattern_ingester_enabled: true — cerberus serves
//     /loki/api/v1/patterns via the drain template miner.
//   - limits.volume_enabled: true — cerberus serves
//     /loki/api/v1/index/volume.
//   - limits.discover_log_levels: true — cerberus synthesizes the
//     `detected_level` label family from the OTel SeverityText column.
//   - limits.max_entries_limit_per_query: the hard per-query line cap
//     cerberus enforces in parseLogLimit (maxLogQueryLimit).
//
// No ClickHouse round-trip — the response is static per build.
func (h *Handler) handleDrilldownLimits(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, DrilldownLimitsResponse{
		Limits: map[string]any{
			"discover_log_levels":         true,
			"max_entries_limit_per_query": maxLogQueryLimit,
			"volume_enabled":              true,
		},
		PatternIngesterEnabled: true,
		Version:                h.Version,
	})
}
