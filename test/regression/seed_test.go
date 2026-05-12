package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// seedDir is resolved relative to this test file. The `go test`
// runner cd's into the package dir, so test/regression/ → ../e2e/seed/.
const seedDir = "../e2e/seed"

// TestSeedScriptsHaveNoInlineCommentsInValues guards against the bug
// fixed in commit 292c183: ClickHouse's VALUES parser rejects `--`
// comments interspersed between value tuples in a single INSERT.
// Symptom in CI: `Code: 27. DB::Exception: Cannot parse input:
// expected '(' before: '-- Trace 2: ...'`.
//
// We scan every *.sql under test/e2e/seed/ for blocks delimited by
// `VALUES` and the terminating `;` and assert no `--` line appears
// inside.
func TestSeedScriptsHaveNoInlineCommentsInValues(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(seedDir)
	if err != nil {
		t.Fatalf("read %s: %v", seedDir, err)
	}

	// Anchor: a literal `VALUES` keyword followed by optional
	// whitespace + `(` — i.e., the start of an actual tuple list.
	// The naive `\bVALUES\b` form was too greedy: it matched the word
	// `values` inside descriptive `--` comments (`/api/v1/label/<n>/values`,
	// "a VALUES tuple list") and then captured the next `;` somewhere
	// far below, producing false-positive "inline comment" hits.
	startRE := regexp.MustCompile(`(?si)\bVALUES\s*\(`)
	commentRE := regexp.MustCompile(`(?m)^\s*--`)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(seedDir, e.Name())
		buf, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		content := string(buf)
		// Locate every `VALUES (` in the file; for each one, find the
		// matching terminating `;` and scan the body in between.
		for _, loc := range startRE.FindAllStringIndex(content, -1) {
			startIdx := loc[1] // immediately after `VALUES (`
			endIdx := strings.Index(content[startIdx:], ";")
			if endIdx < 0 {
				t.Errorf("%s: `VALUES (` at offset %d has no terminating `;`", e.Name(), loc[0])
				continue
			}
			inside := content[startIdx : startIdx+endIdx]
			if commentRE.MatchString(inside) {
				// Find the first offending line for a useful error.
				for _, line := range strings.Split(inside, "\n") {
					if commentRE.MatchString(line) {
						t.Errorf("%s: inline `--` comment inside an INSERT VALUES block — CH rejects this: %q",
							e.Name(), strings.TrimSpace(line))
						break
					}
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
// The proper Prom/OTel naming bridge lives in RC2; until then the
// seed must use the underscored form so cerberus's current code
// finds the row.
func TestLogsSeedUsesUnderscoredServiceName(t *testing.T) {
	t.Parallel()

	path := filepath.Join(seedDir, "otel_logs.sql")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(buf)

	if !strings.Contains(content, "'service_name'") {
		t.Errorf("%s: expected `'service_name'` map key (underscored) — LogQL's matcher.Name is verbatim, dotted form returns empty results", path)
	}
	// The dotted form being absent is the stronger check, but we
	// allow it to appear in comment lines — only reject when it's
	// used as a map() key.
	mapDottedRE := regexp.MustCompile(`map\(\s*'service\.name'`)
	if mapDottedRE.MatchString(content) {
		t.Errorf("%s: found `map('service.name', ...)` — LogQL stream selectors won't match this; use `service_name` instead until the RC2 Prom/OTel naming bridge lands", path)
	}
}

// TestMetricsSeedHasHistogramTable guards against the bug surfaced in
// commit a25edd9: the Prom metadata endpoints (/api/v1/labels,
// /api/v1/label/<n>/values, /api/v1/metadata) UNION ALL across
// gauge + sum + histogram tables. Without otel_metrics_histogram in
// the seed, every metadata query fails with `Table doesn't exist`
// and cerberus returns 502.
//
// An empty histogram table is fine — the UNION just needs the
// schema to exist.
func TestMetricsSeedHasHistogramTable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(seedDir, "otel_metrics.sql")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	createHistogramRE := regexp.MustCompile(`(?i)CREATE\s+TABLE[^(]*otel_metrics_histogram`)
	if !createHistogramRE.MatchString(string(buf)) {
		t.Errorf("%s: missing `CREATE TABLE ... otel_metrics_histogram` — Prom /labels + /label/.../values + /metadata UNION across gauge+sum+histogram, so the histogram table must exist (empty is fine)", path)
	}
}

// TestTracesSeedHasFrontendAndApiServices guards against silent seed
// drift breaking the existing TraceQL E2E tests, which assert that
// {resource.service.name="frontend"} returns rows and the trace ID
// `a0000000000000000000000000000001` exists. If those values drift,
// every Tempo E2E test silently passes with empty results.
func TestTracesSeedHasFrontendAndApiServices(t *testing.T) {
	t.Parallel()

	path := filepath.Join(seedDir, "otel_traces.sql")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(buf)

	for _, needle := range []string{
		"'service.name', 'frontend'",
		"'service.name', 'api'",
		"a0000000000000000000000000000001",
	} {
		if !strings.Contains(content, needle) {
			t.Errorf("%s: expected %q somewhere in the seed; Tempo E2E tests depend on it", path, needle)
		}
	}
}
