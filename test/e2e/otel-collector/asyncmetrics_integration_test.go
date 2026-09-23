//go:build integration

// Package otelcollector_test runs the quickstart collector's ClickHouse
// receiver against real ClickHouse servers on both sides of the 26.8
// asynchronous-metrics schema change (docs/observability.md § "ClickHouse
// asynchronous metrics").
package otelcollector_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/network"
	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/chsql"
)

const (
	// priorServerImage is the server line the quickstart stack pins
	// (docker-compose.yml); TestPriorServerImageTracksQuickstartPin keeps the
	// two on the same major.minor.
	priorServerImage = "clickhouse/clickhouse-server:26.6-alpine"
	// keyedServerImage is the first server line whose
	// system.asynchronous_metrics carries the key_values column.
	keyedServerImage = "clickhouse/clickhouse-server:26.8-alpine"

	composeFile       = "../../../docker-compose.yml"
	collectorConfig   = "compose-config.yaml"
	clickhouseBoard   = "../grafana/compose/dashboards/clickhouse.json"
	receiverID        = "sqlquery/clickhouse"
	asyncMetricsTable = "system.asynchronous_metrics"
	asyncMetricName   = "clickhouse_async_metric"
	// asyncMetricQueries is how many receiver queries read the table: one for
	// scalar rows, one for key-value rows.
	asyncMetricQueries = 2

	chAlias         = "clickhouse"
	chUser          = "cerberus"
	chPassword      = "cerberus"
	collectorOutDir = "/out"
	collectorOut    = "metrics.jsonl"
	// scrapeInterval replaces the quickstart's 15s so the test sees several
	// complete scrapes within scrapeDeadline.
	scrapeInterval = "1s"
	// wantScrapes is the number of complete scrapes each scenario inspects;
	// more than one proves the queries keep succeeding, not just once.
	wantScrapes    = 3
	scrapeDeadline = 2 * time.Minute
	scrapePoll     = time.Second
	startupTimeout = 5 * time.Minute

	// perCoreFamily is present on every Linux host: one key per CPU core, and
	// the legacy form folds the core number straight into the name.
	perCoreFamily = "OSUserTimeCPU"
	legacyMode    = "both"
)

// legacySpellings are the ways a pre-26.8 server folds a key into a metric
// name, as ClickHouse documents asynchronous_metrics_key_values_mode:
// OSUserTimeCPU3, Temperature0 (no separator) and CPUFrequencyMHz_0,
// BlockReadBytes_sda, NetworkReceiveBytes_eth0, DiskTotal_default.
var legacySpellings = []func(family, key string) string{
	func(family, key string) string { return family + key },
	func(family, key string) string { return family + "_" + key },
}

type seriesID struct{ name, key string }

type scrape struct {
	timestamp string
	scalar    map[string]float64
	keyed     map[seriesID]float64
}

type scenario struct {
	image string
	// keyValuesMode is the server's asynchronous_metrics_key_values_mode; ""
	// leaves the server default.
	keyValuesMode string
}

