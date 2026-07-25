package migrate

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
)

// promQuery builds a one-query PromQL corpus for the reach table below.
func promQuery(expr string) Corpus {
	return BuildCorpus([]HarvestedQuery{
		{Expr: expr, Source: "rule:r.yml/g/n", Kind: KindRecord, Lang: LangPromQL},
	}, nil)
}

// TestMaxLookbackReach pins the reach every PromQL shape requires: a bare
// selector reaches back no distance, a range selector by its range, an offset
// adds to it, and a subquery adds its own range and offset ON TOP of the reach
// of the expression it re-evaluates at every anchor.
func TestMaxLookbackReach(t *testing.T) {
	cases := []struct {
		expr string
		want time.Duration
	}{
		{"foo", 0},
		{"foo[5m]", 5 * time.Minute},
		{"foo offset 1h", time.Hour},
		{"rate(foo[5m] offset 30m)", 35 * time.Minute},
		{"rate(foo[7d] offset 7d)", 14 * 24 * time.Hour},
		{"max_over_time(foo[30d])", 30 * 24 * time.Hour},
		// The inner 1h window is re-evaluated at the oldest anchor of the outer
		// 14d window, so the two ranges add rather than the larger winning.
		{"avg_over_time(foo[1h:5m])[14d:1h]", 14*24*time.Hour + time.Hour},
		{"max_over_time(rate(foo[5m])[1d:1m] offset 2h)", 24*time.Hour + 5*time.Minute + 2*time.Hour},
		// A binary expression reaches back as far as its farthest-reaching side,
		// not as far as the side the parser happens to visit last.
		{"sum_over_time(foo[10m]) / sum_over_time(bar[3h])", 3 * time.Hour},
		{"sum_over_time(bar[3h]) / sum_over_time(foo[10m])", 3 * time.Hour},
		// A forward-looking offset must never be folded in as a positive runway,
		// and must not drag the corpus maximum below zero either.
		{"foo offset -1h", 0},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			lb := MaxLookback(promQuery(tc.expr))
			if len(lb.ByQuery) != 1 {
				t.Fatalf("expected 1 walked query, got %d (anchored=%d unparseable=%d)",
					len(lb.ByQuery), len(lb.Anchored), len(lb.Unparseable))
			}
			if got := time.Duration(lb.Longest); got != tc.want {
				t.Errorf("longest reach for %s = %s, want %s", tc.expr, got, tc.want)
			}
			// A negative per-query reach is honest data; only the corpus maximum
			// clamps, so the two figures agree except on the clamp.
			if got := time.Duration(lb.ByQuery[0].Reach); got > tc.want {
				t.Errorf("per-query reach %s exceeds the corpus longest %s", got, tc.want)
			}
		})
	}
}

// TestMaxLookbackAnchoredIsNotFoldedIn proves an `@`-modified query leaves the
// relative walk with a stated reason instead of contributing a reach measured
// against a clock the offline tool does not have.
func TestMaxLookbackAnchoredIsNotFoldedIn(t *testing.T) {
	for _, expr := range []string{"foo @ 1600000000", "foo[30d] @ start()", "max_over_time(foo[1h])[1d:5m] @ end()"} {
		t.Run(expr, func(t *testing.T) {
			lb := MaxLookback(promQuery(expr))
			if len(lb.ByQuery) != 0 {
				t.Fatalf("anchored query was walked as a relative lookback: %+v", lb.ByQuery)
			}
			if len(lb.Anchored) != 1 {
				t.Fatalf("expected 1 anchored entry, got %d", len(lb.Anchored))
			}
			if !strings.Contains(lb.Anchored[0].Reason, "@ modifier") {
				t.Errorf("anchored reason does not name the @ modifier: %q", lb.Anchored[0].Reason)
			}
			if time.Duration(lb.Longest) != 0 {
				t.Errorf("anchored query contributed %s to the longest lookback", time.Duration(lb.Longest))
			}
		})
	}
}

