package migrateverify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// headServer answers ONE head's range endpoint with the supplied per-query body.
//
// It asserts the requested path is exactly the dialect's endpoint and 404s
// anything else. That assertion is the point of the multi-head tests: a dialect
// pointed at the wrong path gets a 4xx, which verifyOne records as the
// non-blocking "unsupported" verdict — i.e. a green-looking gate that compared
// nothing. A shared handler that ignored r.URL.Path could not catch that.
func headServer(t *testing.T, d Dialect, queryParam string, byQuery map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != d.Path() {
			t.Errorf("%s backend got path %q, want %q", d.Name(), r.URL.Path, d.Path())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		expr := r.URL.Query().Get(queryParam)
		body, ok := byQuery[expr]
		if !ok {
			t.Errorf("%s backend got unexpected %s=%q", d.Name(), queryParam, expr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// lokiEnvelope wraps a Prometheus matrix in the FULL Loki envelope — stats,
// warnings, encodingFlags — so the test proves the shared Prometheus decoder
// ignores Loki's extra fields rather than merely tolerating a stripped-down body.
func lokiEnvelope(series ...seriesSpec) string {
	inner := matrix(series...)
	// Splice Loki's extras into the matrix body: `data` gains `stats` and the
	// envelope gains `warnings` / `encodingFlags`.
	withStats := strings.Replace(inner, `"resultType":"matrix"`,
		`"resultType":"matrix","stats":{"summary":{"execTime":0.01,"totalBytesProcessed":1234}}`, 1)
	return strings.TrimSuffix(withStats, "}") +
		`,"warnings":["some limit was applied"],"encodingFlags":["categorize-labels"]}`
}

// tempoBody builds a Tempo metrics-range body in the REFERENCE wire shape: quoted
// millisecond timestamps, typed AnyValue labels, and zero values omitted.
func tempoBody(labels map[string]string, samples []tempoPointSpec) string {
	var b strings.Builder
	b.WriteString(`{"series":[{"labels":[`)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"key":"` + k + `","value":{"stringValue":"` + labels[k] + `"}}`)
	}
	b.WriteString(`],"samples":[`)
	for i, s := range samples {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"timestampMs":"` + s.tsMs + `"`)
		if s.value != "" {
			b.WriteString(`,"value":` + s.value)
		}
		b.WriteString("}")
	}
	b.WriteString(`]}]}`)
	return b.String()
}

// tempoPointSpec is one Tempo sample; an empty value means the key is OMITTED,
// which is how reference Tempo ships a zero.
type tempoPointSpec struct {
	tsMs  string
	value string
}

// TestVerify_RoutesEachHeadToItsOwnLane pins that a mixed corpus reaches three
// separate backend pairs, each on its own endpoint with its own encoding — the
// query is never issued against another head's API.
func TestVerify_RoutesEachHeadToItsOwnLane(t *testing.T) {
	promSeries := seriesSpec{labels: map[string]string{"job": "api"}, points: []pointSpec{{1_700_000_000, "1"}}}
	promBodies := map[string]string{"up": matrix(promSeries)}
	lokiBodies := map[string]string{`sum(rate({job="api"}[5m]))`: lokiEnvelope(promSeries)}
	tempoBodies := map[string]string{
		"{} | rate()": tempoBody(map[string]string{"span.name": "GET /x"},
			[]tempoPointSpec{{tsMs: "1700000000000", value: "1"}}),
	}

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
	if rep.Summary.Total != 3 || rep.Summary.Match != 3 {
		t.Fatalf("summary = %+v, want 3 replayed / 3 match", rep.Summary)
	}
	if rep.Failed() {
		t.Error("three matching lanes must pass the gate")
	}
	if len(rep.Heads) != 3 {
		t.Fatalf("Heads = %+v, want one entry per head", rep.Heads)
	}
	for i, want := range []string{HeadLoki, HeadProm, HeadTempo} {
		if rep.Heads[i].Head != want {
			t.Errorf("Heads[%d] = %q, want %q (head-token sorted)", i, rep.Heads[i].Head, want)
		}
		if rep.Heads[i].Summary.Match != 1 || rep.Heads[i].Summary.Total != 1 {
			t.Errorf("Heads[%d] (%s) summary = %+v, want 1 replayed / 1 match", i, rep.Heads[i].Head, rep.Heads[i].Summary)
		}
	}
}

// TestVerify_UnconfiguredHeadIsReportedNotDropped pins the three load-bearing
// halves of the unconfigured bucket: the entry is ENUMERATED with its head and a
// reason, the gate FAILS (never a non-blocking caveat), and the query is NOT
// replayed against the one lane that happens to be configured.
func TestVerify_UnconfiguredHeadIsReportedNotDropped(t *testing.T) {
	var seen []string
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("query"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(matrix(seriesSpec{
			labels: map[string]string{"job": "api"}, points: []pointSpec{{1_700_000_000, "1"}},
		})))
	}))
	t.Cleanup(promSrv.Close)

	lanes := map[string]Lane{HeadProm: {
		Ref:      NewHTTPBackend(promSrv.URL),
		Cerberus: NewHTTPBackend(promSrv.URL),
	}}
	const logqlExpr = `sum(rate({job="api"}[5m]))`
	corpus := Corpus{Queries: []Query{
		{Expr: "up", Source: "rule:up", Head: HeadProm, Lang: "promql"},
		{Expr: logqlExpr, Source: "panel:lograte", Head: HeadLoki, Lang: "logql"},
	}}

	rep := Verify(context.Background(), corpus, lanes, testParams())

	if rep.Summary.Total != 1 {
		t.Errorf("Total = %d, want 1: an unconfigured query was never replayed, so it must not inflate the denominator", rep.Summary.Total)
	}
	if rep.Summary.Unconfigured != 1 {
		t.Errorf("Unconfigured = %d, want 1", rep.Summary.Unconfigured)
	}
	if len(rep.Unconfigured) != 1 {
		t.Fatalf("Unconfigured = %+v, want the one loki query enumerated", rep.Unconfigured)
	}
	e := rep.Unconfigured[0]
	if e.Head != HeadLoki || e.Lang != "logql" || e.Expr != logqlExpr || e.Source != "panel:lograte" {
		t.Errorf("Unconfigured[0] = %+v, want the loki panel with its head/lang/expr", e)
	}
	if !strings.Contains(e.Reason, "head=loki") || !strings.Contains(e.Reason, "did not judge") {
		t.Errorf("Unconfigured[0].Reason = %q, want it to name the head and state the gate did not judge it", e.Reason)
	}
	if !rep.Failed() {
		t.Error("an unconfigured lane must FAIL the gate: parity cannot be claimed for a query that was never run")
	}
	// The LogQL expression must never have been issued to the Prometheus backend.
	for _, q := range seen {
		if q == logqlExpr {
			t.Fatalf("the logql query was replayed against the prom backend (%v): a query must never cross head lanes", seen)
		}
	}
	if len(seen) != 2 || seen[0] != "up" || seen[1] != "up" {
		t.Errorf("prom backend saw %v, want exactly the `up` reference + cerberus pair", seen)
	}
}