func TestAsyncMetricsReceiverAcrossKeyedSchema(t *testing.T) {
	queries := asyncMetricReceiverQueries(t)
	collectorImage := quickstartImage(t, "otel/opentelemetry-collector-contrib")
	boardNames := dashboardScalarNames(t)

	prior := runScenario(t, scenario{image: priorServerImage}, queries, collectorImage)
	keyed := runScenario(t, scenario{image: keyedServerImage}, queries, collectorImage)
	both := runScenario(t, scenario{image: keyedServerImage, keyValuesMode: legacyMode}, queries, collectorImage)

	t.Run("prior server exports legacy per-key scalars and no keyed series", func(t *testing.T) {
		for _, s := range prior.scrapes {
			if len(s.keyed) != 0 {
				t.Fatalf("scrape %s: %d keyed series on a server without key_values", s.timestamp, len(s.keyed))
			}
			if _, ok := s.scalar[perCoreFamily+"0"]; !ok {
				t.Fatalf("scrape %s: legacy scalar %s0 missing", s.timestamp, perCoreFamily)
			}
		}
	})

	for _, tc := range []struct {
		label string
		run   scenarioResult
	}{{"key_values mode", keyed}, {"both mode", both}} {
		t.Run(tc.label+" exports every key once, under its family", func(t *testing.T) {
			for _, s := range tc.run.scrapes {
				keys := familyKeys(s, perCoreFamily)
				if len(keys) == 0 || strings.Join(keys, ",") != strings.Join(tc.run.familyKeys, ",") {
					t.Fatalf("scrape %s: %s keys %v, want the server's %v", s.timestamp, perCoreFamily, keys, tc.run.familyKeys)
				}
				if _, ok := s.scalar[perCoreFamily]; ok {
					t.Fatalf("scrape %s: the key-value row %s leaked as a scalar series", s.timestamp, perCoreFamily)
				}
				for id := range s.keyed {
					for _, spell := range legacySpellings {
						if _, dup := s.scalar[spell(id.name, id.key)]; dup {
							t.Fatalf("scrape %s: %s{key=%q} is also exported as legacy scalar %q", s.timestamp, id.name, id.key, spell(id.name, id.key))
						}
					}
				}
			}
		})
	}

	t.Run("both mode is live on the server", func(t *testing.T) {
		if both.serverLegacyRows == 0 {
			t.Fatal("the server published no legacy duplicate of any keyed series; the both-mode scenario proves nothing")
		}
	})

	t.Run("both mode exports the same keyed series as key_values mode", func(t *testing.T) {
		want, got := keyedIDs(keyed.scrapes[0]), keyedIDs(both.scrapes[0])
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Fatalf("keyed series differ:\n key_values: %v\n both:       %v", want, got)
		}
	})

	t.Run("dashboard names resolve on every server", func(t *testing.T) {
		for _, run := range []scenarioResult{prior, keyed, both} {
			for _, name := range boardNames {
				if _, ok := run.scrapes[0].scalar[name]; !ok {
					t.Errorf("%s: clickhouse.json selects %s{name=%q}, which the receiver does not export", run.label, asyncMetricName, name)
				}
			}
		}
	})
}

// TestPriorServerImageTracksQuickstartPin keeps priorServerImage on the
// release line docker-compose.yml runs, so "the current pinned server" in the
// test above means the one the quickstart actually monitors.
func TestPriorServerImageTracksQuickstartPin(t *testing.T) {
	pinned := quickstartImage(t, "clickhouse/clickhouse-server")
	tag := strings.TrimPrefix(pinned, "clickhouse/clickhouse-server:")
	if !strings.HasPrefix(priorServerImage, "clickhouse/clickhouse-server:"+tag+"-") &&
		priorServerImage != pinned {
		t.Fatalf("docker-compose.yml pins %s but the receiver test's prior server is %s", pinned, priorServerImage)
	}
}

type scenarioResult struct {
	label            string
	scrapes          []scrape
	familyKeys       []string
	serverLegacyRows uint64
}

