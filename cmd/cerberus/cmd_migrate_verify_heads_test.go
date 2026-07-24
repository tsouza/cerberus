package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

// lokiRate is the LogQL metric query the multi-head fixtures replay, and
// tempoRate its TraceQL counterpart. Both are metric-lane shapes, so both are
// replayable and neither may land out of scope.
const (
	lokiRate  = `sum(rate({job="api"}[5m]))`
	tempoRate = `{} | rate()`
)

// lokiMatrix is a Loki query_range matrix body wrapped in the full upstream
// envelope (stats / warnings alongside the result) so the fixture proves the lane
// tolerates the extra fields rather than only the bare Prometheus shape.
const lokiMatrix = `{"status":"success","data":{"resultType":"matrix","result":[` +
	`{"metric":{"job":"api"},"values":[[1700000000,"7"],[1700000060,"7"]]}],` +
	`"stats":{"summary":{"execTime":0.01}}},"warnings":[]}`

// tempoRefMetrics is the REFERENCE Tempo shape: quoted integer-millisecond
// timestamps and an omitted zero value (gogo/jsonpb with EmitDefaults false).
const tempoRefMetrics = `{"series":[{"labels":[{"key":"job","value":{"stringValue":"api"}}],` +
	`"samples":[{"timestampMs":"1700000000000","value":7},{"timestampMs":"1700000060000"}]}]}`

// tempoCerMetrics is cerberus's own shape for the SAME data: numeric timestamps
// and an explicit zero. The two bodies are deliberately NOT byte-identical, so a
// decoder that ignored the string/number split could not pass on them.
const tempoCerMetrics = `{"status":"COMPLETE","series":[{"labels":[{"key":"job","value":{"stringValue":"api"}}],` +
	`"samples":[{"timestampMs":1700000000000,"value":7},{"timestampMs":1700000060000,"value":0}]}]}`

// pathServer answers only the given path, reading the query out of the given
// parameter, and 404s anything else. The path assertion is load-bearing: a
// wrong-path dialect would otherwise surface as a 400 → "unsupported", which
// passes the gate while comparing nothing.
func pathServer(t *testing.T, path, queryParam string, byQuery map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"error":"this backend serves only %s, got %s"}`, path, r.URL.Path)
			return
		}
		body, ok := byQuery[r.URL.Query().Get(queryParam)]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":"unknown query %q under %q"}`, r.URL.Query().Get(queryParam), queryParam)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeMixedCorpus writes a corpus spanning all three heads' metric lanes.
func writeMixedCorpus(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "corpus.json")
	body := fmt.Sprintf(`{"version":1,"queries":[`+
		`{"expr":"up","source":"rule:up","kind":"record","lang":"promql"},`+
		`{"expr":%s,"source":"panel:lograte","kind":"panel","lang":"logql"},`+
		`{"expr":%s,"source":"panel:spanrate","kind":"panel","lang":"traceql"}`+
		`],"skipped":[]}`,
		strconv.Quote(lokiRate), strconv.Quote(tempoRate))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeLokiLaneCorpus writes a corpus whose only entry is a replayable LogQL metric
// query — the shape that must be reported UNCONFIGURED when no loki lane is given.
func writeLokiLaneCorpus(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "corpus.json")
	body := fmt.Sprintf(`{"version":1,"queries":[`+
		`{"expr":"up","source":"rule:up","kind":"record","lang":"promql"},`+
		`{"expr":%s,"source":"panel:lograte","kind":"panel","lang":"logql"}`+
		`],"skipped":[]}`, strconv.Quote(lokiRate))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// windowArgs is the fixed replay window every multi-head fixture uses, so the
// stub bodies' timestamps line up on both sides.
func windowArgs() []string {
	return []string{"--start", "1700000000", "--end", "1700000600", "--step", "60s"}
}

// headTableRows parses the report's per-head lane table into head → row text. It
// keys on the row's own first field rather than a substring of the whole report,
// so "the loki lane is absent" cannot be satisfied by the word "loki" appearing in
// prose elsewhere, and a per-lane count assertion cannot accidentally read another
// lane's row.
func headTableRows(t *testing.T, report string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	inTable := false
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "# per-head lanes:") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		fields := strings.Fields(line)
		// The table runs until the first line that is not a "#   <head> …" row.
		if len(fields) < 2 || fields[0] != "#" {
			break
		}
		rows[fields[1]] = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	}
	return rows
}