// TestVerify_TempoLaneComparesMatrix pins that the Tempo lane compares real data
// across the TWO DIFFERENT wire shapes the two sides emit: the reference ships
// quoted millisecond timestamps and omits a zero value, cerberus ships numeric
// timestamps and an explicit zero. Feeding both sides identical bytes would pass
// even if the decoder ignored the difference, so the bodies are deliberately
// asymmetric.
func TestVerify_TempoLaneComparesMatrix(t *testing.T) {
	const expr = "{} | rate()"
	refBody := tempoBody(map[string]string{"span.name": "GET /x"}, []tempoPointSpec{
		{tsMs: "1700000000000", value: "1.5"},
		{tsMs: "1700000060000"}, // zero omitted, the reference's own encoding
	})
	const cerBody = `{"series":[{"labels":[{"key":"span.name","value":{"stringValue":"GET /x"}}],` +
		`"samples":[{"timestampMs":1700000000000,"value":1.5},{"timestampMs":1700000060000,"value":0}],` +
		`"exemplars":[]}]}`
	if refBody == cerBody {
		t.Fatal("the two sides must use DIFFERENT wire encodings, else the test proves nothing about the decoder")
	}

	lanes := map[string]Lane{HeadTempo: {
		Ref:      NewHTTPBackend(headServer(t, TempoDialect(), "q", map[string]string{expr: refBody}).URL, WithDialect(TempoDialect())),
		Cerberus: NewHTTPBackend(headServer(t, TempoDialect(), "q", map[string]string{expr: cerBody}).URL, WithDialect(TempoDialect())),
	}}
	corpus := Corpus{Queries: []Query{{Expr: expr, Source: "panel:spanrate", Head: HeadTempo, Lang: "traceql"}}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (detail: %s, first-diff: %+v)", res.Verdict, res.Detail, res.FirstDiff)
	}
	if rep.Summary.Match != 1 {
		t.Errorf("summary = %+v, want 1 match", rep.Summary)
	}
}

