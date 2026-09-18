package regression

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
)

// The self-observability dashboard's "P95 latency by language" panel carries an
// explicit `interval` floor because its query — a range-mode native-histogram
// quantile — is the one provisioned panel whose cost grows with the anchor
// grid Grafana picks for it. Grafana's Prometheus datasource steps a panel
// with no floor at its default scrape interval, and at the dashboard's own
// default view that grid put the panel over the fold-cost ceiling
// (internal/chsql/lwr_fanout_bound.go,
// RangeBucketFanoutFoldCostUnitsForMemory), so the board's default view
// rendered a resource-bound rejection instead of a line (cerberus issues
// #3402 / #3468 / #3514).
//
// The parity tests beside this one prove the three copies agree; nothing
// there proves the floor exists or that it still protects what it was set
// for. This test derives that from the bound itself, so a recalibration of
// the bound, a wider histogram ladder, a faster export cadence, a new head, or
// a dropped/loosened interval fails here rather than on the next nightly.
const (
	// grafanaPrometheusDefaultMinStep is the `timeInterval` (scrape interval)
	// Grafana's Prometheus datasource applies as the step floor to a panel
	// that declares none — the step the P95 panel was stepped at before the
	// floor landed (calculatedMinStep 15000 in the failing nightly runs).
	grafanaPrometheusDefaultMinStep = 15 * time.Second

	// foldCostBoundName names the bound the interval protects in failure
	// messages. Its value is not a constant but a function of
	// CERBERUS_CH_QUERY_MAX_MEMORY; both provisioned stacks
	// (docker-compose.yml, test/e2e/k3s/cerberus-values.yaml) pin that cap
	// at the shipped default, which is the cap the test derives it from.
	foldCostBoundName = "RangeBucketFanoutFoldCostUnitsForMemory(CERBERUS_CH_QUERY_MAX_MEMORY)"

	// expHistogramMaxSizeSourceFile / expHistogramMaxSizeConstName name the
	// OTel exponential-histogram MaxSize cerberus collects the panel's metric
	// with: the hard ceiling on a series' bucket-ladder width W, which is
	// the axis the fold-cost bound charges quadratically.
	expHistogramMaxSizeSourceFile = "../../internal/telemetry/telemetry.go"
	expHistogramMaxSizeConstName  = "queryDurationExpoHistogramMaxSize"

	// p95PanelMetricSuffix identifies the panel: the one target that quantiles
	// the native query-duration histogram.
	p95PanelMetricSuffix = "_exp_hist"
)

// grafanaDurationRe matches the Grafana/PromQL duration literals this test
// reads: the `[5m]` range-vector window and the `1m` interval floor.
var grafanaDurationRe = regexp.MustCompile(`^(\d+)(ms|s|m|h|d)$`)

func TestP95PanelIntervalFloorKeepsTheDefaultViewUnderTheFoldCostBound(t *testing.T) {
	t.Parallel()

	ladderWidth := goConstInt(t, expHistogramMaxSizeSourceFile, expHistogramMaxSizeConstName)

	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	if cfg.ClickHouse.MaxQueryMemoryBytes <= 0 {
		t.Fatalf("default CERBERUS_CH_QUERY_MAX_MEMORY = %d; the fold-cost ceiling is derived from a positive cap", cfg.ClickHouse.MaxQueryMemoryBytes)
	}
	bound := chsql.RangeBucketFanoutFoldCostUnitsForMemory(cfg.ClickHouse.MaxQueryMemoryBytes)
	exportInterval := cfg.OTLP.ExportInterval
	if exportInterval <= 0 {
		t.Fatalf("default OTLP export interval = %s; the samples-per-window term needs a positive cadence", exportInterval)
	}
	// One legend series per head the process serves: the panel groups by
	// cerberus_ql, and every enabled head contributes one.
	heads := int64(len(cfg.EnabledHeads))
	if heads == 0 {
		t.Fatal("config.FromEnv resolved zero enabled heads")
	}

	for _, path := range []string{dashboardK3dPath, dashboardComposePath} {
		doc := readDashboardBody(t, path)
		panel, target := p95Panel(t, path, doc)
		expr, _ := target["expr"].(string)

		// Grafana honours the floor at either level; the checked-in copies
		// carry it on the target.
		rawInterval, _ := target["interval"].(string)
		if strings.TrimSpace(rawInterval) == "" {
			rawInterval, _ = panel["interval"].(string)
		}
		if strings.TrimSpace(rawInterval) == "" {
			t.Fatalf("%s: the P95 panel (%q) carries no `interval` floor; without one Grafana steps it at %s "+
				"and the dashboard's default view exceeds %s", path, panel["title"], grafanaPrometheusDefaultMinStep, foldCostBoundName)
		}
		interval := parseGrafanaDuration(t, rawInterval)
		window := parseGrafanaDuration(t, rangeVectorWindow(t, expr))
		view := defaultViewRange(t, path, doc)

		// Per-group fold cost, E + W^2, at the worst ladder the collector can
		// produce: W = MaxSize, E = S x W with S the samples one series lands
		// in the window at the export cadence (plus the boundary sample).
		samplesPerWindow := int64(window/exportInterval) + 1
		perGroupCost := samplesPerWindow*ladderWidth + ladderWidth*ladderWidth

		admitted := func(step time.Duration) int64 {
			anchors := int64(view/step) + 1
			return bound / (perGroupCost * anchors)
		}

		if got := admitted(interval); got < heads {
			t.Errorf("%s: at the panel's %s interval floor the default %s view admits %d worst-width series "+
				"under %s=%d (per-group cost %d units at W=%d, S=%d), fewer than the %d heads on the legend — "+
				"the panel's own default view would be rejected; raise the interval or recalibrate the bound",
				path, rawInterval, view, got, foldCostBoundName, bound, perGroupCost, ladderWidth, samplesPerWindow, heads)
		}
		// The floor is load-bearing, not decorative: at the step Grafana
		// would pick without it, the same view cannot carry one series per
		// head. If this ever passes without the floor, the interval is no
		// longer protecting anything and should be removed rather than
		// carried as folklore.
		if got := admitted(grafanaPrometheusDefaultMinStep); got >= heads {
			t.Errorf("%s: at Grafana's unfloored %s step the default %s view already admits %d worst-width series "+
				"(>= %d heads) under %s=%d — the %s interval floor no longer protects the panel; drop it or "+
				"re-derive what it is for", path, grafanaPrometheusDefaultMinStep, view, got, heads,
				foldCostBoundName, bound, rawInterval)
		}
	}
}

