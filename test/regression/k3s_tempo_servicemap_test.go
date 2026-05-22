package regression

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestK3sTempoDatasourcesCarryServiceMapDatasourceUID guards against the
// post-#700 regression behind workflow run 26287654251 (e2e dashboard job
// 77379658293, on push-to-main commit 006f160e). PR #700 wired the
// "Service Graph" tab in Grafana's Tempo Explore by adding
// `jsonData.serviceMap.datasourceUid: cerberus-prometheus` to the COMPOSE
// Tempo datasource (test/e2e/grafana/compose/datasources/cerberus.yaml)
// — but the k3d ConfigMap mirror at test/e2e/k3s/grafana.yaml was not
// updated. The Playwright `service_graph.spec.ts:103` smoke pings
// `/api/datasources/uid/cerberus-tempo` on the k3d-deployed Grafana and
// expects `body.jsonData.serviceMap.datasourceUid === 'cerberus-prometheus'`;
// against the un-mirrored ConfigMap it sees `undefined` and the dashboard
// job fails on every push-to-main run.
//
// This test pins the k3d ConfigMap's Tempo datasource entries (both the
// `cerberus-tempo` primary and the `grafanacloud-traces` alias) to carry
// the same `serviceMap.datasourceUid: cerberus-prometheus` block as the
// compose source-of-truth. Catches: any future drop / rename / typo of
// the field in the k3d manifest — the kind of drift that the compose
// stack's live smoke can't see because it doesn't load this file.
//
// The check parses the ConfigMap's `datasources.yaml` payload rather
// than grepping the raw YAML, so YAML key reorderings, comment edits or
// alias-anchor refactors don't false-positive.
func TestK3sTempoDatasourcesCarryServiceMapDatasourceUID(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../e2e/k3s/grafana.yaml")
	if err != nil {
		t.Fatalf("read k3s grafana.yaml: %v", err)
	}

	// The file is a multi-doc YAML (ConfigMap + Service + Deployment).
	// Walk every doc, find the one whose `metadata.name` is
	// `grafana-datasources`, then parse its `data["datasources.yaml"]`
	// inner payload.
	dec := yaml.NewDecoder(strings.NewReader(string(buf)))

	type k8sObj struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}

	var inner string
	for {
		var obj k8sObj
		if err := dec.Decode(&obj); err != nil {
			break
		}
		if obj.Kind == "ConfigMap" && obj.Metadata.Name == "grafana-datasources" {
			inner = obj.Data["datasources.yaml"]
			break
		}
	}
	if inner == "" {
		t.Fatalf("test/e2e/k3s/grafana.yaml: no `grafana-datasources` ConfigMap with `data[\"datasources.yaml\"]` payload found — the file shape changed; update this regression test alongside the manifest")
	}

	type datasource struct {
		Name     string `yaml:"name"`
		UID      string `yaml:"uid"`
		Type     string `yaml:"type"`
		JSONData struct {
			ServiceMap struct {
				DatasourceUID string `yaml:"datasourceUid"`
			} `yaml:"serviceMap"`
		} `yaml:"jsonData"`
	}
	type provisioning struct {
		APIVersion  int          `yaml:"apiVersion"`
		Datasources []datasource `yaml:"datasources"`
	}

	var prov provisioning
	if err := yaml.Unmarshal([]byte(inner), &prov); err != nil {
		t.Fatalf("parse datasources.yaml payload: %v", err)
	}

	// Every `type: tempo` datasource must carry the serviceMap
	// wiring — the k3d ConfigMap mirrors compose, which sets it on
	// both the primary `cerberus-tempo` entry and the
	// `grafanacloud-traces` drilldown alias.
	const want = "cerberus-prometheus"
	tempoEntries := 0
	for _, ds := range prov.Datasources {
		if ds.Type != "tempo" {
			continue
		}
		tempoEntries++
		if ds.JSONData.ServiceMap.DatasourceUID != want {
			t.Errorf(
				"test/e2e/k3s/grafana.yaml: tempo datasource %q (uid=%q) has jsonData.serviceMap.datasourceUid=%q, want %q — Playwright service_graph.spec.ts:103 fails against the k3d-deployed Grafana without this field",
				ds.Name, ds.UID, ds.JSONData.ServiceMap.DatasourceUID, want,
			)
		}
	}
	if tempoEntries == 0 {
		t.Fatalf("test/e2e/k3s/grafana.yaml: no `type: tempo` datasource entries — the cerberus-tempo + grafanacloud-traces entries were removed; the dashboard smoke expects both")
	}
	if tempoEntries < 2 {
		t.Errorf("test/e2e/k3s/grafana.yaml: found %d tempo datasource(s), expected at least 2 (cerberus-tempo primary + grafanacloud-traces alias) — mirror of test/e2e/grafana/compose/datasources/cerberus.yaml drifted", tempoEntries)
	}
}

