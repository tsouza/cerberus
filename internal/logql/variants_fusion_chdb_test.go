//go:build chdb

// Value-level coverage for the fused `variants(...) of (...)` shape.
//
// The TXTAR fixtures pin the fused answer for the shapes they carry, and
// their chDB round-trip values are byte-identical to the per-arm shape's.
// This file covers the one fused shape no fixture exercises and whose
// equivalence is NOT obvious from the plan: an UNGROUPED aggregation
// (`sum(count_over_time(...))`, no `by`).
//
// The per-arm plan aggregates with an EMPTY GroupBy, which carries
// chplan.Aggregate.DropEmptyOnNoGroup — the guard that stops ClickHouse
// returning its default one-row-of-zeros for an aggregate-only query over no
// input. Fusing threads `__variant__` into the group keys, so GroupBy stops
// being empty and that guard goes inert (it is documented as having no effect
// with a non-empty GroupBy). The "aggregation over empty input produces NO
// result" semantics then rest entirely on GROUP BY yielding no groups.
//
// That is a real behavioural cliff — if it were wrong, an ungrouped variants
// query over an empty window would start answering `0` instead of nothing, on
// exactly the queries a dashboard runs when a service goes quiet. Both halves
// are asserted here against real ClickHouse (chDB) rather than against
// emitted SQL text.

package logql_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

const variantsFusionSeedTable = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    SeverityText LowCardinality(String) DEFAULT '',
    ServiceName String DEFAULT '',
    ResourceAttributes Map(String, String),
    LogAttributes Map(String, String)
) ENGINE = Memory`

// variantsFusionSeedRows inserts three `{app="foo"}` lines inside the
// five-minute instant window, of 2, 3 and 4 bytes.
const variantsFusionSeedRows = `INSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES
    (now64(9) - toIntervalSecond(10), 'ab',   map('app', 'foo')),
    (now64(9) - toIntervalSecond(20), 'cde',  map('app', 'foo')),
    (now64(9) - toIntervalSecond(30), 'fghi', map('app', 'foo'))`

// TestFusedUngroupedVariantsEmptyInput pins both halves of the ungrouped
// fused aggregation: three lines produce one row per arm carrying that arm's
// own reduction, and no lines produce no rows at all.
// The subtests share one chDB process-wide session and one `otel_logs` table
// name, so they run sequentially and each drops the table first — running them
// in parallel lets one case's rows answer the other's query.
func TestFusedUngroupedVariantsEmptyInput(t *testing.T) {
	const query = `variants(sum(count_over_time({app="foo"}[5m])), ` +
		`sum(bytes_over_time({app="foo"}[5m]))) of ({app="foo"}[5m])`

	// count_over_time counts the three lines; bytes_over_time sums their
	// lengths (2 + 3 + 4).
	const (
		wantCount = 3.0
		wantBytes = 9.0
	)

	for _, tc := range []struct {
		name     string
		seedRows bool
		want     map[string]float64
	}{
		{
			name:     "three lines in the window",
			seedRows: true,
			want:     map[string]float64{"0": wantCount, "1": wantBytes},
		},
		{
			// The guard the per-arm plan relied on is inert in the fused
			// plan; GROUP BY must carry the semantics instead.
			name:     "empty window emits no row, not a zero",
			seedRows: false,
			want:     map[string]float64{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runFusedVariantsQuery(t, query, tc.seedRows)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows %v; want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for variant, want := range tc.want {
				if got[variant] != want {
					t.Errorf("__variant__=%q value = %v; want %v", variant, got[variant], want)
				}
			}
		})
	}
}

// runFusedVariantsQuery lowers query, asserts it actually fused (a test that
// silently fell back to the per-arm shape would pass while covering nothing),
// executes it over a freshly seeded chDB session, and returns the value per
// `__variant__` label.
func runFusedVariantsQuery(t *testing.T, query string, seedRows bool) map[string]float64 {
	t.Helper()
	s := schema.DefaultOTelLogs()

	expr, err := logql.ParseExprPermissive(query)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := logql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if n := strings.Count(sqlText, "`otel_logs`"); n != 1 {
		t.Fatalf("query reads the log table %d times; the fused shape reads it once, "+
			"so this test would be covering the per-arm plan instead", n)
	}

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE IF EXISTS otel_logs"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := db.Exec(variantsFusionSeedTable); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	if seedRows {
		if _, err := db.Exec(variantsFusionSeedRows); err != nil {
			t.Fatalf("seed rows: %v", err)
		}
	}

	// The chDB driver cannot scan a Map column into `any`, so read the
	// Attributes map back as text and let the caller pick the label out.
	rows, err := db.Query(
		"SELECT toString(`Attributes`) AS `Attributes`, `Value` FROM ("+sqlText+")", args...,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	out := map[string]float64{}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		variant, value := decodeVariantRow(t, cols, cells)
		out[variant] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// decodeVariantRow pulls the `__variant__` label out of the row's Attributes
// map rendering and the numeric value out of its Value column.
func decodeVariantRow(t *testing.T, cols []string, cells []any) (string, float64) {
	t.Helper()
	var variant string
	var value float64
	for i, name := range cols {
		switch name {
		case "Attributes":
			variant = variantTagFromAttrs(t, cells[i])
		case "Value":
			value = toFloat(t, cells[i])
		}
	}
	if variant == "" {
		t.Fatalf("row carries no __variant__ label: cols=%v cells=%v", cols, cells)
	}
	return variant, value
}

// variantTagFromAttrs reads the `__variant__` label out of whatever the chDB
// driver hands back for the Attributes map column — a typed map when the
// driver decodes it, otherwise its text rendering.
func variantTagFromAttrs(t *testing.T, cell any) string {
	t.Helper()
	switch v := cell.(type) {
	case map[string]string:
		return v[variantLabelName]
	case map[string]any:
		if s, ok := v[variantLabelName].(string); ok {
			return s
		}
		return ""
	default:
		// Text rendering, e.g. `{'__variant__':'1','app':'foo'}`.
		text := fmt.Sprint(v)
		marker := "'" + variantLabelName + "':'"
		i := strings.Index(text, marker)
		if i < 0 {
			return ""
		}
		rest := text[i+len(marker):]
		j := strings.Index(rest, "'")
		if j < 0 {
			return ""
		}
		return rest[:j]
	}
}

// variantLabelName mirrors the package's own variantLabel constant, which is
// unexported and unreachable from this external test package.
const variantLabelName = "__variant__"

// toFloat coerces the driver's numeric cell to float64.
func toFloat(t *testing.T, cell any) float64 {
	t.Helper()
	switch v := cell.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case []byte:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			t.Fatalf("parse value %q: %v", v, err)
		}
		return f
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Fatalf("parse value %q: %v", v, err)
		}
		return f
	default:
		t.Fatalf("unhandled value cell type %T (%v)", cell, cell)
		return 0
	}
}
