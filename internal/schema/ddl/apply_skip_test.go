package ddl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// recordingConn is a driver.Conn that records the SQL passed to Exec and
// no-ops it. Only Exec is exercised by ApplyWithConfig; the embedded nil
// driver.Conn supplies the rest of the interface (never called here).
type recordingConn struct {
	driver.Conn
	execs              []string
	bodyTextIndexCount uint64
	queryRowErr        error
}

func (r *recordingConn) Exec(_ context.Context, query string, _ ...any) error {
	r.execs = append(r.execs, query)
	return nil
}

func (r *recordingConn) QueryRow(context.Context, string, ...any) driver.Row {
	return recordingRow{value: r.bodyTextIndexCount, err: r.queryRowErr}
}

type recordingRow struct {
	value uint64
	err   error
}

func (r recordingRow) Err() error { return r.err }

func (r recordingRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*uint64) = r.value
	return nil
}

func (r recordingRow) ScanStruct(any) error { return r.err }

func TestApplyWithConfig_TextIndexReconciliation(t *testing.T) {
	for _, tt := range []struct {
		name              string
		existingTextIndex uint64
		wantAdd           bool
	}{
		{name: "fresh_or_already_upgraded", existingTextIndex: 1, wantAdd: false},
		{name: "legacy_tokenbf_only", existingTextIndex: 0, wantAdd: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rc := &recordingConn{bodyTextIndexCount: tt.existingTextIndex}
			cfg := Config{Database: "otel", TextIndexEnabled: true}
			if err := ApplyWithConfig(context.Background(), rc, cfg, []Signal{Logs}); err != nil {
				t.Fatalf("ApplyWithConfig: %v", err)
			}
			var gotAdd bool
			for _, stmt := range rc.execs {
				gotAdd = gotAdd || strings.Contains(stmt, "ADD INDEX IF NOT EXISTS "+bodyTextIndexName)
			}
			if gotAdd != tt.wantAdd {
				t.Errorf("idx_body_text ADD executed = %v, want %v; statements: %v", gotAdd, tt.wantAdd, rc.execs)
			}
		})
	}
}

func TestApplyWithConfig_TextIndexProbeFailureStopsReconciliation(t *testing.T) {
	rc := &recordingConn{queryRowErr: errors.New("probe failed")}
	err := ApplyWithConfig(context.Background(), rc, Config{TextIndexEnabled: true}, []Signal{Logs})
	if err == nil || !strings.Contains(err.Error(), "inspect logs body text index: probe failed") {
		t.Fatalf("ApplyWithConfig error = %v, want probe failure", err)
	}
}

// TestApplyWithConfig_CreatesDatabaseFirst pins the default cold-cluster
// bootstrap: the first statement executed is the CREATE DATABASE, before any
// CREATE TABLE.
func TestApplyWithConfig_CreatesDatabaseFirst(t *testing.T) {
	rc := &recordingConn{}
	if err := ApplyWithConfig(context.Background(), rc, Config{Database: "otel"}, All); err != nil {
		t.Fatalf("ApplyWithConfig: %v", err)
	}
	if len(rc.execs) == 0 {
		t.Fatal("no statements executed")
	}
	if !strings.HasPrefix(rc.execs[0], "CREATE DATABASE IF NOT EXISTS otel") {
		t.Errorf("first statement must be CREATE DATABASE, got: %s", rc.execs[0])
	}
	for _, s := range rc.execs[1:] {
		if strings.HasPrefix(s, "CREATE DATABASE") {
			t.Errorf("CREATE DATABASE issued more than once: %s", s)
		}
	}
}

