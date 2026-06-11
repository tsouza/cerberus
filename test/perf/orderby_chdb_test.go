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
	return fmt.Sprintf(`INSERT INTO %s
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

	type row struct {
		shape, variant string
		st             explainStats
		wall           time.Duration
	}
	var results []row
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
			results = append(results, row{q.shape, tb.label, st, wall})
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
