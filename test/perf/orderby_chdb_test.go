//go:build chdb

// Metrics-table ORDER BY: the standing execution proof that leading the
// metrics sort key with MetricName still buys the granule-prune win (#791).
//
// Cerberus does not ship the stock OTel-CH metrics layout. The
// tsouza/opentelemetry-collector-contrib:cerberus-ddl fork carries one
// deliberate divergence (docs/upstream-forks.md): the five metrics tables lead
// their sort key with MetricName, where upstream leads with ServiceName. The
// reason is granule pruning. A metric-name-first PromQL instant query with NO
// service.name matcher — the common Grafana / Drilldown-Metrics case — cannot
// PK-range-prune against a leading ServiceName key, so ClickHouse falls back to
// a generic exclusion search that touches granules from every ServiceName
// block.
//
// That divergence is a permanent maintenance cost, so the win it buys has to
// stay MEASURED rather than remembered. Three layers pin different halves of
// it, and only this one executes ClickHouse:
//
//   - internal/schema/ddl renders the CREATE TABLE from the fork's own
//     templates — the layout production is actually created with.
//   - internal/chsql/tableshape_orderby_test.go:`TestMetricsTableShapeLeadsWithMetricName`
//     pins cerberus's granule-pruning MODEL of that key. Always-on, pure
//     function, no libchdb.so.
//   - this harness runs both keys over byte-identical data and measures the
//     pruning difference the other two layers only assert.
//
// So the question it answers is live, not settled: ClickHouse index behaviour
// is what makes the leading column matter, and if a future version pruned a
// ServiceName-first key just as well, the fork patch would be paying
// maintenance for nothing. The ratio this harness reports is the evidence that
// keeps the divergence justified.
//
// The production sort key is READ OUT of the rendered DDL by
// `productionMetricsSortKey` rather than written down here, so this file cannot
// come to disagree with the schema it claims to measure. The comparison key is
// that same key with ServiceName hoisted to the front — the stock upstream
// layout — built by `serviceNameFirst`. If the fork patch were ever dropped the
// two keys would coincide, and `TestOrderByDecision_ChDB` fails on that rather
// than quietly measuring one table against itself.
//
// Two parallel MergeTree tables are seeded with byte-identical data under the
// two keys, then EXPLAIN indexes=1 (parts / granules pruned) and wall-clock
// timing run over two query shapes:
//
//	metric-only : WHERE MetricName = ?                    (no service filter)
//	svc+metric  : WHERE ServiceName = ? AND MetricName = ?
//
// Build-tagged `chdb`, same lane as the rest of the chDB execs.
package perf

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// Data shape — representative of a mid-size OTel deployment:
//
//	nServices  distinct ServiceNames
//	nMetrics   distinct MetricNames, EACH present under EVERY service
//	nAttr      attribute-key cardinality per (service,metric)
//	nTime      timestamps per series
//
// Total rows = nServices * nMetrics * nAttr * nTime.
const (
	nServices = 25
	nMetrics  = 40
	nAttr     = 20
	nTime     = 30
	// → 25 * 40 * 20 * 30 = 600,000 rows. (Scale up for sharper ratios.)
)

// makeInsert builds an INSERT … SELECT FROM numbers() that materialises the
// full grid. The column derivations are arranged so that, regardless of the
// destination sort key, the *same* logical rows land in the table — the only
// difference between the two tables is the ORDER BY ClickHouse applies.
//
//	svc index   = (n / (nMetrics*nAttr*nTime)) % nServices
//	metric idx  = (n / (nAttr*nTime))          % nMetrics
//	attr idx    = (n / nTime)                  % nAttr
//	time idx    =  n                           % nTime
func makeInsert(table string, total int) string {
	return fmt.Sprintf(
		`INSERT INTO %s
SELECT
    concat('service.', leftPad(toString(intDiv(number, %d) %% %d), 3, '0')) AS ServiceName,
    concat('metric_', leftPad(toString(intDiv(number, %d) %% %d), 3, '0')) AS MetricName,
    map('host', concat('h', toString(intDiv(number, %d) %% %d))) AS Attributes,
    toDateTime64('2026-05-11 12:00:00', 9) + INTERVAL (number %% %d) SECOND AS TimeUnix,
    toFloat64(number) AS Value
FROM numbers(%d)`,
		table,
		nMetrics*nAttr*nTime, nServices, // ServiceName
		nAttr*nTime, nMetrics, // MetricName
		nTime, nAttr, // Attributes host
		nTime, // TimeUnix
		total, // numbers(total)
	)
}

