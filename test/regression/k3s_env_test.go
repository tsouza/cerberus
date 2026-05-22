package regression

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestK3sCerberusManifestUsesOTLPEnvNames guards against the bug behind
// the long-running e2e `dashboard` job partition + histogram-
// completeness failures (task #214 / #215 N2 / N5 regression class).
//
// The k3s ConfigMap in test/e2e/k3s/cerberus.yaml originally set
// `CERBERUS_OTEL_ENDPOINT` / `CERBERUS_OTEL_INSECURE` / `CERBERUS_OTEL_
// SERVICE_NAME` / `CERBERUS_OTEL_SAMPLER` — none of which cerberus
// actually reads. The binary's config (see internal/config.otlpFromEnv)
// consumes `CERBERUS_OTLP_*` (note the L). Result: cerberus ran with
// CERBERUS_OTLP_ENDPOINT="", telemetry.New short-circuited to the noop
// MeterProvider, and `cerberus_queries_total` / `cerberus_queries_
// duration_seconds_*` never reached the otel-collector → ClickHouse
// pipeline. The cerberus-self dashboard's "Query rate by language"
// panel then collapsed to a single anonymous bucket because no rows
// existed (the lower correctly emitted `sum by (cerberus_ql)
// (rate(cerberus_queries_total[5m]))` but the query matched zero CH
// rows), and the histogram-completeness probe reported `series count=0`
// for `cerberus_queries_duration_seconds_{bucket,count,sum}` for the
// same reason.
//
// The fix renames the four ConfigMap keys to the spellings cerberus
// reads. This test pins the rename so a future maintainer can't silently
// re-introduce the dead env-var spellings, and complements the unit-
// level coverage in internal/telemetry/metrics_test.go (which verifies
// the attribute set lands on the counter once the SDK is actually wired
// up — the SDK plumbing is what the bug broke, not the attribute set).
//
// Catches: any `CERBERUS_OTEL_*` (with an L missing) key appearing in
// the cerberus k3s ConfigMap. The check is path-bounded to that file
// because:
//
//   - The compose stack (docker-compose.yml) is verified by the live
//     compose-smoke job and uses the correct spellings already (the
//     compose stack was always reading the right vars; the regression
//     was k3d-only).
//   - The `CERBERUS_OTEL_*` token elsewhere in the repo (docs, comments,
//     test fixtures explicitly testing the dead-spelling fallback) is
//     legitimate.
func TestK3sCerberusManifestUsesOTLPEnvNames(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../e2e/k3s/cerberus.yaml")
	if err != nil {
		t.Fatalf("read k3s cerberus.yaml: %v", err)
	}

	// Scan for any `CERBERUS_OTEL_<KEY>:` ConfigMap entry. The
	// trailing `:` keeps the regex tight to YAML-key position so
	// matching strings inside comments don't false-positive — every
	// real ConfigMap key ends in `:`.
	deadKeyRE := regexp.MustCompile(`(?m)^\s*CERBERUS_OTEL_[A-Z_]+:`)

	matches := deadKeyRE.FindAllString(string(buf), -1)
	if len(matches) > 0 {
		var lines []string
		for _, m := range matches {
			lines = append(lines, strings.TrimSpace(m))
		}
		t.Errorf(
			"test/e2e/k3s/cerberus.yaml carries %d `CERBERUS_OTEL_*` ConfigMap key(s) — cerberus reads `CERBERUS_OTLP_*` (note the L). Dead keys here mean the binary runs with OTLP disabled and emits no self-telemetry; the dashboard partition + histogram-completeness e2e probes regress. Found: %v",
			len(matches), lines,
		)
	}

	// Belt-and-braces: confirm the live spellings are present so a
	// future maintainer who hand-deletes the OTLP block (rather than
	// renaming it) sees a loud failure too.
	for _, want := range []string{"CERBERUS_OTLP_ENDPOINT:", "CERBERUS_OTLP_INSECURE:"} {
		if !strings.Contains(string(buf), want) {
			t.Errorf("test/e2e/k3s/cerberus.yaml missing required ConfigMap key %q — the k3d cerberus deployment needs the OTLP exporter wired or the cerberus-self dashboard panels return empty matrices", want)
		}
	}
}
