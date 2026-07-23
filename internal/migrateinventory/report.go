package migrateinventory

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// WriteJSON renders the inventory as machine-readable JSON with a trailing
// newline. It stamps the current schema version so the artifact is self-describing
// and the cutover gate can refuse an inventory shape it does not understand.
func (inv Inventory) WriteJSON(w io.Writer) error {
	inv.SchemaVersion = InventoryVersion
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(inv); err != nil {
		return fmt.Errorf("encode inventory: %w", err)
	}
	return nil
}

// WriteText renders the inventory as scannable human-readable text. It leads
// with the honesty framing (these are source-Prometheus runtime facts that rank
// OOM risk, not a cerberus memory prediction), then the head-block size, then
// the ranked risk tables, then any enrichment notes.
func (inv Inventory) WriteText(w io.Writer) error {
	bw := &errWriter{w: w}
	bw.printf("# cerberus migrate inventory\n")
	bw.printf("#\n")
	bw.printf("# Live cardinality probed from the SOURCE Prometheus TSDB (%s).\n", inv.Source)
	bw.printf("# These are source-Prometheus runtime facts — the realized series counts\n")
	bw.printf("# config can't reveal offline. High-cardinality metrics are the OOM\n")
	bw.printf("# CANDIDATES cerberus can't see before cutover. They RANK RISK; they do\n")
	bw.printf("# NOT predict cerberus's exact memory (that depends on the query + engine).\n")
	if inv.Window != "" {
		bw.printf("# Observation window (operator context): %s. TSDB status is a\n", inv.Window)
		bw.printf("# point-in-time head snapshot, so the window frames the numbers.\n")
	}
	bw.printf("#\n\n")

	bw.printf("== head block\n")
	bw.printf("  series:      %d\n", inv.Head.NumSeries)
	bw.printf("  label pairs: %d\n", inv.Head.NumLabelPairs)
	bw.printf("  chunks:      %d\n", inv.Head.ChunkCount)
	if hasHeadSpan(inv.Head) {
		bw.printf("  head span:   %s .. %s\n", formatMillis(inv.Head.MinTime), formatMillis(inv.Head.MaxTime))
	}
	if inv.MetricNameTotal >= 0 {
		bw.printf("  distinct metric names: %d\n", inv.MetricNameTotal)
	}
	if inv.MetadataMetricTotal >= 0 {
		bw.printf("  metrics with metadata: %d\n", inv.MetadataMetricTotal)
	}
	bw.printf("\n")

	writeRanked(bw, fmt.Sprintf("top %d metrics by series count (OOM candidates)", inv.Top),
		inv.TopMetricsBySeries, "series")
	writeRanked(bw, fmt.Sprintf("top %d labels by value cardinality (fan-out drivers)", inv.Top),
		inv.TopLabelsByValues, "values")
	writeRanked(bw, fmt.Sprintf("top %d labels by head memory", inv.Top),
		inv.TopLabelsByMemory, "bytes")

	if len(inv.Notes) > 0 {
		bw.printf("== notes (%d)\n", len(inv.Notes))
		for _, n := range inv.Notes {
			bw.printf("  %s\n", n)
		}
	}
	return bw.err
}

// writeRanked prints one ranked table, or an explicit "none reported" line so an
// empty array is visible rather than a silent gap.
func writeRanked(bw *errWriter, title string, rows []NameValue, unit string) {
	bw.printf("== %s\n", title)
	if len(rows) == 0 {
		bw.printf("  none reported by the source\n\n")
		return
	}
	for i, r := range rows {
		bw.printf("  %3d. %-*s %d %s\n", i+1, rankNameWidth, r.Name, r.Value, unit)
	}
	bw.printf("\n")
}

// rankNameWidth pads the name column so the value column lines up in the ranked
// tables. It is a cosmetic alignment width, not a data limit.
const rankNameWidth = 48

// hasHeadSpan reports whether the head block carries a real time span worth
// printing. An EMPTY head is not all-zero: Prometheus reports it with sentinel
// bounds (MinTime = math.MaxInt64, MaxTime = math.MinInt64), so MinTime > MaxTime.
// Printing those verbatim yields garbage year-292-billion timestamps, so a span
// is shown only when MaxTime >= MinTime and at least one bound is non-zero.
func hasHeadSpan(h HeadStats) bool {
	return h.MaxTime >= h.MinTime && (h.MinTime != 0 || h.MaxTime != 0)
}

// formatMillis renders a Prometheus millisecond epoch as an RFC3339 UTC instant.
func formatMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// errWriter collapses the Fprintf error checks in the text writers into a single
// short-circuiting sink: once a write fails, later printf calls no-op and the
// first error is returned.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
