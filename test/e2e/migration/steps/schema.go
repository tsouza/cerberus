package steps

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// metricsTTLOverride is the per-signal metrics retention the override case
// renders under. Ninety days is long enough to be a realistic operator choice
// and is distinguishable from the unset default, which emits no TTL clause at
// all. The value lives here rather than in the feature file: an epsilon or a
// tuned number in feature text is a per-case allow-list.
const metricsTTLOverride = "90d"

// schemaTTLMetricsEnv is the per-signal metrics retention override the renderer
// reads.
const schemaTTLMetricsEnv = "CERBERUS_SCHEMA_TTL_METRICS"

// schemaCases maps a Scenario Outline case name to the additional environment
// that case renders under. The Examples table carries only the case name, so
// the numbers stay in Go.
var schemaCases = map[string][]string{
	"default":              nil,
	"metrics-ttl-override": {schemaTTLMetricsEnv + "=" + metricsTTLOverride},
}

// schemaGoldenDir is the directory, under the harness tree, holding one golden
// per schema case.
const schemaGoldenDir = "expected/schema"

// createObjectRe captures the object a CREATE statement declares, quoting
// style and `IF NOT EXISTS` included.
var createObjectRe = regexp.MustCompile(`(?is)^CREATE\s+(?:DATABASE|TABLE|MATERIALIZED\s+VIEW)\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(]+)`)

// leadingVerbRe captures a statement's leading SQL verb.
var leadingVerbRe = regexp.MustCompile(`(?is)^([A-Za-z]+)`)

// addProjectionRe matches the one non-CREATE shape the renderer emits: an
// idempotent projection addition on a table it just declared. Any other ALTER
// would be a schema mutation the operator did not ask to review.
var addProjectionRe = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+\S+\s+ADD\s+PROJECTION\s+IF\s+NOT\s+EXISTS\s+`)

// destructiveRe matches a statement that would destroy or rewrite data. The
// rendered schema is a provisioning preview an operator pipes into a client by
// hand; a destructive statement hiding in it would turn that review into a
// data-loss event.
var destructiveRe = regexp.MustCompile(`(?i)\b(DROP|TRUNCATE|INSERT|DELETE|UPDATE|RENAME|DETACH)\b`)

// schemaRender is what the MIG-10 When step produced: which case ran, under
// which extra environment, and the child process's captured result.
type schemaRender struct {
	caseName string
	env      []string
	ran      bool
	result   lib.Result
}

// registerSchemaSteps binds the MIG-10 schema-render steps.
func (w *World) registerSchemaSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the (\S+) cerberus schema environment$`, w.givenSchemaEnvironment)
	ctx.Step(`^the operator renders the ClickHouse schema offline$`, w.whenRenderSchema)
	ctx.Step(`^the rendered schema matches the committed golden for that environment$`, w.thenSchemaMatchesGolden)
	ctx.Step(`^the rendered schema declares a table for every signal cerberus reads$`, w.thenSchemaDeclaresEverySignalTable)
	ctx.Step(`^the renderer reports success without opening a database connection$`, w.thenRendererSucceededOffline)
	ctx.Step(`^the renderer applies nothing to any database$`, w.thenRendererAppliesNothing)
}

// givenSchemaEnvironment selects the environment the named case renders under.
// An unrecognised case name is a hard failure: a scenario naming a case the Go
// table does not carry would otherwise render the default and assert against
// the wrong golden.
func (w *World) givenSchemaEnvironment(caseName string) error {
	env, ok := schemaCases[caseName]
	if !ok {
		return fmt.Errorf("unknown schema case %q; the harness declares %v", caseName, sortedCaseNames())
	}
	w.schema = schemaRender{caseName: caseName, env: env}
	return nil
}

// whenRenderSchema runs `cerberus migrate schema` under the case's environment,
// with the network blackholed and ClickHouse pointed at a closed port.
func (w *World) whenRenderSchema() error {
	if w.schema.caseName == "" {
		return fmt.Errorf("no schema case selected; the scenario must name its schema environment first")
	}
	res, err := w.run(w.schema.env, "migrate", "schema")
	if err != nil {
		return err
	}
	w.schema.result = res
	w.schema.ran = true
	return nil
}

// thenSchemaMatchesGolden compares the rendered schema against the case's
// committed golden, and asserts the render is non-empty so an empty artifact
// cannot match an empty golden into a green scenario.
func (w *World) thenSchemaMatchesGolden() error {
	stmts, err := w.renderedStatements()
	if err != nil {
		return err
	}
	if len(stmts) == 0 {
		return fmt.Errorf("the %s case rendered no statements at all", w.schema.caseName)
	}
	golden := harnessPath(w.root, schemaGoldenDir, w.schema.caseName+".sql")
	return lib.AssertGolden(golden, w.schema.result.Stdout)
}

