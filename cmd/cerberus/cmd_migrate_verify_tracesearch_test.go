package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrateverify"
)

// spansetSearch is a bare TraceQL spanset filter — the commonest Grafana trace
// panel, and the shape both backends answer with trace summaries rather than a
// metric matrix.
const spansetSearch = `{ span.http.status_code = 500 }`

// tempoRefSearch is the REFERENCE Tempo shape for one matching trace: the trace ID
// with its leading zeros stripped, 64-bit fields quoted, and job counters in the
// metrics block.
const tempoRefSearch = `{"traces":[{"traceID":"a1b2","rootServiceName":"checkout",` +
	`"rootTraceName":"GET /cart","startTimeUnixNano":"1780000000000000001","durationMs":42,` +
	`"spanSets":[{"spans":[{"spanID":"c3","startTimeUnixNano":"1780000000000000001",` +
	`"durationNanos":"42000000"}],"matched":1}]}],` +
	`"metrics":{"totalJobs":6,"completedJobs":6,"totalBlockBytes":"81920"}}`

// tempoCerSearch is cerberus's own shape for the SAME trace: fixed-width lowercase
// identifiers, the legacy spanSet field emitted next to spanSets, and an aggregate
// block of hard zeros. The two bodies are deliberately NOT byte-identical, so a
// comparator that keyed on the raw identifier strings or diffed the metrics block
// could not pass on them.
const tempoCerSearch = `{"traces":[{"traceID":"0000000000000000000000000000a1b2",` +
	`"rootServiceName":"checkout","rootTraceName":"GET /cart",` +
	`"startTimeUnixNano":"1780000000000000001","durationMs":42,` +
	`"spanSet":{"spans":[{"spanID":"00000000000000c3","startTimeUnixNano":"1780000000000000001",` +
	`"durationNanos":"42000000"}],"matched":1},` +
	`"spanSets":[{"spans":[{"spanID":"00000000000000c3","startTimeUnixNano":"1780000000000000001",` +
	`"durationNanos":"42000000"}],"matched":1}]}],` +
	`"metrics":{"inspectedTraces":3,"inspectedBytes":0,"totalBlocks":0}}`

// tempoCerSearchWrongService renames the root service on the trace both backends
// return, which is a real defect on a dimension that is a pure function of the
// query and the data.
const tempoCerSearchWrongService = `{"traces":[{"traceID":"0000000000000000000000000000a1b2",` +
	`"rootServiceName":"checkout-v2","rootTraceName":"GET /cart",` +
	`"startTimeUnixNano":"1780000000000000001","durationMs":42,` +
	`"spanSets":[{"spans":[{"spanID":"00000000000000c3","startTimeUnixNano":"1780000000000000001",` +
	`"durationNanos":"42000000"}],"matched":1}]}],"metrics":{}}`

// writeTracePanelCorpus writes a corpus of one PromQL rule and one TraceQL search
// panel, so the run exercises both the matrix family and the trace-search family on
// real lanes.
func writeTracePanelCorpus(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "corpus.json")
	body := fmt.Sprintf(`{"version":1,"queries":[`+
		`{"expr":"up","source":"rule:up","kind":"record","lang":"promql"},`+
		`{"expr":%s,"source":"panel:failed-checkouts","kind":"panel","lang":"traceql"}`+
		`],"skipped":[]}`, strconv.Quote(spansetSearch))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tempoSearchStub answers Tempo's search endpoint with a fixed body, and rejects a
