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

// logSelector is a bare LogQL selector with a line filter — the commonest
// Grafana log panel, and the shape cerberus answers with log lines rather than a
// metric matrix.
const logSelector = `{job="api"} |= "error"`

// lokiStreams is the streams body both backends return for logSelector. Cerberus's
// default (non-categorized) value is the two-element [ts, line] array reference
// Loki emits, so one fixture stands in for both sides.
const lokiStreams = `{"status":"success","data":{"resultType":"streams","result":[` +
	`{"stream":{"job":"api"},"values":[` +
	`["1780000000000000001","boom"],["1780000000000000002","kaboom"]]}]},"stats":{}}`

// lokiStreamsShort drops one of the two entries, so a cerberus side using it is a
// real, entry-level divergence rather than a formatting difference.
const lokiStreamsShort = `{"status":"success","data":{"resultType":"streams","result":[` +
	`{"stream":{"job":"api"},"values":[["1780000000000000001","boom"]]}]},"stats":{}}`

// writeLogPanelCorpus writes a corpus of one PromQL rule and one LogQL log panel,
// so a run exercises both a matrix family and a log-stream family on real lanes.
func writeLogPanelCorpus(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "corpus.json")
	body := fmt.Sprintf(`{"version":1,"queries":[`+
		`{"expr":"up","source":"rule:up","kind":"record","lang":"promql"},`+
		`{"expr":%s,"source":"panel:api-logs","kind":"panel","lang":"logql"}`+
		`],"skipped":[]}`, strconv.Quote(logSelector))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// lokiLogServer answers the Loki query_range endpoint with the streams body,
// asserting the log-query parameters the replay must pin. A backend that received
// no limit or no direction would truncate on its own defaults, and the two sides
// would answer different questions while still looking comparable.
func lokiLogServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveIfDiscoveryProbe(w, r) {
			return
		}
		const path = "/loki/api/v1/query_range"
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"error":"this backend serves only %s, got %s"}`, path, r.URL.Path)
			return
		}
		q := r.URL.Query()
		if q.Get("query") != logSelector {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":"unknown query %q"}`, q.Get("query"))
			return
		}
		if q.Get("limit") == "" || q.Get("direction") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":"log query must pin limit and direction, got limit=%q direction=%q"}`,
				q.Get("limit"), q.Get("direction"))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunVerify_LogPanelLaneReplaysAndReportsItsOwnEvidence drives the whole CLI
