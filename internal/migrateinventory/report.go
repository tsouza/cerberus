package migrateinventory

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/tsouza/cerberus/internal/migrate"
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
	bw := migrate.NewErrWriter(w)
	bw.Printf("# cerberus migrate inventory\n")
	bw.Printf("#\n")
	bw.Printf("# Live cardinality probed from the SOURCE Prometheus TSDB (%s).\n", inv.Source)
	bw.Printf("# These are source-Prometheus runtime facts — the realized series counts\n")
	bw.Printf("# config can't reveal offline. High-cardinality metrics are the OOM\n")
	bw.Printf("# CANDIDATES cerberus can't see before cutover. They RANK RISK; they do\n")
	bw.Printf("# NOT predict cerberus's exact memory (that depends on the query + engine).\n")
	if inv.Window != "" {
		bw.Printf("# Observation window (operator context): %s. TSDB status is a\n", inv.Window)
		bw.Printf("# point-in-time head snapshot, so the window frames the numbers.\n")
	}
	bw.Printf("#\n\n")

	bw.Printf("== head block\n")
	bw.Printf("  series:      %d\n", inv.Head.NumSeries)
	bw.Printf("  label pairs: %d\n", inv.Head.NumLabelPairs)
	bw.Printf("  chunks:      %d\n", inv.Head.ChunkCount)
	if hasHeadSpan(inv.Head) {
		bw.Printf("  head span:   %s .. %s\n", formatMillis(inv.Head.MinTime), formatMillis(inv.Head.MaxTime))
	}
	if inv.MetricNameTotal >= 0 {
		bw.Printf("  distinct metric names: %d\n", inv.MetricNameTotal)
	}
	if inv.MetadataMetricTotal >= 0 {
		bw.Printf("  metrics with metadata: %d\n", inv.MetadataMetricTotal)
	}
	bw.Printf("\n")

	writeRanked(bw, fmt.Sprintf("top %d metrics by series count (OOM candidates)", inv.Top),
		inv.TopMetricsBySeries, "series")
	writeRanked(bw, fmt.Sprintf("top %d labels by value cardinality (fan-out drivers)", inv.Top),
		inv.TopLabelsByValues, "values")
	writeRanked(bw, fmt.Sprintf("top %d labels by head memory", inv.Top),
		inv.TopLabelsByMemory, "bytes")

	if len(inv.Notes) > 0 {
		bw.Printf("== notes (%d)\n", len(inv.Notes))
		for _, n := range inv.Notes {
			bw.Printf("  %s\n", n)
		}
	}

	writeLokiSection(bw, inv.Loki)
	writeTempoSection(bw, inv.Tempo)

	return bw.Err
}

// writeLokiSection prints the optional per-selector Loki section, or nothing
// at all when the operator never supplied a Loki source — a nil section is
// simply absent from the report, never rendered as an empty ranked table.
func writeLokiSection(bw *migrate.ErrWriter, loki *LokiInventory) {
	if loki == nil {
		return
	}
	bw.Printf("\n== loki: %s\n", loki.Source)
	if loki.Window != "" {
		bw.Printf("  window: %s\n", loki.Window)
	}
	if len(loki.Selectors) == 0 {
		bw.Printf("  none reported by the source\n")
	}
	for i, s := range loki.Selectors {
		bw.Printf("  %3d. %-*s %d streams %d chunks %d entries %d bytes\n",
			i+1, rankNameWidth, s.Selector, s.Streams, s.Chunks, s.Entries, s.Bytes)
	}
	if len(loki.Notes) > 0 {
		bw.Printf("  notes (%d):\n", len(loki.Notes))
		for _, n := range loki.Notes {
			bw.Printf("    %s\n", n)
		}
	}
}

// writeTempoSection prints the optional, always-fixed Tempo out-of-scope
// section, or nothing at all when the operator never supplied a Tempo
// source.
func writeTempoSection(bw *migrate.ErrWriter, tempo *TempoInventory) {
	if tempo == nil {
		return
	}
	bw.Printf("\n== tempo: %s\n", tempo.Source)
	bw.Printf("  out of scope: %s\n", tempo.OutOfScope)
}

// writeRanked prints one ranked table, or an explicit "none reported" line so an
// empty array is visible rather than a silent gap.
func writeRanked(bw *migrate.ErrWriter, title string, rows []NameValue, unit string) {
	bw.Printf("== %s\n", title)
	if len(rows) == 0 {
		bw.Printf("  none reported by the source\n\n")
		return
	}
	for i, r := range rows {
		bw.Printf("  %3d. %-*s %d %s\n", i+1, rankNameWidth, r.Name, r.Value, unit)
	}
	bw.Printf("\n")
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
