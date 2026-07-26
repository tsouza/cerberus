package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// tenantProbeBudget bounds the live ClickHouse and cerberus round-trips this
// probe makes.
const tenantProbeBudget = 30 * time.Second

// foreignTenantDatabase is the ClickHouse database MIG-15 plants a foreign
// tenant's data into — a real per-tenant CH database boundary, the isolation
// pattern a mimir-cortex-style multi-tenant migration maps onto (each tenant
// keeps its own CH database; the operator deploys one cerberus per tenant,
// each bound to its own tenant's database via CERBERUS_CH_DATABASE). It is
// deliberately named outside the fixture namespace the migration corpus
// selects, mirroring tier1_stack_test.go's own probeMetric convention.
const foreignTenantDatabase = "cerberus_migration_tenant_b_probe"

// foreignTenantMetric is the synthetic metric name planted only in the
// foreign tenant's database. Its absence from cerberus's own metric-name
// answer is the isolation assertion; its presence would mean cerberus somehow
// reached outside its configured database.
const foreignTenantMetric = "cerberus_migration_foreign_tenant_probe"

// foreignTenantLabel / foreignTenantLabelValue are a datapoint attribute
// carried ONLY by the foreign tenant's row. cerberus surfaces datapoint
// attribute keys as label names on /api/v1/labels, so a key that exists in
// exactly one tenant's database is the metadata half of the isolation claim
// MIG-15's PASS cell asks for ("metadata endpoints return correct per-tenant
// label sets"): a cross-tenant read leaks the label as surely as the series.
const (
	foreignTenantLabel      = "cerberus_migration_foreign_tenant_label"
	foreignTenantLabelValue = "tenant-b-only"
)

// foreignTenantValue is the gauge value the plant carries, and
// foreignTenantService the service it is attributed to. The value is
// deliberately unlike any value the fixture's own generators draw, so a
// read-back that picked up some other row is visible rather than accidentally
// agreeing.
const (
	foreignTenantValue   = 424.25
	foreignTenantService = "migration-tenant-b"
)

// foreignTenantPlantedRows is how many rows the plant writes. One row is
// enough to prove the boundary, and naming the count is what lets the negative
// control compare the foreign database's row count against a declared number
// rather than against a literal restated at the assertion site.
const foreignTenantPlantedRows = 1

// gaugeProbeColumns is the gauge column layout every direct-INSERT probe in
// this harness writes — byte-identical to insertRecordedSeriesSQL's list, held
// equal by TestForeignTenantPlantMatchesGaugeProbeLayout. The plant matters
// only because it lands in the SAME shape cerberus reads every day: a bespoke
// marker table would be invisible to cerberus for a reason that has nothing to
// do with the tenant boundary, which is exactly the hollow assertion this
// replaced.
var gaugeProbeColumns = []string{
	"ResourceAttributes", "ServiceName", "MetricName", "MetricDescription", "MetricUnit",
	"Attributes", "StartTimeUnix", "TimeUnix", "Value", "Flags",
}

// Tier1QuerySampleBudget mirrors the CERBERUS_QUERY_MAX_SAMPLES the tier-1
// stack's cerberus runs under. Under MIG-15's deployment model — one cerberus
// per tenant, each bound to its own CH database — that per-process budget IS
// the per-tenant read budget the story asks to see enforced. It is exported so
// test/regression's TestMigrationTier1SampleBudgetIsPinned can hold it equal to
// the compose stack's own setting, without which the derived over-budget grid
// below could silently drift into being under budget.
const Tier1QuerySampleBudget = 5_000_000

// overBudgetSubqueryStep is the finest resolution a PromQL subquery can ask
// for, so a grid one second wider than the budget materialises exactly one
// anchor row per second past it — the SMALLEST over-budget grid expressible.
// Keeping it minimal is what makes the rejection attributable to the budget
// rather than to an absurd query any guard would refuse.
const overBudgetSubqueryStep = time.Second