// TestK3sOtelCollectorGatewayCarriesServiceGraphConnector guards against the
// post-#702 regression behind workflow run 26289535170 (e2e dashboard job
// 77385760886, on push-to-main commit e98e3103). PR #700 added the
// `servicegraph` connector + `metrics/servicegraph` pipeline to the COMPOSE
// collector config (test/e2e/otel-collector/compose-config.yaml) and PR #702
// wired Grafana's Tempo datasource to query the resulting metrics — but the
// k3d gateway ConfigMap at test/e2e/k3s/otel-collector.yaml was not updated.
// The Playwright `service_graph.spec.ts:44` test drives a warm-up query
// through cerberus, then polls cerberus's Prom head for
// `traces_service_graph_request_total`; without the connector on the k3d
// collector the metric never lands in ClickHouse and the test times out at
// 60s.
//
// This test pins the k3d gateway's collector config to carry:
//   - a `connectors.servicegraph` block,
//   - the `servicegraph` exporter on the `traces` pipeline (alongside
//     `clickhouse`),
//   - a `metrics/servicegraph` pipeline receiving from `servicegraph` and
//     routing through `transform/servicegraph_drop_exemplars` into the
//     `clickhouse` exporter (the transform processor is required because
//     the v0.116.0 clickhouseexporter nil-derefs on the connector's
//     exemplar payload; the same trap re-appears any time the contrib
//     image is downgraded).
//
// Parses the ConfigMap's `config.yaml` payload rather than grepping the
// raw YAML so reorderings / comment edits don't false-positive.
func TestK3sOtelCollectorGatewayCarriesServiceGraphConnector(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../e2e/k3s/otel-collector.yaml")
	if err != nil {
		t.Fatalf("read k3s otel-collector.yaml: %v", err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(buf)))

	type k8sObj struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}

	var inner string
	for {
		var obj k8sObj
		if err := dec.Decode(&obj); err != nil {
			break
		}
		if obj.Kind == "ConfigMap" && obj.Metadata.Name == "otel-collector-gateway-config" {
			inner = obj.Data["config.yaml"]
			break
		}
	}
	if inner == "" {
		t.Fatalf("test/e2e/k3s/otel-collector.yaml: no `otel-collector-gateway-config` ConfigMap with `data[\"config.yaml\"]` payload found — the file shape changed; update this regression test alongside the manifest")
	}

	type pipeline struct {
		Receivers  []string `yaml:"receivers"`
		Processors []string `yaml:"processors"`
		Exporters  []string `yaml:"exporters"`
	}
	type service struct {
		Pipelines map[string]pipeline `yaml:"pipelines"`
	}
	type collector struct {
		Connectors map[string]map[string]any `yaml:"connectors"`
		Processors map[string]map[string]any `yaml:"processors"`
		Service    service                   `yaml:"service"`
	}

	var cfg collector
	if err := yaml.Unmarshal([]byte(inner), &cfg); err != nil {
		t.Fatalf("parse otel-collector gateway config payload: %v", err)
	}

	if _, ok := cfg.Connectors["servicegraph"]; !ok {
		t.Errorf(
			"test/e2e/k3s/otel-collector.yaml: gateway ConfigMap is missing `connectors.servicegraph` — Playwright service_graph.spec.ts:44 polls cerberus's Prom head for `traces_service_graph_request_total` and times out at 60s without the connector emitting the metric",
		)
	}
	if _, ok := cfg.Processors["transform/servicegraph_drop_exemplars"]; !ok {
		t.Errorf(
			"test/e2e/k3s/otel-collector.yaml: gateway ConfigMap is missing `processors.transform/servicegraph_drop_exemplars` — required to strip the servicegraph connector's exemplar payload before it hits the clickhouseexporter (otherwise the collector crashes on the first metrics_flush_interval tick)",
		)
	}

	traces, ok := cfg.Service.Pipelines["traces"]
	if !ok {
		t.Fatalf("test/e2e/k3s/otel-collector.yaml: gateway ConfigMap has no `service.pipelines.traces` — the traces pipeline was renamed or removed; the dashboard smoke expects it")
	}
	if !contains(traces.Exporters, "servicegraph") {
		t.Errorf(
			"test/e2e/k3s/otel-collector.yaml: `service.pipelines.traces.exporters` = %v, missing `servicegraph` — the connector must tap the traces pipeline to derive caller/callee edges from CH-call spans",
			traces.Exporters,
		)
	}
	if !contains(traces.Exporters, "clickhouse") {
		t.Errorf(
			"test/e2e/k3s/otel-collector.yaml: `service.pipelines.traces.exporters` = %v, missing `clickhouse` — the connector must coexist with the regular CH writer; the servicegraph wiring is a tap, not a swap",
			traces.Exporters,
		)
	}

	sg, ok := cfg.Service.Pipelines["metrics/servicegraph"]
	if !ok {
		t.Fatalf(
			"test/e2e/k3s/otel-collector.yaml: gateway ConfigMap is missing the `service.pipelines.metrics/servicegraph` pipeline — without it the connector's `traces_service_graph_*` series never reach ClickHouse and cerberus's Prom head returns an empty result, timing out service_graph.spec.ts:44",
		)
	}
	if !contains(sg.Receivers, "servicegraph") {
		t.Errorf(
			"test/e2e/k3s/otel-collector.yaml: `metrics/servicegraph.receivers` = %v, must include `servicegraph` — that's the connector wired as the receiver side of the destination pipeline",
			sg.Receivers,
		)
	}
	if !contains(sg.Processors, "transform/servicegraph_drop_exemplars") {
		t.Errorf(
			"test/e2e/k3s/otel-collector.yaml: `metrics/servicegraph.processors` = %v, must include `transform/servicegraph_drop_exemplars` — exemplar stripping must run on this branch before the payload reaches clickhouseexporter",
			sg.Processors,
		)
	}
	if !contains(sg.Exporters, "clickhouse") {
		t.Errorf(
			"test/e2e/k3s/otel-collector.yaml: `metrics/servicegraph.exporters` = %v, must include `clickhouse` — the derived metrics must round-trip into the same CH `otel_metrics_*` tables cerberus's Prom head queries back",
			sg.Exporters,
		)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