func ddlFor(table, orderBy string) string {
	return fmt.Sprintf(`CREATE OR REPLACE TABLE %s (
    ServiceName String,
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY %s SETTINGS index_granularity = 8192;`, table, orderBy)
}

type explainStats struct {
	keys     string
	cond     string
	parts    string
	granules string
}

// explainRow pairs a query shape + sort-key variant with its parsed
// EXPLAIN stats and best wall time. Hoisted to package scope (from a
// local type inside TestOrderByDecision_ChDB) so the granule-prune
// assertion helpers can range over the collected results.
type explainRow struct {
	shape, variant string
	st             explainStats
	wall           time.Duration
}

func runExplain(t *testing.T, db *sql.DB, query string) explainStats {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1 " + query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var st explainStats
	var capKeys bool
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		trim := trimSpace(line)
		switch {
		case trim == "Keys:":
			capKeys = true
		case capKeys && st.keys == "":
			st.keys = trim
			capKeys = false
		case hasPrefix(trim, "Condition:"):
			st.cond = trim
		case hasPrefix(trim, "Parts:"):
			st.parts = trim
		case hasPrefix(trim, "Granules:"):
			st.granules = trim
		}
	}
	return st
}

// timeQuery runs `query` `iters` times and returns the best (min) wall time —
// min is the most stable estimate of the floor cost under chDB's noisy
// single-process engine.
func timeQuery(t *testing.T, db *sql.DB, query string, iters int) time.Duration {
	t.Helper()
	best := time.Hour
	for i := 0; i < iters; i++ {
		start := time.Now()
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var n int
		for rows.Next() {
			n++
		}
		rows.Close()
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

func TestOrderByDecision_ChDB(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	total := nServices * nMetrics * nAttr * nTime

	m := schema.DefaultOTelMetrics()
	prodKey := productionMetricsSortKey(t)
	upstreamKey := serviceNameFirst(t, prodKey, m.ServiceNameColumn)

	// The whole comparison is meaningful only while cerberus actually diverges
	// from upstream. If the rendered DDL ever leads with ServiceName, the two
	// keys below are the same key and the ratio assertion would compare a table
	// against itself and pass on 1x. Fail on that here instead.
	if prodKey[0] == m.ServiceNameColumn {
		t.Fatalf("the rendered metrics DDL leads its sort key with %q (full key %v): "+
			"the cerberus-ddl fork's MetricName-first patch is absent from the DDL "+
			"this build renders, so the granule-prune win this harness exists to "+
			"keep measured has been lost at the schema layer",
			m.ServiceNameColumn, prodKey)
	}

	tables := []struct {
		name    string
		orderBy string
		label   string
	}{
		{"m_production", sortKeyClause(prodKey), productionKeyLabel},
		{"m_svcfirst", sortKeyClause(upstreamKey), upstreamKeyLabel},
	}

	for _, tb := range tables {
		if _, err := db.Exec(ddlFor(tb.name, tb.orderBy)); err != nil {
			t.Fatalf("ddl %s: %v", tb.name, err)
		}
		if _, err := db.Exec(makeInsert(tb.name, total)); err != nil {
			t.Fatalf("insert %s: %v", tb.name, err)
		}
		// Force a single part so granule counts are comparable and not
		// inflated by background-merge timing.
		if _, err := db.Exec("OPTIMIZE TABLE " + tb.name + " FINAL"); err != nil {
			t.Fatalf("optimize %s: %v", tb.name, err)
		}
	}

	var totalGranules int64
	_ = db.QueryRow(`SELECT count() FROM m_svcfirst`).Scan(new(int64))
	db.QueryRow(`SELECT sum(marks) FROM system.parts WHERE table='m_svcfirst' AND active`).Scan(&totalGranules)

	// Query shapes. The metric-only shape pins NO ServiceName (the common
	// Grafana case). svc+metric pins both. We aggregate (sum+count) so the
	// result set is a single row — chDB-go's parquet driver panics draining
	// the ~15k-row raw projection — while the WHERE/PK-prune work (the thing
	// being measured) is identical to the raw SELECT. EXPLAIN below uses the
	// same predicate, so granule/part counts reflect the real scan.
	metricOnly := "SELECT sum(Value), count() FROM %s WHERE MetricName = 'metric_020'"
	svcMetric := "SELECT sum(Value), count() FROM %s WHERE ServiceName = 'service.012' AND MetricName = 'metric_020'"

	const iters = 7

	t.Logf("=== ORDER BY decision: %d rows (%d svc x %d metrics x %d attr x %d ts), total marks=%d ===",
		total, nServices, nMetrics, nAttr, nTime, totalGranules)

	var results []explainRow
	for _, tb := range tables {
		for _, q := range []struct {
			shape string
			tmpl  string
		}{
			{"metric-only", metricOnly},
			{"svc+metric", svcMetric},
		} {
			query := fmt.Sprintf(q.tmpl, tb.name)
			st := runExplain(t, db, query)
			wall := timeQuery(t, db, query, iters)
			results = append(results, explainRow{q.shape, tb.label, st, wall})
		}
	}

	t.Logf("%-12s | %-38s | %-12s | %-16s | %-10s | %s",
		"shape", "sort key", "PK keys", "parts", "granules", "best wall")
	t.Log("-------------+----------------------------------------+--------------+------------------+------------+----------")
	for _, r := range results {
		t.Logf("%-12s | %-38s | %-12s | %-16s | %-10s | %v",
			r.shape, r.variant, r.st.keys,
			stripPrefix(r.st.parts, "Parts: "),
			stripPrefix(r.st.granules, "Granules: "),
			r.wall.Round(time.Microsecond))
	}

	// Emit raw EXPLAIN blocks for the metric-only shape under both keys so
	// the report can quote the exact ClickHouse plan + condition.
	for _, tb := range tables {
		t.Logf("--- EXPLAIN indexes=1  metric-only  [%s] ---", tb.label)
		rows, _ := db.Query("EXPLAIN indexes=1 " + fmt.Sprintf(metricOnly, tb.name))
		for rows.Next() {
			var s string
			rows.Scan(&s)
			t.Log("    " + s)
		}
		rows.Close()
	}

	// --- ASSERTION: granule-prune ratio floor (guards #791) ---------------
	//
	// On the metric-only shape (NO service.name matcher — the Grafana /
	// Drilldown-Metrics default) the production key binary-searches the PK,
	// while the stock upstream key falls to a generic-exclusion scan that
	// touches granules from every ServiceName block.
	//
	// The floor is a RATIO between the two keys, not an absolute granule
	// count: the absolute number is index_granularity-dependent and would
	// drift if ClickHouse changed the default or the grid scaled, but the
	// ratio between two sort keys over byte-identical data is the structural
	// property the fork patch buys. The fixed-grid OPTIMIZE … FINAL
	// single-part setup above makes both counts deterministic.
	//
	// Because the production key is derived from the rendered DDL, this is a
	// real regression guard rather than a statement about a hard-coded key:
	// drop the fork patch and the leading-column check above fails, and change
	// ClickHouse's index behaviour so the leading column stops mattering and
	// the ratio collapses into this floor.
	prodGranules := selectedGranulesFor(t, results, "metric-only", productionKeyLabel)
	upstreamGranules := selectedGranulesFor(t, results, "metric-only", upstreamKeyLabel)

	t.Logf("metric-only granule prune: production=%d  upstream-default=%d  ratio=%.1fx",
		prodGranules, upstreamGranules, float64(upstreamGranules)/float64(maxInt1(prodGranules)))

	if prodGranules <= 0 {
		t.Fatalf("the production-key metric-only query read %d granules — the EXPLAIN "+
			"parse is degenerate (expected ≥1 selected granule); the prune ratio "+
			"cannot be evaluated", prodGranules)
	}
	// A floor well under the ratio this grid actually produces: the assertion
	// is meant to catch the win COLLAPSING, not to pin a measurement that
	// legitimately moves with ClickHouse's index implementation.
	const minRatio = 4
	if prodGranules*minRatio > upstreamGranules {
		t.Fatalf("metrics ORDER BY granule-prune regression: the production key %v "+
			"read %d granules and the stock upstream key %v read %d — only %.1f× "+
			"fewer, below the %d× floor. Leading the metrics sort key with %q has "+
			"stopped letting a no-service.name query PK-range-prune, which is the "+
			"entire justification for the cerberus-ddl fork's divergence.",
			prodKey, prodGranules, upstreamKey, upstreamGranules,
			float64(upstreamGranules)/float64(prodGranules), minRatio, prodKey[0])
	}
}

// The two sort-key variants under test, named once so the label a result row
// carries and the label the assertions look it up by cannot drift apart.
const (
	productionKeyLabel = "production (from the rendered DDL)"
	upstreamKeyLabel   = "stock OTel default (ServiceName-first)"
)

// orderByPrefix opens the ORDER BY line of a rendered CREATE TABLE statement.
const orderByPrefix = "ORDER BY "

// productionMetricsSortKey returns the ORDER BY elements the OTel-CH metrics
// tables are actually created with, read out of the DDL internal/schema/ddl
// renders from the cerberus-ddl fork's own templates.
//
// Reading the key rather than restating it is the point. A hand-written copy is
// a claim that can quietly stop describing production — and when it does, the
// harness goes on measuring a layout nobody runs while still reporting a
// healthy ratio.
func productionMetricsSortKey(t *testing.T) []string {
	t.Helper()

	m := schema.DefaultOTelMetrics()
	stmts, err := ddl.RenderAll(ddl.Config{}, []ddl.Signal{ddl.Metrics})
	if err != nil {
		t.Fatalf("render the metrics DDL: %v", err)
	}
	for _, stmt := range stmts {
		if !strings.Contains(stmt, m.GaugeTable) {
			continue
		}
		for _, line := range strings.Split(stmt, "\n") {
			line = trimSpace(line)
			if !hasPrefix(line, orderByPrefix) {
				continue
			}
			return splitSortKey(t, stripPrefix(line, orderByPrefix))
		}
	}
	t.Fatalf("no %s line in the rendered DDL for %q — the metrics table template "+
		"changed shape and the production sort key can no longer be read from it",
		orderByPrefix, m.GaugeTable)
	return nil
}

// splitSortKey turns the `(a, b, f(c))` expression list of an ORDER BY clause
// into its top-level elements. A comma inside a function call is not a
// separator, so the scan tracks parenthesis depth.
func splitSortKey(t *testing.T, clause string) []string {
	t.Helper()

	clause = trimSpace(clause)
	if !hasPrefix(clause, "(") || !strings.HasSuffix(clause, ")") {
		t.Fatalf("ORDER BY clause %q is not a parenthesised expression list", clause)
	}
	inner := clause[1 : len(clause)-1]

	var out []string
	depth, start := 0, 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, trimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, trimSpace(inner[start:]))

	for _, c := range out {
		if c == "" {
			t.Fatalf("ORDER BY clause %q has an empty element: %v", clause, out)
		}
	}
	return out
}

// serviceNameFirst returns key with its ServiceName element hoisted to the
// front — the stock upstream metrics layout, expressed as a PERMUTATION of
// whatever cerberus actually ships rather than as a second hand-written key.
// Deriving the comparison this way keeps the two tables byte-comparable: they
// differ in the position of one column and in nothing else.
func serviceNameFirst(t *testing.T, key []string, serviceNameCol string) []string {
	t.Helper()

	idx := -1
	for i, c := range key {
		if c == serviceNameCol {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("the metrics sort key %v has no %q element, so the stock upstream "+
			"comparison key cannot be built from it", key, serviceNameCol)
	}

	out := make([]string, 0, len(key))
	out = append(out, key[idx])
	out = append(out, key[:idx]...)
	out = append(out, key[idx+1:]...)
	return out
}

// sortKeyClause renders sort-key elements back into the parenthesised
// expression list a CREATE TABLE ORDER BY takes.
func sortKeyClause(key []string) string {
	return "(" + strings.Join(key, ", ") + ")"
}

// selectedGranulesFor pulls the count of SELECTED granules (the
// numerator of ClickHouse's `Granules: <selected>/<total>` EXPLAIN line)
// for the result row matching the given shape + variant label. The
// selected count is the granules ClickHouse actually reads after PK
// pruning — the quantity the ORDER BY win reduces.
func selectedGranulesFor(t *testing.T, results []explainRow, shape, variant string) int {
	t.Helper()
	for _, r := range results {
		if r.shape == shape && r.variant == variant {
			g := parseSelectedGranules(r.st.granules)
			if g < 0 {
				t.Fatalf("could not parse selected granules from %q (shape=%s variant=%s)",
					r.st.granules, shape, variant)
			}
			return g
		}
	}
	t.Fatalf("no result row for shape=%s variant=%s", shape, variant)
	return -1
}

// parseSelectedGranules extracts the leading integer from a
// `Granules: <selected>/<total>` EXPLAIN line. Returns -1 on a shape it
// can't parse.
func parseSelectedGranules(s string) int {
	s = stripPrefix(trimSpace(s), "Granules: ")
	// s is now "<selected>/<total>" (or just "<selected>" defensively).
	n := 0
	seen := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		seen = true
	}
	if !seen {
		return -1
	}
	return n
}

func maxInt1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// --- EXPLAIN-line string helpers, shared across package perf ---
//
// Kept as package-local helpers rather than folded into `strings` calls at
// every site: the chDB EXPLAIN harnesses in this package all trim and
// prefix-match the same way, and several of them predate any `strings` import.

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func stripPrefix(s, p string) string {
	if hasPrefix(s, p) {
		return s[len(p):]
	}
	return s
}
