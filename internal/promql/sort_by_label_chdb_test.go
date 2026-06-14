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
// a NATURAL-ORDER sort (github.com/facette/natsort) on the named label
// value(s), ascending for `sort_by_label` and descending for
// `sort_by_label_desc`, with later label args acting as tie-breakers.
//
// Natural order — NOT byte/lexicographic — is the parity bar: natsort
// compares digit runs numerically, so `v1 < v2 < v10` (lexicographic
// would give `v1 < v10 < v2`). The `natural_*` cases below carry the
// `v1`/`v2`/`v10` values that diverge between the two orderings; they
// FAIL against a plain `ORDER BY Attributes[label]` emit and pass only
// with the natural-sort key (see promql.naturalSortKeyExpr). The
// alphabetic `a`/`b`/`c` cases sort identically under both orderings, so
// they alone could not catch the lexicographic bug.
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
//
// The `instance` values (`v1`/`v2`/`v10` and `node1`/`node2`/`node10`)
// are the natural-vs-lexicographic discriminators: byte order ranks
// `v10` before `v2`, natural order ranks `v2` before `v10`. They are
// inserted in a scrambled order that matches neither target ordering.
// CREATE OR REPLACE so re-running against the process-shared in-process
// chDB session (every openChDB uses the empty DSN → one global session)
// is idempotent: a sibling fixture (limit_ratio_chdb_test.go) already
// materialises otel_metrics_gauge, and a bare CREATE TABLE trips
// TABLE_ALREADY_EXISTS when the package's chDB tests share the session.
// Mirrors the OR-REPLACE limit_ratio_chdb_test.go already uses.
const sortByLabelSeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge VALUES
    ('http_requests', map('handler', 'b', 'method', 'get'),  toDateTime64('2026-01-01 00:00:00', 9), 30.0),
    ('http_requests', map('handler', 'a', 'method', 'post'), toDateTime64('2026-01-01 00:00:00', 9), 20.0),
    ('http_requests', map('handler', 'a', 'method', 'get'),  toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    ('http_requests', map('handler', 'c', 'method', 'get'),  toDateTime64('2026-01-01 00:00:00', 9), 40.0);
INSERT INTO otel_metrics_gauge VALUES
    ('node_load', map('instance', 'v10', 'rack', 'r2'), toDateTime64('2026-01-01 00:00:00', 9), 100.0),
    ('node_load', map('instance', 'v2',  'rack', 'r2'), toDateTime64('2026-01-01 00:00:00', 9), 20.0),
    ('node_load', map('instance', 'v1',  'rack', 'r2'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    ('node_load', map('instance', 'v2',  'rack', 'r10'), toDateTime64('2026-01-01 00:00:00', 9), 21.0);`

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
		// primary / secondary are the label names projected back out, in
		// the order the sort keys them. secondary == "" means a
		// single-label sort (only primary is read; want holds bare values).
		primary   string
		secondary string
		// want is the reference-Prometheus row order. For a tie-break case
		// (secondary != "") each entry is "<primary>/<secondary>".
		want []string
	}{
		{
			// funcSortByLabel: ascending on handler. Alphabetic values sort
			// the same under natural and byte order — the natural-order
			// cases below are what discriminate the two.
			name:    "asc_single_label",
			query:   `sort_by_label(http_requests, "handler")`,
			primary: "handler",
			want:    []string{"a", "a", "b", "c"},
		},
		{
			// funcSortByLabelDesc: descending on handler.
			name:    "desc_single_label",
			query:   `sort_by_label_desc(http_requests, "handler")`,
			primary: "handler",
			want:    []string{"c", "b", "a", "a"},
		},
		{
			// Tie-breaker: handler ascending, method ascending breaks the
			// two handler='a' rows (get before post).
			name:      "asc_tiebreak",
			query:     `sort_by_label(http_requests, "handler", "method")`,
			primary:   "handler",
			secondary: "method",
			want:      []string{"a/get", "a/post", "b/get", "c/get"},
		},
		{
			// Tie-breaker, descending: handler descending, method
			// descending breaks the handler='a' rows (post before get).
			name:      "desc_tiebreak",
			query:     `sort_by_label_desc(http_requests, "handler", "method")`,
			primary:   "handler",
			secondary: "method",
			want:      []string{"c/get", "b/get", "a/post", "a/get"},
		},
		{
			// NATURAL-ORDER discriminator (ascending). natsort ranks
			// v1 < v2 < v10; a plain byte-order `ORDER BY Attributes[label]`
			// would emit v1, v10, v2 and FAIL here. Three distinct instance
			// values (v1 appears once, v2 twice, v10 once).
			name:    "natural_asc_single_label",
			query:   `sort_by_label(node_load, "instance")`,
			primary: "instance",
			want:    []string{"v1", "v2", "v2", "v10"},
		},
		{
			// NATURAL-ORDER discriminator (descending): v10 > v2 > v1.
			name:    "natural_desc_single_label",
			query:   `sort_by_label_desc(node_load, "instance")`,
			primary: "instance",
			want:    []string{"v10", "v2", "v2", "v1"},
		},
		{
			// NATURAL-ORDER tie-break: instance ascending (v2 < v10), rack
			// ascending breaks the two instance='v2' rows. rack values
			// r2/r10 are themselves natural-order discriminators (r2 < r10),
			// so the tie-break key must ALSO be natural, not byte order.
			name:      "natural_asc_tiebreak",
			query:     `sort_by_label(node_load, "instance", "rack")`,
			primary:   "instance",
			secondary: "rack",
			want:      []string{"v1/r2", "v2/r2", "v2/r10", "v10/r2"},
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
			// projecting `Attributes[<primary>]` / `Attributes[<secondary>]`
			// over the emitted (ORDER BY-bearing) subquery sidesteps the
			// chdb-go parquet Map-scan panic while preserving the row order
			// the inner ORDER BY produced.
			secCol := tc.secondary
			if secCol == "" {
				secCol = tc.primary // harmless duplicate read; m is ignored
			}
			wrapped := "SELECT `Attributes`['" + tc.primary + "'] AS h, `Attributes`['" + secCol + "'] AS m FROM (" + sqlStr + ")"
			rows, err := db.Query(wrapped, args...)
			if err != nil {
				t.Fatalf("query: %v\nSQL: %s", err, wrapped)
			}
			defer func() { _ = rows.Close() }()

			var got []string
			tieBreak := tc.secondary != ""
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
