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
