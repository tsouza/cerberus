package regression

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// seedSource is the single Go file holding the deterministic INSERT
// statements used by the E2E ClickHouse seeder. Path is relative to this
// test package directory (`go test` cd's into the package dir).
const seedSource = "../e2e/seed/cmd/seed/main.go"

// readSeedSource is a small helper since every test below loads the same
// file. Centralised so a future relocation of the seeder is a one-line fix.
func readSeedSource(t *testing.T) string {
	t.Helper()
	buf, err := os.ReadFile(seedSource)
	if err != nil {
		t.Fatalf("read %s: %v", seedSource, err)
	}
	return string(buf)
}

// TestSeedScriptsHaveNoInlineCommentsInValues guards against the bug
// fixed in commit 292c183: ClickHouse's VALUES parser rejects `--`
// comments interspersed between value tuples in a single INSERT.
// Symptom in CI: `Code: 27. DB::Exception: Cannot parse input:
// expected '(' before: '-- Trace 2: ...'`.
//
// The seed used to live in *.sql files; it's now embedded as Go string
// constants in test/e2e/seed/cmd/seed/main.go. We scan that file for any
// `VALUES (` → `;`/closing-backtick span and assert no SQL-style `--`
// comment line appears inside.
func TestSeedScriptsHaveNoInlineCommentsInValues(t *testing.T) {
	t.Parallel()

	content := readSeedSource(t)

	// Anchor: a literal `VALUES` keyword followed by optional whitespace +
	// `(` — i.e., the start of an actual tuple list.
	startRE := regexp.MustCompile(`(?si)\bVALUES\s*\(`)
	// SQL-style `--` line comment: leading whitespace + `-- ` (the trailing
	// space rules out `--`-bordered SQL operators and the rare `--` at the
	// very end of a line). The bug we're guarding against was `-- Trace 2:`,
	// which matches this shape.
	commentRE := regexp.MustCompile(`(?m)^\s*-- `)

	for _, loc := range startRE.FindAllStringIndex(content, -1) {
		startIdx := loc[1]
		// The Go string literal terminates with a backtick. We search for
		// the *next* backtick after the VALUES start; that bounds the SQL
		// statement.
		endIdx := strings.Index(content[startIdx:], "`")
		if endIdx < 0 {
			t.Errorf("`VALUES (` at offset %d has no terminating backtick", loc[0])
			continue
		}
		inside := content[startIdx : startIdx+endIdx]
		if commentRE.MatchString(inside) {
			for _, line := range strings.Split(inside, "\n") {
				if commentRE.MatchString(line) {
					t.Errorf("inline `--` comment inside an INSERT VALUES block — CH rejects this: %q",
						strings.TrimSpace(line))
					break
				}
			}
		}
	}
}

// TestLogsSeedUsesUnderscoredServiceName guards against the bug fixed
// in commit 639625f: LogQL's stream selector `{service_name="api"}`
// keeps the matcher name verbatim in cerberus's labelMatcherToExpr,
// so it looks up ResourceAttributes['service_name']. If the seed
// inserts `service.name` (dotted, OTel convention) instead, every
// Loki E2E test silently returns empty streams.
//
// Cerberus uses matcher names verbatim — there is no automatic
// Prom/OTel naming translation. The seed must use the underscored
// form so cerberus's labelMatcherToExpr lookup finds the row.
func TestLogsSeedUsesUnderscoredServiceName(t *testing.T) {
	t.Parallel()

	content := readSeedSource(t)

	if !strings.Contains(content, "'service_name'") {
		t.Errorf("%s: expected `'service_name'` map key (underscored) — LogQL's matcher.Name is verbatim, dotted form returns empty results", seedSource)
	}
	// The dotted form being absent inside the logs INSERT is the stronger
	// check. The traces INSERT legitimately uses `'service.name'` (Tempo
	// reads ResourceAttributes with the OTel-canonical key), so we narrow
	// the scan to the logs-INSERT SQL block only.
	logsStart := strings.Index(content, "insertLogsSQL")
	if logsStart < 0 {
		t.Fatalf("%s: insertLogsSQL constant not found", seedSource)
	}
	logsEnd := strings.Index(content[logsStart:], "insertTracesSQL")
	if logsEnd < 0 {
		logsEnd = len(content) - logsStart
	}
	logsBlock := content[logsStart : logsStart+logsEnd]
	mapDottedRE := regexp.MustCompile(`map\(\s*'service\.name'`)
	if mapDottedRE.MatchString(logsBlock) {
		t.Errorf("%s: found `map('service.name', ...)` inside the logs INSERT — LogQL stream selectors won't match this; use `service_name` instead (cerberus uses matcher names verbatim — no automatic Prom/OTel naming translation)", seedSource)
	}
}

// TestMetricsSeedHasHistogramTable guards against the bug surfaced in
// commit a25edd9: the Prom metadata endpoints (/api/v1/labels,
// /api/v1/label/<n>/values, /api/v1/metadata) UNION ALL across
// gauge + sum + histogram tables. Without otel_metrics_histogram in the
// seed, every metadata query fails with `Table doesn't exist` and
// cerberus returns 502.
//
// Schema creation is now delegated to internal/schema/ddl which always
// creates all 5 metrics tables (gauge, sum, histogram, exp_histogram,
// summary) as a single Metrics signal. So the table is guaranteed to
// exist as long as the seeder calls `ddl.Apply(ctx, conn, ddl.All)` —
// that's what this test now asserts.
func TestMetricsSeedHasHistogramTable(t *testing.T) {
	t.Parallel()

	content := readSeedSource(t)
	if !strings.Contains(content, "ddl.ApplyWithConfig") && !strings.Contains(content, "ddl.Apply") {
		t.Errorf("%s: expected the seeder to call ddl.Apply / ddl.ApplyWithConfig to create the OTel-CH schema (incl. otel_metrics_histogram)", seedSource)
	}
	if !strings.Contains(content, "ddl.All") {
		t.Errorf("%s: expected the seeder to pass ddl.All — without the Metrics signal, otel_metrics_histogram is missing and Prom /labels + /label/.../values + /metadata fail with 502", seedSource)
	}
}

// TestTracesSeedHasFrontendAndApiServices guards against silent seed
// drift breaking the existing TraceQL E2E tests, which assert that
// {resource.service.name="frontend"} returns rows and the trace ID
// `a0000000000000000000000000000001` exists. If those values drift,
// every Tempo E2E test silently passes with empty results.
func TestTracesSeedHasFrontendAndApiServices(t *testing.T) {
	t.Parallel()

	content := readSeedSource(t)

	for _, needle := range []string{
		"'service.name', 'frontend'",
		"'service.name', 'api'",
		"a0000000000000000000000000000001",
	} {
		if !strings.Contains(content, needle) {
			t.Errorf("%s: expected %q somewhere in the seed; Tempo E2E tests depend on it", seedSource, needle)
		}
	}
}
