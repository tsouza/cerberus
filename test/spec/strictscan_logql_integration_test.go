//go:build integration

package spec_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/testsql"
	"github.com/tsouza/cerberus/test/spec"
	"github.com/tsouza/cerberus/test/spec/logqlwrap"
)

// TestStrictScanDifferentialLogStreams closes the LogQL half of the
// pre-wrap fixture gap: Layer 2a records a log-stream query as SELECT *, while
// production applies ProjectSamples and sends (Line, Attributes, TimeUnix[,
// Metadata]) to ClickHouse. Its TestStrictScanDifferential prefix means the
// canonical `just strict-scan-test` recipe runs this sibling under the same
// real-ClickHouse required check.
func TestStrictScanDifferentialLogStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := newStrictScanClient(ctx, t)
	dir := filepath.Join(repoRootFromTest(t), "test", "spec", "logql")
	var ran, skippedMetric, skippedNonRoundTrip int
	spec.Walk(t, dir, func(t *testing.T, c *spec.Case) {
		rt, err := spec.LoadRoundTrip(c)
		if err != nil {
			t.Fatalf("LoadRoundTrip: %v", err)
		}
		if !rt.IsRoundTrip() || strings.TrimSpace(rt.SQL) == "" {
			skippedNonRoundTrip++
			return
		}

		sqlStr, args, ok, err := logqlwrap.ReconstructLogStreamWrap(c)
		if err != nil {
			t.Fatalf("reconstruct production log-stream wrap: %v", err)
		}
		if !ok {
			skippedMetric++
			return
		}
		rt.SQL, rt.Args = sqlStr, args
		applyStrictScanLogSeed(ctx, t, client, rt.Seed)
		query, queryArgs := spec.SubstituteNow64(rt.SQL, rt.Args)

		logRow, err := isLogRowShape(ctx, client, query, queryArgs)
		if err != nil {
			t.Fatalf("query open failed against real ClickHouse:\n--- sql ---\n%s\n--- err ---\n%v", query, err)
		}
		if !logRow {
			t.Fatalf("reconstructed production LogQL SQL did not expose (Line, Attributes, TimeUnix[, Metadata]):\n%s", query)
		}

		cur, err := client.QueryCursor(ctx, query, queryArgs...)
		if err != nil {
			t.Fatalf("QueryCursor (open) failed:\n--- sql ---\n%s\n--- err ---\n%v", query, err)
		}
		defer func() { _ = cur.Close() }()
		for cur.Next() {
			_ = cur.Sample()
		}
		if err := cur.Err(); err != nil {
			if isStrictScanError(err) {
				t.Fatalf("STRICT-SCAN TYPE ERROR — LogQL wire projection cannot be decoded by the production cursor:\n--- sql ---\n%s\n--- err ---\n%v", query, err)
			}
			t.Fatalf("cursor drain failed:\n--- sql ---\n%s\n--- err ---\n%v", query, err)
		}
		ran++
	})

	if ran == 0 {
		t.Fatalf("strict-scan differential ran zero reconstructed LogQL log-stream fixtures (metric=%d, non-round-trip=%d) — reconstruction or corpus selection is broken", skippedMetric, skippedNonRoundTrip)
	}
	t.Logf("strict-scan differential: strict-scanned %d reconstructed LogQL log-stream fixtures against real ClickHouse (%d metric + %d non-round-trip skipped)", ran, skippedMetric, skippedNonRoundTrip)
}

func isLogRowShape(ctx context.Context, client *chclient.Client, query string, args []any) (bool, error) {
	rows, err := client.Conn().Query(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()

	cols := rows.Columns()
	if len(cols) == 3 {
		return cols[0] == "Line" && cols[1] == "Attributes" && cols[2] == "TimeUnix", nil
	}
	return len(cols) == 4 && cols[0] == "Line" && cols[1] == "Attributes" && cols[2] == "TimeUnix" && cols[3] == "Metadata", nil
}

func applyStrictScanLogSeed(ctx context.Context, t *testing.T, client *chclient.Client, seed string) {
	t.Helper()
	stmts := testsql.BackfillLogsColumns(testsql.SplitStatements(seed))
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmt = testsql.PromoteCreateTable(stmt)
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}
