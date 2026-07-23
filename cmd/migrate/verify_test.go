package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrateverify"
)

// promServer answers /api/v1/query_range with a fixed matrix for known queries
// and a 400 otherwise, so the cmd-level test can drive runVerify end to end
// against real HTTP without a live Prometheus or cerberus.
func promServer(t *testing.T, byQuery map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := byQuery[r.URL.Query().Get("query")]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","error":"unknown"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const upMatrix = `{"status":"success","data":{"resultType":"matrix","result":[` +
	`{"metric":{"__name__":"up","job":"api"},"values":[[1700000000,"1"],[1700000060,"1"]]}]}}`

func writeCorpus(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "corpus.json")
	const body = `{"version":1,"queries":[` +
		`{"expr":"up","source":"rule:up","kind":"record","lang":"promql"}` +
		`],"skipped":[]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunVerify_MatchPasses: identical backends → runVerify returns nil (gate
// passes) and prints a PASS report.
func TestRunVerify_MatchPasses(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})

	var out, errOut bytes.Buffer
	err := runVerify([]string{
		"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL,
		"--start", "1700000000", "--end", "1700000600", "--step", "60s",
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("runVerify should pass on identical backends, got: %v (stderr %s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "PASS:") {
		t.Errorf("expected a PASS report, got:\n%s", out.String())
	}
}

// TestRunVerify_DivergeFails: a value difference makes runVerify return a
// verifyFailedError (mapped to a non-zero exit) and print FAIL.
func TestRunVerify_DivergeFails(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": cerMatrix})

	var out, errOut bytes.Buffer
	err := runVerify([]string{
		"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL,
		"--start", "1700000000", "--end", "1700000600", "--step", "60s",
	}, &out, &errOut)
	var gate verifyFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("runVerify should return verifyFailedError on divergence, got: %v", err)
	}
	if !strings.Contains(out.String(), "FAIL:") {
		t.Errorf("expected a FAIL report, got:\n%s", out.String())
	}
}

// TestRunVerify_JSON: --json emits the machine report.
func TestRunVerify_JSON(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})

	var out, errOut bytes.Buffer
	if err := runVerify([]string{
		"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL,
		"--start", "1700000000", "--end", "1700000600", "--step", "60s", "--json",
	}, &out, &errOut); err != nil {
		t.Fatalf("runVerify --json: %v", err)
	}
	if !strings.Contains(out.String(), `"verdict": "match"`) {
		t.Errorf("expected JSON report with a match verdict, got:\n%s", out.String())
	}
}

// cerMatrix is a divergent up matrix (second point differs from upMatrix), used
// by the failure-path tests.
const cerMatrix = `{"status":"success","data":{"resultType":"matrix","result":[` +
	`{"metric":{"__name__":"up","job":"api"},"values":[[1700000000,"1"],[1700000060,"0"]]}]}}`

// TestRunVerify_DivergeStdoutGuidance: a diverging run's stdout leads with the
// FAILED verdict and ends with the bug-report guidance — the issues URL and a
// copy-pasteable repro command.
func TestRunVerify_DivergeStdoutGuidance(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": cerMatrix})

	var out, errOut bytes.Buffer
	err := runVerify([]string{
		"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL,
		"--start", "1700000000", "--end", "1700000600", "--step", "60s",
	}, &out, &errOut)
	var gate verifyFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("runVerify should fail the gate on divergence, got: %v", err)
	}
	s := out.String()
	if !strings.HasPrefix(s, "VERIFICATION FAILED") {
		t.Errorf("stdout must lead with VERIFICATION FAILED, got:\n%s", s)
	}
	if !strings.Contains(s, "https://github.com/tsouza/cerberus/issues") {
		t.Errorf("stdout must carry the cerberus issues URL, got:\n%s", s)
	}
	if !strings.Contains(s, "migrate verify --corpus") || !strings.Contains(s, "--report verify-report.json") {
		t.Errorf("stdout must carry a copy-pasteable repro command with --report, got:\n%s", s)
	}
}

// TestRunVerify_ReportFile: --report writes valid, parseable JSON carrying the
// schema version, tool version, run params, summary, and per-query verdicts.
func TestRunVerify_ReportFile(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": cerMatrix})
	reportPath := filepath.Join(dir, "verify-report.json")

	var out, errOut bytes.Buffer
	err := runVerify([]string{
		"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL,
		"--start", "1700000000", "--end", "1700000600", "--step", "60s",
		"--report", reportPath,
	}, &out, &errOut)
	var gate verifyFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("runVerify should fail the gate on divergence, got: %v", err)
	}

	data, readErr := os.ReadFile(reportPath) //nolint:gosec // test-controlled temp path.
	if readErr != nil {
		t.Fatalf("--report file was not written: %v", readErr)
	}
	var diag struct {
		SchemaVersion int    `json:"schema_version"`
		ToolVersion   string `json:"tool_version"`
		GeneratedAt   string `json:"generated_at"`
		Note          string `json:"note"`
		Params        struct {
			RefURL      string  `json:"ref_url"`
			CerberusURL string  `json:"cerberus_url"`
			Start       string  `json:"start"`
			End         string  `json:"end"`
			Step        string  `json:"step"`
			Tolerance   float64 `json:"tolerance"`
			Corpus      string  `json:"corpus"`
		} `json:"params"`
		Summary struct {
			Total   int `json:"total"`
			Diverge int `json:"diverge"`
		} `json:"summary"`
		Results []struct {
			Expr    string `json:"expr"`
			Verdict string `json:"verdict"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &diag); err != nil {
		t.Fatalf("--report must be valid JSON: %v\n%s", err, string(data))
	}
	if diag.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", diag.SchemaVersion)
	}
	if diag.ToolVersion == "" {
		t.Error("tool_version must be populated")
	}
	if diag.GeneratedAt == "" || diag.Note == "" {
		t.Errorf("generated_at and note must be populated, got %q / %q", diag.GeneratedAt, diag.Note)
	}
	if diag.Params.RefURL != ref.URL || diag.Params.CerberusURL != cer.URL || diag.Params.Corpus != corpus {
		t.Errorf("params did not capture the run inputs: %+v", diag.Params)
	}
	if diag.Params.Step != "1m0s" || diag.Params.Tolerance == 0 {
		t.Errorf("params step/tolerance not captured: step=%q tol=%v", diag.Params.Step, diag.Params.Tolerance)
	}
	if diag.Summary.Total != 1 || diag.Summary.Diverge != 1 {
		t.Errorf("summary = %+v, want total 1 / diverge 1", diag.Summary)
	}
	if len(diag.Results) != 1 || diag.Results[0].Verdict != "diverge" {
		t.Errorf("results = %+v, want one diverge result", diag.Results)
	}
}

