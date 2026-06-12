//go:build chdb

// Deliverable 1 (task #70): quantify the metrics-table ORDER BY decision.
//
// Production renders the OTel-CH metrics tables with
//
//	ORDER BY (ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix))
//
// (ServiceName FIRST — see internal/chsql/tableshape.go:136). A
// metric-name-first PromQL instant query with NO service.name matcher —
// the common Grafana / Drilldown-Metrics case — cannot PK-range-prune on
// the leading ServiceName key, so ClickHouse falls back to a generic
// exclusion search that touches granules from every ServiceName block.
//
// This bench seeds two parallel MergeTree tables with byte-identical data
// and contrasting sort keys:
//
//	svcfirst : (ServiceName, MetricName, Attributes, ts)  — production
//	metric   : (MetricName, Attributes, ServiceName, ts)  — proposed fork patch
//
// then runs EXPLAIN indexes=1 (parts/granules pruned) + wall-clock timing
// for two query shapes:
//
//	metric-only : WHERE MetricName = ?                    (no service filter)
//	svc+metric  : WHERE ServiceName = ? AND MetricName = ?
//
// The numbers feed the RC1 accept-OTel-default vs patch-cerberus-ddl-fork
// decision. Build-tagged `chdb`, same lane as the rest of the chDB execs.
package perf

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver
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

	tables := []struct {
		name    string
		orderBy string
		label   string
	}{
		{"m_svcfirst", "(ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix))", "ServiceName-first (PRODUCTION)"},
		{"m_metricfirst", "(MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))", "MetricName-first (PROPOSED)"},
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

	t.Logf("%-12s | %-32s | %-12s | %-16s | %-10s | %s",
		"shape", "ORDER BY", "PK keys", "parts", "granules", "best wall")
	t.Log("-------------+----------------------------------+--------------+------------------+------------+----------")
	for _, r := range results {
		t.Logf("%-12s | %-32s | %-12s | %-16s | %-10s | %v",
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
	// The landed cerberus-ddl fork patch leads the metrics ORDER BY with
	// MetricName so the common metric-name-first query (NO service.name
	// matcher — the Grafana / Drilldown-Metrics default) binary-searches
	// the PK instead of falling to a generic-exclusion scan that touches
	// granules from every ServiceName block. The measured win on this grid
	// is 8–17× fewer granules read on the metric-only shape.
	//
	// We assert a generous ratio FLOOR (≥4×), not an absolute granule
	// count: the absolute number is index_granularity-dependent and would
	// drift if CH changed the default or the grid scaled, but the *ratio*
	// between the two sort keys on byte-identical data is the structural
	// property the fork patch buys. The fixed-grid OPTIMIZE … FINAL
	// single-part setup above makes both granule counts deterministic. A
	// regression that re-led the sort key with ServiceName (the OTel
	// upstream default) collapses the ratio toward 1× and trips this floor.
	svcFirstGranules := selectedGranulesFor(t, results, "metric-only", "ServiceName-first (PRODUCTION)")
	metricFirstGranules := selectedGranulesFor(t, results, "metric-only", "MetricName-first (PROPOSED)")

	t.Logf("metric-only granule prune: svcfirst=%d  metricfirst=%d  ratio=%.1fx",
		svcFirstGranules, metricFirstGranules, float64(svcFirstGranules)/float64(maxInt1(metricFirstGranules)))

	if metricFirstGranules <= 0 {
		t.Fatalf("metric-first metric-only query read %d granules — EXPLAIN parse is "+
			"degenerate (expected ≥1 selected granule); cannot evaluate the prune ratio",
			metricFirstGranules)
	}
	const minRatio = 4 // generous floor vs the measured 8–17×
	if metricFirstGranules*minRatio > svcFirstGranules {
		t.Fatalf("metrics ORDER BY granule-prune regression: the metric-name-first key "+
			"read %d granules and the service-name-first key read %d — only %.1f× fewer, "+
			"below the %d× floor. The cerberus-ddl fork's MetricName-first ORDER BY win "+
			"(measured 8–17× on this grid) has regressed; the leading sort key is no "+
			"longer letting a no-service.name query PK-range-prune.",
			metricFirstGranules, svcFirstGranules,
			float64(svcFirstGranules)/float64(metricFirstGranules), minRatio)
	}
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

// --- tiny string helpers (avoid importing strings for two calls) ---

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
