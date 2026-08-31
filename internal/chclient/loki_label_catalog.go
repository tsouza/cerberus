package chclient

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// LabelCardinalityRow is one (label key, distinct-value count) row decoded
// from the Loki label-cardinality catalog table (cerberus issue #2770) —
// the shape internal/api/loki's detected_labels catalog read path queries
// via `SELECT LabelKey, uniqMerge(CardinalityState) FROM
// <loki_label_catalog> GROUP BY LabelKey`.
type LabelCardinalityRow struct {
	LabelKey    string
	Cardinality uint64
}

// QueryLabelCardinalities runs sql and decodes each row into a
// LabelCardinalityRow. Used by the /detected_labels catalog-eligible read
// path (internal/api/loki/detected_labels.go) — a small, bounded result
// (one row per distinct label key), so unlike QueryLabelSets this applies
// no drain-budget check (see queryScanRows' own doc comment on that).
//
// Guarded by the circuit breaker (see [Client] doc).
func (c *Client) QueryLabelCardinalities(ctx context.Context, sqlStr string, args ...any) ([]LabelCardinalityRow, error) {
	return queryScanRows(ctx, c, sqlStr, args, func(rows driver.Rows) (LabelCardinalityRow, error) {
		var r LabelCardinalityRow
		err := rows.Scan(&r.LabelKey, &r.Cardinality)
		return r, err
	})
}