// TestRunVerify_PromOnlyInvocationUnchanged pins that the existing single-head
// invocation is byte-identical in meaning AND in its repro line: same flags, same
// exit, and — critically — no mention of the two new lanes. An operator's pinned
// prom-only command must not start emitting flags they never supplied.
func TestRunVerify_PromOnlyInvocationUnchanged(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": cerMatrix})

	var out, errOut bytes.Buffer
	args := append([]string{"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL}, windowArgs()...)
	err := runVerify(args, &out, &errOut)
	var gate verifyFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("prom-only divergence must still fail the gate, got: %v", err)
	}

	s := out.String()
	wantRepro := fmt.Sprintf(
		"cerberus migrate verify --corpus %s --ref %s --cerberus %s "+
			"--start 2023-11-14T22:13:20Z --end 2023-11-14T22:23:20Z --step 1m0s --tolerance %s "+
			"--report verify-report.json",
		corpus, ref.URL, cer.URL,
		strconv.FormatFloat(migrateverify.DefaultTolerance, 'g', -1, 64),
	)
	if !strings.Contains(s, wantRepro) {
		t.Errorf("prom-only repro line must be exactly the prom-only invocation.\nwant: %s\ngot:\n%s", wantRepro, s)
	}
	for _, unwanted := range []string{"--ref-loki", "--cerberus-loki", "--ref-tempo", "--cerberus-tempo"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("a prom-only run must not mention %s anywhere in its report, got:\n%s", unwanted, s)
		}
	}
	// The per-head table must show exactly the one lane that ran, and no other.
	rows := headTableRows(t, s)
	if len(rows) != 1 || !strings.Contains(rows[migrateverify.HeadProm], "1 replayed: 0 match, 1 diverge") {
		t.Errorf("prom-only run must report exactly one prom lane with its divergence, got rows %+v", rows)
	}
}

// TestRunVerify_AllThreeLanesReplayOnTheirOwnEndpoints drives the full CLI over
// six stub backends, each answering ONLY its head's endpoint and query parameter.
// A dialect wired to the wrong path or parameter 404s/400s and the run fails, so
// this is the end-to-end pin that each lane talks to its own API.
func TestRunVerify_AllThreeLanesReplayOnTheirOwnEndpoints(t *testing.T) {
	dir := t.TempDir()
	corpus := writeMixedCorpus(t, dir)

	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	lokiRef := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})
	lokiCer := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})
	tempoRef := pathServer(t, "/api/metrics/query_range", "q", map[string]string{tempoRate: tempoRefMetrics})
	tempoCer := pathServer(t, "/api/metrics/query_range", "q", map[string]string{tempoRate: tempoCerMetrics})

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
		"--ref-tempo", tempoRef.URL, "--cerberus-tempo", tempoCer.URL,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err != nil {
		t.Fatalf("three matching lanes must pass, got: %v\n%s", err, out.String())
	}

	s := out.String()
	if !strings.Contains(s, "VERIFICATION PASSED — all 3 queries matched") {
		t.Errorf("want all three lanes matched, got:\n%s", s)
	}
	rows := headTableRows(t, s)
	if len(rows) != 3 {
		t.Errorf("per-head table must carry exactly three lanes, got %+v", rows)
	}
	for _, head := range []string{migrateverify.HeadLoki, migrateverify.HeadProm, migrateverify.HeadTempo} {
		if !strings.Contains(rows[head], "1 replayed: 1 match") {
			t.Errorf("head %q row must read 1 replayed / 1 match, got %q (all rows %+v)", head, rows[head], rows)
		}
	}
	// No lane may be reported unjudged: all three were configured.
	if strings.Contains(s, "== unconfigured") {
		t.Errorf("no lane should be unconfigured here, got:\n%s", s)
	}
}

// TestRunVerify_MultiHeadReproEmitsEveryConfiguredPair pins that the repro line
// of a multi-head run reproduces every lane it ran and nothing else — a repro that
// dropped a lane would regenerate a report that judged fewer queries.
func TestRunVerify_MultiHeadReproEmitsEveryConfiguredPair(t *testing.T) {
	dir := t.TempDir()
	corpus := writeLokiLaneCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": cerMatrix}) // diverges, so the repro prints
	lokiRef := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})
	lokiCer := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
	}, windowArgs()...)
	var gate verifyFailedError
	if err := runVerify(args, &out, &errOut); !errors.As(err, &gate) {
		t.Fatalf("the prom divergence must fail the gate, got: %v", err)
	}

	s := out.String()
	for _, want := range []string{
		"--ref-loki " + lokiRef.URL, "--cerberus-loki " + lokiCer.URL,
		"--ref " + promRef.URL, "--cerberus " + promCer.URL,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("repro line must carry %q, got:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--ref-tempo") {
		t.Errorf("repro line must not invent an unconfigured tempo lane, got:\n%s", s)
	}
}

