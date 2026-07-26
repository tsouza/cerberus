package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cucumber/godog"

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
// foreign tenant's database. Its absence from cerberus's own label-values
// answer is the isolation assertion; its presence would mean cerberus somehow
// reached outside its configured database.
const foreignTenantMetric = "cerberus_migration_foreign_tenant_probe"

// tenantProbe is MIG-15's state: whether the foreign-tenant plant succeeded,
// and what cerberus's own metric-name enumeration returned.
type tenantProbe struct {
	established bool
	metricNames []string
}

// registerTenantIsolationSteps binds MIG-15: a foreign tenant's data planted
// directly in ClickHouse outside cerberus's configured database, and the
// assertion that cerberus's own read surface never surfaces it while its own
// tenant's real data remains fully queryable.
func (w *World) registerTenantIsolationSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^a foreign tenant's series planted directly in ClickHouse, outside cerberus's configured database$`,
		w.givenForeignTenantPlanted)
	ctx.Step(`^the operator queries cerberus for every metric name it can see$`, w.whenQueryTenantMetricNames)
	ctx.Step(`^the foreign tenant's metric name never appears in cerberus's answer$`, w.thenForeignTenantAbsent)
	ctx.Step(`^cerberus's own configured-tenant metrics still appear in cerberus's answer$`, w.thenOwnTenantPresent)
}

// givenForeignTenantPlanted writes a distinctly-named marker directly into a
// SEPARATE ClickHouse database that cerberus's live instance is never
// configured to read — the empirical half of "per-tenant CH database"
// isolation, proven by planting real data rather than assumed from
// deployment topology alone. The marker table's shape carries no
// significance beyond holding one distinguishable value: cerberus cannot see
// it regardless of shape, because it never queries this database at all.
func (w *World) givenForeignTenantPlanted() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; scenario must establish it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), tenantProbeBudget)
	defer cancel()

	conn, err := seed.DialCH(ctx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: "default",
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
	if err != nil {
		return fmt.Errorf("migration harness: dial the live clickhouse for the tenant plant: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+foreignTenantDatabase); err != nil {
		return fmt.Errorf("migration harness: create the foreign tenant database: %w", err)
	}
	createTable := "CREATE TABLE IF NOT EXISTS " + foreignTenantDatabase + ".marker " +
		"(MetricName String, Value Float64, TimeUnix DateTime) ENGINE = MergeTree ORDER BY MetricName"
	if err := conn.Exec(ctx, createTable); err != nil {
		return fmt.Errorf("migration harness: create the foreign tenant marker table: %w", err)
	}
	insert := "INSERT INTO " + foreignTenantDatabase + ".marker (MetricName, Value, TimeUnix) VALUES (?, ?, ?)"
	if err := conn.Exec(ctx, insert, foreignTenantMetric, float64(1), time.Now().UTC()); err != nil {
		return fmt.Errorf("migration harness: insert the foreign tenant marker: %w", err)
	}

	w.tenant = tenantProbe{established: true}
	return nil
}

// tenantLabelValuesResponse is the standard Prometheus label-values envelope.
type tenantLabelValuesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

// whenQueryTenantMetricNames asks cerberus's own Prometheus-compatible API
// for every metric name it can see, over the same window the live fixture
// occupies.
func (w *World) whenQueryTenantMetricNames() error {
	if !w.tenant.established {
		return fmt.Errorf("no foreign tenant has been planted; the scenario must establish one first")
	}
	body, err := correlationGET(w.live.CerberusURL + "/api/v1/label/__name__/values")
	if err != nil {
		return fmt.Errorf("migration harness: query cerberus's label-values API: %w", err)
	}
	var resp tenantLabelValuesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode label-values response: %w", err)
	}
	if resp.Status != "success" {
		return fmt.Errorf("label-values status %q, want success", resp.Status)
	}
	w.tenant.metricNames = resp.Data
	return nil
}

// thenForeignTenantAbsent asserts the foreign tenant's synthetic metric name
// never reaches cerberus's own answer — the isolation claim, checked against
// what cerberus itself returned rather than assumed from where the data was
// written.
func (w *World) thenForeignTenantAbsent() error {
	if err := w.requireTenantQueried(); err != nil {
		return err
	}
	for _, name := range w.tenant.metricNames {
		if name == foreignTenantMetric {
			return fmt.Errorf("cerberus's label-values answer includes %q, which was planted only in the foreign tenant's database %s",
				foreignTenantMetric, foreignTenantDatabase)
		}
	}
	return nil
}

// thenOwnTenantPresent asserts cerberus's own configured-tenant data remains
// fully queryable — without this, an empty answer would satisfy
// thenForeignTenantAbsent vacuously by returning nothing for anything.
func (w *World) thenOwnTenantPresent() error {
	if err := w.requireTenantQueried(); err != nil {
		return err
	}
	if len(w.tenant.metricNames) == 0 {
		return fmt.Errorf("cerberus's label-values answer is empty; the isolation check would be vacuous without its own tenant's data present")
	}
	return nil
}

// requireTenantQueried fails when the When step never ran.
func (w *World) requireTenantQueried() error {
	if !w.tenant.established {
		return fmt.Errorf("no foreign tenant has been planted; the scenario must establish one first")
	}
	if w.tenant.metricNames == nil {
		return fmt.Errorf("cerberus has not been queried for metric names; the scenario must run that first")
	}
	return nil
}
