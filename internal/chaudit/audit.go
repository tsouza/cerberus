// Package chaudit answers one question about a LIVE deployment: which
// classic-histogram metrics are close enough to a resource-bound ceiling that
// a dashboard panel over them is about to start failing, and what an operator
// can do about it.
//
// Why it exists. internal/migrateinventory probes a SOURCE Prometheus before a
// cutover, which is exactly the right question at that moment and the wrong
// one afterwards: once cerberus is serving, the facts that decide whether a
// panel succeeds live in the connected ClickHouse, and they drift with real
// traffic. Issue #2677's production incident was diagnosed only after a panel
// went red, by hand-running uniqExact and length(ExplicitBounds) against the
// live table and re-deriving the guard's cost model from its source comments.
// This package is that diagnosis, performed before the panel breaks.
//
// It opens no connection of its own. The caller supplies an already-configured
// Querier, so this package holds no credentials, no pool, and no lifecycle —
// the same separation internal/migrateinventory keeps from the HTTP sources it
// probes.
package chaudit

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
)

// plainIdentifier is what Options.Table must match: a ClickHouse identifier,
// optionally database-qualified. Anchored at both ends so nothing rides along
// behind a valid-looking prefix.
var plainIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// Querier is the read-only subset of *sql.DB this package needs. Narrow on
// purpose: an audit must never be able to mutate the deployment it inspects,
// and a one-method interface makes that checkable by reading the type rather
// than by auditing call sites.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Options selects what to audit and against which ceiling.
type Options struct {
	// Table is the classic-histogram table to probe.
	Table string
	// WindowSeconds is the lookback the audit evaluates, matching the widest
	// dashboard window an operator cares about defending.
	WindowSeconds int64
	// Anchors is the grid point count that window implies at the panel's step
	// — the `anchors` factor of the guard's own cost model.
	Anchors int64
	// DensityUnitBudget is the ceiling to compare against: the resolved
	// CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_DENSITY_UNITS the deployment runs.
	DensityUnitBudget int64
	// Top bounds how many metrics are reported, worst headroom first.
	Top int
}

// Validate rejects an Options that would produce a meaningless report rather
// than letting it run and emit confident nonsense.
func (o Options) Validate() error {
	switch {
	case o.Table == "":
		return fmt.Errorf("chaudit: Table is required")
	case !plainIdentifier.MatchString(o.Table):
		// ClickHouse has no bind form for an identifier, so Table reaches the
		// query through string interpolation. It arrives from a command-line
		// flag, so "the caller configured it" is not the same as "the caller
		// meant it" — a typo becomes a confusing ClickHouse parse error and a
		// pasted fragment becomes something worse. Restricting it to a plain
		// (optionally database-qualified) identifier is what lets the queries
		// interpolate it without further ceremony.
		return fmt.Errorf(
			"chaudit: Table %q is not a plain identifier (letters, digits, underscore, "+
				"optionally database-qualified)", o.Table,
		)
	case o.WindowSeconds <= 0:
		return fmt.Errorf("chaudit: WindowSeconds must be > 0, got %d", o.WindowSeconds)
	case o.Anchors <= 0:
		return fmt.Errorf("chaudit: Anchors must be > 0, got %d", o.Anchors)
	case o.DensityUnitBudget <= 0:
		return fmt.Errorf("chaudit: DensityUnitBudget must be > 0, got %d", o.DensityUnitBudget)
	case o.Top <= 0:
		return fmt.Errorf("chaudit: Top must be > 0, got %d", o.Top)
	}
	return nil
}

