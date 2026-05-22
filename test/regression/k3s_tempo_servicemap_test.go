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
