//go:build chdb

// chDB-backed ORDER-SENSITIVE parity pin for PromQL `sort_by_label` /
// `sort_by_label_desc`.
//
// The TXTAR round-trip harness (test/spec/runner_chdb.go) canonicalises
// both sides through sortRows before comparison — it asserts SET
// equality, not row order — so it cannot pin the one thing these
// functions exist to produce: a stable ROW ORDER keyed on label values.
// This test closes that gap. It lowers each function through the real
// promql.Lower → chsql.Emit pipeline, executes the emitted SQL against
// an ephemeral chDB session seeded with a Map-typed Attributes column,
// and asserts the resulting handler/method label order matches what
// reference Prometheus's funcSortByLabel / funcSortByLabelDesc produce:
// a lexicographic sort on the named label value(s), ascending for
// `sort_by_label` and descending for `sort_by_label_desc`, with later
// label args acting as tie-breakers.
//
// Gated by `//go:build chdb` so the default `check` lane (CGO off, no
// libchdb.so) skips it; the dedicated `chdb` workflow runs it.
package promql_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// sortByLabelSeed creates and populates an Attributes Map table with a
// deliberately scrambled insert order so a passing assertion can only
// come from the emitted ORDER BY — not from the storage order. The
// `handler` values exercise the primary key; `method` breaks the
// `handler='a'` tie.
const sortByLabelSeed = `
CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge VALUES
    ('http_requests', map('handler', 'b', 'method', 'get'),  toDateTime64('2026-01-01 00:00:00', 9), 30.0),
    ('http_requests', map('handler', 'a', 'method', 'post'), toDateTime64('2026-01-01 00:00:00', 9), 20.0),
    ('http_requests', map('handler', 'a', 'method', 'get'),  toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    ('http_requests', map('handler', 'c', 'method', 'get'),  toDateTime64('2026-01-01 00:00:00', 9), 40.0);`

func TestSortByLabel_OrderParityVsPrometheus(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	for _, stmt := range strings.Split(strings.TrimSpace(sortByLabelSeed), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	instantEval := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		// want is the reference-Prometheus row order, expressed as the
		// "<handler>/<method>" label-value pairs the sort should yield.
		want []string
	}{
		{
			// funcSortByLabel: ascending lexicographic on handler.
			name:  "asc_single_label",
			query: `sort_by_label(http_requests, "handler")`,
			want:  []string{"a", "a", "b", "c"},
		},
		{
			// funcSortByLabelDesc: descending lexicographic on handler.
			name:  "desc_single_label",
			query: `sort_by_label_desc(http_requests, "handler")`,
			want:  []string{"c", "b", "a", "a"},
		},
		{
			// Tie-breaker: handler ascending, method ascending breaks the
			// two handler='a' rows (get before post).
			name:  "asc_tiebreak",
			query: `sort_by_label(http_requests, "handler", "method")`,
			want:  []string{"a/get", "a/post", "b/get", "c/get"},
		},
		{
			// Tie-breaker, descending: handler descending, method
			// descending breaks the handler='a' rows (post before get).
			name:  "desc_tiebreak",
			query: `sort_by_label_desc(http_requests, "handler", "method")`,
			want:  []string{"c/get", "b/get", "a/post", "a/get"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, instantEval, instantEval)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			// Read the sort-key label values out as plain String columns —
			// projecting `Attributes['handler']` / `Attributes['method']`
			// over the emitted (ORDER BY-bearing) subquery sidesteps the
			// chdb-go parquet Map-scan panic while preserving the row order
			// the inner ORDER BY produced.
			wrapped := "SELECT `Attributes`['handler'] AS h, `Attributes`['method'] AS m FROM (" + sqlStr + ")"
			rows, err := db.Query(wrapped, args...)
			if err != nil {
				t.Fatalf("query: %v\nSQL: %s", err, wrapped)
			}
			defer func() { _ = rows.Close() }()

			var got []string
			tieBreak := strings.Contains(tc.want[0], "/")
			for rows.Next() {
				var h, m string
				if err := rows.Scan(&h, &m); err != nil {
					t.Fatalf("scan: %v", err)
				}
				if tieBreak {
					got = append(got, h+"/"+m)
				} else {
					got = append(got, h)
				}
			}
			if err := rows.Err(); err != nil && !strings.Contains(err.Error(), "empty row") {
				t.Fatalf("rows.Err: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%s: got %d rows %v, want %d %v", tc.query, len(got), got, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("%s: row order mismatch at %d: got %v, want %v", tc.query, i, got, tc.want)
					break
				}
			}
		})
	}
}
