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

	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/test/e2e/seed/cadence"
)

// The Playwright harness, the compose stack and the CI launcher scripts
// cannot import Go, so a handful of production values are typed into them by
// hand: the resource-bound rejection messages the specs recognise as pinned
// contracts, the per-query ClickHouse memory cap both stacks set, the OTLP
// export cadence the seed waits are sized from, and the rolling re-seed
// interval. Each copy is load-bearing — a drifted message turns a pinned
// 422 into a hard failure, a drifted cadence silently loosens a wait — and
// nothing but these pins keeps them equal to the source they mirror.
const (
	resourceBoundsTSPath  = "../../test/e2e/playwright/helpers/resource-bounds.ts"
	promHandlerPath       = "../../internal/api/prom/handler.go"
	composeFilePath       = "../../docker-compose.yml"
	k3sCerberusValuesPath = "../../test/e2e/k3s/cerberus-values.yaml"
	seedRollingScriptPath = "../../.github/scripts/e2e-seed-rolling.mjs"

	chQueryMaxMemoryEnv = "CERBERUS_CH_QUERY_MAX_MEMORY"

	// promMemoryLimitFormatMarker identifies the Prom head's memory-limit
	// wire format among handler.go's string literals: the one that carries
	// the per-query cap as a %d verb.
	promMemoryLimitFormatMarker = "per-query cap %d bytes"
)

func TestPlaywrightResourceBoundMessagesMatchProduction(t *testing.T) {
	t.Parallel()
	ts := readFileString(t, resourceBoundsTSPath)

	// The guard messages: every production constant must appear verbatim in
	// the TS array, and the array must carry nothing else — a message the
	// specs would excuse that production never emits is as wrong as one
	// they would fail on.
	want := []string{
		chplan.HistogramMergeBudgetMessage,
		chplan.ExpHistogramWindowSampleBudgetMessage,
		chsql.RangeBucketFanoutGroupBudgetMessage,
	}
	got := tsStringArray(t, ts, "RESOURCE_BOUND_GUARD_MESSAGES")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s RESOURCE_BOUND_GUARD_MESSAGES = %q; production emits %q (internal/chplan, internal/chsql) — "+
			"update the TS array in lock-step", resourceBoundsTSPath, got, want)
	}

	// The memory-limit message: the TS template must be the handler's own
	// format with the cap interpolated where the handler puts %d.
	tsTemplate := tsTemplateLiteral(t, ts, "MEMORY_LIMIT_MESSAGE")
	tsFormat := strings.ReplaceAll(tsTemplate, "${CH_QUERY_MAX_MEMORY_BYTES}", "%d")
	goFormat := goStringLiteralContaining(t, promHandlerPath, promMemoryLimitFormatMarker)
	if tsFormat != goFormat {
		t.Errorf("%s MEMORY_LIMIT_MESSAGE template = %q; %s formats %q — update the TS template in lock-step",
			resourceBoundsTSPath, tsFormat, promHandlerPath, goFormat)
	}

	// The cap itself: both stacks set the same env, and the TS literal must
	// equal it, or the interpolated message never matches the wire.
	tsCap := tsNumericConst(t, ts, "CH_QUERY_MAX_MEMORY_BYTES")
	for _, path := range []string{composeFilePath, k3sCerberusValuesPath} {
		values := yamlEnvValues(t, path, chQueryMaxMemoryEnv)
		if len(values) == 0 {
			t.Errorf("%s does not set %s; the memory-cap parity check would otherwise pass over no values", path, chQueryMaxMemoryEnv)
			continue
		}
		for _, v := range values {
			if v != tsCap {
				t.Errorf("%s sets %s=%d but %s CH_QUERY_MAX_MEMORY_BYTES = %d", path, chQueryMaxMemoryEnv, v, resourceBoundsTSPath, tsCap)
			}
		}
	}
}

func TestPlaywrightExportIntervalMatchesConfigDefault(t *testing.T) {
	t.Parallel()
	ts := readFileString(t, resourceBoundsTSPath)
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	wantSeconds := int64(cfg.OTLP.ExportInterval.Seconds())
	if got := tsNumericConst(t, ts, "OTLP_EXPORT_INTERVAL_SECONDS"); got != wantSeconds {
		t.Errorf("%s OTLP_EXPORT_INTERVAL_SECONDS = %d; the CERBERUS_OTLP_EXPORT_INTERVAL default is %s — "+
			"the self-traffic seed waits are sized from this and would silently loosen",
			resourceBoundsTSPath, got, cfg.OTLP.ExportInterval)
	}
	overrides := regexp.MustCompile(`(?m)^\s*CERBERUS_OTLP_EXPORT_INTERVAL:`)
	for _, path := range []string{composeFilePath, k3sCerberusValuesPath} {
		if overrides.MatchString(readFileString(t, path)) {
			t.Errorf("%s overrides CERBERUS_OTLP_EXPORT_INTERVAL; the Playwright seed waits assume the config default", path)
		}
	}
}

