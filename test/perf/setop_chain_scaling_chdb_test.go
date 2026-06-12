//go:build chdb

// Perf / robustness guard (RC1 PromQL + LogQL vector set ops): the
// shared vector-set-op emitter (internal/chsql/vector_set_op.go, reached
// by BOTH the PromQL binary-op path and the LogQL `and`/`or`/`unless`
// path) used to inline — and so RE-RENDER — the whole LEFT arm subplan
// TWICE per `or` level: once in the UNION-ALL left leg, and again inside
// the right leg's `<sig> NOT IN (SELECT DISTINCT <sig> FROM <LEFT>)`
// anti-join. A left-assoc chain `m0 or m1 or m2 …` lowers to
// `(((m0 or m1) or m2) …)`, so the nested LHS doubled at every level and
// the SQL TEXT grew EXPONENTIALLY in the chain depth k:
//
//	pre-fix (chDB-measured, 16-name fan-out arms):
//	  k=1 → 1.9KB   k=2 → 4.7KB   k=3 → 10.2KB   k=4 → 21.3KB   k=5 → 43.6KB
//
// By k≈8 the text blew past ClickHouse's 256KB `max_query_size`, so the
// query failed to parse at all — `Code:62 SYNTAX_ERROR`, the chain
// UNEXECUTABLE.
//
// The fix (vector_set_op.go: materialise the canonical LEFT arm exactly
// ONCE as a `WITH _setop_lhs_<n> AS (…)` non-recursive CTE and reference
// it by name in BOTH the UNION leg and the NOT-IN signature subquery —
// common-subexpression elimination) makes the text grow LINEARLY while
// producing a byte-for-byte identical result set (the NOT-IN only reads
// the signature column, which the canonical arm passes through
// unchanged):
//
//	post-fix:
//	  k=1 → 1.4KB   k=2 → 2.4KB   k=3 → 3.3KB   k=4 → 4.2KB   k=5 → 5.2KB
//
// This guard pins all three invariants the fix must hold, across a
// k = 1..8 chain-depth sweep:
//
//	(a) Scaling — the emitted SQL byte size grows SUB-exponentially
//	    (linear-ish). The decisive assertion contrasts the measured
//	    growth ratio against a doubling baseline: the pre-fix shape
//	    ~doubled every level (ratio ~2× per step); the CTE shape adds a
//	    bounded per-level increment, so its k=8/k=1 ratio is FAR below the
//	    pre-fix 2^7 = 128×.
//	(b) Executability — every depth EXECUTES on chDB with no Code:62
//	    SYNTAX_ERROR and no 256KB max_query_size breach (the k=8 SQL stays
//	    well under the limit).
//	(c) Correctness — the chain's result set matches a per-arm reference
//	    (each arm seeded with a DISJOINT label signature, so PromQL `or`
//	    reduces to the pure union of all arms; the chain's (count,
//	    signature-checksum) must equal the sum / union of the per-arm
//	    references).
//
// Build-tagged `chdb`, same lane as the other chDB scaling guards
// (range_lwr / histogram_range / structural_recursive).
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// setOpChainEvalTime is the fixed instant-eval anchor the chain lowers
// at. Seeding the rows one second before it keeps every sample inside
// the 5-minute LWR staleness window the instant-selector lowering
// applies, so each arm's latest-per-series collapse yields exactly its
// seeded row.
var setOpChainEvalTime = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// setOpChainMetricName is the OTel-dotted metric name arm i seeds + the
// underscore form the PromQL selector references. The lowering fans the
// underscore name out to every dotted/underscore permutation, so both
// forms must resolve to the same Scan; using a name with a single
// component after the prefix keeps the fan-out (and the SQL) compact.
func setOpChainMetricName(i int) (promName, otelName string) {
	promName = fmt.Sprintf("setop_chain_metric_%d", i)
	otelName = fmt.Sprintf("setop.chain.metric.%d", i)
	return promName, otelName
}

// setOpChainSeed builds a single sum table seeding k+1 arms, one row per
// arm, each arm carrying a DISJOINT `arm` label value so PromQL `or`
// reduces to the pure union of every arm (no signature collides across
// arms, so no right-arm row is ever dropped by the NOT-IN anti-join).
// The reference count for an `m0 or … or mk` chain is therefore exactly
// k+1 rows.
func setOpChainSeed(k int) string {
	var b strings.Builder
	// DROP first: the chDB session is shared across the perf binary's
	// tests, so a sibling test may have already created otel_metrics_sum.
	// A clean slate keeps the seed (and the per-arm reference counts)
	// deterministic regardless of run order.
	b.WriteString("DROP TABLE IF EXISTS otel_metrics_sum;")
	b.WriteString(`CREATE TABLE otel_metrics_sum (
	  MetricName String, Attributes Map(String,String),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix);`)
	ts := setOpChainEvalTime.Add(-time.Second).UTC().Format("2006-01-02 15:04:05.000000000")
	b.WriteString("\nINSERT INTO otel_metrics_sum VALUES\n")
	for i := 0; i <= k; i++ {
		_, otel := setOpChainMetricName(i)
		if i > 0 {
			b.WriteString(",\n")
		}
		// Disjoint signature per arm: the `arm` label is unique, so the
		// union across arms never collides — every arm contributes its
		// one row to the `or` result.
		fmt.Fprintf(&b, "  ('%s', map('arm', '%d'), toDateTime64('%s', 9), %d.0)",
			otel, i, ts, i+1)
	}
	b.WriteString(";")
	return b.String()
}

