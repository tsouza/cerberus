// Package loki serves the subset of the Loki HTTP API that Grafana exercises,
// translating it into cerberus query plans.
//
// HTTP handlers land after the PromQL vertical slice (v0.1+).
package loki