// TestVerify_TempoPartialReferenceIsError pins that a TRUNCATED reference result
// is an error, not a divergence: blaming cerberus for the reference's own
// truncation would manufacture a false divergence and send the operator hunting a
// cerberus bug that does not exist.
func TestVerify_TempoPartialReferenceIsError(t *testing.T) {
	const expr = "{} | rate()"
	const refPartial = `{"status":"PARTIAL","message":"exceeded max blocks","series":[]}`
	cerBody := tempoBody(map[string]string{"span.name": "GET /x"},
		[]tempoPointSpec{{tsMs: "1700000000000", value: "1"}})

	lanes := map[string]Lane{HeadTempo: {
		Ref:      NewHTTPBackend(headServer(t, TempoDialect(), "q", map[string]string{expr: refPartial}).URL, WithDialect(TempoDialect())),
		Cerberus: NewHTTPBackend(headServer(t, TempoDialect(), "q", map[string]string{expr: cerBody}).URL, WithDialect(TempoDialect())),
	}}
	corpus := Corpus{Queries: []Query{{Expr: expr, Source: "panel:spanrate", Head: HeadTempo, Lang: "traceql"}}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	res := rep.Results[0]
	if res.Verdict != VerdictError {
		t.Fatalf("verdict = %q, want error (a truncated baseline is not a divergence)", res.Verdict)
	}
	if !strings.Contains(res.Detail, "PARTIAL") {
		t.Errorf("detail = %q, want it to name the PARTIAL status", res.Detail)
	}
	if !rep.Failed() {
		t.Error("an untrustworthy baseline must fail the gate")
	}
}

// TestVerify_LokiLaneReusesPromDecoder pins that a full Loki envelope — stats,
// warnings, encodingFlags around the matrix — decodes through the shared
// Prometheus decoder, so no second decoder is needed and Loki's extras are simply
// not read.
func TestVerify_LokiLaneReusesPromDecoder(t *testing.T) {
	const expr = `sum(rate({job="api"}[5m]))`
	series := seriesSpec{labels: map[string]string{"job": "api"}, points: []pointSpec{{1_700_000_000, "2"}}}
	body := lokiEnvelope(series)
	if !strings.Contains(body, "encodingFlags") || !strings.Contains(body, `"stats"`) {
		t.Fatalf("the fixture must carry Loki's extra fields, got %s", body)
	}
	bodies := map[string]string{expr: body}

	lanes := map[string]Lane{HeadLoki: {
		Ref:      NewHTTPBackend(headServer(t, LokiDialect(), "query", bodies).URL, WithDialect(LokiDialect())),
		Cerberus: NewHTTPBackend(headServer(t, LokiDialect(), "query", bodies).URL, WithDialect(LokiDialect())),
	}}
	corpus := Corpus{Queries: []Query{{Expr: expr, Source: "panel:lograte", Head: HeadLoki, Lang: "logql"}}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	res := rep.Results[0]
	if res.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (detail: %s)", res.Verdict, res.Detail)
	}
	if res.Head != HeadLoki {
		t.Errorf("result head = %q, want %q", res.Head, HeadLoki)
	}
}

// TestVerify_ResultCarriesHead pins that every verdict names its lane, in the
// JSON result AND in the human report line — a divergence an operator cannot
// attribute to a head is a divergence they cannot triage.
func TestVerify_ResultCarriesHead(t *testing.T) {
	const expr = `sum(rate({job="api"}[5m]))`
	refBody := map[string]string{expr: lokiEnvelope(seriesSpec{
		labels: map[string]string{"job": "api"}, points: []pointSpec{{1_700_000_000, "1"}},
	})}
	cerBody := map[string]string{expr: lokiEnvelope(seriesSpec{
		labels: map[string]string{"job": "api"}, points: []pointSpec{{1_700_000_000, "9"}},
	})}

	lanes := map[string]Lane{HeadLoki: {
		Ref:      NewHTTPBackend(headServer(t, LokiDialect(), "query", refBody).URL, WithDialect(LokiDialect())),
		Cerberus: NewHTTPBackend(headServer(t, LokiDialect(), "query", cerBody).URL, WithDialect(LokiDialect())),
	}}
	corpus := Corpus{Queries: []Query{{Expr: expr, Source: "panel:lograte", Head: HeadLoki, Lang: "logql"}}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	for i, res := range rep.Results {
		if res.Head == "" {
			t.Errorf("Results[%d] carries no head", i)
		}
		if res.Head != HeadLoki {
			t.Errorf("Results[%d].Head = %q, want %q", i, res.Head, HeadLoki)
		}
	}
	var text strings.Builder
	if err := rep.WriteText(&text); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(text.String(), "[diverge] loki panel:lograte") {
		t.Errorf("the diverging result line must name its head, got:\n%s", text.String())
	}
}

// TestVerify_AttributionOnlyOnPromLane pins that candidate-cause attribution is
// gated to the Prometheus lane. Its hotspot matcher keys on bare rate / increase /
// histogram_quantile tokens, which occur verbatim in LogQL and TraceQL, and its
// note text describes a PromQL-only experimental CH path — so firing it elsewhere
// prints a confidently wrong cause. Both halves are asserted: silent on loki, and
// still present on prom.
func TestVerify_AttributionOnlyOnPromLane(t *testing.T) {
	const logqlExpr = `sum(rate({job="a"}[5m]))`
	lokiRef := map[string]string{logqlExpr: lokiEnvelope(seriesSpec{
		labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}},
	})}
	lokiCer := map[string]string{logqlExpr: lokiEnvelope(seriesSpec{
		labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "9"}},
	})}
	const promExpr = "rate(x[5m])"
	promRef := map[string]string{promExpr: matrix(seriesSpec{
		labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "1"}},
	})}
	promCer := map[string]string{promExpr: matrix(seriesSpec{
		labels: map[string]string{"job": "a"}, points: []pointSpec{{1_700_000_000, "9"}},
	})}

	lanes := map[string]Lane{
		HeadLoki: {
			Ref:      NewHTTPBackend(headServer(t, LokiDialect(), "query", lokiRef).URL, WithDialect(LokiDialect())),
			Cerberus: NewHTTPBackend(headServer(t, LokiDialect(), "query", lokiCer).URL, WithDialect(LokiDialect())),
		},
		HeadProm: {
			Ref:      NewHTTPBackend(headServer(t, PromDialect(), "query", promRef).URL),
			Cerberus: NewHTTPBackend(headServer(t, PromDialect(), "query", promCer).URL),
		},
	}
	corpus := Corpus{Queries: []Query{
		{Expr: logqlExpr, Source: "panel:lograte", Head: HeadLoki, Lang: "logql"},
		{Expr: promExpr, Source: "rule:r", Head: HeadProm, Lang: "promql"},
	}}

	rep := Verify(context.Background(), corpus, lanes, testParams())
	if len(rep.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(rep.Results))
	}
	loki, prom := rep.Results[0], rep.Results[1]
	if loki.Verdict != VerdictDiverge || prom.Verdict != VerdictDiverge {
		t.Fatalf("both queries must diverge, got loki=%q prom=%q", loki.Verdict, prom.Verdict)
	}
	if len(loki.Attribution) != 0 {
		t.Errorf("loki divergence carries PromQL attribution %+v: its hotspot matcher and note text are PromQL-only and would be confidently wrong here", loki.Attribution)
	}
	if len(prom.Attribution) == 0 {
		t.Error("prom divergence must still carry candidate-cause attribution")
	}
}

