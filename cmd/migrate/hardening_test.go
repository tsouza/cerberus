package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/migrate"
)

// withUserinfo returns rawURL with basic-auth userinfo injected, so a test can
// drive runVerify with a credential-carrying --ref / --cerberus URL that still
// points at the local httptest server.
func withUserinfo(t *testing.T, rawURL, user, pass string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// TestRunVerify_RedactsCredentials pins the security fix: basic-auth credentials
// embedded in --ref / --cerberus must never reach the stdout repro line or the
// --report JSON. The live requests still use the real (credentialed) URL, but
// every artifact is redacted.
func TestRunVerify_RedactsCredentials(t *testing.T) {
	const secret = "sup3rs3cret"
	dir := t.TempDir()
	corpus := writeCorpus(t, dir)
	// Diverge so the failing run prints the bug-report section with the repro line.
	ref := promServer(t, map[string]string{"up": upMatrix})
	cer := promServer(t, map[string]string{"up": cerMatrix})
	reportPath := filepath.Join(dir, "verify-report.json")

	refURL := withUserinfo(t, ref.URL, "refuser", secret)
	cerURL := withUserinfo(t, cer.URL, "ceruser", secret)

	var out, errOut bytes.Buffer
	err := runVerify([]string{
		"--corpus", corpus, "--ref", refURL, "--cerberus", cerURL,
		"--start", "1700000000", "--end", "1700000600", "--step", "60s",
		"--report", reportPath,
	}, &out, &errOut)
	var gate verifyFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("runVerify should fail the gate on divergence, got: %v", err)
	}

	stdout := out.String()
	if strings.Contains(stdout, secret) {
		t.Errorf("stdout leaked the credential %q:\n%s", secret, stdout)
	}
	if !strings.Contains(stdout, "REDACTED") {
		t.Errorf("stdout repro line should carry a redacted URL, got:\n%s", stdout)
	}

	data, readErr := os.ReadFile(reportPath) //nolint:gosec // test-controlled temp path.
	if readErr != nil {
		t.Fatalf("--report file was not written: %v", readErr)
	}
	if strings.Contains(string(data), secret) {
		t.Errorf("report JSON leaked the credential %q:\n%s", secret, data)
	}
	var diag struct {
		Params struct {
			RefURL      string `json:"ref_url"`
			CerberusURL string `json:"cerberus_url"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &diag); err != nil {
		t.Fatalf("report JSON did not parse: %v", err)
	}
	if !strings.Contains(diag.Params.RefURL, "REDACTED") || !strings.Contains(diag.Params.CerberusURL, "REDACTED") {
		t.Errorf("report params must carry redacted URLs, got ref=%q cerberus=%q", diag.Params.RefURL, diag.Params.CerberusURL)
	}
}

// cfgWithBudget builds a config.Config carrying the default-on per-query sample
// budget, without touching the process environment.
func cfgWithBudget() config.Config {
	var cfg config.Config
	cfg.ClickHouse.MaxQuerySamples = 5_000_000
	return cfg
}

// TestNewExplainEngine_CarriesBudget pins the byte-parity fix: the offline
// preview engine must carry the resolved per-query sample budget, not 0 (which
// DISABLES the subquery budget gate the server always enforces).
func TestNewExplainEngine_CarriesBudget(t *testing.T) {
	eng := newExplainEngine(cfgWithBudget())
	if eng.MaxQuerySamples <= 0 {
		t.Errorf("newExplainEngine.MaxQuerySamples = %d; a non-positive budget disables the subquery gate the server enforces", eng.MaxQuerySamples)
	}
}

// TestExplain_SubqueryBudgetRejectedOffline drives the budget end to end: an
// anchor-grid-busting subquery must preview UNSUPPORTED (rejected by the sample
// budget) offline, matching what the live server would 422 — not clean SQL.
func TestExplain_SubqueryBudgetRejectedOffline(t *testing.T) {
	t.Setenv("CERBERUS_QUERY_MAX_SAMPLES", "5000000")
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.yml")
	// 1000d:1s inner grid = ~86.4M anchors, far past the 5M budget.
	const rules = `
groups:
  - name: heavy
    rules:
      - record: heavy:sub
        expr: max_over_time(rate(http_requests_total[5m])[1000d:1s])
`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := run([]string{"explain", "--rules", file}, &out, &errOut); err != nil {
		t.Fatalf("run explain: %v (stderr: %s)", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "UNSUPPORTED") || !strings.Contains(got, "sample budget exceeded") {
		t.Errorf("budget-busting subquery must preview UNSUPPORTED with a sample-budget reason, got:\n%s", got)
	}
}

// TestDryRunExplainer_PanelExplainedAsRange pins the eval-mode fix: a
// dashboard-panel query is previewed as a RANGE query (a non-zero step grid),
// while a rule is previewed as an INSTANT query — so the two emit different SQL
// for the same expr, and a panel no longer previews SQL the server never runs.
func TestDryRunExplainer_PanelExplainedAsRange(t *testing.T) {
	ex := newDryRunExplainer(cfgWithBudget())
	const expr = "sum(rate(http_requests_total[5m]))"

	instant := ex.Explain(context.Background(), migrate.HarvestedQuery{Expr: expr, Kind: migrate.KindRecord})
	if instant.Err != nil {
		t.Fatalf("instant explain errored: %v", instant.Err)
	}
	panel := ex.Explain(context.Background(), migrate.HarvestedQuery{Expr: expr, Kind: migrate.KindPanel})
	if panel.Err != nil {
		t.Fatalf("panel explain errored: %v", panel.Err)
	}
	if instant.SQL == "" || panel.SQL == "" {
		t.Fatalf("both modes must emit SQL; instant=%q panel=%q", instant.SQL, panel.SQL)
	}
	if instant.SQL == panel.SQL {
		t.Errorf("panel (range) SQL must differ from rule (instant) SQL, both:\n%s", instant.SQL)
	}
}
