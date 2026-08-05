package regression

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestK3sCerberusDeploymentWiresOTLP guards against the bug behind the
// long-running e2e `dashboard` job partition + histogram-completeness
// failures (task #214 / #215 N2 / N5 regression class).
//
// The k3d cerberus deployment originally set `CERBERUS_OTEL_ENDPOINT` /
// `CERBERUS_OTEL_INSECURE` / … — none of which cerberus actually reads.
// The binary's config (see internal/config.otlpFromEnv) consumes
// `CERBERUS_OTLP_*` (note the L). Result: cerberus ran with
// CERBERUS_OTLP_ENDPOINT="", telemetry.New short-circuited to the noop
// MeterProvider, and `cerberus_queries_total` / `cerberus_queries_
// duration_seconds_*` never reached the otel-collector → ClickHouse
// pipeline. The cerberus dashboard's "Query rate by language" panel then
// collapsed to a single anonymous bucket and the histogram-completeness
// probe reported `series count=0`.
//
// cerberus is now deployed in the e2e cluster via its Helm chart
// (deploy/helm/cerberus, values in test/e2e/k3s/cerberus-values.yaml — see
// `just e2e-up`), so the dead-spelling class can recur in two places, and
// this test pins BOTH — statically, with no `helm` dependency in the
// `check` lane:
//
//  1. The chart helper (_helpers.tpl) that LOWERS the typed `otlp` block
//     must emit `CERBERUS_OTLP_*` and never `CERBERUS_OTEL_*`. This is the
//     real source of truth now — the chart hardcodes the env key names, so
//     the typed block cannot reintroduce the dead spelling.
//  2. The e2e values must actually WIRE OTLP (`otlp.endpoint` set) and must
//     not smuggle a dead `CERBERUS_OTEL_*` key through the free-form
//     `config:` passthrough (the one remaining hand-written env surface).
//
// Complements the unit-level coverage in internal/telemetry/metrics_test.go
// (which verifies the attribute set lands on the counter once the SDK is
// actually wired up — the SDK plumbing is what the bug broke).
func TestK3sCerberusDeploymentWiresOTLP(t *testing.T) {
	t.Parallel()

	// Any `CERBERUS_OTEL_<KEY>` token (with the L missing) is the dead
	// spelling — in a chart helper it would be an emitted env key, in the
	// values file a hand-written `config:`/passthrough key.
	deadKeyRE := regexp.MustCompile(`CERBERUS_OTEL_[A-Z_]+`)

	// (1) The chart helper that lowers the typed otlp block.
	helperPath := "../../deploy/helm/cerberus/templates/_helpers.tpl"
	helper, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read chart helper %s: %v", helperPath, err)
	}
	if dead := deadKeyRE.FindAllString(string(helper), -1); len(dead) > 0 {
		t.Errorf("%s emits dead `CERBERUS_OTEL_*` env key(s) — cerberus reads `CERBERUS_OTLP_*` (note the L). The chart would deploy cerberus with OTLP disabled and emit no self-telemetry; the dashboard partition + histogram-completeness e2e probes regress. Found: %v", helperPath, dead)
	}
	for _, want := range []string{"CERBERUS_OTLP_ENDPOINT", "CERBERUS_OTLP_INSECURE"} {
		if !strings.Contains(string(helper), want) {
			t.Errorf("%s no longer emits %q — the chart must lower the typed otlp block to the env key cerberus reads, or the cerberus dashboard panels return empty matrices", helperPath, want)
		}
	}

	// (2) The e2e values: OTLP wired, no dead passthrough key.
	valuesPath := "../e2e/k3s/cerberus-values.yaml"
	values, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read e2e values %s: %v", valuesPath, err)
	}
	if dead := deadKeyRE.FindAllString(string(values), -1); len(dead) > 0 {
		t.Errorf("%s carries dead `CERBERUS_OTEL_*` key(s) (likely in the `config:` passthrough) — cerberus reads `CERBERUS_OTLP_*`. Found: %v", valuesPath, dead)
	}
	// The typed otlp block must set a non-empty endpoint: an empty/absent
	// endpoint disables OTLP export entirely (zero-collector binary), which
	// is exactly the dead-telemetry state the dashboard probes regress on.
	endpointRE := regexp.MustCompile(`(?m)^\s+endpoint:\s*\S`)
	if !strings.Contains(string(values), "otlp:") || !endpointRE.MatchString(string(values)) {
		t.Errorf("%s does not wire OTLP — expected an `otlp:` block with a non-empty `endpoint:` so the k3d cerberus deployment exports self-telemetry; without it the cerberus dashboard panels return empty matrices", valuesPath)
	}
}