// TestRunVerify_UnconfiguredLaneFailsTheRun pins the honesty rule at the CLI
// boundary: a corpus carrying replayable LogQL metric queries with no loki lane
// configured exits non-zero, enumerates the unjudged queries, and never replays
// them against the prom backend.
func TestRunVerify_UnconfiguredLaneFailsTheRun(t *testing.T) {
	dir := t.TempDir()
	corpus := writeLokiLaneCorpus(t, dir)
	// Both prom backends answer ONLY "up": if the LogQL expr were misrouted here it
	// would 400 and surface as unsupported rather than unconfigured.
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})

	var out, errOut bytes.Buffer
	args := append([]string{"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL}, windowArgs()...)
	err := runVerify(args, &out, &errOut)
	var gate verifyFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("a replayable query with no lane must fail the run, got: %v\n%s", err, out.String())
	}
	if !strings.Contains(err.Error(), "unjudged on an unconfigured lane") {
		t.Errorf("the exit error must name the unconfigured cause, got: %v", err)
	}

	s := out.String()
	if !strings.HasPrefix(s, "VERIFICATION FAILED") {
		t.Errorf("stdout must lead with VERIFICATION FAILED, got:\n%s", s)
	}
	if !strings.Contains(s, "== unconfigured (1)") {
		t.Errorf("the report must enumerate the unconfigured queries, got:\n%s", s)
	}
	if !strings.Contains(s, "panel:lograte") || !strings.Contains(s, "head=loki") {
		t.Errorf("the unconfigured entry must name its source and head, got:\n%s", s)
	}
	// The prom lane still compared its own query honestly, and the loki lane's row
	// must account for the unjudged query rather than omitting it.
	rows := headTableRows(t, s)
	if !strings.Contains(rows[migrateverify.HeadProm], "1 replayed: 1 match") {
		t.Errorf("the prom lane must still report its own match, got %q", rows[migrateverify.HeadProm])
	}
	if !strings.Contains(rows[migrateverify.HeadLoki], "0 replayed") || !strings.Contains(rows[migrateverify.HeadLoki], "1 unconfigured") {
		t.Errorf("the loki lane row must show 0 replayed / 1 unconfigured, got %q", rows[migrateverify.HeadLoki])
	}
}

// TestRunVerify_HalfPairIsUsageError pins that supplying one side of a lane is a
// usage error naming the missing flag, for both new heads and both directions. A
// half-supplied lane must never be silently skipped: the operator asked for it.
func TestRunVerify_HalfPairIsUsageError(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})

	cases := []struct {
		name        string
		extra       []string
		wantMissing string
	}{
		{"loki ref without cerberus", []string{"--ref-loki", "http://loki:3100"}, "--cerberus-loki"},
		{"loki cerberus without ref", []string{"--cerberus-loki", "http://cer:8080"}, "--ref-loki"},
		{"tempo ref without cerberus", []string{"--ref-tempo", "http://tempo:3200"}, "--cerberus-tempo"},
		{"tempo cerberus without ref", []string{"--cerberus-tempo", "http://cer:8080"}, "--ref-tempo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			args := append([]string{"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL}, tc.extra...)
			err := runVerify(append(args, windowArgs()...), &out, &errOut)
			if err == nil {
				t.Fatalf("a half-supplied lane must be a usage error, got a successful run:\n%s", out.String())
			}
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("the error must name the missing flag %s, got: %v", tc.wantMissing, err)
			}
			if !strings.Contains(err.Error(), "needs both a reference and a cerberus backend") {
				t.Errorf("the error must explain why a lane needs both sides, got: %v", err)
			}
		})
	}
}

