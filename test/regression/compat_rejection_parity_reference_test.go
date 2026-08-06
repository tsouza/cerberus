package regression

import (
	"os"
	"strings"
	"testing"

	promparser "github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	rejectionparity "github.com/tsouza/cerberus/test/rejection-parity"
)

// The rejection-parity driver proves that every deliberate 4xx in
// internal/promql is also a 4xx in reference Prometheus. That proof is only
// worth something when the two backends are asked the same question under
// the same conditions. Three conditions have to hold, and each one failed
// silently at some point (#1770):
//
//   - The reference must run with the SAME PromQL surface cerberus parses
//     with. cerberus parses with EnableExperimentalFunctions; a reference
//     without --enable-feature=promql-experimental-functions rejects every
//     experimental symbol at the function-existence stage, so "both 4xx"
//     records agreement about a feature flag rather than about the guard
//     under test.
//   - The driver must evaluate INSIDE the fixture's window. The harness
//     fixture is anchored in a fixed past hour; at wall-clock `now` every
//     selector is empty, so upstream's eval-time per-series guards never
//     run and a data-dependent rejection claim proves nothing.
//   - Every trigger query must name metric families the seeder actually
//     writes. A trigger over an unseeded family is empty on both sides for
//     the same reason, whatever the flag and the eval time say.
//
// The three tests below pin one condition each.
const (
	compatComposePath     = "../../compatibility/prometheus/docker-compose.yml"
	compatDriverScript    = "../../compatibility/prometheus/scripts/run-prometheus-compatibility.sh"
	rejectionCatalogueDir = "../../test/rejection-parity/catalogue"

	// promExperimentalFunctionsFlag is the reference-Prometheus flag that
	// admits the experimental PromQL surface (limitk, limit_ratio,
	// double_exponential_smoothing, sort_by_label, mad_over_time, info,
	// histogram_quantiles, …). cerberus's parser has that surface on
	// unconditionally.
	promExperimentalFunctionsFlag = "--enable-feature=promql-experimental-functions"

	// rejectionDriverPath is how the harness scripts spell the driver's
	// package path in their `go run` invocation.
	rejectionDriverPath = "./compatibility/cmd/rejection-parity"

	// rejectionDriverEvalTimeArg pins the driver onto the same instant the
	// upstream compliance tester uses, which the seeder places inside the
	// fixture window.
	rejectionDriverEvalTimeArg = `-eval-time "$END_TIME"`

	promHead = "promql"
)

// composeModel is the sliver of the compat compose file this test reads: the
// reference Prometheus service's argv.
type composeModel struct {
	Services struct {
		Prometheus struct {
			Image   string   `yaml:"image"`
			Command []string `yaml:"command"`
		} `yaml:"prometheus"`
	} `yaml:"services"`
}

// TestCompatReferencePrometheusEnablesExperimentalFunctions pins the flag that
// makes the reference comparable to cerberus. Without it the reference
// rejects experimental symbols before reaching the semantic guard each
// rejection-parity entry claims to check.
func TestCompatReferencePrometheusEnablesExperimentalFunctions(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(compatComposePath)
	if err != nil {
		t.Fatalf("read %s: %v", compatComposePath, err)
	}
	var model composeModel
	if err := yaml.Unmarshal(raw, &model); err != nil {
		t.Fatalf("parse %s: %v", compatComposePath, err)
	}

	cmd := model.Services.Prometheus.Command
	if len(cmd) == 0 {
		t.Fatalf("%s declares no command for the prometheus service; this test would "+
			"compare nothing", compatComposePath)
	}
	for _, arg := range cmd {
		if strings.TrimSpace(arg) == promExperimentalFunctionsFlag {
			return
		}
	}
	t.Errorf("reference Prometheus in %s runs without %s (argv: %v).\n"+
		"cerberus parses PromQL with EnableExperimentalFunctions, so without this flag "+
		"the reference rejects every experimental symbol at the function-existence stage "+
		"and the rejection-parity driver scores agreement about a feature flag instead of "+
		"about the guard under test.",
		compatComposePath, promExperimentalFunctionsFlag, cmd)
}