// over a corpus holding one metric rule and one log panel, and pins that the run
// judged BOTH — with each family's evidence reported in its own unit.
//
// A run that quietly dropped the log panel would still print a green banner and
// exit 0: the prom lane matched. Only the per-family accounting shows whether the
// operator's log panels were proved at all.
func TestRunVerify_LogPanelLaneReplaysAndReportsItsOwnEvidence(t *testing.T) {
	dir := t.TempDir()
	corpus := writeLogPanelCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	lokiRef := lokiLogServer(t, lokiStreams)
	lokiCer := lokiLogServer(t, lokiStreams)

	report := filepath.Join(dir, "verify-report.json")
	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
		"--report", report,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err != nil {
		t.Fatalf("a matching metric lane plus a matching log lane must pass: %v\n%s", err, out.String())
	}

	s := out.String()
	// 1 prom rule + 1 loki log panel + the two discovery probes the log panel's
	// {job="api"} matcher anchors (an unfiltered "labels" probe, plus one
	// "label-values" probe for the one matcher key "job" — see
	// discoveryProbesForCorpus).
	if !strings.Contains(s, "VERIFICATION PASSED — all 4 queries matched") {
		t.Errorf("both the rule and the log panel (plus their anchored discovery probes) must be judged, got:\n%s", s)
	}
	// The per-head table states each lane's evidence in ITS OWN unit. "2 series
	// compared" on a lane that diffed log lines would be a false statement about
	// what was proved, and "1 log entry compared" about the prom lane equally so.
	// Loki now carries TWO families (log-stream + the anchored tag-discovery), so
	// its head row itself omits the compact "N compared" clause (see
	// Report.writeHeadTable) — the log-entry count lives on the log-stream
	// FAMILY sub-row instead, which headTableRows deliberately skips.
	rows := headTableRows(t, s)
	if !strings.Contains(s, "log-stream") || !strings.Contains(s, "2 log entries compared") {
		t.Errorf("the loki log-stream family row must count log entries, got:\n%s", s)
	}
	if !strings.Contains(rows[migrateverify.HeadProm], "1 series compared") {
		t.Errorf("the prom row must still count series, got %q", rows[migrateverify.HeadProm])
	}
	// And what was NOT judged is stated with the count it covers.
	if !strings.Contains(s, "== not judged") {
		t.Errorf("a run with limitations must print the not-judged section, got:\n%s", s)
	}
	for _, want := range []string{"[log-entry-order] 2:", "[log-structured-metadata] 2:"} {
		if !strings.Contains(s, want) {
			t.Errorf("the not-judged section must name each dimension with its count (%q), got:\n%s", want, s)
		}
	}

	// The artifact carries the same split, and the replay parameters that produced it.
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var diag struct {
		SchemaVersion int `json:"schema_version"`
		Params        struct {
			Replay migrateverify.VerifyReportReplay `json:"replay"`
		} `json:"params"`
		Heads []migrateverify.HeadSummary `json:"heads"`
	}
	if err := json.Unmarshal(raw, &diag); err != nil {
		t.Fatalf("report must be JSON: %v", err)
	}
	if diag.Params.Replay != migrateverify.ReplayParams() {
		t.Errorf("params.replay = %+v, want the pinned replay parameters %+v: an artifact must say how strict the run was",
			diag.Params.Replay, migrateverify.ReplayParams())
	}
	var logFamily *migrateverify.FamilySummary
	for _, h := range diag.Heads {
		if h.Head != migrateverify.HeadLoki {
			continue
		}
		for i := range h.Families {
			if h.Families[i].Kind == migrateverify.KindLogStream {
				logFamily = &h.Families[i]
			}
		}
	}
	if logFamily == nil {
		t.Fatalf("the artifact carries no loki log-stream family: %+v", diag.Heads)
	}
	if logFamily.Compared != 2 || logFamily.Unit != migrateverify.UnitLogEntries || logFamily.Match != 1 {
		t.Errorf("log-stream family = %+v, want 2 log entries compared on one match", *logFamily)
	}
}

// TestRunVerify_LogPanelDivergenceFailsTheRun pins the other half end to end: an
// entry cerberus did not return is a blocking divergence, anchored at that exact
// entry with its nanosecond timestamp intact.
func TestRunVerify_LogPanelDivergenceFailsTheRun(t *testing.T) {
	dir := t.TempDir()
	corpus := writeLogPanelCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	lokiRef := lokiLogServer(t, lokiStreams)
	lokiCer := lokiLogServer(t, lokiStreamsShort)

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err == nil {
		t.Fatalf("a missing log entry must fail the run, got success:\n%s", out.String())
	}

	s := out.String()
	if !strings.Contains(s, `first-diff: stream={job="api"} ts=1780000000000000002ns`) {
		t.Errorf("the divergence must be anchored at its exact entry, with the nanosecond timestamp intact, got:\n%s", s)
	}
	if !strings.Contains(s, "cerberus=<missing entry>") {
		t.Errorf("the diff must name the side that did not return the entry, got:\n%s", s)
	}
	// The float rendering the matrix lane uses would print this epoch as 1.78e+18,
	// which points at no entry at all.
	if strings.Contains(s, "1.78e+18") {
		t.Errorf("a nanosecond timestamp must never be rendered through the float path, got:\n%s", s)
	}
}