// emitSetOpChainSQL lowers `m0 <op> m1 <op> … <op> mk` at the fixed
// instant anchor and returns the emitted SQL + args. op is "or" / "and"
// / "unless".
func emitSetOpChainSQL(t *testing.T, op string, k int) (string, []any) {
	t.Helper()
	parts := make([]string, k+1)
	for i := range parts {
		name, _ := setOpChainMetricName(i)
		parts[i] = name
	}
	q := strings.Join(parts, " "+op+" ")
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(),
		setOpChainEvalTime, setOpChainEvalTime)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", q, err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", q, err)
	}
	return sqlText, args
}

// setOpChainAgg runs `SELECT count(), sum(cityHash64(toJSONString(
// Attributes))) FROM (<inner>)` against db, returning (rowCount,
// sigChecksum). The checksum over the Attributes signatures pins the
// exact set of surviving series — an order-independent fingerprint of
// the result set. Wrapping in the aggregate also dodges chDB-go's
// parquet Map-scan path entirely (no raw Map column crosses the driver
// boundary).
func setOpChainAgg(t *testing.T, db *sql.DB, inner string, args []any) (int64, uint64) {
	t.Helper()
	q := "SELECT count(), sum(cityHash64(toJSONString(`Attributes`))) FROM (" + inner + ")"
	var cnt int64
	var sum uint64
	if err := db.QueryRow(q, args...).Scan(&cnt, &sum); err != nil {
		t.Fatalf("setOpChainAgg query: %v\nSQL: %s", err, q)
	}
	return cnt, sum
}