// thenSchemaDeclaresEverySignalTable asserts the render declares the table
// cerberus reads for each signal. The expected names come from cerberus's own
// schema defaults, so a renamed table breaks here rather than in production.
func (w *World) thenSchemaDeclaresEverySignalTable() error {
	stmts, err := w.renderedStatements()
	if err != nil {
		return err
	}
	declared := declaredObjects(stmts)
	if len(declared) == 0 {
		return fmt.Errorf("the %s case declared no objects", w.schema.caseName)
	}

	var missing []string
	for _, want := range signalTables() {
		if _, ok := declared[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the %s case does not declare %v; it declares %v",
			w.schema.caseName, missing, sortedKeys(declared))
	}
	return nil
}

// thenRendererSucceededOffline asserts the render exited cleanly with a silent
// stderr while every network route was closed — the offline claim proven by
// construction rather than taken from the tool.
func (w *World) thenRendererSucceededOffline() error {
	if !w.schema.ran {
		return fmt.Errorf("the schema renderer has not been run")
	}
	if w.schema.result.ExitCode != 0 {
		return fmt.Errorf("the %s case exited %d: %s",
			w.schema.caseName, w.schema.result.ExitCode, strings.TrimSpace(string(w.schema.result.Stderr)))
	}
	if len(w.schema.result.Stderr) != 0 {
		return fmt.Errorf("the %s case wrote to stderr: %q", w.schema.caseName, string(w.schema.result.Stderr))
	}
	return nil
}

// thenRendererAppliesNothing asserts every rendered statement is a schema
// declaration the operator applies by hand — a CREATE, or the idempotent
// projection addition on a table the same render declares — and that none of
// them destroys or rewrites data.
func (w *World) thenRendererAppliesNothing() error {
	stmts, err := w.renderedStatements()
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		verb := strings.ToUpper(leadingVerbRe.FindString(stmt))
		switch verb {
		case "CREATE":
		case "ALTER":
			if !addProjectionRe.MatchString(stmt) {
				return fmt.Errorf("the %s case renders an ALTER that is not an idempotent projection addition: %s",
					w.schema.caseName, firstLine(stmt))
			}
		default:
			return fmt.Errorf("the %s case renders a %q statement; only schema declarations may be rendered: %s",
				w.schema.caseName, verb, firstLine(stmt))
		}
		if hit := destructiveRe.FindString(stmt); hit != "" {
			return fmt.Errorf("the %s case renders a statement carrying %q: %s",
				w.schema.caseName, hit, firstLine(stmt))
		}
	}
	return nil
}

// renderedStatements returns the statements the current case rendered, failing
// when the When step has not run.
func (w *World) renderedStatements() ([]string, error) {
	if !w.schema.ran {
		return nil, fmt.Errorf("the schema renderer has not been run")
	}
	return schemaStatements(w.schema.result.Stdout), nil
}

// schemaStatements splits a rendered schema into statements. The renderer
// terminates each statement with ';' and no statement body carries one, so
// splitting on the terminator is exact.
func schemaStatements(out []byte) []string {
	parts := strings.Split(string(out), ";")
	stmts := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			stmts = append(stmts, trimmed)
		}
	}
	return stmts
}

// declaredObjects returns the set of object names the CREATE statements
// declare, unquoted and stripped of their database qualifier.
func declaredObjects(stmts []string) map[string]struct{} {
	out := make(map[string]struct{}, len(stmts))
	for _, stmt := range stmts {
		m := createObjectRe.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		out[unqualify(m[1])] = struct{}{}
	}
	return out
}

// unqualify strips quoting and any database qualifier from an object reference.
func unqualify(ref string) string {
	name := strings.Trim(ref, "\"`")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.Trim(name, "\"`")
}

// signalTables lists the table cerberus reads for each signal, taken from the
// schema defaults the server itself resolves.
func signalTables() []string {
	metrics := schema.DefaultOTelMetrics()
	return []string{
		metrics.GaugeTable,
		metrics.SumTable,
		metrics.HistogramTable,
		metrics.ExpHistogramTable,
		metrics.SummaryTable,
		schema.DefaultOTelLogs().LogsTable,
		schema.DefaultOTelTraces().SpansTable,
	}
}

// firstLine trims a statement to its opening line for an error message.
func firstLine(stmt string) string {
	if i := strings.IndexByte(stmt, '\n'); i >= 0 {
		return stmt[:i]
	}
	return stmt
}

// sortedCaseNames lists the declared schema cases for an error message.
func sortedCaseNames() []string {
	names := make([]string, 0, len(schemaCases))
	for name := range schemaCases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedKeys lists a set's members in a deterministic order.
func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
