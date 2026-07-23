package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrate"
)

// writeTraceQLCorpus writes a deterministic corpus.json carrying TraceQL queries
// and returns its path. The corpus format is exactly what `migrate harvest`
// emits, so driving `explain --corpus` / `classify --corpus` over it exercises
// the real three-headed offline pipeline (parse -> lower -> emit via
// engine.DryRunSQL) for the TraceQL head without a ClickHouse connection.
func writeTraceQLCorpus(t *testing.T, queries []migrate.CorpusQuery) string {
	t.Helper()
	c := migrate.Corpus{Version: migrate.CorpusVersion, Queries: queries, Skipped: []migrate.SkippedEntry{}}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal corpus: %v", err)
	}
	path := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write corpus: %v", err)
	}
	return path
}

// TestExplainTraceQLCorpusBounded is the load-bearing pin for this wave: a real
// TraceQL SEARCH query and a TraceQL METRICS query must BOTH preview real,
// BOUNDED ClickHouse SQL over the traces table (otel_traces), never UNSUPPORTED.
//
// Boundedness is the whole point. TraceQL lowering reads its search window +
// trace limit off the ctx (WithSearchWindow / WithSearchTraceLimit); if the
// offline dispatcher forgets to thread them, lowering emits an unbounded spans
// scan that chsql.Emit's RequireSpansScansBounded chokepoint REJECTS — which
// would surface here as an UNSUPPORTED entry. So a non-UNSUPPORTED preview that
// scans otel_traces proves the ctx window/limit were threaded end to end.
func TestExplainTraceQLCorpusBounded(t *testing.T) {
	corpus := writeTraceQLCorpus(t, []migrate.CorpusQuery{
		{Expr: `{ span.http.status_code = 500 }`, Source: "corpus:search", Kind: migrate.KindPanel, Lang: migrate.LangTraceQL},
		{Expr: `{ } | rate()`, Source: "corpus:metrics", Kind: migrate.KindPanel, Lang: migrate.LangTraceQL},
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"explain", "--corpus", corpus}, &out, &errOut); err != nil {
		t.Fatalf("explain: %v (stderr: %s)", err, errOut.String())
	}
	report := out.String()

	if strings.Contains(report, "UNSUPPORTED") {
		t.Fatalf("TraceQL corpus must preview real bounded SQL, got an UNSUPPORTED entry:\n%s", report)
	}
	// Both queries scan the OTel traces table — the emitted SQL must reference it,
	// which only happens if the query lowered against the traces schema.
	if got := strings.Count(report, "otel_traces"); got < 2 {
		t.Errorf("expected both TraceQL queries to emit SQL over otel_traces (>=2 occurrences), got %d\n%s", got, report)
	}
	// The bound itself must be present: the search window lowers to a Timestamp
	// partition-prune predicate (fromUnixTimestamp64Nano(...)). Its presence is
	// the observable proof the ctx window threaded through to emit.
	if !strings.Contains(report, "fromUnixTimestamp64Nano") {
		t.Errorf("expected a bounded (windowed) scan predicate in the emitted SQL, got:\n%s", report)
	}
	for _, want := range []string{
		`{ span.http.status_code = 500 }`,
		`{ } | rate()`,
		"sql:",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("explain report missing %q\n---\n%s", want, report)
		}
	}
}

// TestClassifyTraceQLCorpus drives `migrate classify --corpus --json` over the
// same TraceQL corpus: both the search and the metrics query lower + emit
// cleanly, so both bucket as supported through the identical DryRunSQL path used
// by the other two heads.
func TestClassifyTraceQLCorpus(t *testing.T) {
	corpus := writeTraceQLCorpus(t, []migrate.CorpusQuery{
		{Expr: `{ span.http.status_code = 500 }`, Source: "corpus:search", Kind: migrate.KindPanel, Lang: migrate.LangTraceQL},
		{Expr: `{ } | rate()`, Source: "corpus:metrics", Kind: migrate.KindPanel, Lang: migrate.LangTraceQL},
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"classify", "--corpus", corpus, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("classify --json: %v (stderr: %s)", err, errOut.String())
	}

	var cl migrate.Classification
	if err := json.Unmarshal(out.Bytes(), &cl); err != nil {
		t.Fatalf("unmarshal classify JSON: %v\n%s", err, out.String())
	}
	if cl.Counts.Total != 2 || cl.Counts.Supported != 2 || cl.Counts.Unsupported != 0 {
		t.Fatalf("counts = %+v, want total 2 / supported 2 / unsupported 0\n%s", cl.Counts, out.String())
	}
	for _, q := range cl.Queries {
		if q.Bucket != migrate.BucketSupported {
			t.Errorf("TraceQL query %q bucketed as %q, want supported (construct: %q)", q.Expr, q.Bucket, q.Construct)
		}
	}
}

// TestClassifyTraceQLStructuralRisky pins the "risky" flag for the TraceQL head:
// a structural-join query (`{...} >> {...}`) lowers + emits cleanly (so it is
// SUPPORTED) but carries the per-trace recursive-closure fan-out risk, so it must
// also be flagged risky — the honest "clean but expensive" signal, not a demoted
// bucket.
func TestClassifyTraceQLStructuralRisky(t *testing.T) {
	corpus := writeTraceQLCorpus(t, []migrate.CorpusQuery{
		{Expr: `{ span.http.status_code = 500 } >> { name = "db.query" }`, Source: "corpus:structural", Kind: migrate.KindPanel, Lang: migrate.LangTraceQL},
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"classify", "--corpus", corpus, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("classify --json: %v (stderr: %s)", err, errOut.String())
	}

	var cl migrate.Classification
	if err := json.Unmarshal(out.Bytes(), &cl); err != nil {
		t.Fatalf("unmarshal classify JSON: %v\n%s", err, out.String())
	}
	if cl.Counts.Supported != 1 || cl.Counts.Risky != 1 {
		t.Fatalf("counts = %+v, want supported 1 / risky 1\n%s", cl.Counts, out.String())
	}
	q := cl.Queries[0]
	if q.Bucket != migrate.BucketSupported || !q.Risky {
		t.Fatalf("structural-join query must be supported AND risky, got %+v", q)
	}
	joined := strings.Join(q.Risks, " ")
	if !strings.Contains(joined, "structural-join fan-out") {
		t.Errorf("risk flags must name the structural-join fan-out, got %v", q.Risks)
	}
}