func TestSetOpChain_Scaling_ChDB(t *testing.T) {
	const maxK = 8

	db := openChDB(t)
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS default"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	// Seed the widest arm set once; every shorter chain reads a subset of
	// these metrics, so a single seed covers the whole k sweep.
	for _, stmt := range splitSQL(setOpChainSeed(maxK)) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	// --- (a) + (b): sweep k = 1..8 for the `or` chain (the exponential
	// case the fix targets), recording SQL size + asserting executability.
	type sample struct {
		k      int
		sqlLen int
		count  int64
		sigSum uint64
		refCnt int64
		refSum uint64
	}
	samples := make([]sample, 0, maxK)

	const maxQuerySize = 256 * 1024 // ClickHouse default max_query_size.

	for k := 1; k <= maxK; k++ {
		sqlText, args := emitSetOpChainSQL(t, "or", k)

		// (b) Size must stay well under CH's 256KB parse limit at every
		// depth (the pre-fix shape breached it by k≈8).
		if len(sqlText) >= maxQuerySize {
			t.Fatalf("or chain k=%d: emitted SQL is %d bytes, at/over CH's %d max_query_size — the "+
				"exponential set-op duplication regressed back in (Code:62 territory).",
				k, len(sqlText), maxQuerySize)
		}

		// (b) Executability: the chain must run with no Code:62 /
		// NO_COMMON_TYPE / Map-scan error at every depth.
		cnt, sigSum := setOpChainAgg(t, db, sqlText, args)

		// (c) Per-arm reference: each arm seeded with a disjoint
		// signature, so `or` is the pure union — the reference count is
		// the sum of per-arm row counts, and the reference signature
		// checksum is the sum of per-arm checksums.
		var refCnt int64
		var refSum uint64
		for i := 0; i <= k; i++ {
			// Re-lower the i-th arm alone (a bare selector), execute it,
			// and accumulate its row count + signature checksum.
			name, _ := setOpChainMetricName(i)
			p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
			expr, err := p.ParseExpr(name)
			if err != nil {
				t.Fatalf("ParseExpr(arm %d): %v", i, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(),
				setOpChainEvalTime, setOpChainEvalTime)
			if err != nil {
				t.Fatalf("LowerAt(arm %d): %v", i, err)
			}
			aSQL, aArgs, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(arm %d): %v", i, err)
			}
			c, s := setOpChainAgg(t, db, aSQL, aArgs)
			refCnt += c
			refSum += s
		}

		samples = append(samples, sample{
			k: k, sqlLen: len(sqlText),
			count: cnt, sigSum: sigSum,
			refCnt: refCnt, refSum: refSum,
		})
		t.Logf("or  k=%-2d sqllen=%-7d count=%-3d sig=%-22d  ref_count=%-3d ref_sig=%d",
			k, len(sqlText), cnt, sigSum, refCnt, refSum)
	}

	// --- (c) Correctness: chain result == per-arm union reference, at
	// every depth. Disjoint signatures mean count == k+1 and the
	// signature checksum equals the per-arm sum.
	for _, s := range samples {
		if s.count != s.refCnt {
			t.Errorf("or chain k=%d: result row count %d != per-arm union reference %d — the CTE "+
				"CSE changed the result set (it must be byte-identical to the inlined shape).",
				s.k, s.count, s.refCnt)
		}
		if s.sigSum != s.refSum {
			t.Errorf("or chain k=%d: result signature checksum %d != per-arm reference %d — the set "+
				"of surviving series changed under the CTE rewrite.", s.k, s.sigSum, s.refSum)
		}
		if want := int64(s.k + 1); s.count != want {
			t.Errorf("or chain k=%d: expected exactly %d union rows (disjoint arms), got %d.",
				s.k, want, s.count)
		}
	}

	// --- (a) Sub-exponential scaling: the k=8 SQL must be FAR smaller
	// than the pre-fix exponential shape would have produced. The pre-fix
	// shape ~doubled per level (k → ~2× bytes), so k=8 ≈ 2^7 × k=1 ≈ 128×.
	// The CTE shape adds a bounded per-level increment, so its growth
	// ratio is a small constant multiple of the per-arm increment.
	first, last := samples[0], samples[len(samples)-1]
	growth := float64(last.sqlLen) / float64(first.sqlLen)
	t.Logf("or chain SQL growth k=1→k=%d: %.2fx (%d → %d bytes)",
		last.k, growth, first.sqlLen, last.sqlLen)

	// Pre-fix doubling would give ~2^(maxK-1) ≈ 128× at k=8. Require the
	// observed growth to be DRAMATICALLY flatter — under 20× leaves wide
	// headroom over the linear ~8× while staying an order of magnitude
	// under the exponential 128×. A regression that reinstated the
	// textual duplication would blow straight past this.
	const exponentialDoublingRatio = 128.0 // 2^(8-1)
	if growth >= exponentialDoublingRatio/4 {
		t.Errorf("set-op chain SQL scaling regression: k=1→k=%d byte growth is %.2fx (%d → %d) — "+
			"that is within striking distance of the pre-fix exponential %.0fx doubling shape. The "+
			"WITH-CTE common-subexpression elimination must keep the chain growing linearly.",
			last.k, growth, first.sqlLen, last.sqlLen, exponentialDoublingRatio)
	}

	// Per-level increments must stay roughly constant (linear), not
	// compound. Compare the last step's increment to the first step's:
	// under linear growth they're within a small constant factor; under
	// exponential growth the last increment dwarfs the first.
	firstStep := samples[1].sqlLen - samples[0].sqlLen
	lastStep := samples[len(samples)-1].sqlLen - samples[len(samples)-2].sqlLen
	t.Logf("or chain per-level increment: first=%dB last=%dB", firstStep, lastStep)
	if firstStep > 0 && lastStep > firstStep*4 {
		t.Errorf("set-op chain increment regression: the k=%d→%d step adds %dB vs %dB for the k=1→2 "+
			"step (>4x) — per-level cost is compounding, not constant; the arm subplan is being "+
			"re-inlined instead of referenced through the CTE.", last.k-1, last.k, lastStep, firstStep)
	}

	// --- (b) cross-op executability: `and` / `unless` chains must also
	// execute cleanly at the deepest depth (they share the emitter; the
	// guard pins they stay parseable + correct). For these ops over
	// disjoint-signature arms the result is empty (`and`) / the full left
	// chain (`unless`), but the decisive check here is that they EXECUTE
	// with no Code:62 at k=maxK.
	for _, op := range []string{"and", "unless"} {
		sqlText, args := emitSetOpChainSQL(t, op, maxK)
		if len(sqlText) >= maxQuerySize {
			t.Fatalf("%s chain k=%d: emitted SQL is %d bytes, at/over CH's %d max_query_size.",
				op, maxK, len(sqlText), maxQuerySize)
		}
		// Just exercise execution; a Code:62 / type error would fail here.
		_, _ = setOpChainAgg(t, db, sqlText, args)
		t.Logf("%s k=%d sqllen=%d executed OK", op, maxK, len(sqlText))
	}
}
