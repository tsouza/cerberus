package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// logStreamSelector is the commonest LogQL panel shape and the commonest TraceQL
// one: a bare stream selector and a bare spanset search. Both return rows, not a
// metric matrix, so both route out of scope — which is honest, and is exactly why a
// corpus made only of them must not read as a passing parity gate.
const (
	logStreamSelector = `{job="api"} |= "error"`
	traceSearchQuery  = `{ span.http.status_code = 500 }`
)

// writeAllOutOfScopeCorpus writes a corpus whose every entry is a shape no metric
// lane can judge — the realistic output of harvesting a Loki/Tempo-heavy Grafana
// export.
func writeAllOutOfScopeCorpus(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "corpus.json")
	body := fmt.Sprintf(`{"version":1,"queries":[`+
		`{"expr":%s,"source":"panel:api-logs","kind":"panel","lang":"logql"},`+
		`{"expr":%s,"source":"panel:errors","kind":"panel","lang":"traceql"}`+
		`],"skipped":[]}`,
		strconv.Quote(logStreamSelector), strconv.Quote(traceSearchQuery))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunVerify_AllOutOfScopeCorpusExitsNonZero pins the CLI end of the
// judged-nothing rule. An operator whose dashboards are all log-stream panels ran
// verify with a complete loki pair, every entry routed out of scope, and the
// command printed "VERIFICATION PASSED — all 0 queries matched" and exited 0 — a
// CI job gating the cutover on that exit code goes green having judged nothing.
func TestRunVerify_AllOutOfScopeCorpusExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	corpus := writeAllOutOfScopeCorpus(t, dir)
	lokiRef := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{})
	lokiCer := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{})

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
	}, windowArgs()...)
	err := runVerify(args, &out, &errOut)
	if err == nil {
		t.Fatalf("a run that replayed nothing must exit non-zero, got success:\n%s", out.String())
	}
	if strings.Contains(out.String(), "VERIFICATION PASSED") {
		t.Errorf("a run that judged nothing must not print PASSED:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the gate judged nothing") {
		t.Errorf("the report must say the gate judged nothing, got:\n%s", out.String())
	}
	// The out-of-scope entries are still enumerated with their reasons: the run
	// blocks BECAUSE nothing was judged, not by pretending the entries vanished.
	for _, want := range []string{"kind=log-stream", "kind=trace-search"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report must still name the out-of-scope shapes (%q), got:\n%s", want, out.String())
		}
	}
}

// TestRunVerify_ConfiguredLaneWithOnlyOutOfScopeEntriesIsVisible pins that a lane
// the operator supplied backends for is never absent from the per-head table just
// because it had nothing to replay. Without a row, that lane is indistinguishable
// from one that was never configured, and the operator flips its datasource on a
// green report that never mentions it.
func TestRunVerify_ConfiguredLaneWithOnlyOutOfScopeEntriesIsVisible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.json")
	body := fmt.Sprintf(`{"version":1,"queries":[`+
		`{"expr":"up","source":"rule:up","kind":"record","lang":"promql"},`+
		`{"expr":%s,"source":"panel:errors","kind":"panel","lang":"traceql"}`+
		`],"skipped":[]}`, strconv.Quote(traceSearchQuery))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	// Configured but never contacted: the corpus holds no replayable TraceQL query.
	tempoRef := pathServer(t, "/api/metrics/query_range", "q", map[string]string{})
	tempoCer := pathServer(t, "/api/metrics/query_range", "q", map[string]string{})

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", path,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-tempo", tempoRef.URL, "--cerberus-tempo", tempoCer.URL,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err != nil {
		t.Fatalf("a healthy prom lane plus an idle tempo lane must not fail: %v\n%s", err, out.String())
	}
	rows := headTableRows(t, out.String())
	row, ok := rows["tempo"]
	if !ok {
		t.Fatalf("the configured tempo lane must have a per-head row, got rows %v:\n%s", rows, out.String())
	}
	if !strings.Contains(row, "[configured]") {
		t.Errorf("the tempo row must record that the lane was configured, got %q", row)
	}
	if !strings.Contains(row, "0 replayed") || !strings.Contains(row, "1 out of scope") {
		t.Errorf("the tempo row must show 0 replayed / 1 out of scope, got %q", row)
	}
	if !strings.Contains(out.String(), "head tempo was configured but had no replayable query") {
		t.Errorf("the report must say the configured tempo lane was not judged, got:\n%s", out.String())
	}
}

// TestRunVerify_LaneAuthFlagsWithoutAPairAreAUsageError pins the last silent-drop
// path in the flag surface. A mistyped URL env leaves the lane unconfigured while
// its token / tenant flags were supplied and thrown away; the run then reports the
// lane's queries as merely "unconfigured", a message that never mentions the input
// the operator actually gave — which is the clue to the typo.
func TestRunVerify_LaneAuthFlagsWithoutAPairAreAUsageError(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})

	cases := map[string]struct {
		flag, value, wantsPair string
	}{
		"loki tenant":   {"--ref-loki-org-id", "tenant-a", "--ref-loki"},
		"loki token":    {"--ref-loki-token", "s3cr3t", "--ref-loki"},
		"cerberus loki": {"--cerberus-loki-token", "s3cr3t", "--cerberus-loki"},
		"tempo tenant":  {"--ref-tempo-org-id", "tenant-a", "--ref-tempo"},
		"tempo token":   {"--cerberus-tempo-token", "s3cr3t", "--cerberus-tempo"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			args := append([]string{
				"--corpus", corpus,
				"--ref", ref.URL, "--cerberus", cer.URL,
				tc.flag, tc.value,
			}, windowArgs()...)
			err := runVerify(args, &out, &errOut)
			if err == nil {
				t.Fatalf("%s supplied with no backend pair must be a usage error, got success:\n%s", tc.flag, out.String())
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("the error must name the discarded flag %q, got %v", tc.flag, err)
			}
			if !strings.Contains(err.Error(), tc.wantsPair) {
				t.Errorf("the error must name the missing URL flag %q, got %v", tc.wantsPair, err)
			}
		})
	}
}

// TestRunVerify_LaneAuthFlagsWithACompletePairAreAccepted is the other half: the
// validation must reject only the DANGLING case. A lane with both URLs plus its
// token and tenant is the normal multi-tenant invocation and must run.
func TestRunVerify_LaneAuthFlagsWithACompletePairAreAccepted(t *testing.T) {
	dir := t.TempDir()
	corpus := writeLokiLaneCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	lokiRef := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})
	lokiCer := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
		"--ref-loki-org-id", "tenant-a", "--ref-loki-token", "s3cr3t",
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err != nil {
		t.Fatalf("a complete lane with auth flags must run: %v\n%s", err, out.String())
	}
}