// TestRunVerify_NoPairsIsUsageError pins that a corpus with no complete backend
// pair for ANY head is a usage error — the run would judge nothing.
func TestRunVerify_NoPairsIsUsageError(t *testing.T) {
	for _, key := range []string{
		"CERBERUS_VERIFY_REF", "CERBERUS_VERIFY_CERBERUS",
		"CERBERUS_VERIFY_REF_LOKI", "CERBERUS_VERIFY_CERBERUS_LOKI",
		"CERBERUS_VERIFY_REF_TEMPO", "CERBERUS_VERIFY_CERBERUS_TEMPO",
	} {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)

	var out, errOut bytes.Buffer
	err := runVerify([]string{"--corpus", corpus}, &out, &errOut)
	if err == nil {
		t.Fatal("a run with no complete backend pair must error")
	}
	if !strings.Contains(err.Error(), "at least one complete pair") {
		t.Errorf("the error must state the at-least-one-pair rule, got: %v", err)
	}
	// It must enumerate the pairs so the operator does not have to read --help.
	for _, want := range []string{"--ref/--cerberus", "--ref-loki/--cerberus-loki", "--ref-tempo/--cerberus-tempo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must offer %s, got: %v", want, err)
		}
	}
}

// TestRunVerify_EnvFallbackPerHead pins that each new lane has a working
// CERBERUS_VERIFY_* fallback, so a CI job can configure all three heads from the
// environment exactly as it configures the prom one today.
func TestRunVerify_EnvFallbackPerHead(t *testing.T) {
	dir := t.TempDir()
	corpus := writeLokiLaneCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	lokiRef := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})
	lokiCer := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})

	t.Setenv("CERBERUS_VERIFY_CORPUS", corpus)
	t.Setenv("CERBERUS_VERIFY_REF", promRef.URL)
	t.Setenv("CERBERUS_VERIFY_CERBERUS", promCer.URL)
	t.Setenv("CERBERUS_VERIFY_REF_LOKI", lokiRef.URL)
	t.Setenv("CERBERUS_VERIFY_CERBERUS_LOKI", lokiCer.URL)
	t.Setenv("CERBERUS_VERIFY_START", "1700000000")
	t.Setenv("CERBERUS_VERIFY_END", "1700000600")
	t.Setenv("CERBERUS_VERIFY_STEP", "60s")

	var out, errOut bytes.Buffer
	if err := runVerify(nil, &out, &errOut); err != nil {
		t.Fatalf("env-driven two-lane run: %v (stderr %s)", err, errOut.String())
	}
	s := out.String()
	if !strings.Contains(s, "VERIFICATION PASSED — all 2 queries matched") {
		t.Errorf("both env-configured lanes must be judged, got:\n%s", s)
	}
	if strings.Contains(s, "== unconfigured") {
		t.Errorf("the loki lane came from the environment and must be configured, got:\n%s", s)
	}
}

// TestRunVerify_RefOrgIDIsSentAndNeverRecorded pins both halves of the tenant
// header: the reference Loki receives X-Scope-OrgID, and the org-id never reaches
// the report or the repro line — it is a credential, like the bearer token.
func TestRunVerify_RefOrgIDIsSentAndNeverRecorded(t *testing.T) {
	const tenant = "team-observability"
	dir := t.TempDir()
	corpus := writeLokiLaneCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})

	var refTenants, cerTenants []string
	tenantRecorder := func(sink *[]string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*sink = append(*sink, r.Header.Get("X-Scope-OrgID"))
			_, _ = w.Write([]byte(lokiMatrix))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	lokiRef := tenantRecorder(&refTenants)
	lokiCer := tenantRecorder(&cerTenants)
	reportPath := filepath.Join(dir, "verify-report.json")

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
		"--ref-loki-org-id", tenant,
		"--report", reportPath,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err != nil {
		t.Fatalf("tenant-scoped run: %v\n%s", err, out.String())
	}

	if len(refTenants) != 1 || refTenants[0] != tenant {
		t.Errorf("the reference Loki must receive X-Scope-OrgID=%q, got %v", tenant, refTenants)
	}
	// Cerberus reads no tenant header, so the flag is reference-side only and must
	// not be forwarded there — sending it would advertise tenancy cerberus lacks.
	if len(cerTenants) != 1 || cerTenants[0] != "" {
		t.Errorf("cerberus must receive no tenant header, got %v", cerTenants)
	}

	data, err := os.ReadFile(reportPath) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if strings.Contains(string(data), tenant) {
		t.Errorf("the report must not record the tenant org-id (a credential), got:\n%s", data)
	}
	if strings.Contains(out.String(), tenant) {
		t.Errorf("the repro line must not record the tenant org-id, got:\n%s", out.String())
	}
}