// TestReport_JSONIsDeterministic pins that two marshals of the same report are
// byte-identical and that Heads is head-token sorted, so a re-run of the same
// inputs produces a diffable artifact rather than churning on Go's map order.
func TestReport_JSONIsDeterministic(t *testing.T) {
	rep := Report{
		SchemaVersion: ReportVersion,
		Summary:       Summary{Total: 2, Match: 2},
		Heads: sortedHeadSummaries(map[string]*Summary{
			HeadTempo: {Total: 1, Match: 1, ComparedSeries: 1},
			HeadLoki:  {Total: 1, Match: 1, ComparedSeries: 1},
			HeadProm:  {Total: 0},
		}, map[string]Lane{HeadTempo: {}, HeadLoki: {}}),
	}
	var first, second strings.Builder
	if err := rep.WriteJSON(&first); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if err := rep.WriteJSON(&second); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("two marshals of the same report differ:\n%s\nvs\n%s", first.String(), second.String())
	}
	wantOrder := []string{HeadLoki, HeadProm, HeadTempo}
	if len(rep.Heads) != len(wantOrder) {
		t.Fatalf("Heads = %+v, want 3 entries", rep.Heads)
	}
	for i, want := range wantOrder {
		if rep.Heads[i].Head != want {
			t.Errorf("Heads[%d] = %q, want %q", i, rep.Heads[i].Head, want)
		}
	}
}

