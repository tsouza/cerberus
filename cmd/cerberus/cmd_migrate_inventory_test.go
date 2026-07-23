package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrateinventory"
)

// tsdbServer answers the source-Prometheus endpoints the inventory probe calls:
// the mandatory /api/v1/status/tsdb cardinality source plus the two optional
// enrichments, all with fixed bodies, so the cmd-level test drives runInventory
// end to end over real HTTP without a live Prometheus.
func tsdbServer(t *testing.T) *httptest.Server {
	t.Helper()
	const tsdb = `{"status":"success","data":{` +
		`"headStats":{"numSeries":100,"numLabelPairs":200,"chunkCount":50,"minTime":1700000000000,"maxTime":1700000600000},` +
		`"seriesCountByMetricName":[{"name":"http_requests_total","value":250000},{"name":"up","value":12}],` +
		`"labelValueCountByLabelName":[{"name":"instance","value":30}],` +
		`"memoryInBytesByLabelName":[{"name":"instance","value":4096}]}}`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status/tsdb", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tsdb))
	})
	mux.HandleFunc("/api/v1/label/__name__/values", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":["http_requests_total","up"]}`))
	})
	mux.HandleFunc("/api/v1/metadata", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"up":[{"type":"gauge","help":"x","unit":""}]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// clearInventoryEnv unsets the CERBERUS_INVENTORY_* fallbacks so a test drives the
// probe purely through explicit flags (or, for the env-fallback test, sets them).
func clearInventoryEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CERBERUS_INVENTORY_SOURCE", "")
	t.Setenv("CERBERUS_INVENTORY_WINDOW", "")
}

// TestRunInventory_JSON: --json emits the machine-readable inventory, carrying the
// stamped schema version and the ranked high-cardinality metric.
func TestRunInventory_JSON(t *testing.T) {
	clearInventoryEnv(t)
	srv := tsdbServer(t)

	var out, errOut bytes.Buffer
	if err := runInventory([]string{"--source", srv.URL, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("runInventory --json: %v (stderr: %s)", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, `"schema_version": 1`) {
		t.Errorf("JSON inventory should stamp schema_version, got:\n%s", got)
	}
	if !strings.Contains(got, "http_requests_total") {
		t.Errorf("JSON inventory should rank the high-cardinality metric, got:\n%s", got)
	}
}

// TestRunInventory_Text: the default (non-JSON) form renders the scannable text
// report with the head-block section and the ranked metric.
func TestRunInventory_Text(t *testing.T) {
	clearInventoryEnv(t)
	srv := tsdbServer(t)

	var out, errOut bytes.Buffer
	if err := runInventory([]string{"--source", srv.URL}, &out, &errOut); err != nil {
		t.Fatalf("runInventory: %v (stderr: %s)", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "head block") {
		t.Errorf("text inventory should carry the head-block section, got:\n%s", got)
	}
	if !strings.Contains(got, "http_requests_total") {
		t.Errorf("text inventory should rank the high-cardinality metric, got:\n%s", got)
	}
}

// TestRunInventory_MissingSource: with neither --source nor CERBERUS_INVENTORY_SOURCE
// set, runInventory reports a clear error naming the source flag rather than
// panicking or silently probing nothing.
func TestRunInventory_MissingSource(t *testing.T) {
	clearInventoryEnv(t)

	var out, errOut bytes.Buffer
	err := runInventory(nil, &out, &errOut)
	if err == nil {
		t.Fatal("runInventory should reject a missing --source")
	}
	if !strings.Contains(err.Error(), "--source") {
		t.Errorf("error should name the source flag, got: %v", err)
	}
}

// TestRunInventory_InvalidTop: a non-positive --top fails Options.Validate, and
// runInventory propagates that error (rather than probing with a bad rank size).
func TestRunInventory_InvalidTop(t *testing.T) {
	clearInventoryEnv(t)
	srv := tsdbServer(t)

	var out, errOut bytes.Buffer
	err := runInventory([]string{"--source", srv.URL, "--top", "0"}, &out, &errOut)
	if err == nil {
		t.Fatal("runInventory should reject --top 0 via Options.Validate")
	}
	if !strings.Contains(err.Error(), "top must be positive") {
		t.Errorf("error should come from Options.Validate, got: %v", err)
	}
}

// TestRunInventory_SourceFromEnv: the source falls back to CERBERUS_INVENTORY_SOURCE
// when --source is absent, so a run can be driven by env alone.
func TestRunInventory_SourceFromEnv(t *testing.T) {
	srv := tsdbServer(t)
	t.Setenv("CERBERUS_INVENTORY_SOURCE", srv.URL)
	t.Setenv("CERBERUS_INVENTORY_WINDOW", "")

	var out, errOut bytes.Buffer
	if err := runInventory([]string{"--json"}, &out, &errOut); err != nil {
		t.Fatalf("runInventory (env source): %v (stderr: %s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "http_requests_total") {
		t.Errorf("env-sourced inventory should rank the metric, got:\n%s", out.String())
	}
}

// TestRunInventory_OutFile: --out writes the inventory to the named file (checked,
// via writeOut) rather than stdout, following the file-output convention.
func TestRunInventory_OutFile(t *testing.T) {
	clearInventoryEnv(t)
	srv := tsdbServer(t)
	outPath := filepath.Join(t.TempDir(), "inventory.json")

	var out, errOut bytes.Buffer
	if err := runInventory([]string{"--source", srv.URL, "--json", "--out", outPath}, &out, &errOut); err != nil {
		t.Fatalf("runInventory --out: %v (stderr: %s)", err, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("inventory --out should not write to stdout, got: %q", out.String())
	}
	data, err := os.ReadFile(outPath) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if !strings.Contains(string(data), "http_requests_total") {
		t.Errorf("out file should carry the inventory JSON, got:\n%s", data)
	}
	// The gate consumes this file: it must carry the schema version so the gate's
	// version check accepts it. Tie the expectation to the source-of-truth const.
	wantVer := fmt.Sprintf(`"schema_version": %d`, migrateinventory.InventoryVersion)
	if !strings.Contains(string(data), wantVer) {
		t.Errorf("out file should stamp %s for the gate, got:\n%s", wantVer, data)
	}
}
