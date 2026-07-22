package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestClassifyBuckets exercises the full classify ledger over a mixed corpus: a
// clean query lands in supported, a broken query in unsupported with its
// offending construct captured, and a clean-but-fan-out query is supported AND
// flagged risky. Counts and the rendered text are both asserted.
func TestClassifyBuckets(t *testing.T) {
	src := staticSource{
		queries: []HarvestedQuery{
			{Expr: "up", Source: "rule:f/g/up_rec", Kind: KindRecord},
			{Expr: "sum_over_time(up[10m:1m])", Source: "rule:f/g/subq", Kind: KindRecord},
			{Expr: "!!!", Source: "rule:f/g/broken", Kind: KindAlert},
		},
		skipped: []SkippedEntry{{Source: "rule:f/g/empty", Reason: "rule has an empty expr"}},
	}
	ex := fakeExplainer{byQuery: map[string]Explanation{
		"up": {
			SQL:  "SELECT * FROM otel_metrics_gauge",
			Plan: &chplan.Scan{Table: "otel_metrics_gauge"},
		},
		"sum_over_time(up[10m:1m])": {
			SQL: "SELECT * FROM otel_metrics_gauge",
			Plan: &chplan.RangeWindow{
				Input:      &chplan.Scan{Table: "otel_metrics_gauge"},
				Func:       "sum_over_time",
				OuterRange: 10 * time.Minute,
				Step:       time.Minute,
			},
		},
		"!!!": {Err: errors.New("parse error: unexpected character inside braces")},
	}}

	rep, err := BuildReport(context.Background(), src, ex)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	cl := Classify(rep)

	if cl.Counts.Total != 3 {
		t.Errorf("Total = %d, want 3", cl.Counts.Total)
	}
	if cl.Counts.Supported != 2 {
		t.Errorf("Supported = %d, want 2", cl.Counts.Supported)
	}
	if cl.Counts.Unsupported != 1 {
		t.Errorf("Unsupported = %d, want 1", cl.Counts.Unsupported)
	}
	if cl.Counts.Risky != 1 {
		t.Errorf("Risky = %d, want 1", cl.Counts.Risky)
	}

	byExpr := map[string]ClassifiedQuery{}
	for _, q := range cl.Queries {
		byExpr[q.Expr] = q
	}
	if got := byExpr["up"]; got.Bucket != BucketSupported || got.Risky {
		t.Errorf("`up` should be supported and not risky, got %+v", got)
	}
	if got := byExpr["sum_over_time(up[10m:1m])"]; got.Bucket != BucketSupported || !got.Risky {
		t.Errorf("subquery should be supported AND risky, got %+v", got)
	}
	broken := byExpr["!!!"]
	if broken.Bucket != BucketUnsupported {
		t.Fatalf("`!!!` should be unsupported, got %+v", broken)
	}
	// The offending construct must be NAMED, not silently dropped.
	if !strings.Contains(broken.Construct, "unexpected character inside braces") {
		t.Errorf("unsupported query must name the offending construct, got %q", broken.Construct)
	}

	var sb strings.Builder
	if err := cl.Write(&sb); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"3 queries: 2 supported (1 risky), 1 unsupported; 1 skipped",
		"only `migrate verify` proves parity",
		"unexpected character inside braces",
		"RISKY:",
		"rule has an empty expr",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("classify text missing %q\n---\n%s", want, out)
		}
	}
}

// TestClassifyJSON pins the machine-readable ledger: the bucket, construct, and
// counts round-trip through JSON, and nil query/skip slices render as [] not
// null so downstream readers never choke on a null.
func TestClassifyJSON(t *testing.T) {
	src := staticSource{queries: []HarvestedQuery{
		{Expr: "up", Source: "rule:f/g/up", Kind: KindRecord},
		{Expr: "!!!", Source: "rule:f/g/broken", Kind: KindAlert},
	}}
	ex := fakeExplainer{byQuery: map[string]Explanation{
		"up":  {SQL: "SELECT 1", Plan: &chplan.Scan{Table: "otel_metrics_gauge"}},
		"!!!": {Err: errors.New("parse error: bad token")},
	}}
	rep, err := BuildReport(context.Background(), src, ex)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	var sb strings.Builder
	if err := Classify(rep).WriteJSON(&sb); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got Classification
	if err := json.Unmarshal([]byte(sb.String()), &got); err != nil {
		t.Fatalf("unmarshal classify JSON: %v\n%s", err, sb.String())
	}
	if got.Counts.Supported != 1 || got.Counts.Unsupported != 1 {
		t.Errorf("JSON counts wrong: %+v", got.Counts)
	}
	if len(got.Queries) != 2 {
		t.Fatalf("expected 2 queries in JSON, got %d", len(got.Queries))
	}

	// Empty-corpus classify must emit [] for queries/skipped, never null.
	var empty strings.Builder
	if err := (Classification{}).WriteJSON(&empty); err != nil {
		t.Fatalf("WriteJSON empty: %v", err)
	}
	if s := empty.String(); strings.Contains(s, "null") {
		t.Errorf("empty classification must render [] not null, got:\n%s", s)
	}
}