// drilldownBreakdownDimension is the resource axis Grafana Traces
// Drilldown auto-advances to after the operator includes a service:
// clicking "Include" on `resource.service.name` sets the app's groupBy to
// `resource.service.namespace` and appends `&& resource.service.namespace
// != nil` to the breakdown query.
const drilldownBreakdownDimension = "service.namespace"

// TestK3sCerberusPublishesBreakdownResourceDimensions pins the manifest
// half of the fix for #1818: the Traces Drilldown app drilled only one
// level in the k3d `dashboard` lane, failing
// test/e2e/playwright/iterate-drilldown-apps.spec.ts's two-level floor
// with `levelsClicked=1`.
//
// The chain: the harness clicks the first "Include" affordance, which
// selects `resource.service.name = cerberus` — cerberus's own
// self-telemetry. The app then breaks that selection down by
// `resource.service.namespace`. Cerberus published only service.name /
// service.version / service.instance.id, so the breakdown query was
// genuinely empty, the panel rendered "No data for selected query", and
// no second-level affordance existed to click.
//
// Two things had to be true, and this test pins the one that is a
// manifest rather than code:
//
//  1. The binary must honour OTEL_RESOURCE_ATTRIBUTES at all —
//     internal/telemetry.buildResource installs resource.WithFromEnv, and
//     internal/telemetry's TestBuildResource_HonoursResourceAttributesEnv
//     asserts it.
//  2. The k3d deployment must actually SET it, along the same axes the
//     telemetrygen sample apps already describe themselves by, so every
//     OTLP producer in the cluster is breakdown-able.
//
// The lane that would otherwise catch a regression here (`dashboard`) is
// a release gate: it short-circuits to a green no-op on an ordinary pull
// request and does its real work on the merge commit. A static pin in the
// `check` lane fails on the PR instead.
func TestK3sCerberusPublishesBreakdownResourceDimensions(t *testing.T) {
	t.Parallel()

	valuesPath := "../e2e/k3s/cerberus-values.yaml"
	values, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read e2e values %s: %v", valuesPath, err)
	}

	// The chart renders `extraEnv` verbatim into the container's env, so
	// the name/value pair appearing there IS the deployed variable.
	attrsRE := regexp.MustCompile(`(?m)^\s*-\s*name:\s*OTEL_RESOURCE_ATTRIBUTES\s*$\n\s*value:\s*(\S.*?)\s*$`)
	m := attrsRE.FindStringSubmatch(string(values))
	if m == nil {
		t.Fatalf("%s does not set OTEL_RESOURCE_ATTRIBUTES in extraEnv — cerberus then describes itself along service.name / service.version / service.instance.id only, every Traces Drilldown breakdown by another resource axis is empty, and iterate-drilldown-apps.spec.ts fails its two-level floor (#1818)", valuesPath)
	}
	deployed := strings.Trim(m[1], `"'`)
	if !strings.Contains(deployed, drilldownBreakdownDimension+"=") {
		t.Errorf("%s sets OTEL_RESOURCE_ATTRIBUTES=%q, which carries no %s — that is the exact axis Traces Drilldown breaks a service selection down by, so the drill dead-ends one level in (#1818)", valuesPath, deployed, drilldownBreakdownDimension)
	}

	// Lock-step with the telemetrygen sample apps: the whole cluster has to
	// describe itself along the SAME axis, or a breakdown that works for
	// the sample services silently dead-ends on cerberus (and vice versa).
	appPath := "../e2e/k3s/sample-app.yaml"
	app, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("read sample app %s: %v", appPath, err)
	}
	if !strings.Contains(string(app), drilldownBreakdownDimension+"=") {
		t.Fatalf("%s no longer sets %s on the telemetrygen producers — this test's lock-step premise is gone; re-derive which resource axis the cluster is uniformly breakdown-able on", appPath, drilldownBreakdownDimension)
	}
	sampleNSRE := regexp.MustCompile(regexp.QuoteMeta(drilldownBreakdownDimension) + `="?([A-Za-z0-9_.-]+)"?`)
	sample := sampleNSRE.FindStringSubmatch(string(app))
	if sample == nil {
		t.Fatalf("%s sets %s but its value did not parse — adjust the pattern rather than dropping the lock-step assertion", appPath, drilldownBreakdownDimension)
	}
	if !strings.Contains(deployed, drilldownBreakdownDimension+"="+sample[1]) {
		t.Errorf("%s deploys OTEL_RESOURCE_ATTRIBUTES=%q but %s puts the telemetrygen producers in %s=%q — the two must agree so the whole k3d cluster is breakdown-able along one namespace", valuesPath, deployed, appPath, drilldownBreakdownDimension, sample[1])
	}
}