// p95Panel returns the one panel whose Prometheus target quantiles the
// native query-duration histogram, plus that target.
func p95Panel(t *testing.T, path string, doc map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	panels, _ := doc["panels"].([]any)
	var found, foundTarget map[string]any
	for _, p := range panels {
		panel, ok := p.(map[string]any)
		if !ok {
			continue
		}
		targets, _ := panel["targets"].([]any)
		for _, tg := range targets {
			target, ok := tg.(map[string]any)
			if !ok {
				continue
			}
			e, _ := target["expr"].(string)
			if strings.Contains(e, "histogram_quantile") && strings.Contains(e, p95PanelMetricSuffix) {
				if found != nil {
					t.Fatalf("%s: more than one panel quantiles a %s metric (%q and %q); this pin expects exactly one",
						path, p95PanelMetricSuffix, found["title"], panel["title"])
				}
				found, foundTarget = panel, target
			}
		}
	}
	if found == nil {
		t.Fatalf("%s: no panel quantiles a %s metric — the P95-by-language panel this pin protects is gone",
			path, p95PanelMetricSuffix)
	}
	return found, foundTarget
}

// rangeVectorWindow extracts the `[<window>]` range selector the panel's rate()
// reads over.
func rangeVectorWindow(t *testing.T, expr string) string {
	t.Helper()
	m := regexp.MustCompile(`\[(\d+(?:ms|s|m|h|d))\]`).FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("no range-vector window in %q", expr)
	}
	return m[1]
}

// defaultViewRange reads the dashboard's default `time.from` (`now-<d>`) —
// the view an operator lands on, and the one the failing nightlies rendered.
func defaultViewRange(t *testing.T, path string, doc map[string]any) time.Duration {
	t.Helper()
	tm, _ := doc["time"].(map[string]any)
	from, _ := tm["from"].(string)
	const prefix = "now-"
	if !strings.HasPrefix(from, prefix) {
		t.Fatalf("%s: time.from = %q; expected a relative `now-<duration>` default view", path, from)
	}
	return parseGrafanaDuration(t, strings.TrimPrefix(from, prefix))
}

func parseGrafanaDuration(t *testing.T, s string) time.Duration {
	t.Helper()
	m := grafanaDurationRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		t.Fatalf("unparseable Grafana duration %q", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("unparseable Grafana duration %q: %v", s, err)
	}
	units := map[string]time.Duration{
		"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour,
	}
	return time.Duration(n) * units[m[2]]
}

// goConstInt reads the integer value of a top-level `const` from a Go source
// file. It is for the unexported calibration constants production code
// deliberately does not export (their only runtime override is an env var);
// the test fails, rather than guessing, when the constant is missing or is
// not a plain integer literal.
func goConstInt(t *testing.T, path, name string) int64 {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s: const %s is not a plain integer literal (%s)", path, name, fmt.Sprint(vs.Values[i]))
				}
				n, err := strconv.ParseInt(strings.ReplaceAll(lit.Value, "_", ""), 0, 64)
				if err != nil {
					t.Fatalf("%s: const %s = %s: %v", path, name, lit.Value, err)
				}
				return n
			}
		}
	}
	t.Fatalf("%s: no top-level const %s", path, name)
	return 0
}
