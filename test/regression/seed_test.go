package regression

import (
	"os"
	"regexp"
	"strconv"
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
// The compose stack's OTel-CH schema is provisioned by the external OTel
// collector, so the seeder blocks (waitForTables) until every table it inserts
// into — otel_metrics_histogram included — has been created before it writes.
// The histogram table is therefore guaranteed present as long as the seeder
// keeps otel_metrics_histogram in its wait set; that's what this test asserts.
func TestMetricsSeedHasHistogramTable(t *testing.T) {
	t.Parallel()

	content := readSeedSource(t)
	if !strings.Contains(content, "waitForTables") {
		t.Errorf("%s: expected the seeder to waitForTables until the external writer provisions the OTel-CH schema (incl. otel_metrics_histogram)", seedSource)
	}
	if !strings.Contains(content, `"otel_metrics_histogram"`) {
		t.Errorf("%s: expected the seeder to require otel_metrics_histogram (in its wait set) — without it, Prom /labels + /label/.../values + /metadata fail with 502", seedSource)
	}
}

// TestLokiBenchTagsImpossibleFilterAsEmptyResult guards against an
// upstream re-vendor of grafana/loki:pkg/logql/bench/queries/ that
// drops the `empty-result` tag on the
// `fast/basic-selectors.yaml#Log query with impossible filter ...`
// entry. The cerberus diff driver
// (compatibility/loki/cmd/loki-compliance-tester/main.go) keys its
// "baseline-empty is the expected outcome" branch on that tag — losing
// it would silently flip the case back into a `baseline returned empty`
// row in the compat report, masking real parity drift behind a
// harness-shape false positive. See PR introducing this regression.
func TestLokiBenchTagsImpossibleFilterAsEmptyResult(t *testing.T) {
	t.Parallel()

	const corpusPath = "../../compatibility/loki/upstream/loki-bench/queries/fast/basic-selectors.yaml"
	buf, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("read %s: %v", corpusPath, err)
	}
	content := string(buf)

	// The impossible-filter entry — pinned by both the description and the
	// `empty-result` tag literal the driver reads at runtime. An upstream
	// rename of either drops parity for this case, so we require both.
	const (
		description = "Log query with impossible filter (guarantees empty results, exercises log result cache)"
		tagLiteral  = "- empty-result"
	)
	if !strings.Contains(content, description) {
		t.Errorf("%s: expected description %q — corpus may have been re-vendored under a different label", corpusPath, description)
	}
	if !strings.Contains(content, tagLiteral) {
		t.Errorf("%s: expected `%s` tag — the diff driver relies on it to treat baseline-empty as the parity-expected outcome (otherwise the case shows up as `baseline returned empty`)", corpusPath, tagLiteral)
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

// showcaseTraceSeedSource is the Go file holding the showcase-traceql
// rolling re-seed (INSERT + stale-row DELETE) for the b0... trace range.
const showcaseTraceSeedSource = "../e2e/seed/cmd/seed/showcase_traceql.go"

// TestShowcaseTraceReseedInsertsBeforeDelete pins the fix for the
// compose-smoke flake on PR #769: the showcase-trace rolling re-seed
// used to DELETE the whole b0... range and only then re-INSERT it.
// ClickHouse lightweight DELETEs are asynchronous mutations, so every
// 30 s tick opened a visible window in which the showcase range was
// empty — `{ name="GET /api/checkout" } | rate() by (name) > 0.0001`
// returned 0 series right after its own baseline listed the name, and
// failOnFlakyTests turned the retry-pass into a CI failure.
//
// The invariant: inside insertShowcaseTraces the INSERT must execute
// strictly before the stale-row DELETE, so readers never observe an
// empty (or partially-deleted) showcase range.
func TestShowcaseTraceReseedInsertsBeforeDelete(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile(showcaseTraceSeedSource)
	if err != nil {
		t.Fatalf("read %s: %v", showcaseTraceSeedSource, err)
	}
	content := string(buf)

	fnStart := strings.Index(content, "func insertShowcaseTraces(")
	if fnStart < 0 {
		t.Fatalf("%s: func insertShowcaseTraces not found", showcaseTraceSeedSource)
	}
	body := content[fnStart:]

	insertIdx := strings.Index(body, "insertShowcaseTracesSQL")
	deleteIdx := strings.Index(body, "deleteStaleShowcaseTracesSQL")
	switch {
	case insertIdx < 0:
		t.Fatalf("%s: insertShowcaseTraces does not exec insertShowcaseTracesSQL", showcaseTraceSeedSource)
	case deleteIdx < 0:
		t.Fatalf("%s: insertShowcaseTraces does not exec deleteStaleShowcaseTracesSQL — without the stale-row delete every tick stacks another copy of each span (#762)", showcaseTraceSeedSource)
	case deleteIdx < insertIdx:
		t.Errorf("%s: insertShowcaseTraces runs the DELETE before the INSERT — lightweight DELETEs are async mutations, so delete-first opens an empty-range window every re-seed tick (compose-smoke flake on PR #769); INSERT must come first", showcaseTraceSeedSource)
	}
}

// TestShowcaseTraceStaleDeleteIsDataAnchored pins the shape of the
// stale-row DELETE that makes insert-first ordering race-free:
//
//  1. the cutoff must be anchored on the showcase data itself
//     (max(Timestamp) over the b0... range), not on the server clock —
//     a clock-anchored cutoff can swallow the freshest tick when the
//     mutation's predicate is evaluated late under load;
//  2. the margin must exceed the INSERT's own timestamp spread (so the
//     freshest tick's oldest row is always spared) and stay below the
//     30 s re-seed interval (so the previous tick is always collected
//     and duplication stays bounded at ≤2 copies).
//
// The offsets are parsed from the SQL constants rather than hardcoded,
// so editing the seed topology without rebalancing the margin fails
// here instead of as a compose-smoke flake.
func TestShowcaseTraceStaleDeleteIsDataAnchored(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile(showcaseTraceSeedSource)
	if err != nil {
		t.Fatalf("read %s: %v", showcaseTraceSeedSource, err)
	}
	content := string(buf)

	deleteSQL := extractBacktickConst(t, content, "deleteStaleShowcaseTracesSQL")
	if !strings.Contains(deleteSQL, "max(Timestamp)") {
		t.Errorf("%s: deleteStaleShowcaseTracesSQL must anchor its cutoff on max(Timestamp) over the showcase range (data-anchored), not the server clock", showcaseTraceSeedSource)
	}
	if got := strings.Count(deleteSQL, "TraceId LIKE 'b00000000000000000000000000000%'"); got != 2 {
		t.Errorf("%s: deleteStaleShowcaseTracesSQL must scope BOTH the outer DELETE and the max(Timestamp) subquery to the b0... showcase range (got %d scoped predicates, want 2) — an unscoped subquery anchors the cutoff on foreign rows, an unscoped delete eats the base fixture", showcaseTraceSeedSource, got)
	}

	intervalRE := regexp.MustCompile(`INTERVAL (\d+) SECOND`)

	// Margin: the single INTERVAL in the DELETE's cutoff expression.
	deleteIntervals := intervalRE.FindAllStringSubmatch(deleteSQL, -1)
	if len(deleteIntervals) != 1 {
		t.Fatalf("%s: expected exactly one `INTERVAL <n> SECOND` margin in deleteStaleShowcaseTracesSQL, got %d", showcaseTraceSeedSource, len(deleteIntervals))
	}
	margin, err := strconv.Atoi(deleteIntervals[0][1])
	if err != nil {
		t.Fatalf("parse margin: %v", err)
	}

	// Spread: every row/event timestamp in the INSERT is
	// `now64(9) - INTERVAL <n> SECOND`; the spread is max-min.
	insertSQL := extractBacktickConst(t, content, "insertShowcaseTracesSQL")
	insertIntervals := intervalRE.FindAllStringSubmatch(insertSQL, -1)
	if len(insertIntervals) == 0 {
		t.Fatalf("%s: no `INTERVAL <n> SECOND` offsets found in insertShowcaseTracesSQL", showcaseTraceSeedSource)
	}
	minOff, maxOff := 1<<31, 0
	for _, m := range insertIntervals {
		off, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse insert offset %q: %v", m[1], err)
		}
		if off < minOff {
			minOff = off
		}
		if off > maxOff {
			maxOff = off
		}
	}
	spread := maxOff - minOff

	// 30 s is the rolling re-seed cadence: docker-compose.yml passes
	// `--re-seed-interval=30s` to the seed container.
	const tickSeconds = 30
	if margin <= spread {
		t.Errorf("%s: stale-delete margin (%ds) must exceed the INSERT timestamp spread (%ds = %d-%d) or the freshest tick's oldest rows fall past the cutoff and get deleted", showcaseTraceSeedSource, margin, spread, maxOff, minOff)
	}
	if margin >= tickSeconds {
		t.Errorf("%s: stale-delete margin (%ds) must stay below the %ds re-seed interval or previous ticks survive every delete and duplicates accumulate unbounded (#762)", showcaseTraceSeedSource, margin, tickSeconds)
	}
}

// extractBacktickConst returns the backtick-delimited string literal of
// the named top-level `const <name> = `...“ declaration in content.
func extractBacktickConst(t *testing.T, content, name string) string {
	t.Helper()
	declStart := strings.Index(content, "const "+name)
	if declStart < 0 {
		t.Fatalf("const %s not found", name)
	}
	open := strings.Index(content[declStart:], "`")
	if open < 0 {
		t.Fatalf("const %s: no opening backtick", name)
	}
	rest := content[declStart+open+1:]
	closeIdx := strings.Index(rest, "`")
	if closeIdx < 0 {
		t.Fatalf("const %s: no closing backtick", name)
	}
	return rest[:closeIdx]
}

// TestLogsSeedOmitsTimestampTime pins the schema-skew fix from the
// 2026-06-10 dashboard-job failures: upstream's clickhouseexporter
// removed the TimestampTime column from the logs DDL in v0.150.0, so
// (before the fork bump to the v0.152 templates) which schema
// `otel_logs` carried depended on who created it first — the seeder's
// ddl.Apply (then on legacy fork templates, column present and
// materialized from Timestamp) or the k3d otel-collector's own
// exporter (0.152.x, column gone). Cerberus's startup warmup (#712)
// made the collector reliably win that race, and the seeder's INSERT
// naming the column hard-failed with "No such column TimestampTime".
// ddl.Apply now renders the same column-free v0.152 schema, so the
// column never exists; the INSERT must keep omitting it.
func TestLogsSeedOmitsTimestampTime(t *testing.T) {
	t.Parallel()

	content := readSeedSource(t)
	logsStart := strings.Index(content, "insertLogsSQL")
	if logsStart < 0 {
		t.Fatalf("%s: insertLogsSQL constant not found", seedSource)
	}
	logsEnd := strings.Index(content[logsStart:], "insertTracesSQL")
	if logsEnd < 0 {
		logsEnd = len(content) - logsStart
	}
	logsBlock := content[logsStart : logsStart+logsEnd]
	if strings.Contains(logsBlock, "TimestampTime") {
		t.Errorf("%s: logs INSERT names TimestampTime — the column does not exist in the post-v0.150.0 exporter schema (which ddl.Apply now renders too), so the INSERT hard-fails; omit it", seedSource)
	}
}