// TestRunVerify_MultiHeadReportRecordsEveryLane pins the --report diagnostic's
// per-head shape: one backend entry per configured lane in head-token order, and a
// per-head summary an operator can read lane by lane.
func TestRunVerify_MultiHeadReportRecordsEveryLane(t *testing.T) {
	dir := t.TempDir()
	corpus := writeMixedCorpus(t, dir)
	promRef := promServer(t, map[string]string{"up": upMatrix})
	promCer := promServer(t, map[string]string{"up": upMatrix})
	lokiRef := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})
	lokiCer := pathServer(t, "/loki/api/v1/query_range", "query", map[string]string{lokiRate: lokiMatrix})
	tempoRef := pathServer(t, "/api/metrics/query_range", "q", map[string]string{tempoRate: tempoRefMetrics})
	tempoCer := pathServer(t, "/api/metrics/query_range", "q", map[string]string{tempoRate: tempoCerMetrics})
	reportPath := filepath.Join(dir, "verify-report.json")

	var out, errOut bytes.Buffer
	args := append([]string{
		"--corpus", corpus,
		"--ref", promRef.URL, "--cerberus", promCer.URL,
		"--ref-loki", lokiRef.URL, "--cerberus-loki", lokiCer.URL,
		"--ref-tempo", tempoRef.URL, "--cerberus-tempo", tempoCer.URL,
		"--report", reportPath,
	}, windowArgs()...)
	if err := runVerify(args, &out, &errOut); err != nil {
		t.Fatalf("three-lane run: %v\n%s", err, out.String())
	}

	data, err := os.ReadFile(reportPath) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var diag migrateverify.VerifyReport
	if err := json.Unmarshal(data, &diag); err != nil {
		t.Fatalf("report must parse: %v\n%s", err, data)
	}
	if diag.SchemaVersion != migrateverify.VerifyReportVersion {
		t.Errorf("schema_version = %d, want %d", diag.SchemaVersion, migrateverify.VerifyReportVersion)
	}

	wantHeads := []string{migrateverify.HeadLoki, migrateverify.HeadProm, migrateverify.HeadTempo}
	gotBackendHeads := make([]string, 0, len(diag.Params.Backends))
	for _, b := range diag.Params.Backends {
		gotBackendHeads = append(gotBackendHeads, b.Head)
		if b.RefURL == "" || b.CerberusURL == "" {
			t.Errorf("backend %+v must record both URLs", b)
		}
	}
	if strings.Join(gotBackendHeads, ",") != strings.Join(wantHeads, ",") {
		t.Errorf("params.backends heads = %v, want head-token order %v", gotBackendHeads, wantHeads)
	}

	gotSummaryHeads := make([]string, 0, len(diag.Heads))
	for _, h := range diag.Heads {
		gotSummaryHeads = append(gotSummaryHeads, h.Head)
		if h.Summary.Total != 1 || h.Summary.Match != 1 {
			t.Errorf("head %s summary = %+v, want 1 replayed / 1 match", h.Head, h.Summary)
		}
	}
	if strings.Join(gotSummaryHeads, ",") != strings.Join(wantHeads, ",") {
		t.Errorf("heads = %v, want head-token order %v", gotSummaryHeads, wantHeads)
	}
	// Every result must name its lane: a divergence with no head is untriageable.
	for _, r := range diag.Results {
		if r.Head == "" {
			t.Errorf("result %+v carries no head", r)
		}
	}
}

// TestReproCommand_EmitsOnlyConfiguredPairs drives reproCommand directly over the
// recorded backend list: every configured lane's flag pair appears, in the same
// head-token order the report uses, and no lane the run did not configure is
// invented.
func TestReproCommand_EmitsOnlyConfiguredPairs(t *testing.T) {
	cmd := reproCommand(migrateverify.VerifyReportParams{
		Backends: []migrateverify.VerifyReportBackend{
			{Head: migrateverify.HeadLoki, RefURL: "http://loki:3100", CerberusURL: "http://cer:8080"},
			{Head: migrateverify.HeadProm, RefURL: "http://prom:9090", CerberusURL: "http://cer:8080"},
		},
		Start: "2023-11-14T22:13:20Z", End: "2023-11-14T22:23:20Z",
		Step: "1m0s", Tolerance: migrateverify.DefaultTolerance, Corpus: "corpus.json",
	})
	wantOrdered := "--ref-loki http://loki:3100 --cerberus-loki http://cer:8080 " +
		"--ref http://prom:9090 --cerberus http://cer:8080"
	if !strings.Contains(cmd, wantOrdered) {
		t.Errorf("repro must emit both configured pairs in head-token order.\nwant: %s\ngot:  %s", wantOrdered, cmd)
	}
	if strings.Contains(cmd, "tempo") {
		t.Errorf("repro must not invent the unconfigured tempo lane, got: %s", cmd)
	}
}