// TestRunVerify_OutFile: --out writes the report to a file (checked, via
// writeOut) instead of stdout, following the file-output convention every other
// gate-input producer uses.
func TestRunVerify_OutFile(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})
	outPath := filepath.Join(dir, "verify.json")

	var out, errOut bytes.Buffer
	if err := runVerify([]string{
		"--corpus", corpus, "--ref", ref.URL, "--cerberus", cer.URL,
		"--start", "1700000000", "--end", "1700000600", "--step", "60s",
		"--json", "--out", outPath,
	}, &out, &errOut); err != nil {
		t.Fatalf("runVerify --out: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("verify --out should not write the report to stdout, got: %q", out.String())
	}
	data, err := os.ReadFile(outPath) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if !strings.Contains(string(data), `"verdict": "match"`) {
		t.Errorf("out file should carry the JSON report, got:\n%s", data)
	}
}

// TestReproCommand_ShellQuoting: a URL with special characters is single-quoted so
// the repro command stays copy-pasteable.
func TestReproCommand_ShellQuoting(t *testing.T) {
	cmd := reproCommand(migrateverify.VerifyReportParams{
		RefURL: "http://ref:9090", CerberusURL: "http://cer/path?x=1&y=2",
		Start: "2023-11-14T22:13:20Z", End: "2023-11-14T22:23:20Z",
		Step: "1m0s", Tolerance: migrateverify.DefaultTolerance, Corpus: "corpus.json",
	})
	if !strings.Contains(cmd, "'http://cer/path?x=1&y=2'") {
		t.Errorf("URL with shell-special chars must be single-quoted, got:\n%s", cmd)
	}
	if !strings.Contains(cmd, "http://ref:9090") {
		t.Errorf("safe URL should appear bare, got:\n%s", cmd)
	}
	if !strings.HasPrefix(cmd, "migrate verify ") || !strings.Contains(cmd, "--report verify-report.json") {
		t.Errorf("repro must be a full migrate verify invocation ending in --report, got:\n%s", cmd)
	}
}