// TestMaxLookbackAccountsForEveryCorpusEntry proves the accounting identity the
// retention scenario rests on: every corpus query lands in exactly one of the
// four lists, so a query the walk cannot read can never silently shrink the
// runway by being treated as reaching back no distance.
func TestMaxLookbackAccountsForEveryCorpusEntry(t *testing.T) {
	c := BuildCorpus([]HarvestedQuery{
		{Expr: "rate(http_requests_total[30d])", Source: "rule:a.yml/g/slo", Kind: KindRecord, Lang: LangPromQL},
		{Expr: "foo @ 1600000000", Source: "rule:a.yml/g/anchored", Kind: KindRecord, Lang: LangPromQL},
		{Expr: `sum(rate({app="x"} |= "e" [5m]))`, Source: "dashboard:d.json/p/A", Kind: KindPanel, Lang: LangLogQL},
		{Expr: `{ duration > 2s }`, Source: "dashboard:d.json/p/B", Kind: KindPanel, Lang: LangTraceQL},
		{Expr: "rate(foo[5m)", Source: "rule:a.yml/g/broken", Kind: KindAlert, Lang: LangPromQL},
	}, nil)

	lb := MaxLookback(c)
	accounted := len(lb.ByQuery) + len(lb.Anchored) + len(lb.NonPromQL) + len(lb.Unparseable)
	if accounted != len(c.Queries) {
		t.Fatalf("accounted for %d of %d corpus queries (by_query=%d anchored=%d non_promql=%d unparseable=%d)",
			accounted, len(c.Queries), len(lb.ByQuery), len(lb.Anchored), len(lb.NonPromQL), len(lb.Unparseable))
	}
	if len(lb.ByQuery) != 1 || len(lb.Anchored) != 1 || len(lb.NonPromQL) != 2 || len(lb.Unparseable) != 1 {
		t.Fatalf("wrong partition: by_query=%d anchored=%d non_promql=%d unparseable=%d",
			len(lb.ByQuery), len(lb.Anchored), len(lb.NonPromQL), len(lb.Unparseable))
	}
	if want := model.Duration(30 * 24 * time.Hour); lb.Longest != want {
		t.Errorf("longest = %s, want %s", lb.Longest, want)
	}
	for _, e := range lb.NonPromQL {
		if !strings.Contains(e.Reason, "PromQL AST") {
			t.Errorf("non-PromQL entry %s does not state why it is outside the walk: %q", e.Source, e.Reason)
		}
	}
	if !strings.Contains(lb.Unparseable[0].Reason, "unparseable PromQL") {
		t.Errorf("unparseable entry does not state the parse failure: %q", lb.Unparseable[0].Reason)
	}
	if lb.SchemaVersion != LookbackVersion {
		t.Errorf("schema version = %d, want %d", lb.SchemaVersion, LookbackVersion)
	}
}

// TestMaxLookbackMarshalsDeterministically proves the artifact the retention
// scenario diffs against a golden is byte-stable and renders its durations in
// the units an operator provisions retention in.
func TestMaxLookbackMarshalsDeterministically(t *testing.T) {
	c := BuildCorpus([]HarvestedQuery{
		{Expr: "b[1h]", Source: "rule:z.yml/g/b", Kind: KindRecord, Lang: LangPromQL},
		{Expr: "a[30d]", Source: "rule:a.yml/g/a", Kind: KindRecord, Lang: LangPromQL},
	}, nil)

	first, err := MaxLookback(c).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := MaxLookback(c).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("two marshals of the same corpus differ:\n%s\n---\n%s", first, second)
	}
	if !strings.Contains(string(first), `"longest": "30d"`) {
		t.Errorf("longest is not rendered in operator retention units:\n%s", first)
	}
	if i, j := strings.Index(string(first), "rule:a.yml"), strings.Index(string(first), "rule:z.yml"); i > j {
		t.Errorf("by_query is not sorted by source:\n%s", first)
	}
}