// sampleBudgetErrorMessage / sampleBudgetErrorType are the operator-facing
// wire contract for a budget rejection: upstream Prometheus's verbatim message
// and errorType, which cerberus mirrors (internal/api/prom's
// promMaxSamplesMessage + ErrExecution, HTTP 422). They are restated here
// rather than imported because the symbols there are unexported on purpose —
// and because a test that read the same symbol would agree with itself if the
// message ever changed silently, which is the opposite of what "refused with a
// SPECIFIC error" means.
const (
	sampleBudgetErrorMessage = "query processing would load too many samples into memory in query execution"
	sampleBudgetErrorType    = "execution"
)

// tenantProbe is MIG-15's state: what was planted into the foreign tenant's
// database, what cerberus's own metadata surface returned, and how cerberus
// answered a read inside and a read outside its configured sample budget.
type tenantProbe struct {
	established bool
	archetype   string

	// gaugeTable is the table the plant landed in — resolved from the same
	// env-backed schema config the live server reads its own table names from,
	// so a renamed gauge table moves the plant with it.
	gaugeTable string
	// planted is how many rows the Given actually wrote. The negative control
	// compares the foreign database's row count against THIS, rather than
	// against a literal restated at the assertion site.
	planted int
	// created records that this run made the foreign database, so the After
	// hook drops exactly what the harness minted on the shared live stack.
	created bool

	// metricNames / labelNames are cerberus's own answers, read back from its
	// Prometheus-compatible metadata surface.
	metricNames []string
	labelNames  []string
	queried     bool

	// metricNamesProven / labelNamesProven record that the POSITIVE CONTROL
	// over each surface passed in THIS run: cerberus's catalog really carried
	// every name the archetype declares, so the surface was populated when the
	// matching absence leg looked at it. Without them "the foreign tenant's
	// name is not in this list" is satisfied by an empty list — cerberus
	// answering nothing at all would read as perfect isolation.
	metricNamesProven bool
	labelNamesProven  bool

	// underBudgetServices holds one entry per series the in-budget read
	// returned, carrying that series' service_name — the cardinality and the
	// identity the manifest and the declaration are checked against.
	underBudgetServices []string
	underBudgetSeries   int
	budgetRead          bool

	// overBudget* is the verbatim rejection cerberus answered the out-of-budget
	// read with.
	overBudgetStatus  int
	overBudgetType    string
	overBudgetMessage string
}

// registerTenantIsolationSteps binds MIG-15: a foreign tenant's data planted
// directly in ClickHouse in its own database, in the exact table layout
// cerberus reads, and the assertions that (a) the plant is genuinely there,
// (b) cerberus's own read surface never surfaces it while its own tenant's
// declared data stays fully queryable, and (c) a read past the configured
// per-tenant budget is refused with the specific budget error.
func (w *World) registerTenantIsolationSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^a foreign tenant's series planted in its own ClickHouse database, in the table layout cerberus reads$`,
		w.givenForeignTenantPlanted)
	ctx.Step(`^the operator queries cerberus for every metric name and label it can see$`, w.whenQueryTenantMetadata)
	ctx.Step(`^the operator asks cerberus to read within, and then beyond, its configured sample budget$`,
		w.whenReadTenantBudget)
	ctx.Step(`^the planted series is readable in the foreign tenant's own database$`, w.thenForeignTenantPlantReadable)
	ctx.Step(`^the foreign tenant's metric name never appears in cerberus's answer$`, w.thenForeignTenantAbsent)
	ctx.Step(`^every metric name the archetype declares appears in cerberus's answer$`, w.thenDeclaredMetricNamesPresent)
	ctx.Step(`^the foreign tenant's label never appears in cerberus's label surface$`, w.thenForeignTenantLabelAbsent)
	ctx.Step(`^every mapped label the archetype declares appears in cerberus's label surface$`, w.thenMappedLabelsPresent)
	ctx.Step(`^the read within the sample budget returns every series the archetype declares$`, w.thenUnderBudgetReadComplete)
	ctx.Step(`^the read beyond the sample budget is refused with cerberus's sample-budget error$`, w.thenOverBudgetReadRefused)
}

