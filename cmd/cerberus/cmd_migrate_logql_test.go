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

// writeLogQLCorpus writes a deterministic corpus.json carrying LogQL queries and
// returns its path. The corpus format is exactly what `migrate harvest` emits, so
// driving `explain --corpus` / `classify --corpus` over it exercises the real
// three-headed offline pipeline (parse -> lower -> emit via engine.DryRunSQL) for
// the LogQL head without a ClickHouse connection.
func writeLogQLCorpus(t *testing.T, queries []migrate.CorpusQuery) string {
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

// TestExplainLogQLCorpus drives `migrate explain --corpus` over a LogQL corpus
// through the real offline pipeline. A LogQL metric query and a LogQL log-stream
// query must both preview REAL ClickHouse SQL against the logs table (otel_logs),
// never UNSUPPORTED — the offline explainer now routes LogQL to the logs schema.
func TestExplainLogQLCorpus(t *testing.T) {
	corpus := writeLogQLCorpus(t, []migrate.CorpusQuery{
		{Expr: `sum(rate({job="x"}[5m]))`, Source: "corpus:metric", Kind: migrate.KindRecord, Lang: migrate.LangLogQL},
		{Expr: `{job="x"} |= "err"`, Source: "corpus:logstream", Kind: migrate.KindRecord, Lang: migrate.LangLogQL},
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"explain", "--corpus", corpus}, &out, &errOut); err != nil {
		t.Fatalf("explain: %v (stderr: %s)", err, errOut.String())
	}
	report := out.String()

	if strings.Contains(report, "UNSUPPORTED") {
		t.Fatalf("LogQL corpus must preview real SQL, got an UNSUPPORTED entry:\n%s", report)
	}
	// Both queries scan the OTel logs table — the emitted SQL must reference it,
	// which only happens if the query lowered against the logs schema.
	if got := strings.Count(report, "otel_logs"); got < 2 {
		t.Errorf("expected both LogQL queries to emit SQL over otel_logs (>=2 occurrences), got %d\n%s", got, report)
	}
	for _, want := range []string{
		`sum(rate({job="x"}[5m]))`,
		`{job="x"} |= "err"`,
		"sql:",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("explain report missing %q\n---\n%s", want, report)
		}
	}
}

// TestClassifyLogQLCorpus drives `migrate classify --corpus --json` over the same
// LogQL corpus: both a metric and a log-stream query lower + emit cleanly, so both
// bucket as supported through the identical DryRunSQL path used by PromQL.
func TestClassifyLogQLCorpus(t *testing.T) {
	corpus := writeLogQLCorpus(t, []migrate.CorpusQuery{
		{Expr: `sum(rate({job="x"}[5m]))`, Source: "corpus:metric", Kind: migrate.KindRecord, Lang: migrate.LangLogQL},
		{Expr: `{job="x"} |= "err"`, Source: "corpus:logstream", Kind: migrate.KindRecord, Lang: migrate.LangLogQL},
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
			t.Errorf("LogQL query %q bucketed as %q, want supported (construct: %q)", q.Expr, q.Bucket, q.Construct)
		}
	}
}

// TestClassifyLogQLUnsupported pins that a genuinely broken LogQL query still
// lands in the unsupported bucket with its construct named — the honest failure
// path, not a silent drop or a mislabelled success.
func TestClassifyLogQLUnsupported(t *testing.T) {
	corpus := writeLogQLCorpus(t, []migrate.CorpusQuery{
		// Unterminated stream selector: a real LogQL parse error.
		{Expr: `{job="x"`, Source: "corpus:broken", Kind: migrate.KindRecord, Lang: migrate.LangLogQL},
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"classify", "--corpus", corpus, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("classify --json: %v (stderr: %s)", err, errOut.String())
	}

	var cl migrate.Classification
	if err := json.Unmarshal(out.Bytes(), &cl); err != nil {
		t.Fatalf("unmarshal classify JSON: %v\n%s", err, out.String())
	}
	if cl.Counts.Total != 1 || cl.Counts.Unsupported != 1 {
		t.Fatalf("counts = %+v, want total 1 / unsupported 1\n%s", cl.Counts, out.String())
	}
	if cl.Queries[0].Bucket != migrate.BucketUnsupported {
		t.Errorf("broken LogQL query bucketed as %q, want unsupported", cl.Queries[0].Bucket)
	}
	if cl.Queries[0].Construct == "" {
		t.Error("unsupported LogQL query must name its offending construct, got empty")
	}
}

// TestExplainTraceQLCorpusHonestlyUnsupported pins that a TraceQL corpus entry —
// which has no offline SQL preview in this wave — is reported UNSUPPORTED with the
// language named, rather than mis-parsed as PromQL/LogQL and surfaced as a
// confusing parse error.
func TestExplainTraceQLCorpusHonestlyUnsupported(t *testing.T) {
	corpus := writeLogQLCorpus(t, []migrate.CorpusQuery{
		{Expr: `{ span.http.status_code = 500 }`, Source: "corpus:trace", Kind: migrate.KindPanel, Lang: migrate.LangTraceQL},
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"explain", "--corpus", corpus}, &out, &errOut); err != nil {
		t.Fatalf("explain: %v (stderr: %s)", err, errOut.String())
	}
	report := out.String()
	if !strings.Contains(report, "UNSUPPORTED") {
		t.Fatalf("TraceQL corpus entry must report UNSUPPORTED, got:\n%s", report)
	}
	if !strings.Contains(report, migrate.LangTraceQL) {
		t.Errorf("UNSUPPORTED reason must name the traceql language, got:\n%s", report)
	}
}