func runScenario(t *testing.T, sc scenario, queries []any, collectorImage string) scenarioResult {
	t.Helper()
	label := sc.image
	if sc.keyValuesMode != "" {
		label += " (" + sc.keyValuesMode + ")"
	}
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("%s: network: %v", label, err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })

	chOpts := []testcontainers.ContainerCustomizer{
		tcclickhouse.WithUsername(chUser),
		tcclickhouse.WithPassword(chPassword),
		network.WithNetwork([]string{chAlias}, nw),
	}
	if sc.keyValuesMode != "" {
		cfg := filepath.Join(t.TempDir(), "key_values_mode.xml")
		body := fmt.Sprintf("<clickhouse><asynchronous_metrics_key_values_mode>%s</asynchronous_metrics_key_values_mode></clickhouse>", sc.keyValuesMode)
		if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
			t.Fatalf("%s: write config: %v", label, err)
		}
		chOpts = append(chOpts, tcclickhouse.WithConfigFile(cfg))
	}
	ch, err := tcclickhouse.Run(ctx, sc.image, chOpts...)
	if err != nil {
		t.Fatalf("%s: start clickhouse: %v", label, err)
	}
	t.Cleanup(func() { _ = ch.Terminate(context.Background()) })
	conn := dial(ctx, t, ch)

	out := t.TempDir()
	// The contrib image runs as an unprivileged user; the bind-mounted output
	// directory has to accept its writes.
	if err := os.Chmod(out, 0o777); err != nil {
		t.Fatalf("%s: chmod: %v", label, err)
	}
	cfgPath := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(cfgPath, collectorYAML(t, queries), 0o644); err != nil {
		t.Fatalf("%s: write collector config: %v", label, err)
	}
	col, err := testcontainers.Run(
		ctx, collectorImage,
		network.WithNetwork(nil, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      cfgPath,
			ContainerFilePath: "/etc/otelcol/receiver-test.yaml",
			FileMode:          0o644,
		}),
		testcontainers.WithCmd("--config=/etc/otelcol/receiver-test.yaml"),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, out+":"+collectorOutDir)
		}),
	)
	if err != nil {
		t.Fatalf("%s: start collector: %v", label, err)
	}
	t.Cleanup(func() { _ = col.Terminate(context.Background()) })

	scrapes := waitForScrapes(t, label, filepath.Join(out, collectorOut))
	assertCollectorLogsClean(ctx, t, label, col)
	for _, s := range scrapes {
		assertFiniteAndUnique(t, label, s)
	}

	res := scenarioResult{label: label, scrapes: scrapes}
	if sc.image == keyedServerImage {
		res.familyKeys = serverFamilyKeys(ctx, t, conn, perCoreFamily)
	}
	if sc.keyValuesMode == legacyMode {
		res.serverLegacyRows = serverLegacyRows(ctx, t, conn, scrapes[0])
	}
	return res
}

// asyncMetricReceiverQueries returns the quickstart receiver's queries that
// read system.asynchronous_metrics, verbatim from compose-config.yaml.
func asyncMetricReceiverQueries(t *testing.T) []any {
	t.Helper()
	raw, err := os.ReadFile(collectorConfig)
	if err != nil {
		t.Fatalf("read %s: %v", collectorConfig, err)
	}
	var cfg struct {
		Receivers map[string]struct {
			Queries []map[string]any `yaml:"queries"`
		} `yaml:"receivers"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", collectorConfig, err)
	}
	var out []any
	for _, q := range cfg.Receivers[receiverID].Queries {
		if sql, _ := q["sql"].(string); strings.Contains(sql, asyncMetricsTable) {
			out = append(out, q)
		}
	}
	if len(out) != asyncMetricQueries {
		t.Fatalf("%s: %d %s queries read %s, want %d", collectorConfig, len(out), receiverID, asyncMetricsTable, asyncMetricQueries)
	}
	return out
}

func collectorYAML(t *testing.T, queries []any) []byte {
	t.Helper()
	cfg := map[string]any{
		"receivers": map[string]any{
			receiverID: map[string]any{
				"driver":              "clickhouse",
				"datasource":          fmt.Sprintf("clickhouse://%s:%s@%s:9000/default?dial_timeout=5s", chUser, chPassword, chAlias),
				"collection_interval": scrapeInterval,
				"queries":             queries,
			},
		},
		"exporters": map[string]any{
			"file": map[string]any{"path": collectorOutDir + "/" + collectorOut},
		},
		"service": map[string]any{
			"pipelines": map[string]any{
				"metrics": map[string]any{"receivers": []string{receiverID}, "exporters": []string{"file"}},
			},
		},
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal collector config: %v", err)
	}
	return b
}

// quickstartImage returns the ref docker-compose.yml runs for repo.
func quickstartImage(t *testing.T, repo string) string {
	t.Helper()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("read %s: %v", composeFile, err)
	}
	m := regexp.MustCompile(`(?m)^\s*image:\s*(` + regexp.QuoteMeta(repo) + `:\S+)\s*$`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s names no %s image", composeFile, repo)
	}
	return string(m[1])
}

// dashboardScalarNames returns every metric name clickhouse.json selects from
// clickhouse_async_metric through a `name=~"a|b|c"` matcher.
func dashboardScalarNames(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(clickhouseBoard)
	if err != nil {
		t.Fatalf("read %s: %v", clickhouseBoard, err)
	}
	re := regexp.MustCompile(asyncMetricName + `\{name=~\\"([^"\\]+)\\"\}`)
	var names []string
	for _, m := range re.FindAllSubmatch(raw, -1) {
		names = append(names, strings.Split(string(m[1]), "|")...)
	}
	if len(names) == 0 {
		t.Fatalf("%s selects no %s names; the dashboard check would be vacuous", clickhouseBoard, asyncMetricName)
	}
	return names
}