// foreignTenantInsertSQL builds the plant's INSERT against the foreign
// tenant's own database. The column list is gaugeProbeColumns, the same list
// the recorded-series probe writes into the CONFIGURED tenant's gauge table,
// so the two rows differ only in which database holds them.
func foreignTenantInsertSQL(database, table string) string {
	return "INSERT INTO " + database + "." + table + " (" + strings.Join(gaugeProbeColumns, ", ") + ")"
}

// givenForeignTenantPlanted writes one distinctly-named gauge row into a
// SEPARATE ClickHouse database that cerberus's live instance is never
// configured to read.
//
// The foreign table is created with `CREATE TABLE … AS <tenant>.<gauge>`,
// which copies the live tenant table's column list AND its engine definition:
// the plant therefore lands in a table cerberus's read path WOULD query if it
// were pointed at this database. That is the whole point — it makes the
// absence of the planted name from cerberus's answer a statement about the
// DATABASE BOUNDARY, not about a table shape cerberus could never have read.
//
// The database is dropped first rather than reused: a stale copy left behind
// by a crashed run would carry a second planted row, and the negative control
// compares the row count against what THIS run wrote.
func (w *World) givenForeignTenantPlanted() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; the scenario must establish it first")
	}
	if err := w.requireArchetypes("the tenant-isolation probe"); err != nil {
		return err
	}
	if len(w.archetypes) != 1 {
		return fmt.Errorf("the tenant-isolation probe reads one archetype's fixture; got %v", w.archetypes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), tenantProbeBudget)
	defer cancel()

	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	gauge := schema.DefaultOTelMetricsFromEnv().GaugeTable
	if err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+foreignTenantDatabase); err != nil {
		return fmt.Errorf("migration harness: drop a stale foreign tenant database: %w", err)
	}
	if err := conn.Exec(ctx, "CREATE DATABASE "+foreignTenantDatabase); err != nil {
		return fmt.Errorf("migration harness: create the foreign tenant database: %w", err)
	}
	w.tenant = tenantProbe{archetype: w.archetypes[0], gaugeTable: gauge, created: true}

	clone := fmt.Sprintf("CREATE TABLE %s.%s AS %s.%s", foreignTenantDatabase, gauge, w.live.CHDatabase, gauge)
	if err := conn.Exec(ctx, clone); err != nil {
		return fmt.Errorf("migration harness: clone %s.%s into the foreign tenant database: %w", w.live.CHDatabase, gauge, err)
	}

	batch, err := conn.PrepareBatch(ctx, foreignTenantInsertSQL(foreignTenantDatabase, gauge))
	if err != nil {
		return fmt.Errorf("migration harness: prepare the foreign tenant plant: %w", err)
	}
	const noFlags = uint32(0)
	const description = "MIG-15 foreign-tenant isolation probe"
	const unit = "1"
	at := time.Now().UTC().Truncate(time.Second)
	if err := batch.Append(
		map[string]string{"service.name": foreignTenantService},
		foreignTenantService, foreignTenantMetric, description, unit,
		map[string]string{foreignTenantLabel: foreignTenantLabelValue},
		at, at, float64(foreignTenantValue), noFlags,
	); err != nil {
		return fmt.Errorf("migration harness: append the foreign tenant plant: %w", err)
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("migration harness: send the foreign tenant plant: %w", err)
	}
	w.tenant.planted = foreignTenantPlantedRows
	w.tenant.established = true
	return nil
}

