package spec

import "github.com/tsouza/cerberus/internal/schema"

// FixtureMetrics is the metrics schema the TXTAR fixtures are lowered
// against: the OTel-CH default with its Flags column established
// (schema.Metrics.FlagsColumnProbed). Every fixture seed carries the column
// — the chDB seed backfill injects it into any metric table a seed declares
// without it (internal/testsql's backfilledColumns) — so the fixtures
// exercise the stale-marker read path a probed deployment runs.
func FixtureMetrics() schema.Metrics {
	m := schema.DefaultOTelMetrics()
	m.FlagsColumnProbed = true
	return m
}