// TestRunVerify_MissingFlags: absent required inputs are a clear error, not a
// panic or a silent no-op.
func TestRunVerify_MissingFlags(t *testing.T) {
	t.Setenv("CERBERUS_VERIFY_CORPUS", "")
	t.Setenv("CERBERUS_VERIFY_REF", "")
	t.Setenv("CERBERUS_VERIFY_CERBERUS", "")
	var out, errOut bytes.Buffer
	if err := runVerify(nil, &out, &errOut); err == nil {
		t.Fatal("runVerify with no corpus/ref/cerberus should error")
	}
}

// TestRunVerify_EnvFallback: CERBERUS_VERIFY_* supply the inputs when flags are
// omitted.
func TestRunVerify_EnvFallback(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})
	t.Setenv("CERBERUS_VERIFY_CORPUS", corpus)
	t.Setenv("CERBERUS_VERIFY_REF", ref.URL)
	t.Setenv("CERBERUS_VERIFY_CERBERUS", cer.URL)
	t.Setenv("CERBERUS_VERIFY_START", "1700000000")
	t.Setenv("CERBERUS_VERIFY_END", "1700000600")
	t.Setenv("CERBERUS_VERIFY_STEP", "60s")

	var out, errOut bytes.Buffer
	if err := runVerify(nil, &out, &errOut); err != nil {
		t.Fatalf("runVerify via env: %v (stderr %s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "PASS:") {
		t.Errorf("expected PASS via env-driven run, got:\n%s", out.String())
	}
}

// TestRunVerify_BadToleranceEnv: a set-but-unparseable CERBERUS_VERIFY_TOLERANCE
// is a loud error, not a silent fallback to the tiny default that would tighten
// the gate into spurious divergences.
func TestRunVerify_BadToleranceEnv(t *testing.T) {
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": upMatrix})
	t.Setenv("CERBERUS_VERIFY_CORPUS", corpus)
	t.Setenv("CERBERUS_VERIFY_REF", ref.URL)
	t.Setenv("CERBERUS_VERIFY_CERBERUS", cer.URL)
	t.Setenv("CERBERUS_VERIFY_TOLERANCE", "not-a-float")

	var out, errOut bytes.Buffer
	err := runVerify(nil, &out, &errOut)
	if err == nil {
		t.Fatal("runVerify should reject an unparseable CERBERUS_VERIFY_TOLERANCE")
	}
	if !strings.Contains(err.Error(), "CERBERUS_VERIFY_TOLERANCE") {
		t.Errorf("error should name the offending variable, got: %v", err)
	}
}

// TestEnvFloat covers the unset / valid / unparseable branches directly.
func TestEnvFloat(t *testing.T) {
	const key = "CERBERUS_VERIFY_TOLERANCE_TESTKEY"

	t.Setenv(key, "")
	if got, err := envFloat(key, 1e-9); err != nil || got != 1e-9 {
		t.Errorf("unset: got %v, %v; want 1e-9, nil", got, err)
	}

	t.Setenv(key, "0.25")
	if got, err := envFloat(key, 1e-9); err != nil || got != 0.25 {
		t.Errorf("valid: got %v, %v; want 0.25, nil", got, err)
	}

	t.Setenv(key, "banana")
	if _, err := envFloat(key, 1e-9); err == nil {
		t.Error("unparseable: envFloat should error, not fall back to the default")
	}
}