// TestRejectionParityDriverEvaluatesInsideFixtureWindow pins the -eval-time
// argument onto the driver invocation. The harness fixture is seeded into a
// fixed past hour (compatibility/prometheus/cmd/seed/main.go's anchor); a
// driver left at wall-clock `now` reads empty selectors on both sides, and
// upstream's eval-time per-series validation (double_exponential_smoothing's
// smoothing/trend-factor bounds, for one) never executes.
func TestRejectionParityDriverEvaluatesInsideFixtureWindow(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(compatDriverScript)
	if err != nil {
		t.Fatalf("read %s: %v", compatDriverScript, err)
	}
	script := string(raw)

	start := strings.Index(script, rejectionDriverPath)
	if start < 0 {
		t.Fatalf("%s never invokes %s; this test would compare nothing",
			compatDriverScript, rejectionDriverPath)
	}
	// The invocation is a parenthesised subshell whose arguments are
	// backslash-continued; the first unescaped `)` after the package path
	// closes it.
	rest := script[start:]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("%s: the %s invocation is never closed", compatDriverScript, rejectionDriverPath)
	}
	invocation := rest[:end]

	if !strings.Contains(invocation, rejectionDriverEvalTimeArg) {
		t.Errorf("%s invokes %s without %s:\n%s\n"+
			"The fixture lives in a fixed past window, so a driver evaluating at "+
			"wall-clock now reads empty selectors from BOTH backends and no "+
			"data-dependent reference guard fires.",
			compatDriverScript, rejectionDriverPath, rejectionDriverEvalTimeArg, invocation)
	}
}

// TestPromQLRejectionTriggersUseSeededMetrics pins every PromQL trigger query
// in the rejection catalogue onto a metric family the compat seeder writes.
// A trigger over an unseeded family returns empty from both backends however
// the reference is configured, which is the same hollow-green shape
// TestCompatCorpusReferencesOnlySeededMetrics closes for the corpus.
func TestPromQLRejectionTriggersUseSeededMetrics(t *testing.T) {
	t.Parallel()

	seeded := seededPromFamilies(t)
	if len(seeded) == 0 {
		t.Fatal("no metric families extracted from the compat seeder; this test would compare nothing")
	}

	cat, err := rejectionparity.LoadCatalogue(rejectionCatalogueDir)
	if err != nil {
		t.Fatalf("load %s: %v", rejectionCatalogueDir, err)
	}

	m := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	checked := 0
	sawExpHistogramReference := false
	for _, e := range cat.Entries {
		if e.Head != promHead || e.TriggerQuery == "" {
			continue
		}
		expr, perr := p.ParseExpr(e.TriggerQuery)
		if perr != nil {
			t.Errorf("%s: trigger %q does not parse: %v", e.Site, e.TriggerQuery, perr)
			continue
		}
		names := map[string]bool{}
		collectSelectorNames(expr, names)
		checked++
		for _, name := range sortedSetKeys(names) {
			// Exp-histogram routing rejects on the NAME alone
			// (schema.Metrics.IsExpHistogramMetric is a pure suffix
			// predicate, consulted before any row is read), so the
			// suffix IS the trigger: such an entry scores the guard at
			// its site whether or not the exact family it names is one
			// the seeder writes. This arm names no specific metric and
			// grows no entries.
			if m.IsExpHistogramMetric(name) {
				sawExpHistogramReference = true
				continue
			}
			if !seeded[name] {
				t.Errorf("%s: trigger %q selects %q, which the compat seeder never writes.\n"+
					"Both backends answer it empty, so the parity (or divergence) verdict "+
					"recorded for this entry is about an absent family rather than about the "+
					"guard at that site. Re-pin the trigger onto a seeded family (see %s).",
					e.Site, e.TriggerQuery, name, compatSeedSource)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no PromQL trigger queries found in the catalogue; this test would compare nothing")
	}
	if !sawExpHistogramReference {
		t.Error("no PromQL trigger selects an exp-histogram-suffixed name; the exemption arm " +
			"of this gate is dead and should be removed")
	}
}

// seededPromFamilies returns every metric name a PromQL query may legitimately
// name given the compat seeder's fixture set: the Prom-wire series names of
// each seeded family, plus each classic histogram's bare storage name (which
// histogram_quantile / histogram_quantiles take as their argument).
func seededPromFamilies(t *testing.T) map[string]bool {
	t.Helper()

	m := schema.DefaultOTelMetrics()
	out := map[string]bool{}
	for _, f := range parseSeedFixtures(t) {
		switch f.table {
		case m.HistogramTable:
			synthetic := promql.HistogramSyntheticNames(f.name, m)
			if len(synthetic) == 0 {
				t.Fatalf("fixture %q inserts into the histogram table but yields no "+
					"Prom-wire series names", f.name)
			}
			for _, name := range synthetic {
				out[name] = true
			}
			out[f.name] = true
		case m.ExpHistogramTable:
			// A native histogram is a single Prom-wire series carrying the
			// whole distribution, so there are no `_bucket` / `_sum` /
			// `_count` companions to synthesise: the storage name is the
			// series name, and the histogram_* value functions take it
			// directly.
			out[f.name] = true
		case m.GaugeTable, m.SumTable:
			out[f.name] = true
		default:
			t.Fatalf("fixture %q inserts into %q: teach this test how that table's "+
				"rows surface as PromQL series names", f.name, f.table)
		}
	}
	return out
}