// TestReproCommand_UnknownHeadIsNotGuessedOntoAnotherLane pins that a head token
// this build has no flags for is dropped rather than mapped onto whichever lane
// happens to be first — a repro line that silently reassigns a backend would
// regenerate a report verifying the wrong thing.
func TestReproCommand_UnknownHeadIsNotGuessedOntoAnotherLane(t *testing.T) {
	cmd := reproCommand(migrateverify.VerifyReportParams{
		Backends: []migrateverify.VerifyReportBackend{
			{Head: "cypher", RefURL: "http://neo:7474", CerberusURL: "http://cer:8080"},
			{Head: migrateverify.HeadProm, RefURL: "http://prom:9090", CerberusURL: "http://cer:8080"},
		},
		Start: "2023-11-14T22:13:20Z", End: "2023-11-14T22:23:20Z",
		Step: "1m0s", Tolerance: migrateverify.DefaultTolerance, Corpus: "corpus.json",
	})
	if strings.Contains(cmd, "http://neo:7474") {
		t.Errorf("an unknown head's URL must not be emitted under another lane's flag, got: %s", cmd)
	}
	if !strings.Contains(cmd, "--ref http://prom:9090") {
		t.Errorf("the known prom lane must still be emitted, got: %s", cmd)
	}
}

// TestVerifyHeadsCoversEveryHeadToken pins that the CLI's flag table has an entry
// for every head the parity gate can route a query to. A head with no flags could
// only ever be reported unconfigured, so the gate would be permanently unable to
// judge it — a silent dead end rather than a usable lane.
func TestVerifyHeadsCoversEveryHeadToken(t *testing.T) {
	want := map[string]bool{
		migrateverify.HeadProm:  false,
		migrateverify.HeadLoki:  false,
		migrateverify.HeadTempo: false,
	}
	for _, h := range verifyHeads {
		if _, ok := want[h.head]; !ok {
			t.Errorf("verifyHeads carries head %q, which is not a parity-gate head token", h.head)
			continue
		}
		want[h.head] = true
		if h.refFlag == "" || h.cerFlag == "" || h.refEnv == "" || h.cerEnv == "" || h.dialect == nil {
			t.Errorf("head %q has an incomplete flag entry: %+v", h.head, h)
		}
		if h.dialect.Name() != h.head {
			t.Errorf("head %q is wired to the %q dialect", h.head, h.dialect.Name())
		}
	}
	for head, covered := range want {
		if !covered {
			t.Errorf("head %q has no CLI flag pair: its queries could only ever be reported unconfigured", head)
		}
	}
	// Head-token order is what makes the backend list and the repro line
	// deterministic, so the table itself must be sorted.
	for i := 1; i < len(verifyHeads); i++ {
		if verifyHeads[i-1].head >= verifyHeads[i].head {
			t.Errorf("verifyHeads must be head-token sorted for deterministic output, got %q before %q",
				verifyHeads[i-1].head, verifyHeads[i].head)
		}
	}
}

// TestRunVerify_TempoPartialReferenceFailsTheRun pins that a truncated reference
// baseline fails the CLI run rather than being diffed as if it were complete —
// comparing against a partial result would manufacture divergences that blame
// cerberus for the reference's own truncation.
func TestRunVerify_TempoPartialReferenceFailsTheRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.json")
	body := fmt.Sprintf(`{"version":1,"queries":[{"expr":%s,"source":"panel:spanrate","kind":"panel","lang":"traceql"}],"skipped":[]}`,
		strconv.Quote(tempoRate))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	const partial = `{"status":"PARTIAL","message":"exceeded the query limit","series":[]}`
	tempoRef := pathServer(t, "/api/metrics/query_range", "q", map[string]string{tempoRate: partial})
	tempoCer := pathServer(t, "/api/metrics/query_range", "q", map[string]string{tempoRate: tempoCerMetrics})

	var out, errOut bytes.Buffer
	args := append([]string{"--corpus", path, "--ref-tempo", tempoRef.URL, "--cerberus-tempo", tempoCer.URL}, windowArgs()...)
	err := runVerify(args, &out, &errOut)
	var gate verifyFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("a PARTIAL reference must fail the run, got: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "PARTIAL") {
		t.Errorf("the report must name the truncated reference status, got:\n%s", s)
	}
	if !strings.Contains(s, "[error]") {
		t.Errorf("a truncated reference is an error verdict, not a divergence, got:\n%s", s)
	}
}
