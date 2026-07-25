package migrateverify

import (
	"context"
	"strings"
	"testing"
)

// Bodies each head sends when a query matched nothing over the window — the shape
// every backend emits on a quiet window, a tenant with no data, or a
// metrics-generator that is not enabled yet. They are the inputs that separate
// AGREEMENT from ABSENCE: both sides return one of these and the comparator scores
// a match having diffed nothing.
const (
	emptyPromMatrixBody  = `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	emptyLokiMatrixBody  = `{"status":"success","data":{"resultType":"matrix","result":[]},"warnings":[]}`
	emptyTempoMatrixBody = `{"series":[]}`
)

// TestVerify_EmptyVsEmptyIsNotProvenParity pins the counter that distinguishes
// agreement from absence, on all three dialects.
//
// Two empty matrices score VerdictMatch — correctly; neither backend claimed
// anything the other contradicts — so a lane replayed over a window where nothing
// matched reads "3 replayed: 3 match" and satisfies any guard keyed on verdict
// counts. Not one series was ever compared, so flipping those datasources on the
// strength of it is exactly the zero-evidence cutover this report exists to
// prevent. The evidence counter is what makes it visible, and Failed /
// DeadFamilies are what make it block.
func TestVerify_EmptyVsEmptyIsNotProvenParity(t *testing.T) {
	promBodies := map[string]string{"up": emptyPromMatrixBody}
	lokiBodies := map[string]string{`sum(rate({job="api"}[5m]))`: emptyLokiMatrixBody}
	tempoBodies := map[string]string{"{} | rate()": emptyTempoMatrixBody}

	lanes := map[string]Lane{
		HeadProm: {
			Ref:      NewHTTPBackend(headServer(t, PromDialect(), "query", promBodies).URL),
			Cerberus: NewHTTPBackend(headServer(t, PromDialect(), "query", promBodies).URL),
		},
		HeadLoki: {
			Ref:      NewHTTPBackend(headServer(t, LokiDialect(), "query", lokiBodies).URL, WithDialect(LokiDialect())),
			Cerberus: NewHTTPBackend(headServer(t, LokiDialect(), "query", lokiBodies).URL, WithDialect(LokiDialect())),
		},
		HeadTempo: {
			Ref:      NewHTTPBackend(headServer(t, TempoDialect(), "q", tempoBodies).URL, WithDialect(TempoDialect())),
			Cerberus: NewHTTPBackend(headServer(t, TempoDialect(), "q", tempoBodies).URL, WithDialect(TempoDialect())),
		},
	}
	corpus := Corpus{Queries: []Query{
		{Expr: "up", Source: "rule:up", Head: HeadProm, Lang: "promql"},
		{Expr: `sum(rate({job="api"}[5m]))`, Source: "panel:lograte", Head: HeadLoki, Lang: "logql"},
		{Expr: "{} | rate()", Source: "panel:spanrate", Head: HeadTempo, Lang: "traceql"},
	}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	if rep.Summary.Match != 3 {
		t.Fatalf("summary = %+v, want 3 match (two empty matrices do agree)", rep.Summary)
	}
	if rep.Summary.ComparedSeries != 0 {
		t.Errorf("ComparedSeries = %d, want 0: nothing was diffed", rep.Summary.ComparedSeries)
	}
	if !rep.Failed() {
		t.Error("a run whose every lane diffed zero series proved nothing and must fail")
	}
	dead := rep.DeadFamilies()
	if len(dead) != 3 {
		t.Fatalf("DeadFamilies = %+v, want all three lanes reported dead", dead)
	}
	for _, f := range dead {
		if f.Kind != KindMetricMatrix || f.Unit != UnitSeries {
			t.Errorf("dead family %+v: want the metric-matrix family named in its own unit", f)
		}
		if f.Total != 1 || f.Compared != 0 {
			t.Errorf("dead family %+v: want 1 replayed, 0 compared — the all-match shape a verdict-count guard would clear", f)
		}
	}
	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(out.String(), "VERIFICATION PASSED") {
		t.Errorf("the banner must not read PASSED when every lane compared nothing:\n%s", out.String())
	}
	for _, want := range []string{
		"prom/metric-matrix compared nothing",
		"loki/metric-matrix compared nothing",
		"tempo/metric-matrix compared nothing",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report must name each dead lane (%q), got:\n%s", want, out.String())
		}
	}
}

// TestVerify_ComparedSeriesCountsTheUnion pins what the evidence counter means: the
// distinct series the comparator walked, not the number of responses. A series both
// backends returned counts once; a series only one side returned still counts,
// because the comparator did diff it (and reported it missing).
func TestVerify_ComparedSeriesCountsTheUnion(t *testing.T) {
	shared := seriesSpec{labels: map[string]string{"job": "api"}, points: []pointSpec{{1_700_000_000, "1"}}}
	refOnly := seriesSpec{labels: map[string]string{"job": "worker"}, points: []pointSpec{{1_700_000_000, "1"}}}

	refBodies := map[string]string{"up": matrix(shared, refOnly)}
	cerBodies := map[string]string{"up": matrix(shared)}
	lanes := map[string]Lane{HeadProm: {
		Ref:      NewHTTPBackend(headServer(t, PromDialect(), "query", refBodies).URL),
		Cerberus: NewHTTPBackend(headServer(t, PromDialect(), "query", cerBodies).URL),
	}}
	corpus := Corpus{Queries: []Query{{Expr: "up", Source: "rule:up", Head: HeadProm, Lang: "promql"}}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	if rep.Summary.ComparedSeries != 2 {
		t.Errorf("ComparedSeries = %d, want 2 (union of {job=api} and {job=worker}, not 3 responses)", rep.Summary.ComparedSeries)
	}
	if len(rep.Results) != 1 || rep.Results[0].ComparedSeries != 2 {
		t.Errorf("per-query ComparedSeries = %+v, want 2", rep.Results)
	}
	if rep.Summary.Diverge != 1 {
		t.Errorf("summary = %+v, want the missing series reported as a divergence", rep.Summary)
	}
}

// TestVerify_DeadLaneFailsEvenWhenAnotherLaneIsHealthy pins the case the per-head
// split exists for: 40 matched PromQL queries must not carry a Loki lane that
// replayed 12 and compared none of them. Keyed on the aggregate, the banner read
// PASSED and the CLI exited 0 while `migrate gate`, reading the same artifact,
// blocked on that lane — the two must never disagree.
func TestVerify_DeadLaneFailsEvenWhenAnotherLaneIsHealthy(t *testing.T) {
	rep := Report{
		Summary: Summary{Total: 52, Match: 40, Unsupported: 12, ComparedSeries: 40, ComparedUnits: 40},
		Heads: []HeadSummary{
			{
				Head: HeadLoki, Configured: true,
				Summary:  Summary{Total: 12, Unsupported: 12},
				Families: []FamilySummary{{Kind: KindMetricMatrix, Unit: UnitSeries, Total: 12, Unsupported: 12}},
			},
			{
				Head: HeadProm, Configured: true,
				Summary:  Summary{Total: 40, Match: 40, ComparedSeries: 40, ComparedUnits: 40},
				Families: []FamilySummary{{Kind: KindMetricMatrix, Unit: UnitSeries, Total: 40, Match: 40, Compared: 40}},
			},
		},
	}
	if !rep.Failed() {
		t.Fatal("a lane that replayed 12 queries and compared none of them must fail the run")
	}
	dead := rep.DeadFamilies()
	if len(dead) != 1 || dead[0].Head != HeadLoki || dead[0].Kind != KindMetricMatrix {
		t.Fatalf("DeadFamilies = %+v, want exactly the loki metric-matrix family", dead)
	}
	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(out.String(), "VERIFICATION PASSED") {
		t.Errorf("the banner must not read PASSED while a lane compared nothing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "loki/metric-matrix compared nothing") {
		t.Errorf("the failure must name the dead lane, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "prom/metric-matrix compared nothing") {
		t.Errorf("the healthy prom lane must not be reported dead, got:\n%s", out.String())
	}
}

// TestVerify_ConfiguredLaneWithNoReplayableQueryStillGetsARow pins the other
// masking shape: a lane the operator explicitly supplied backends for whose every
// corpus entry routed out of scope. Built only from replayed queries, the per-head
// table omitted it outright — so one green prom row was the entire evidence behind
// flipping a Loki datasource the gate never touched, and the artifact could not
// tell "loki configured, judged nothing" from "loki never configured".
func TestVerify_ConfiguredLaneWithNoReplayableQueryStillGetsARow(t *testing.T) {
	promSeries := seriesSpec{labels: map[string]string{"job": "api"}, points: []pointSpec{{1_700_000_000, "1"}}}
	promBodies := map[string]string{"up": matrix(promSeries)}
	lanes := map[string]Lane{
		HeadProm: {
			Ref:      NewHTTPBackend(headServer(t, PromDialect(), "query", promBodies).URL),
			Cerberus: NewHTTPBackend(headServer(t, PromDialect(), "query", promBodies).URL),
		},
		// Configured, but the corpus holds only log-stream selectors for it, so
		// these backends are never contacted.
		HeadLoki: {
			Ref:      NewHTTPBackend("http://loki.invalid", WithDialect(LokiDialect())),
			Cerberus: NewHTTPBackend("http://cerberus.invalid", WithDialect(LokiDialect())),
		},
	}
	corpus := Corpus{
		Queries: []Query{{Expr: "up", Source: "rule:up", Head: HeadProm, Lang: "promql"}},
		OutOfScope: []OutOfScopeEntry{
			{Source: "panel:logs", Expr: `{job="api"}`, Head: HeadLoki, Lang: "logql", Kind: KindLogStream, Reason: "returns log lines"},
			{Source: "panel:errors", Expr: `{job="api"} |= "error"`, Head: HeadLoki, Lang: "logql", Kind: KindLogStream, Reason: "returns log lines"},
		},
	}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	byHead := map[string]HeadSummary{}
	for _, h := range rep.Heads {
		byHead[h.Head] = h
	}
	loki, ok := byHead[HeadLoki]
	if !ok {
		t.Fatalf("the configured loki lane must have a row, got %+v", rep.Heads)
	}
	if !loki.Configured {
		t.Error("the loki row must record that the operator configured the lane")
	}
	if loki.Summary.Total != 0 || loki.Summary.OutOfScope != 2 {
		t.Errorf("loki row = %+v, want 0 replayed / 2 out of scope", loki.Summary)
	}
	// A configured lane with nothing to run is not a WRONG answer, so it does not
	// block on its own — but it must never be invisible.
	if rep.Failed() {
		t.Errorf("a configured-but-idle lane alongside a healthy one must not fail, got %+v", rep.Summary)
	}
	idle := rep.IdleLanes()
	if len(idle) != 1 || idle[0].Head != HeadLoki {
		t.Fatalf("IdleLanes = %+v, want exactly the loki lane", idle)
	}
	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(out.String(), "head loki was configured but had no replayable query") {
		t.Errorf("the report must say the configured loki lane was not judged, got:\n%s", out.String())
	}
}

// TestVerify_AllOutOfScopeCorpusFailsRatherThanClaimingSuccess pins the banner an
// all-log-stream corpus used to print: "VERIFICATION PASSED — all 0 queries
// matched", exit 0. A CI job gating a cutover on that exit code would go green
// having judged nothing at all.
func TestVerify_AllOutOfScopeCorpusFailsRatherThanClaimingSuccess(t *testing.T) {
	lanes := map[string]Lane{HeadLoki: {
		Ref:      NewHTTPBackend("http://loki.invalid", WithDialect(LokiDialect())),
		Cerberus: NewHTTPBackend("http://cerberus.invalid", WithDialect(LokiDialect())),
	}}
	corpus := Corpus{OutOfScope: []OutOfScopeEntry{
		{Source: "panel:logs", Expr: `{job="api"}`, Head: HeadLoki, Lang: "logql", Kind: KindLogStream, Reason: "returns log lines"},
	}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	if !rep.JudgedNothing() {
		t.Fatalf("summary = %+v, want a run that replayed nothing", rep.Summary)
	}
	if !rep.Failed() {
		t.Error("a run that replayed 0 queries must fail rather than claim success")
	}
	var out strings.Builder
	if err := rep.WriteText(&out); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(out.String(), "VERIFICATION PASSED") {
		t.Errorf("a run that judged nothing must not print PASSED:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the gate judged nothing") {
		t.Errorf("the banner must say the gate judged nothing, got:\n%s", out.String())
	}
}
