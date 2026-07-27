package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeConfigFile stages a cerberus.yaml in a fresh temp dir and makes it the
// working directory, so the loader's AddConfigPath(".") equivalent finds it.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cerberus.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write cerberus.yaml: %v", err)
	}
	chdir(t, dir)
	return path
}

// The headline contract: a file written in the chart's shape configures
// cerberus exactly as the matching environment variables would, across every
// value kind the shape has — scalar, unquoted bool, bare int, duration string,
// nested block, and a list that lowers to a comma-joined scalar.
func TestFromEnv_NestedConfigFile(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, `
clickhouse:
  addr:
    - ch-a.internal:9000
    - ch-b.internal:9000
  database: otel
  username: cerberus
  dialTimeout: 11s
  pool:
    maxOpenConns: 42
    maxIdleConns: 7
query:
  maxSamples: 5000000
  timeout: 90s
autoCreate:
  schema: false
http:
  addr: ":27080"
logLevel: debug
`)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got, want := cfg.ClickHouse.Addrs, []string{"ch-a.internal:9000", "ch-b.internal:9000"}; !reflect.DeepEqual(got, want) {
		t.Errorf("clickhouse.addr = %v, want %v", got, want)
	}
	if cfg.ClickHouse.Addr != "ch-a.internal:9000" {
		t.Errorf("clickhouse.addr[0] = %q, want ch-a.internal:9000", cfg.ClickHouse.Addr)
	}
	if cfg.ClickHouse.Database != "otel" {
		t.Errorf("clickhouse.database = %q, want otel", cfg.ClickHouse.Database)
	}
	if cfg.ClickHouse.Username != "cerberus" {
		t.Errorf("clickhouse.username = %q, want cerberus", cfg.ClickHouse.Username)
	}
	if cfg.ClickHouse.DialTimeout != 11*time.Second {
		t.Errorf("clickhouse.dialTimeout = %s, want 11s", cfg.ClickHouse.DialTimeout)
	}
	if cfg.ClickHouse.MaxOpenConns != 42 {
		t.Errorf("clickhouse.pool.maxOpenConns = %d, want 42", cfg.ClickHouse.MaxOpenConns)
	}
	if cfg.ClickHouse.MaxIdleConns != 7 {
		t.Errorf("clickhouse.pool.maxIdleConns = %d, want 7", cfg.ClickHouse.MaxIdleConns)
	}
	if cfg.ClickHouse.MaxQuerySamples != 5_000_000 {
		t.Errorf("query.maxSamples = %d, want 5000000", cfg.ClickHouse.MaxQuerySamples)
	}
	if cfg.ClickHouse.QueryTimeout != 90*time.Second {
		t.Errorf("query.timeout = %s, want 90s", cfg.ClickHouse.QueryTimeout)
	}
	// A default-ON toggle turned off by the file. A value that AGREED with the
	// default would prove nothing.
	if cfg.AutoCreateSchema {
		t.Error("autoCreate.schema = true, want the file's false")
	}
	if cfg.HTTPAddr != ":27080" {
		t.Errorf("http.addr = %q, want :27080", cfg.HTTPAddr)
	}
	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("logLevel = %s, want debug", cfg.Log.Level)
	}
}

// The environment still wins. Nesting is a spelling of the same settings, not a
// new source with its own precedence.
func TestFromEnv_NestedConfigFileLosesToEnv(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, "clickhouse:\n  database: fromfile\n")
	t.Setenv(envCHDatabase, "fromenv")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.Database != "fromenv" {
		t.Errorf("clickhouse.database = %q, want the environment's fromenv", cfg.ClickHouse.Database)
	}
}