// TestWriteText_ShowsUnconfiguredAndOutOfScopeReasons pins that the human report
// carries the PER-ENTRY accounting, not just section headers: an operator must be
// able to read exactly how many of their queries the gate did not judge and why.
func TestWriteText_ShowsUnconfiguredAndOutOfScopeReasons(t *testing.T) {
	rep := Report{
		SchemaVersion: ReportVersion,
		Summary:       Summary{Total: 1, Match: 1, Unconfigured: 1, OutOfScope: 2},
		Heads: sortedHeadSummaries(map[string]*Summary{
			HeadProm: {Total: 1, Match: 1, ComparedSeries: 1},
			HeadLoki: {Unconfigured: 1},
		}, map[string]Lane{HeadProm: {}}),
		Results: []QueryResult{{Head: HeadProm, Source: "rule:up", Expr: "up", Verdict: VerdictMatch}},
		Unconfigured: []UnconfiguredEntry{{
			Source: "panel:lograte", Expr: `sum(rate({job="a"}[5m]))`, Head: HeadLoki, Lang: "logql",
			Reason: "head=loki has replayable logql queries but no reference/cerberus backend pair was configured for it; the gate did not judge them",
		}},
		OutOfScope: []OutOfScopeEntry{
			{
				Source: "panel:logs", Expr: `{job="a"}`, Head: HeadLoki, Lang: "logql", Kind: KindLogStream,
				Reason: "lang=logql kind=log-stream: this query returns log lines, not a metric matrix",
			},
			{
				Source: "panel:weird", Expr: "MATCH (n)", Lang: "cypher", Kind: KindUnknownLang,
				Reason: `lang="cypher": this build has no metric lane for that query language`,
			},
		},
	}

	var buf strings.Builder
	if err := rep.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		// The unconfigured section, its count, the head, and the full reason.
		"== unconfigured (1)",
		"loki  panel:lograte",
		"no reference/cerberus backend pair was configured",
		// The out-of-scope section with per-entry kind + reason.
		"== out of scope (2)",
		"loki/logql log-stream panel:logs",
		"returns log lines, not a metric matrix",
		"lang=cypher unknown-lang panel:weird",
		"no metric lane for that query language",
		// The per-head roll-up, so a healthy head cannot mask a dead one.
		"# per-head lanes:",
		// The verdict must be FAILED and name the unconfigured cause.
		"VERIFICATION FAILED",
		"unconfigured lane",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q, got:\n%s", want, out)
		}
	}
}