func TestRollingReSeedIntervalLaunchersMatchTheCadencePackage(t *testing.T) {
	t.Parallel()
	if want := "--re-seed-interval=" + cadence.RollingReSeedInterval.String(); cadence.ReSeedIntervalFlag != want {
		t.Fatalf("cadence.ReSeedIntervalFlag = %q; RollingReSeedInterval implies %q", cadence.ReSeedIntervalFlag, want)
	}
	compose := readFileString(t, composeFilePath)
	if !strings.Contains(compose, fmt.Sprintf("command: [%q]", cadence.ReSeedIntervalFlag)) {
		t.Errorf("%s seed service does not pass %q; the stale-row margins in test/e2e/seed/cmd/seed/stale.go are reasoned "+
			"against cadence.RollingReSeedInterval", composeFilePath, cadence.ReSeedIntervalFlag)
	}
	script := readFileString(t, seedRollingScriptPath)
	if !strings.Contains(script, fmt.Sprintf("const reseedIntervalFlag = '%s';", cadence.ReSeedIntervalFlag)) {
		t.Errorf("%s reseedIntervalFlag is not %q", seedRollingScriptPath, cadence.ReSeedIntervalFlag)
	}
}

// tsStringArray returns the single-quoted string elements of
// `export const <name> = [ ... ];` in a TS source.
func tsStringArray(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)export const ` + regexp.QuoteMeta(name) + ` = \[(.*?)\];`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no `export const %s = [...]` in the TS source", name)
	}
	var out []string
	for _, e := range regexp.MustCompile(`'((?:[^'\\]|\\.)*)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, e[1])
	}
	return out
}

// tsTemplateLiteral returns the body of `export const <name> = \`...\`;`.
func tsTemplateLiteral(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile("export const " + regexp.QuoteMeta(name) + " = `([^`]*)`;")
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no `export const %s = `...`` in the TS source", name)
	}
	return m[1]
}

// tsNumericConst returns the integer value of `export const <name> = <n>;`,
// accepting TS digit separators.
func tsNumericConst(t *testing.T, src, name string) int64 {
	t.Helper()
	re := regexp.MustCompile(`export const ` + regexp.QuoteMeta(name) + ` = ([0-9_]+);`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no `export const %s = <integer>;` in the TS source", name)
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(m[1], "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("parse %s = %q: %v", name, m[1], err)
	}
	return n
}

// yamlEnvValues returns every `<env>: "<n>"` value in a YAML file (the
// quoted-integer form both stacks use for their env blocks).
func yamlEnvValues(t *testing.T, path, env string) []int64 {
	t.Helper()
	src := readFileString(t, path)
	values, err := parseYAMLEnvValues(src, env)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return values
}

func parseYAMLEnvValues(src, env string) ([]int64, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, err
	}
	var values []int64
	var visit func(*yaml.Node) error
	visit = func(node *yaml.Node) error {
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				key, value := node.Content[i], node.Content[i+1]
				if key.Value != env {
					continue
				}
				n, err := strconv.ParseInt(value.Value, 10, 64)
				if value.Kind != yaml.ScalarNode || err != nil || n <= 0 {
					return fmt.Errorf("line %d: %s has malformed positive integer %q", value.Line, env, value.Value)
				}
				values = append(values, n)
			}
		}
		for _, child := range node.Content {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(&doc); err != nil {
		return nil, err
	}
	return values, nil
}

func TestYAMLEnvValuesRejectMalformedMemoryCap(t *testing.T) {
	t.Parallel()
	for _, malformed := range []string{"not-a-size", "", "0", "-1", "[]", `"1024`} {
		src := "valid:\n  CERBERUS_CH_QUERY_MAX_MEMORY: '1024'\ninvalid:\n  CERBERUS_CH_QUERY_MAX_MEMORY: " + malformed + "\n"
		if _, err := parseYAMLEnvValues(src, chQueryMaxMemoryEnv); err == nil {
			t.Errorf("valid occurrence concealed malformed memory-cap value %q", malformed)
		}
	}
	values, err := parseYAMLEnvValues("env:\n  'CERBERUS_CH_QUERY_MAX_MEMORY': '1024' # bytes\n", chQueryMaxMemoryEnv)
	if err != nil || len(values) != 1 || values[0] != 1024 {
		t.Fatalf("valid YAML cap: values=%v err=%v", values, err)
	}
}

// goStringLiteralContaining returns the unquoted value of the one string
// literal in a Go source file that contains marker.
func goStringLiteralContaining(t *testing.T, path, marker string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v, err := strconv.Unquote(lit.Value)
		if err == nil && strings.Contains(v, marker) {
			found = append(found, v)
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("%s: expected exactly one string literal containing %q, found %d: %q", path, marker, len(found), found)
	}
	return found[0]
}