// The flat CERBERUS_* form is the long tail's escape hatch and keeps working
// beside the nested shape in one file.
func TestFromEnv_FlatAndNestedInOneFile(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, `
clickhouse:
  database: otel
CERBERUS_CH_BREAKER_THRESHOLD: 9
`)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.Database != "otel" {
		t.Errorf("clickhouse.database = %q, want otel", cfg.ClickHouse.Database)
	}
	if cfg.ClickHouse.BreakerThreshold != 9 {
		t.Errorf("CERBERUS_CH_BREAKER_THRESHOLD = %d, want 9", cfg.ClickHouse.BreakerThreshold)
	}
}

// The read-side schema shape is resolved by internal/schema rather than the
// typed registry, so it needs its own proof that a file reaches it — otherwise
// the key is accepted and does nothing.
func TestFromEnv_NestedConfigFileSchemaShape(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, `
schema:
  metrics:
    gaugeTable: my_gauge
  logs:
    table: my_logs
  traces:
    table: my_spans
prom:
  resourceLabels:
    - service.name
    - k8s.namespace.name
`)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Schema.GaugeTable != "my_gauge" {
		t.Errorf("schema.metrics.gaugeTable = %q, want my_gauge", cfg.Schema.GaugeTable)
	}
	if cfg.Logs.LogsTable != "my_logs" {
		t.Errorf("schema.logs.table = %q, want my_logs", cfg.Logs.LogsTable)
	}
	if cfg.Traces.SpansTable != "my_spans" {
		t.Errorf("schema.traces.table = %q, want my_spans", cfg.Traces.SpansTable)
	}
	if got, want := cfg.Schema.PromResourceLabels, []string{"service.name", "k8s.namespace.name"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prom.resourceLabels = %v, want %v", got, want)
	}
}

// An unknown key is refused rather than ignored, and the message names the
// nearest known path — the mistakes this catches are near-misses.
func TestFromEnv_NestedConfigFileRejectsUnknownKey(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, "clickhouse:\n  databse: otel\n")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv accepted a misspelled key; a typo must not silently leave the default in place")
	}
	if !strings.Contains(err.Error(), "clickhouse.databse") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

// The commonest mistake is the right leaf under the wrong parent, so the
// suggestion is matched on the leaf name.
func TestFromEnv_NestedConfigFileSuggestsNearestPath(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, "clickhouse:\n  maxSamples: 100\n")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv accepted a wrongly-nested key")
	}
	if !strings.Contains(err.Error(), "did you mean query.maxSamples?") {
		t.Errorf("error does not point at query.maxSamples: %v", err)
	}
}

// A whole chart values.yaml is not a cerberus.yaml. Refusing its chart-only
// keys is what stops an operator believing they configured a gateway when they
// configured a deployment.
func TestFromEnv_NestedConfigFileRejectsChartOnlyKeys(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, "replicaCount: 3\nimage:\n  tag: v1.12.0\n")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv accepted chart-only keys")
	}
	for _, want := range []string{"replicaCount", "image.tag"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// A file that exists but is not YAML is fatal. Tolerating it would start the
// gateway on defaults nobody chose.
func TestFromEnv_NestedConfigFileRejectsMalformedYAML(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, "clickhouse:\n  addr: [unterminated\n")

	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv accepted a malformed config file")
	}
}

// No file at all stays the ordinary case: the environment configures cerberus
// completely on its own.
func TestFromEnv_NoConfigFileIsNotAnError(t *testing.T) {
	clearAllEnv(t)
	chdir(t, t.TempDir())

	if _, err := FromEnv(); err != nil {
		t.Fatalf("FromEnv with no config file: %v", err)
	}
}

// An empty file is a file with nothing to say, not a malformed one.
func TestFromEnv_EmptyConfigFileIsNotAnError(t *testing.T) {
	clearAllEnv(t)
	writeConfigFile(t, "")

	if _, err := FromEnv(); err != nil {
		t.Fatalf("FromEnv with an empty config file: %v", err)
	}
}