// probe that did not pin the window, the limit and the spans-per-set. The rejection
// is load-bearing: an unpinned probe is answered by both backends, so it fails
// silently rather than loudly unless the stub says so.
func tempoSearchStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"error":"this backend serves only /api/search, got %s"}`, r.URL.Path)
			return
		}
		q := r.URL.Query()
		if q.Get("q") != spansetSearch {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":"unknown query %q under q"}`, q.Get("q"))
			return
		}
		for _, required := range []string{"start", "end", "limit", "spss"} {
			if q.Get(required) == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":"a trace search must pin %s"}`, required)
				return
			}
		}
		// Unix seconds, not RFC3339: an instant this parser cannot read would
		// otherwise degrade to an empty result on both sides.
		for _, bound := range []string{"start", "end"} {
			if _, err := strconv.ParseInt(q.Get(bound), 10, 64); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":"%s=%q is not Unix seconds"}`, bound, q.Get(bound))
				return
			}
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunVerify_TracePanelLaneReplaysAndReportsItsOwnEvidence drives the whole CLI
// over a corpus holding one metric rule and one trace-search panel, and pins that
// the run judged BOTH — with each family's evidence reported in its own unit.
//
// A run that quietly dropped the trace panel would still print the green banner and
// exit 0, because the prom lane matched. Only the per-family accounting shows
// whether the operator's trace panels were proved at all.
func TestRunVerify_TracePanelLaneReplaysAndReportsItsOwnEvidence(t *testing.T) {
	dir := t.TempDir()
	corpus := writeTracePanelCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	tempoRef := tempoSearchStub(t, tempoRefSearch)
	tempoCer := tempoSearchStub(t, tempoCerSearch)

	report := filepath.Join(dir, "verify-report.json")
	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-tempo", tempoRef.URL, "--cerberus-tempo", tempoCer.URL,
		"--report", report,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err != nil {
		t.Fatalf("a matching run must succeed: %v\n%s", err, out.String())
	}

	rows := headTableRows(t, out.String())
	tempoRow, ok := rows[migrateverify.HeadTempo]
	if !ok {
		t.Fatalf("the report must carry a tempo lane row, got:\n%s", out.String())
	}
	if !strings.Contains(tempoRow, "1 trace") || !strings.Contains(tempoRow, migrateverify.UnitTraces) {
		t.Errorf("the tempo row must count its evidence in %q, got %q", migrateverify.UnitTraces, tempoRow)
	}
	if !strings.Contains(rows[migrateverify.HeadProm], "1 series compared") {
		t.Errorf("the prom row must still count series, got %q", rows[migrateverify.HeadProm])
	}

	// The artifact carries the same split, and the replay parameters that produced it.
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var diag struct {
		Params struct {
			Replay migrateverify.VerifyReportReplay `json:"replay"`
		} `json:"params"`
		Heads []migrateverify.HeadSummary `json:"heads"`
	}
	if err := json.Unmarshal(raw, &diag); err != nil {
		t.Fatalf("report must be JSON: %v", err)
	}
	if diag.Params.Replay != migrateverify.ReplayParams() {
		t.Errorf("params.replay = %+v, want the pinned replay parameters %+v: the artifact must say how strict the run was",
			diag.Params.Replay, migrateverify.ReplayParams())
	}
	var searchFamily *migrateverify.FamilySummary
	for _, h := range diag.Heads {
		if h.Head != migrateverify.HeadTempo {
			continue
		}
		for i := range h.Families {
			if h.Families[i].Kind == migrateverify.KindTraceSearch {
				searchFamily = &h.Families[i]
			}
		}
	}
	if searchFamily == nil {
		t.Fatalf("the artifact must carry a tempo trace-search family, got %+v", diag.Heads)
	}
	if searchFamily.Compared != 1 || searchFamily.Match != 1 || searchFamily.Unit != migrateverify.UnitTraces {
		t.Errorf("trace-search family = %+v, want one match over one compared trace in %q",
			*searchFamily, migrateverify.UnitTraces)
	}
}

// TestRunVerify_TracePanelDivergenceFailsTheRun pins that a real trace-summary
// difference reaches the operator anchored at the trace it happened on, and that
// the run exits non-zero.
func TestRunVerify_TracePanelDivergenceFailsTheRun(t *testing.T) {
	dir := t.TempDir()
	corpus := writeTracePanelCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	tempoRef := tempoSearchStub(t, tempoRefSearch)
	tempoCer := tempoSearchStub(t, tempoCerSearchWrongService)

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-tempo", tempoRef.URL, "--cerberus-tempo", tempoCer.URL,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err == nil {
		t.Fatalf("a differing root service must fail the run, got success:\n%s", out.String())
	}

	s := out.String()
	want := "first-diff: trace=0000000000000000000000000000a1b2 field=rootServiceName"
	if !strings.Contains(s, want) {
		t.Errorf("the divergence must be anchored at its trace and field (%q), got:\n%s", want, s)
	}
	if !strings.Contains(s, "ref=checkout cerberus=checkout-v2") {
		t.Errorf("the diff must print both backends' own renderings, got:\n%s", s)
	}
	// The matrix lane's anchor names a series and a float timestamp, neither of
	// which exists on this shape.
	if strings.Contains(s, "first-diff: series=") {
		t.Errorf("a trace-search divergence must not borrow the matrix anchor, got:\n%s", s)
	}
}