// TestApplyWithConfig_SkipDatabaseCreate pins the externally-managed-database
// path: with SkipDatabaseCreate the CREATE DATABASE is omitted entirely, but
// the (fully-qualified) table creates still run.
func TestApplyWithConfig_SkipDatabaseCreate(t *testing.T) {
	rc := &recordingConn{}
	cfg := Config{Database: "otel", SkipDatabaseCreate: true}
	if err := ApplyWithConfig(context.Background(), rc, cfg, All); err != nil {
		t.Fatalf("ApplyWithConfig: %v", err)
	}
	if len(rc.execs) == 0 {
		t.Fatal("no statements executed — tables should still be created")
	}
	for _, s := range rc.execs {
		if strings.HasPrefix(s, "CREATE DATABASE") {
			t.Errorf("SkipDatabaseCreate must omit CREATE DATABASE, got: %s", s)
		}
	}
	// Sanity: the table creates DID run and are qualified to the database.
	var sawTable bool
	for _, s := range rc.execs {
		if strings.Contains(s, "otel") && strings.Contains(s, "CREATE TABLE") {
			sawTable = true
		}
	}
	if !sawTable {
		t.Errorf("expected qualified CREATE TABLE statements, got: %v", rc.execs)
	}
}

// TestApplyWithConfig_SkipDatabaseCreate_NoReplicatedValidation confirms the
// Replicated-zoo-path validation is not enforced when the database is
// externally managed (we never emit CREATE DATABASE, so the engine is moot).
func TestApplyWithConfig_SkipDatabaseCreate_NoReplicatedValidation(t *testing.T) {
	rc := &recordingConn{}
	cfg := Config{
		Database:           "otel",
		SkipDatabaseCreate: true,
		DatabaseEngine:     DatabaseEngine{Replicated: true}, // no zoo path
	}
	if err := ApplyWithConfig(context.Background(), rc, cfg, All); err != nil {
		t.Fatalf("SkipDatabaseCreate should bypass the Replicated zoo-path check: %v", err)
	}
}

// cloudRefusingStatisticsConn is a driver.Conn that mimics ClickHouse Cloud:
// every ADD STATISTICS statement fails with a "not supported ... Cloud"
// refusal, every other statement succeeds. It exercises applySignal's
// tolerance path end to end (issue #2766) — the goal is that a Cloud
// deployment with column_statistics enabled still boots successfully, with
// every non-statistics statement (CREATE, projections, skip index) still
// applied for real.
type cloudRefusingStatisticsConn struct {
	driver.Conn
	execs   []string
	skipped []string
}

func (c *cloudRefusingStatisticsConn) Exec(_ context.Context, query string, _ ...any) error {
	if strings.Contains(query, "ADD STATISTICS") {
		c.skipped = append(c.skipped, query)
		return errors.New("Code: 48. DB::Exception: Statistics is not supported in ClickHouse Cloud")
	}
	c.execs = append(c.execs, query)
	return nil
}

// TestApplyWithConfig_ColumnStatisticsUnsupported_Tolerated pins the
// end-to-end contract behind isColumnStatisticsUnsupported: on a server that
// refuses every ADD STATISTICS ALTER (ClickHouse Cloud), ApplyWithConfig
// still succeeds as a whole, still executes every other statement for real
// (CREATE tables, ADD PROJECTION, ADD INDEX), and the refused statements are
// the ones — and only the ones — that were skipped.
func TestApplyWithConfig_ColumnStatisticsUnsupported_Tolerated(t *testing.T) {
	rc := &cloudRefusingStatisticsConn{}
	cfg := Config{Database: "otel", ColumnStatisticsEnabled: true}
	if err := ApplyWithConfig(context.Background(), rc, cfg, All); err != nil {
		t.Fatalf("ApplyWithConfig should tolerate a Cloud ADD STATISTICS refusal, got: %v", err)
	}
	if len(rc.skipped) == 0 {
		t.Fatal("expected at least one ADD STATISTICS statement to be attempted and skipped")
	}
	for _, s := range rc.skipped {
		if !strings.Contains(s, "ADD STATISTICS") {
			t.Errorf("skipped a non-statistics statement: %s", s)
		}
	}
	var sawCreate, sawProjection, sawIndex bool
	for _, s := range rc.execs {
		switch {
		case strings.Contains(s, "CREATE TABLE"):
			sawCreate = true
		case strings.Contains(s, "ADD PROJECTION"):
			sawProjection = true
		case strings.Contains(s, "ADD INDEX"):
			sawIndex = true
		}
	}
	if !sawCreate || !sawProjection || !sawIndex {
		t.Errorf("expected CREATE/PROJECTION/INDEX statements to still apply for real: creates=%v projections=%v indexes=%v",
			sawCreate, sawProjection, sawIndex)
	}
}
