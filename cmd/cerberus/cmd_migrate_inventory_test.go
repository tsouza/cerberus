package main

import (
	"bytes"
	"encoding/json"
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
	wantVer := fmt.Sprintf(`"schema_version": %d`, migrateinventory.InventoryVersion)
	if !strings.Contains(got, wantVer) {
		t.Errorf("JSON inventory should stamp %s, got:\n%s", wantVer, got)
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

// TestRunInventory_LegacyInvocationHasNoHeadSections: a bare --source/--top/
// --window/--json invocation (no --loki-source, no --tempo-source) must not
// grow "loki" or "tempo" JSON keys — the per-head sections are additive, and
// their absence must be visible absence, not an empty object.
func TestRunInventory_LegacyInvocationHasNoHeadSections(t *testing.T) {
	clearInventoryEnv(t)
	t.Setenv("CERBERUS_INVENTORY_LOKI_SOURCE", "")
	t.Setenv("CERBERUS_INVENTORY_LOKI_SELECTORS", "")
	t.Setenv("CERBERUS_INVENTORY_TEMPO_SOURCE", "")
	srv := tsdbServer(t)

	var out, errOut bytes.Buffer
	if err := runInventory([]string{"--source", srv.URL, "--top", "10", "--window", "1h", "--json"}, &out, &errOut); err != nil {
		t.Fatalf("runInventory: %v (stderr: %s)", err, errOut.String())
	}
	got := out.String()
	if strings.Contains(got, `"loki"`) {
		t.Errorf("legacy invocation should carry no \"loki\" key, got:\n%s", got)
	}
	if strings.Contains(got, `"tempo"`) {
		t.Errorf("legacy invocation should carry no \"tempo\" key, got:\n%s", got)
	}
}

// lokiIndexStatsServer answers /loki/api/v1/index/stats with a fixed
// {streams, chunks, entries, bytes} body per selector (keyed by the "query"
// param), so the cmd-level test drives the Loki wiring over real HTTP.
func lokiIndexStatsServer(t *testing.T) *httptest.Server {
	t.Helper()
	byQuery := map[string]string{
		`{app="checkout"}`: `{"streams":500,"chunks":900,"entries":40000,"bytes":8000000}`,
		`{app="frontend"}`: `{"streams":50,"chunks":90,"entries":4000,"bytes":800000}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/index/stats" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, ok := byQuery[r.URL.Query().Get("query")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunInventory_LokiAndTempoSections: --loki-source/--loki-selector and
// --tempo-source wire through the CLI into the Loki-ranked section and the
// fixed Tempo out-of-scope section, ranked highest-streams-first.
func TestRunInventory_LokiAndTempoSections(t *testing.T) {
	clearInventoryEnv(t)
	promSrv := tsdbServer(t)
	lokiSrv := lokiIndexStatsServer(t)

	var out, errOut bytes.Buffer
	err := runInventory([]string{
		"--source", promSrv.URL, "--json",
		"--loki-source", lokiSrv.URL,
		"--loki-selector", `{app="frontend"}`,
		"--loki-selector", `{app="checkout"}`,
		"--tempo-source", "http://tempo.example:3200",
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("runInventory (loki+tempo): %v (stderr: %s)", err, errOut.String())
	}

	var inv migrateinventory.Inventory
	if unmarshalErr := json.Unmarshal(out.Bytes(), &inv); unmarshalErr != nil {
		t.Fatalf("decode inventory JSON: %v\n%s", unmarshalErr, out.String())
	}

	if inv.Loki == nil {
		t.Fatalf("inventory should carry a Loki section, got:\n%s", out.String())
	}
	if len(inv.Loki.Selectors) != 2 {
		t.Fatalf("loki selectors = %d, want 2, got: %+v", len(inv.Loki.Selectors), inv.Loki.Selectors)
	}
	if inv.Loki.Selectors[0].Selector != `{app="checkout"}` || inv.Loki.Selectors[0].Streams != 500 {
		t.Errorf("rank #1 loki selector = %+v, want checkout (500 streams) first", inv.Loki.Selectors[0])
	}
	if inv.Loki.Selectors[1].Selector != `{app="frontend"}` || inv.Loki.Selectors[1].Streams != 50 {
		t.Errorf("rank #2 loki selector = %+v, want frontend (50 streams) second", inv.Loki.Selectors[1])
	}

	if inv.Tempo == nil {
		t.Fatalf("inventory should carry a Tempo section, got:\n%s", out.String())
	}
	if inv.Tempo.Source != "http://tempo.example:3200" {
		t.Errorf("tempo source = %q, want the supplied --tempo-source", inv.Tempo.Source)
	}
	if inv.Tempo.OutOfScope != migrateinventory.TempoInventoryOutOfScopeReason {
		t.Errorf("tempo out-of-scope reason = %q, want the specific reason constant", inv.Tempo.OutOfScope)
	}
}

// TestRunInventory_LokiSelectorsEnvPreservesCommaBearingSelector pins that the
// CERBERUS_INVENTORY_LOKI_SELECTORS env fallback treats one line as one whole
// selector, never comma-splitting it — an ordinary multi-label LogQL stream
// selector like `{app="checkout", env="prod"}` contains a comma of its own,
// and the documented env-var form must round-trip it exactly like the
// equivalent repeated --loki-selector flag does (StringArrayVar, no split).
func TestRunInventory_LokiSelectorsEnvPreservesCommaBearingSelector(t *testing.T) {
	clearInventoryEnv(t)
	promSrv := tsdbServer(t)

	const multiLabelSelector = `{app="checkout", env="prod"}`
	var gotQueries []string
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.Query().Get("query"))
		_, _ = w.Write([]byte(`{"streams":10,"chunks":20,"entries":100,"bytes":1000}`))
	}))
	defer lokiSrv.Close()

	t.Setenv("CERBERUS_INVENTORY_LOKI_SELECTORS", multiLabelSelector)

	var out, errOut bytes.Buffer
	err := runInventory([]string{
		"--source", promSrv.URL, "--json",
		"--loki-source", lokiSrv.URL,
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("runInventory: %v (stderr: %s)", err, errOut.String())
	}

	if len(gotQueries) != 1 || gotQueries[0] != multiLabelSelector {
		t.Fatalf("loki index/stats queries = %+v, want exactly one query %q "+
			"(the env selector must not be split on its internal comma)", gotQueries, multiLabelSelector)
	}
}

// TestRunInventory_LokiProbeAllSelectorsFail pins the CLI-level enforcement of
// the honesty contract LokiClient.Probe documents: if every --loki-selector's
// index/stats call fails, runInventory must return a hard error and must NOT
// write an inventory.json — a failed probe must never read as a clean, empty
// top-N.
func TestRunInventory_LokiProbeAllSelectorsFail(t *testing.T) {
	clearInventoryEnv(t)
	promSrv := tsdbServer(t)
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer lokiSrv.Close()

	outPath := filepath.Join(t.TempDir(), "inventory.json")
	var out, errOut bytes.Buffer
	err := runInventory([]string{
		"--source", promSrv.URL, "--json", "--out", outPath,
		"--loki-source", lokiSrv.URL,
		"--loki-selector", `{app="checkout"}`,
		"--loki-selector", `{app="frontend"}`,
	}, &out, &errOut)
	if err == nil {
		t.Fatal("runInventory should hard-fail when every --loki-selector's probe call fails")
	}
	if !strings.Contains(err.Error(), `{app="checkout"}`) || !strings.Contains(err.Error(), `{app="frontend"}`) {
		t.Errorf("error should name both failed selectors, got: %v", err)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Errorf("--out file should not be written on a hard probe failure, stat err: %v", statErr)
	}
}

// TestRunInventory_LokiProbePartialFailureRecordsNotes pins that a PARTIAL
// Loki probe failure (some selectors succeed, one fails) is not an error at
// the CLI layer: runInventory succeeds and the emitted JSON carries the
// failed selector in inv.Loki.Notes rather than silently dropping it.
func TestRunInventory_LokiProbePartialFailureRecordsNotes(t *testing.T) {
	clearInventoryEnv(t)
	promSrv := tsdbServer(t)
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == `{app="checkout"}` {
			_, _ = w.Write([]byte(`{"streams":500,"chunks":900,"entries":40000,"bytes":8000000}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer lokiSrv.Close()

	var out, errOut bytes.Buffer
	err := runInventory([]string{
		"--source", promSrv.URL, "--json",
		"--loki-source", lokiSrv.URL,
		"--loki-selector", `{app="checkout"}`,
		"--loki-selector", `{app="broken"}`,
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("runInventory (partial loki failure): %v (stderr: %s)", err, errOut.String())
	}

	var inv migrateinventory.Inventory
	if unmarshalErr := json.Unmarshal(out.Bytes(), &inv); unmarshalErr != nil {
		t.Fatalf("decode inventory JSON: %v\n%s", unmarshalErr, out.String())
	}
	if inv.Loki == nil {
		t.Fatalf("inventory should still carry a Loki section, got:\n%s", out.String())
	}
	if len(inv.Loki.Selectors) != 1 || inv.Loki.Selectors[0].Selector != `{app="checkout"}` {
		t.Fatalf("loki selectors = %+v, want exactly the surviving checkout selector", inv.Loki.Selectors)
	}
	if len(inv.Loki.Notes) != 1 || !strings.Contains(inv.Loki.Notes[0], `{app="broken"}`) {
		t.Fatalf("loki notes = %+v, want one note naming the failed broken selector", inv.Loki.Notes)
	}
}

// TestRunInventory_LokiSelectorWithoutSourceIsValidationError: --loki-selector
// with no --loki-source is rejected before any HTTP call, naming both flags.
func TestRunInventory_LokiSelectorWithoutSourceIsValidationError(t *testing.T) {
	clearInventoryEnv(t)
	srv := tsdbServer(t)

	var out, errOut bytes.Buffer
	err := runInventory([]string{"--source", srv.URL, "--loki-selector", `{app="x"}`}, &out, &errOut)
	if err == nil {
		t.Fatal("runInventory should reject --loki-selector without --loki-source")
	}
	if !strings.Contains(err.Error(), "--loki-selector") || !strings.Contains(err.Error(), "--loki-source") {
		t.Errorf("error should name both flags, got: %v", err)
	}
}

// TestRunInventory_LokiSourceWithoutSelectorIsValidationError: --loki-source
// with zero --loki-selector entries is rejected before any HTTP call — Loki
// has no whole-tenant top-N call to rank without an operator-named selector.
func TestRunInventory_LokiSourceWithoutSelectorIsValidationError(t *testing.T) {
	clearInventoryEnv(t)
	srv := tsdbServer(t)

	var out, errOut bytes.Buffer
	err := runInventory([]string{"--source", srv.URL, "--loki-source", "http://loki.example:3100"}, &out, &errOut)
	if err == nil {
		t.Fatal("runInventory should reject --loki-source without any --loki-selector")
	}
	if !strings.Contains(err.Error(), "--loki-selector") {
		t.Errorf("error should name --loki-selector, got: %v", err)
	}
}