// TestVerify_TransportErrorRedactsCredentialsAllDialects extends the credential
// redaction pin to EVERY head. redactTransportErr and the bearer/org-id headers
// live in one QueryRange body precisely so this one test covers all three lanes;
// if a future change forks QueryRange per head, this test is what catches the
// resulting unpinned credential-leak paths.
func TestVerify_TransportErrorRedactsCredentialsAllDialects(t *testing.T) {
	const (
		user = "s3cr3t-token"
		pass = "hunter2-pw"
	)
	cases := []struct {
		head    string
		dialect Dialect
		lang    string
		expr    string
	}{
		{HeadProm, PromDialect(), "promql", "up"},
		{HeadLoki, LokiDialect(), "logql", `sum(rate({job="a"}[5m]))`},
		{HeadTempo, TempoDialect(), "traceql", "{} | rate()"},
	}
	for _, tc := range cases {
		t.Run(tc.head, func(t *testing.T) {
			addr := strings.TrimPrefix(deadBackendURL(t), "http://")
			badURL := "http://" + user + ":" + pass + "@" + addr
			lanes := map[string]Lane{tc.head: {
				Ref:      NewHTTPBackend(badURL, WithDialect(tc.dialect)),
				Cerberus: NewHTTPBackend(badURL, WithDialect(tc.dialect)),
			}}
			corpus := Corpus{Queries: []Query{{Expr: tc.expr, Source: "s", Head: tc.head, Lang: tc.lang}}}
			rep := Verify(context.Background(), corpus, lanes, testParams())
			res := rep.Results[0]
			if res.Verdict != VerdictError {
				t.Fatalf("verdict = %q, want error", res.Verdict)
			}
			if strings.Contains(res.Detail, user) || strings.Contains(res.Detail, pass) {
				t.Fatalf("%s lane leaked credentials into the verdict Detail: %q", tc.head, res.Detail)
			}
			if !strings.Contains(res.Detail, redactedUserinfo) {
				t.Errorf("%s lane Detail should show the userinfo was redacted (%q), got %q", tc.head, redactedUserinfo, res.Detail)
			}
		})
	}
}

// TestWithOrgID_SendsTenantHeader pins that the tenant header reaches the wire
// (and is absent when unset), so a multi-tenant reference Loki / Tempo answers
// instead of 401ing the whole lane into an error verdict indistinguishable from a
// real parity failure.
func TestWithOrgID_SendsTenantHeader(t *testing.T) {
	var gotOrg string
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrg = r.Header.Get(scopeOrgIDHeader)
		_, sawHeader = r.Header[http.CanonicalHeaderKey(scopeOrgIDHeader)]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	t.Cleanup(srv.Close)

	b := NewHTTPBackend(srv.URL, WithDialect(LokiDialect()), WithOrgID("tenant-7"))
	if _, err := b.QueryRange(context.Background(), `sum(rate({j="a"}[5m]))`, testParams()); err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if gotOrg != "tenant-7" {
		t.Errorf("%s = %q, want tenant-7", scopeOrgIDHeader, gotOrg)
	}

	gotOrg, sawHeader = "", false
	plain := NewHTTPBackend(srv.URL, WithDialect(LokiDialect()))
	if _, err := plain.QueryRange(context.Background(), `sum(rate({j="a"}[5m]))`, testParams()); err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if sawHeader {
		t.Errorf("an unset org-id must send NO %s header, got %q", scopeOrgIDHeader, gotOrg)
	}
}