func dial(ctx context.Context, t *testing.T, ch *tcclickhouse.ClickHouseContainer) driver.Conn {
	t.Helper()
	host, err := ch.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := ch.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{host + ":" + port.Port()},
		Auth: clickhouse.Auth{Database: "default", Username: chUser, Password: chPassword},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return conn
}

// waitForScrapes polls the collector's file export until wantScrapes complete
// scrapes (both queries of one collection) are on disk.
func waitForScrapes(t *testing.T, label, path string) []scrape {
	t.Helper()
	deadline := time.Now().Add(scrapeDeadline)
	for {
		scrapes := readScrapes(t, path)
		if len(scrapes) > wantScrapes {
			// The newest collection may still be half written.
			return scrapes[:wantScrapes]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d complete scrapes after %s, want %d", label, len(scrapes), scrapeDeadline, wantScrapes)
		}
		time.Sleep(scrapePoll)
	}
}

type otlpAttribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

type otlpExport struct {
	ResourceMetrics []struct {
		ScopeMetrics []struct {
			Metrics []struct {
				Name  string `json:"name"`
				Gauge struct {
					DataPoints []struct {
						Attributes []otlpAttribute `json:"attributes"`
						AsDouble   json.RawMessage `json:"asDouble"`
					} `json:"dataPoints"`
				} `json:"gauge"`
			} `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

// readScrapes returns one scrape per complete line of the collector's file
// export: the receiver hands each collection (both queries) to the exporter
// as one batch. A duplicate series inside one collection fails the test here,
// since the maps below would otherwise hide it.
func readScrapes(t *testing.T, path string) []scrape {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []scrape
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var exp otlpExport
		if err := json.Unmarshal(sc.Bytes(), &exp); err != nil {
			// The line still being written.
			continue
		}
		s := scrape{timestamp: strconv.Itoa(len(out)), scalar: map[string]float64{}, keyed: map[seriesID]float64{}}
		for _, rm := range exp.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != asyncMetricName {
						continue
					}
					for _, dp := range m.Gauge.DataPoints {
						addPoint(t, &s, dp.Attributes, parseOTLPDouble(t, dp.AsDouble))
					}
				}
			}
		}
		if len(s.scalar) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func addPoint(t *testing.T, s *scrape, attributes []otlpAttribute, v float64) {
	t.Helper()
	attrs := map[string]string{}
	for _, a := range attributes {
		attrs[a.Key] = a.Value.StringValue
	}
	if key, keyed := attrs["key"]; keyed {
		id := seriesID{attrs["name"], key}
		if _, dup := s.keyed[id]; dup {
			t.Fatalf("scrape %s: series %v exported twice", s.timestamp, id)
		}
		s.keyed[id] = v
		return
	}
	if _, dup := s.scalar[attrs["name"]]; dup {
		t.Fatalf("scrape %s: series %q exported twice", s.timestamp, attrs["name"])
	}
	s.scalar[attrs["name"]] = v
}

// parseOTLPDouble decodes an OTLP/JSON double, which encodes NaN and the
// infinities as strings.
func parseOTLPDouble(t *testing.T, raw json.RawMessage) float64 {
	t.Helper()
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("asDouble %s: %v", raw, err)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("asDouble %q: %v", s, err)
	}
	return f
}

func assertFiniteAndUnique(t *testing.T, label string, s scrape) {
	t.Helper()
	for name, v := range s.scalar {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("%s scrape %s: %s{name=%q} = %v", label, s.timestamp, asyncMetricName, name, v)
		}
	}
	for id, v := range s.keyed {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("%s scrape %s: %s{name=%q,key=%q} = %v", label, s.timestamp, asyncMetricName, id.name, id.key, v)
		}
		if _, clash := s.scalar[id.name]; clash {
			t.Errorf("%s scrape %s: %q is exported both as a scalar and as a keyed family", label, s.timestamp, id.name)
		}
	}
}

// assertCollectorLogsClean fails on any error the receiver logged — a query
// the server rejects (an unknown column on an older server) surfaces there and
// nowhere else, since the other query keeps exporting.
func assertCollectorLogsClean(ctx context.Context, t *testing.T, label string, col testcontainers.Container) {
	t.Helper()
	rc, err := col.Logs(ctx)
	if err != nil {
		t.Fatalf("%s: collector logs: %v", label, err)
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.Contains(line, "\terror\t") || strings.Contains(line, "\twarn\t") {
			t.Errorf("%s: collector logged: %s", label, line)
		}
	}
}

func familyKeys(s scrape, family string) []string {
	var keys []string
	for id := range s.keyed {
		if id.name == family {
			keys = append(keys, id.key)
		}
	}
	sort.Strings(keys)
	return keys
}

func keyedIDs(s scrape) []string {
	var ids []string
	for id := range s.keyed {
		ids = append(ids, id.name+"{"+id.key+"}")
	}
	sort.Strings(ids)
	return ids
}

// serverFamilyKeys reads the keys the server itself publishes for family.
func serverFamilyKeys(ctx context.Context, t *testing.T, conn driver.Conn, family string) []string {
	t.Helper()
	query, args := chsql.NewQuery().
		Select(chsql.Call("mapKeys", chsql.Col("key_values"))).
		From(chsql.Qual("system", "asynchronous_metrics")).
		Where(chsql.Eq(chsql.Col("metric"), chsql.Lit(family))).
		Build()
	var keys []string
	if err := conn.QueryRow(ctx, query, args...).Scan(&keys); err != nil {
		t.Fatalf("read %s keys: %v", family, err)
	}
	sort.Strings(keys)
	return keys
}

// serverLegacyRows counts the scalar rows the server itself publishes under a
// legacy spelling of a series the collector exported as keyed.
func serverLegacyRows(ctx context.Context, t *testing.T, conn driver.Conn, s scrape) uint64 {
	t.Helper()
	var names []string
	for id := range s.keyed {
		for _, spell := range legacySpellings {
			names = append(names, spell(id.name, id.key))
		}
	}
	lits := make([]chsql.Frag, 0, len(names))
	for _, name := range names {
		lits = append(lits, chsql.Lit(name))
	}
	query, args := chsql.NewQuery().
		Select(chsql.Call("count")).
		From(chsql.Qual("system", "asynchronous_metrics")).
		Where(chsql.In(chsql.Col("metric"), lits...)).
		Build()
	var n uint64
	if err := conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count legacy rows: %v", err)
	}
	return n
}
