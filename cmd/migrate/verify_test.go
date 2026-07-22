package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	const cerMatrix = `{"status":"success","data":{"resultType":"matrix","result":[` +
		`{"metric":{"__name__":"up","job":"api"},"values":[[1700000000,"1"],[1700000060,"0"]]}]}}`
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