// MetricAudit is one metric's standing against the ceiling.
type MetricAudit struct {
	Metric string `json:"metric"`

	// Series / RawRows / BucketWidth are the three factors the density guard
	// multiplies. Reported individually because they have different remedies:
	// width is fixed at the instrumentation, rows by retention or scrape
	// interval, series by the label set.
	Series      int64 `json:"seriesCount"`
	RawRows     int64 `json:"rawRows"`
	BucketWidth int64 `json:"bucketWidth"`

	// CostUnits is the guard's own model — (series x anchors) + (rawRows x
	// width^2) — evaluated on these facts, and Budget the ceiling it is
	// compared against. HeadroomPct is negative once the metric would be
	// rejected, which is the number worth alerting on.
	CostUnits   int64   `json:"costUnits"`
	Budget      int64   `json:"budget"`
	HeadroomPct float64 `json:"headroomPct"`

	// AmplifyingLabel is the label key whose removal would collapse series
	// cardinality the most, and AmplificationRatio by how much. This is the
	// actionable half of the report: a label that multiplies identity without
	// changing what a panel displays is a fixable instrumentation defect, not
	// a cerberus limit. Empty when no single label dominates.
	AmplifyingLabel    string  `json:"amplifyingLabel,omitempty"`
	AmplificationRatio float64 `json:"amplificationRatio,omitempty"`
}

// OverBudget reports whether this metric would be rejected outright.
func (m MetricAudit) OverBudget() bool { return m.CostUnits > m.Budget }

// Report is the audit result.
type Report struct {
	SchemaVersion int           `json:"schema_version"`
	Table         string        `json:"table"`
	WindowSeconds int64         `json:"windowSeconds"`
	Anchors       int64         `json:"anchors"`
	Metrics       []MetricAudit `json:"metrics"`

	// Notes records every fact the audit could NOT establish, so a thin report
	// is never mistaken for a clean one.
	Notes []string `json:"notes,omitempty"`
}

// ReportVersion is stamped on every Report so a consumer can tell which shape
// it is reading, mirroring migrateinventory.InventoryVersion's contract.
const ReportVersion = 1

// OverBudget returns the metrics that would be rejected today.
func (r Report) OverBudget() []MetricAudit {
	var out []MetricAudit
	for _, m := range r.Metrics {
		if m.OverBudget() {
			out = append(out, m)
		}
	}
	return out
}

// costUnits evaluates the density guard's model. Kept as one function so the
// report and any future gate agree by construction rather than by two authors
// transcribing the same formula.
//
// It mirrors internal/chsql's bucketGridDensityGuardFrag:
// `(groups x anchors) + (rawRows x width^2)`.
//
// `groups` is NOT the series count. The emitter groups by the query's key
// columns AND the `le` rung (range_bucket_grid_native_bound.go's
// probeGroups.GroupBy(keyCols..., Col(bucketGridLeAlias))), because Level-0
// unnests one row per (sample, rung) and Level-1 runs a grid per (series,
// rung). That file's own header states it: "`groups` is series cardinality
// times the number of distinct `le` rungs a series' own layout carries".
//
// Reading `groups` as `series` understated the first term by the rung count —
// 12 to 16 on the calibrated production shapes — so the audit reported
// headroom on metrics the engine would reject. That is the one failure this
// package must not have: a clean audit that precedes an incident is worse than
// no audit, because it was consulted and believed.
//
// The duplication is deliberate and bounded: chsql emits this as SQL for
// ClickHouse to evaluate at query time, which an audit cannot reuse because
// there is no query to attach it to. TestCostUnits_MatchesTheEmittedGuardModel
// pins the two together.
func costUnits(series, anchors, rawRows, width int64) int64 {
	return series*width*anchors + rawRows*width*width
}

// headroomPct is how much of the budget remains, negative once exceeded.
func headroomPct(cost, budget int64) float64 {
	if budget <= 0 {
		return 0
	}
	return (float64(budget) - float64(cost)) / float64(budget) * 100
}

// rankByHeadroom orders worst-first, so the metrics an operator must act on
// appear before the ones that merely exist.
func rankByHeadroom(m []MetricAudit) {
	sort.SliceStable(m, func(i, j int) bool { return m[i].HeadroomPct < m[j].HeadroomPct })
}
