//go:build integration

package chclient_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// TestProbeTSGridCapability_HealthyServerIsAvailable is the regression test for
// the capability canary that silently disabled the native timeSeries*ToGrid
// aggregates on EVERY healthy server. The canary runs its probe over
// Client.QueryStrings (ProbeVersion's proven path), which scans each row into a
// Go string. A bare `SELECT 1` returns a UInt8 column, and clickhouse-go's
// strict scanner rejects `converting UInt8 to *string is unsupported`
// CLIENT-SIDE. That scan error is not a *clickhouse.Exception, so
// classifyTSGridCapability mapped a capable, unconstrained server to
// CapabilityUnreachable -- and the resolver then dropped the native aggregates
// to fan-out. The fix projects a String column (`SELECT '1'`) that QueryStrings
// decodes cleanly, so a healthy server reads as the Available verdict it is.
//
// This stands up a real, unconstrained ClickHouse on the 25.8 line (which has
// the experimental setting) and asserts the verdict is Available -- the assertion
// that fails on the pre-fix `SELECT 1` body. Gated by the `integration` build
// tag (requires Docker); the E2E workflow runs it, regular CI doesn't.
func TestProbeTSGridCapability_HealthyServerIsAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	// Bind to the always-present `default` database, exactly as the boot probe
	// does over bootstrapClickHouseConfig.
	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Username: "cerberus",
		Password: "cerberus",
		Database: "default",
	})
	if err != nil {
		t.Fatalf("new default-bound client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Sanity: the version probe (String column) must succeed over this path, so a
	// capability Unreachable verdict can only mean the capability probe's own
	// query path is broken -- not a connectivity problem.
	if _, err := client.ProbeVersion(ctx); err != nil {
		t.Fatalf("ProbeVersion over healthy server failed: %v", err)
	}

	got := client.ProbeTSGridCapability(ctx)
	if got != chopt.CapabilityAvailable {
		t.Fatalf("ProbeTSGridCapability on a healthy, unconstrained 25.8 server = %v; want %v "+
			"(a UInt8 probe body that QueryStrings cannot scan misclassifies this as Unreachable)",
			got, chopt.CapabilityAvailable)
	}
}