// schema.settings is a mapping bound to one setting, so it must reach the
// renderer intact and lower to the sorted k=v list the chart builds — sorted so
// one file always produces one value.
func TestTranslateDoc_SettingsMapLowersSorted(t *testing.T) {
	doc := map[string]any{
		"schema": map[string]any{
			"settings": map[string]any{
				"min_bytes_for_wide_part": 0,
				"index_granularity":       8192,
			},
		},
	}
	got, err := translateDoc("cerberus.yaml", doc)
	if err != nil {
		t.Fatalf("translateDoc: %v", err)
	}
	if want := "index_granularity=8192,min_bytes_for_wide_part=0"; got[envSchemaSettings] != want {
		t.Errorf("%s = %q, want %q", envSchemaSettings, got[envSchemaSettings], want)
	}
}

// Stream selectors carry commas as ordinary syntax, so their list lowers on
// newlines. Getting this wrong would split one selector into two.
func TestTranslateDoc_SelectorListLowersOnNewlines(t *testing.T) {
	doc := map[string]any{
		"migrate": map[string]any{
			"inventory": map[string]any{
				"loki": map[string]any{
					"selectors": []any{`{job="a", env="prod"}`, `{job="b"}`},
				},
			},
		},
	}
	got, err := translateDoc("cerberus.yaml", doc)
	if err != nil {
		t.Fatalf("translateDoc: %v", err)
	}
	want := "{job=\"a\", env=\"prod\"}\n{job=\"b\"}"
	if got[envInventoryLokiSelectors] != want {
		t.Errorf("%s = %q, want %q", envInventoryLokiSelectors, got[envInventoryLokiSelectors], want)
	}
}

// A large integer must not reach an integer parser in scientific notation.
func TestTranslateDoc_WholeNumbersRenderWithoutExponent(t *testing.T) {
	doc := map[string]any{"query": map[string]any{"maxSamples": float64(5_000_000)}}
	got, err := translateDoc("cerberus.yaml", doc)
	if err != nil {
		t.Fatalf("translateDoc: %v", err)
	}
	if got[envQueryMaxSamples] != "5000000" {
		t.Errorf("%s = %q, want 5000000", envQueryMaxSamples, got[envQueryMaxSamples])
	}
}

// A setting that takes one value must refuse a block: the operator wrote a
// shape cerberus cannot honour, and honouring the nearest thing would be a
// guess.
func TestTranslateDoc_ScalarSettingRefusesABlock(t *testing.T) {
	doc := map[string]any{"schema": map[string]any{"ttl": map[string]any{"days": 30}}}
	if _, err := translateDoc("cerberus.yaml", doc); err == nil {
		t.Fatal("translateDoc accepted a mapping for a scalar setting")
	}
}

// Every nested path resolves to exactly one setting and every setting to
// exactly one path. A collision would leave one of the two silently dead — and
// the table is hand-maintained, so nothing but this notices.
func TestBindings_PathsAndSettingsAreUnique(t *testing.T) {
	if len(bindingByPath) != len(bindings) || len(bindingBySetting) != len(bindings) {
		t.Fatalf("bindings=%d, byPath=%d, bySetting=%d: the table has a duplicate",
			len(bindings), len(bindingByPath), len(bindingBySetting))
	}
	for _, b := range bindings {
		if !strings.HasPrefix(b.setting, settingPrefix) {
			t.Errorf("%s binds %q, which is not a CERBERUS_* setting", b.path, b.setting)
		}
		if b.path == "" || strings.HasPrefix(b.path, settingPrefix) {
			t.Errorf("%q is not a usable nested path for %s", b.path, b.setting)
		}
	}
}

// ConfigFileSettings is what the chart-parity gate reads; it must report the
// whole table, sorted, or the gate silently checks a subset.
func TestConfigFileSettings_ReportsEveryBinding(t *testing.T) {
	got := ConfigFileSettings()
	if len(got) != len(bindings) {
		t.Fatalf("ConfigFileSettings() has %d entries, want %d", len(got), len(bindings))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("ConfigFileSettings() is not sorted at %d: %q then %q", i, got[i-1], got[i])
		}
	}
}