// dropForeignTenantDatabase removes the foreign tenant's database from the
// SHARED live ClickHouse. The After hook calls it unconditionally, so a
// scenario that failed halfway through still cleans up: leaving the database
// behind would leak one run's plant into every later run of the same stack —
// including the next run's own isolation assertion, which would then be
// judging a row nobody in that run planted.
func (w *World) dropForeignTenantDatabase() error {
	if !w.tenant.created {
		return nil
	}
	w.tenant.created = false
	ctx, cancel := context.WithTimeout(context.Background(), tenantProbeBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return fmt.Errorf("migration harness: drop the foreign tenant database: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+foreignTenantDatabase); err != nil {
		return fmt.Errorf("migration harness: drop the foreign tenant database: %w", err)
	}
	return nil
}

// whenQueryTenantMetadata asks cerberus's own Prometheus-compatible metadata
// surface for both halves of what a tenant can discover: every metric name,
// and every label name.
func (w *World) whenQueryTenantMetadata() error {
	if !w.tenant.established {
		return fmt.Errorf("no foreign tenant has been planted; the scenario must establish one first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), tenantProbeBudget)
	defer cancel()

	names, err := fetchLabelValues(ctx, w.live.CerberusURL, "__name__")
	if err != nil {
		return fmt.Errorf("migration harness: query cerberus's metric-name catalog: %w", err)
	}
	labels, err := fetchLabelNames(ctx, w.live.CerberusURL)
	if err != nil {
		return fmt.Errorf("migration harness: query cerberus's label-name surface: %w", err)
	}
	w.tenant.metricNames = names
	w.tenant.labelNames = labels
	w.tenant.queried = true
	return nil
}

// whenReadTenantBudget drives two reads of the SAME shape against the
// configured tenant, differing only in how many anchor rows their subquery
// grid materialises: one comfortably inside the tenant's configured sample
// budget, one deliberately past it.
//
// Both are `max_over_time(<gauge>[<range>:<step>])`. A PromQL subquery
// materialises range/step + 1 anchor rows per series, and cerberus rejects a
// grid that alone exceeds CERBERUS_QUERY_MAX_SAMPLES before any SQL runs
// (internal/engine's requireSubquerySampleBudget). Pairing them is what makes
// the rejection falsifiable: an in-budget grid over the same metric MUST be
// answered, so a cerberus that refused every subquery would fail here rather
// than green the budget assertion.
func (w *World) whenReadTenantBudget() error {
	if !w.tenant.established {
		return fmt.Errorf("no foreign tenant has been planted; the scenario must establish one first")
	}
	manifest, err := w.live.LoadManifest(w.tenant.archetype)
	if err != nil {
		return err
	}
	step, err := manifest.StepDuration()
	if err != nil {
		return err
	}
	if manifest.GaugeMetric == "" {
		return fmt.Errorf("archetype %s's manifest declares no gauge metric to read", w.tenant.archetype)
	}
	w.manifest[w.tenant.archetype] = manifest

	// The in-budget grid spans the manifest's whole verify window at the
	// seeder's own step, so every gauge series the seeder wrote is inside it by
	// construction — the read cannot come back short for a windowing reason.
	inWindow := manifest.VerifyEnd.Sub(manifest.VerifyStart)
	if inWindow <= 0 {
		return fmt.Errorf("archetype %s's manifest carries an empty verify window", w.tenant.archetype)
	}
	inBudget := subqueryOverTime(manifest.GaugeMetric, inWindow, step)
	overRange := time.Duration(Tier1QuerySampleBudget+1) * overBudgetSubqueryStep
	overBudget := subqueryOverTime(manifest.GaugeMetric, overRange, overBudgetSubqueryStep)

	ctx, cancel := context.WithTimeout(context.Background(), tenantProbeBudget)
	defer cancel()

	status, body, err := tenantQuery(ctx, w.live.CerberusURL, inBudget, manifest.VerifyEnd)
	if err != nil {
		return fmt.Errorf("migration harness: read within the sample budget: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("migration harness: the in-budget read %q returned %d: %s", inBudget, status, string(body))
	}
	services, err := vectorServices(body)
	if err != nil {
		return fmt.Errorf("migration harness: decode the in-budget read of %q: %w", inBudget, err)
	}
	w.tenant.underBudgetServices = services
	w.tenant.underBudgetSeries = len(services)

	status, body, err = tenantQuery(ctx, w.live.CerberusURL, overBudget, manifest.VerifyEnd)
	if err != nil {
		return fmt.Errorf("migration harness: read beyond the sample budget: %w", err)
	}
	var rejection promErrorResponse
	if err := json.Unmarshal(body, &rejection); err != nil {
		return fmt.Errorf("migration harness: decode the over-budget answer to %q (%d): %w", overBudget, status, err)
	}
	w.tenant.overBudgetStatus = status
	w.tenant.overBudgetType = rejection.ErrorType
	w.tenant.overBudgetMessage = rejection.Error
	w.tenant.budgetRead = true
	return nil
}

// subqueryOverTime renders `max_over_time(<metric>[<range>:<step>])` with both
// durations in whole seconds. The arithmetic lives here rather than in the
// feature text: the step names the relation ("within" / "beyond" the budget),
// the grid that realises it is derived.
func subqueryOverTime(metric string, window, step time.Duration) string {
	secs := func(d time.Duration) string { return strconv.FormatInt(int64(d/time.Second), 10) + "s" }
	return "max_over_time(" + metric + "[" + secs(window) + ":" + secs(step) + "])"
}

// thenForeignTenantPlantReadable is the NEGATIVE CONTROL: before claiming
// cerberus cannot see the foreign tenant's row, prove the row is genuinely
// there and genuinely readable — with the value and the attribute it was
// planted with — by reading the foreign database directly. Without this, a
// plant that silently failed (wrong table, rejected batch, dropped database)
// would satisfy the absence assertion perfectly.
func (w *World) thenForeignTenantPlantReadable() error {
	if !w.tenant.established {
		return fmt.Errorf("no foreign tenant has been planted; the scenario must establish one first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), tenantProbeBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, value, label, err := readForeignTenantPlant(ctx, conn, w.tenant.gaugeTable)
	if err != nil {
		return err
	}
	if rows != w.tenant.planted {
		return fmt.Errorf("the foreign tenant's %s.%s holds %d row(s) named %q, want the %d the harness planted",
			foreignTenantDatabase, w.tenant.gaugeTable, rows, foreignTenantMetric, w.tenant.planted)
	}
	if value != foreignTenantValue {
		return fmt.Errorf("the foreign tenant's planted %q reads back as %v, want %v",
			foreignTenantMetric, value, foreignTenantValue)
	}
	if label != foreignTenantLabelValue {
		return fmt.Errorf("the foreign tenant's planted %q carries %s=%q, want %q",
			foreignTenantMetric, foreignTenantLabel, label, foreignTenantLabelValue)
	}
	return nil
}

// readForeignTenantPlant reads the planted row back out of the foreign
// tenant's own database: how many rows carry the planted metric name, the
// value they hold, and the attribute they carry.
func readForeignTenantPlant(ctx context.Context, conn driver.Conn, gauge string) (rows int, value float64, label string, err error) {
	q := fmt.Sprintf("SELECT count(), any(Value), any(Attributes['%s']) FROM %s.%s WHERE MetricName = ?",
		foreignTenantLabel, foreignTenantDatabase, gauge)
	var n uint64
	if err := conn.QueryRow(ctx, q, foreignTenantMetric).Scan(&n, &value, &label); err != nil {
		return 0, 0, "", fmt.Errorf("read the foreign tenant's plant back from %s.%s: %w",
			foreignTenantDatabase, gauge, err)
	}
	return int(n), value, label, nil
}

// thenForeignTenantAbsent asserts the foreign tenant's planted metric name
// never reaches cerberus's own answer — the "exactly zero foreign-tenant
// series" claim, checked against what cerberus itself returned.
//
// Two controls make it mean something, and both are REQUIRED here rather than
// merely sitting next to it in the feature file. The plant-readable step
// proved the name is real and readable one database over; the declared-names
// step proved cerberus's catalog was POPULATED in this same run. A loop over
// zero names satisfies "the foreign name is not among them" perfectly, so
// without the second control an outage that emptied the catalog would read as
// airtight isolation.
func (w *World) thenForeignTenantAbsent() error {
	if err := w.requireTenantQueried(); err != nil {
		return err
	}
	if !w.tenant.metricNamesProven {
		return fmt.Errorf("cerberus's metric-name catalog has not been proven populated in this run; the " +
			"scenario must assert the archetype's declared metric names are present before this absence claim")
	}
	for _, name := range w.tenant.metricNames {
		if name == foreignTenantMetric {
			return fmt.Errorf("cerberus's metric-name catalog includes %q, which exists only in the foreign tenant's database %s",
				foreignTenantMetric, foreignTenantDatabase)
		}
	}
	return nil
}

// thenDeclaredMetricNamesPresent is the POSITIVE CONTROL, and it is checked
// against the archetype's own committed declaration rather than against a
// non-empty length: every metric name seed/fixture.json declares must be in
// cerberus's catalog. An empty or truncated answer — which would satisfy the
// absence assertion vacuously — fails here by name.
//
// The classic histogram expands to its three Prom-wire companions:
// cerberus's `__name__` catalog advertises a classic-histogram row as
// `<base>_bucket` / `<base>_count` / `<base>_sum` and never as the bare family
// name, because a bare selector on the base name routes to the gauge/sum
// tables and returns nothing (internal/api/prom's fetchMetricNameValues, which
// matches reference Prometheus). Asserting the companions is asserting what an
// operator's dashboards can actually select.
func (w *World) thenDeclaredMetricNamesPresent() error {
	if err := w.requireTenantQueried(); err != nil {
		return err
	}
	decl, err := w.tenantDeclaration()
	if err != nil {
		return err
	}
	want := declaredMetricNames(decl)
	// A declaration that leaves a metric name blank yields entries like "_sum",
	// which cerberus's catalog would never carry — but a blank GaugeMetric
	// would yield "", and "" is in no catalog either, so the failure would read
	// as a cerberus bug. Naming it here keeps a broken fixture a fixture
	// failure, and keeps this control's oracle non-empty by construction.
	for i, n := range want {
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("archetype %s's fixture declaration leaves metric name %d blank, so this "+
				"control would measure cerberus's catalog against nothing", w.tenant.archetype, i)
		}
	}
	have := make(map[string]struct{}, len(w.tenant.metricNames))
	for _, n := range w.tenant.metricNames {
		have[n] = struct{}{}
	}
	var missing []string
	for _, n := range want {
		if _, ok := have[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("archetype %s declares metric names cerberus's catalog does not carry: %v (catalog holds %v)",
			w.tenant.archetype, missing, w.tenant.metricNames)
	}
	w.tenant.metricNamesProven = true
	return nil
}

// declaredMetricNames is the archetype declaration's own metric surface, in
// the shape cerberus advertises it: the gauge and counter names as declared,
// and the classic histogram as its three Prom-wire companions.
func declaredMetricNames(d seed.Declaration) []string {
	return []string{
		d.GaugeMetric,
		d.CounterMetric,
		d.HistogramMetric + "_bucket",
		d.HistogramMetric + "_count",
		d.HistogramMetric + "_sum",
	}
}

// thenForeignTenantLabelAbsent asserts the attribute key planted only in the
// foreign tenant's database never appears as a label name on cerberus's
// metadata surface. The metric-name check alone would miss a leak that
// surfaced the foreign tenant's DIMENSIONS while hiding its series names.
//
// It requires its own positive control for the reason thenForeignTenantAbsent
// does: an empty label surface satisfies every absence claim ever made about
// it, so the surface has to have been proven populated in the same run.
func (w *World) thenForeignTenantLabelAbsent() error {
	if err := w.requireTenantQueried(); err != nil {
		return err
	}
	if !w.tenant.labelNamesProven {
		return fmt.Errorf("cerberus's label surface has not been proven populated in this run; the scenario " +
			"must assert the fixture's mapped labels are present before this absence claim")
	}
	for _, name := range w.tenant.labelNames {
		if name == foreignTenantLabel {
			return fmt.Errorf("cerberus's label surface includes %q, which exists only in the foreign tenant's database %s",
				foreignTenantLabel, foreignTenantDatabase)
		}
	}
	return nil
}

// thenMappedLabelsPresent is the positive control for the label half: the
// labels the fixture's own resource attribute and datapoint attribute map onto
// (mappedLabels, the same set MIG-11 pins its cross-backend agreement on) must
// all be present. Without it, an empty label surface would satisfy the absence
// assertion above by answering nothing at all.
func (w *World) thenMappedLabelsPresent() error {
	if err := w.requireTenantQueried(); err != nil {
		return err
	}
	have := make(map[string]struct{}, len(w.tenant.labelNames))
	for _, n := range w.tenant.labelNames {
		have[n] = struct{}{}
	}
	var missing []string
	for _, want := range mappedLabels {
		if _, ok := have[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("cerberus's label surface is missing the fixture's mapped labels %v (surface holds %v)",
			missing, w.tenant.labelNames)
	}
	w.tenant.labelNamesProven = true
	return nil
}

// thenUnderBudgetReadComplete asserts the in-budget read came back with
// exactly the gauge-series cardinality the seeder's manifest declares it
// wrote, and that the services those series name are EXACTLY the ones the
// archetype declares. This is the control that keeps the budget rejection
// honest — a cerberus that refused, or silently emptied, every subquery would
// fail here — and it is a positive-cardinality assertion against the seeder's
// own manifest, not a comparison of two query results.
//
// Set EQUALITY, not containment: the seeder builds one gauge series per
// (service × status code), so every declared service has to come back. A
// one-way "everything returned is declared" check is satisfied by a proper
// subset — including a single surviving service — which is exactly the read
// regression this step exists to catch.
func (w *World) thenUnderBudgetReadComplete() error {
	if err := w.requireTenantBudgetRead(); err != nil {
		return err
	}
	manifest, ok := w.manifest[w.tenant.archetype]
	if !ok {
		return fmt.Errorf("archetype %s's manifest was never loaded", w.tenant.archetype)
	}
	if manifest.Series.Gauge == 0 {
		return fmt.Errorf("archetype %s's manifest declares no gauge series; the read would be vacuous", w.tenant.archetype)
	}
	if w.tenant.underBudgetSeries != manifest.Series.Gauge {
		return fmt.Errorf("the in-budget read returned %d series, want the %d gauge series the manifest declares",
			w.tenant.underBudgetSeries, manifest.Series.Gauge)
	}
	decl, err := w.tenantDeclaration()
	if err != nil {
		return err
	}
	if len(decl.Services) == 0 {
		return fmt.Errorf("archetype %s declares no service; the identity half of this read would be vacuous",
			w.tenant.archetype)
	}
	declared := make(map[string]struct{}, len(decl.Services))
	for _, s := range decl.Services {
		declared[s] = struct{}{}
	}
	returned := make(map[string]struct{}, len(w.tenant.underBudgetServices))
	for _, got := range w.tenant.underBudgetServices {
		returned[got] = struct{}{}
	}
	var foreign []string
	for got := range returned {
		if _, ok := declared[got]; !ok {
			foreign = append(foreign, got)
		}
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return fmt.Errorf("the in-budget read returned series for %v, which archetype %s does not declare (declares %v)",
			foreign, w.tenant.archetype, decl.Services)
	}
	var unread []string
	for _, want := range decl.Services {
		if _, ok := returned[want]; !ok {
			unread = append(unread, want)
		}
	}
	if len(unread) > 0 {
		sort.Strings(unread)
		return fmt.Errorf("the in-budget read returned no series for %v, which archetype %s declares (it named %v)",
			unread, w.tenant.archetype, sortedKeys(returned))
	}
	return nil
}

// thenOverBudgetReadRefused asserts the out-of-budget read was refused with
// the SPECIFIC rejection MIG-15's PASS cell asks for — Prometheus's 422,
// errorType "execution" and verbatim sample-budget message — rather than with
// any error, an empty result, or a timeout.
func (w *World) thenOverBudgetReadRefused() error {
	if err := w.requireTenantBudgetRead(); err != nil {
		return err
	}
	if w.tenant.overBudgetStatus != http.StatusUnprocessableEntity {
		return fmt.Errorf("the over-budget read returned HTTP %d, want %d (%s)",
			w.tenant.overBudgetStatus, http.StatusUnprocessableEntity, sampleBudgetErrorMessage)
	}
	if w.tenant.overBudgetType != sampleBudgetErrorType {
		return fmt.Errorf("the over-budget read was refused with errorType %q, want %q",
			w.tenant.overBudgetType, sampleBudgetErrorType)
	}
	if w.tenant.overBudgetMessage != sampleBudgetErrorMessage {
		return fmt.Errorf("the over-budget read was refused with %q, want the sample-budget message %q",
			w.tenant.overBudgetMessage, sampleBudgetErrorMessage)
	}
	return nil
}

// tenantDeclaration loads the archetype's committed fixture declaration — the
// oracle every positive control in this scenario is measured against.
func (w *World) tenantDeclaration() (seed.Declaration, error) {
	path := harnessPath(w.root, archetypeDir, w.tenant.archetype, "seed", "fixture.json")
	return seed.LoadDeclaration(path)
}

// requireTenantQueried fails when the metadata When step never ran.
func (w *World) requireTenantQueried() error {
	if !w.tenant.established {
		return fmt.Errorf("no foreign tenant has been planted; the scenario must establish one first")
	}
	if !w.tenant.queried {
		return fmt.Errorf("cerberus's metadata surface has not been queried; the scenario must run that first")
	}
	return nil
}

// requireTenantBudgetRead fails when the budget When step never ran.
func (w *World) requireTenantBudgetRead() error {
	if !w.tenant.established {
		return fmt.Errorf("no foreign tenant has been planted; the scenario must establish one first")
	}
	if !w.tenant.budgetRead {
		return fmt.Errorf("cerberus has not been read inside and outside its sample budget; the scenario must run that first")
	}
	return nil
}

// promErrorResponse is the Prometheus HTTP API's error envelope. The
// errorType and message are the operator-facing contract a client parses, so
// the budget assertion reads both rather than settling for a status code.
type promErrorResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// promVectorResponse keeps the per-series label set the histogram probe's own
// scalar decoder discards — the in-budget read is asserted on its cardinality
// AND on whose services it names.
type promVectorResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	} `json:"data"`
}

// vectorServices decodes an instant-vector answer into one service_name per
// returned series, failing when the envelope is not a successful vector — a
// scalar or a matrix here means the query did not mean what the scenario
// thinks it means.
func vectorServices(body []byte) ([]string, error) {
	var resp promVectorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode instant-query response: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("instant-query status %q, want success", resp.Status)
	}
	if resp.Data.ResultType != "vector" {
		return nil, fmt.Errorf("instant-query resultType %q, want vector", resp.Data.ResultType)
	}
	out := make([]string, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		out = append(out, r.Metric["service_name"])
	}
	return out, nil
}

// tenantQuery issues one instant PromQL query and returns the status and body
// WITHOUT requiring a 200. The budget assertion's whole subject is a non-200
// answer, which the harness's 200-only GET helper would collapse into a
// transport error, losing the errorType and message the story asks to be
// specific about.
func tenantQuery(ctx context.Context, baseURL, query string, at time.Time) (int, []byte, error) {
	// RFC3339 rather than a Unix stamp: the `time` parameter takes Unix
	// SECONDS (optionally fractional) or RFC3339, and a millisecond count with
	// an `ms` suffix is neither — cerberus rejected it with
	// `bad_data: time parameter must be Unix seconds/milliseconds or RFC3339`,
	// correctly. RFC3339Nano keeps the instant exact without raising the
	// question of how many fractional digits the seconds form should carry.
	target := strings.TrimRight(baseURL, "/") + "/api/v1/query?" + url.Values{
		"query": []string{query},
		"time":  []string{at.UTC().Format(time.RFC3339Nano)},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build request for %s: %w", target, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, labelValuesResponseLimit))
	if err != nil {
		return 0, nil, fmt.Errorf("read %s body: %w", target, err)
	}
	return resp.StatusCode, body, nil
}

// fetchLabelNames GETs /api/v1/labels and returns the label names, sorted so a
// failure message reads the same on every run.
func fetchLabelNames(ctx context.Context, baseURL string) ([]string, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/v1/labels"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, labelValuesResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", target, resp.StatusCode, string(body))
	}
	var decoded labelValuesResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", target, err)
	}
	if decoded.Status != "success" {
		return nil, fmt.Errorf("GET %s reported status %q, want success", target, decoded.Status)
	}
	sort.Strings(decoded.Data)
	return decoded.Data, nil
}
