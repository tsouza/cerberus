//go:build integration

package ddl_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// The tiered deployment this file provisions: a storage policy with a HOT and a
// COLD volume, the standard ClickHouse hot/cold setup and the entire reason
// cerberus exposes a storage-policy knob. Both disks are plain local paths —
// what the volumes are backed by is irrelevant to the DDL under test; what
// matters is that the policy has more than one volume for parts to move
// BETWEEN.
const (
	tieredPolicyName = "cerberus_tiered"
	tieredHotVolume  = "hot"
	tieredColdVolume = "cold"
	tieredColdDisk   = "cold_disk"
)

const tieredServerConfig = `<clickhouse>
    <storage_configuration>
        <disks>
            <hot_disk>
                <path>/var/lib/clickhouse/hot/</path>
            </hot_disk>
            <cold_disk>
                <path>/var/lib/clickhouse/cold/</path>
            </cold_disk>
        </disks>
        <policies>
            <cerberus_tiered>
                <volumes>
                    <hot>
                        <disk>hot_disk</disk>
                    </hot>
                    <cold>
                        <disk>cold_disk</disk>
                    </cold>
                </volumes>
            </cerberus_tiered>
        </policies>
    </storage_configuration>
</clickhouse>
`

// startClickHouseTiered spins up a real ClickHouse carrying the two-volume
// storage policy above, with the `otel` database pre-created (the tables are
// what this test provisions). It mirrors startClickHouseReplicated's shape:
// an embedded XML config mounted through the testcontainers module.
func startClickHouseTiered(t *testing.T) driver.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfgPath := filepath.Join(t.TempDir(), "tiered.xml")
	if err := os.WriteFile(cfgPath, []byte(tieredServerConfig), 0o644); err != nil {
		t.Fatalf("write tiered storage config: %v", err)
	}

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
		tcclickhouse.WithConfigFile(cfgPath),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%s", host, port.Port())},
		Auth: clickhouse.Auth{
			Database: "otel",
			Username: "cerberus",
			Password: "cerberus",
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pingCancel()
	if err := conn.Ping(pingCtx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return conn
}

// policyVolumes reads the volumes of a storage policy in the order ClickHouse
// fills them. This is the EXACT probe internal/preflight's storage-tiering gate
// makes (system.storage_policies, ordered by volume_priority), so running it
// here pins that the table and columns that gate depends on exist and answer as
// the gate assumes — something a stubbed unit test cannot establish.
func policyVolumes(ctx context.Context, t *testing.T, conn driver.Conn, policy string) []string {
	t.Helper()
	rows, err := conn.Query(ctx,
		"SELECT volume_name FROM system.storage_policies WHERE policy_name = ? ORDER BY volume_priority", policy)
	if err != nil {
		t.Fatalf("query system.storage_policies: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan volume_name: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// createTableQuery returns the CREATE statement ClickHouse stored for a table —
// the server's own normalised reading of the DDL cerberus emitted.
func createTableQuery(ctx context.Context, t *testing.T, conn driver.Conn, database, table string) string {
	t.Helper()
	var q string
	err := conn.QueryRow(ctx,
		"SELECT create_table_query FROM system.tables WHERE database = ? AND name = ?", database, table).Scan(&q)
	if err != nil {
		t.Fatalf("query create_table_query for %s.%s: %v", database, table, err)
	}
	return q
}

// activePartDisks returns the disk each active part of a table sits on.
func activePartDisks(ctx context.Context, t *testing.T, conn driver.Conn, database, table string) []string {
	t.Helper()
	rows, err := conn.Query(ctx,
		"SELECT disk_name FROM system.parts WHERE database = ? AND table = ? AND active ORDER BY name",
		database, table)
	if err != nil {
		t.Fatalf("query system.parts: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan disk_name: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// TestApply_StorageTiering is the end-to-end proof for the behaviour no
// string-assertion unit test can observe: that the `TTL … TO VOLUME` clause
// cerberus emits is ACCEPTED by a real ClickHouse and that parts past the move
// age actually land on the cold volume.
//
// It closes the gap that let the inert-setting bug ship: cerberus attached a
// multi-volume storage_policy to every table and emitted no move rule, so data
// stayed on the hot volume until retention deleted it — everything "worked",
// and the only symptom was the storage bill. A unit test asserting the emitted
// string cannot tell an accepted clause from a rejected one, and chDB is not a
// ClickHouse server; only a real server settles it.
func TestApply_StorageTiering(t *testing.T) {
	conn := startClickHouseTiered(t)
	ctx := context.Background()

	const (
		database = "otel"
		day      = 24 * time.Hour
	)

	// Precondition: the server really does carry a MULTI-volume policy. Without
	// this the rest of the test would pass vacuously against a single-volume
	// policy, where nothing can tier by construction.
	volumes := policyVolumes(ctx, t, conn, tieredPolicyName)
	wantVolumes := []string{tieredHotVolume, tieredColdVolume}
	if len(volumes) != len(wantVolumes) {
		t.Fatalf("storage policy %q volumes = %v; want %v", tieredPolicyName, volumes, wantVolumes)
	}
	for i, v := range volumes {
		if v != wantVolumes[i] {
			t.Fatalf("storage policy %q volumes = %v; want %v (in fill order)", tieredPolicyName, volumes, wantVolumes)
		}
	}

	cfg := ddl.Config{
		Database: database,
		TTL:      ddl.TTL{Metrics: 90 * day, Logs: 30 * day, Traces: 30 * day},
		Tiering: ddl.Tiering{
			Volume:  tieredColdVolume,
			Metrics: 14 * day,
			Logs:    3 * day,
			Traces:  3 * day,
		},
		Settings: []schema.KV{{Key: "storage_policy", Value: tieredPolicyName}},
	}
	// A malformed TTL clause — a bad action order, a missing comma, an unquoted
	// volume — fails HERE, which is the primary assertion of this test.
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, ddl.All); err != nil {
		t.Fatalf("Apply with storage tiering: %v", err)
	}

	// The server's own reading of what it stored: BOTH actions, in the one TTL
	// clause, move first. Note the explicit `DELETE` keyword cerberus emits is
	// absent here — ClickHouse normalises it away, because a bare TTL action IS
	// a delete. That normalisation is itself the proof the server parsed the
	// multi-action clause rather than swallowing it whole.
	const (
		logsAndSpans = "TTL toDateTime(Timestamp) + toIntervalDay(3) TO VOLUME 'cold', " +
			"toDateTime(Timestamp) + toIntervalDay(30)"
		traceIDTs = "TTL toDateTime(Start) + toIntervalDay(3) TO VOLUME 'cold', " +
			"toDateTime(Start) + toIntervalDay(30)"
		metrics = "TTL toDateTime(TimeUnix) + toIntervalWeek(2) TO VOLUME 'cold', " +
			"toDateTime(TimeUnix) + toIntervalDay(90)"
	)
	for _, tc := range []struct {
		table string
		want  string
	}{
		{"otel_logs", logsAndSpans},
		{"otel_traces", logsAndSpans},
		{"otel_traces_trace_id_ts", traceIDTs},
		{"otel_metrics_gauge", metrics},
		{"otel_metrics_sum", metrics},
		{"otel_metrics_histogram", metrics},
		{"otel_metrics_exponential_histogram", metrics},
		{"otel_metrics_summary", metrics},
	} {
		stored := createTableQuery(ctx, t, conn, database, tc.table)
		if !strings.Contains(stored, tc.want) {
			t.Errorf("%s: stored CREATE does not carry the tiered TTL clause %q:\n%s", tc.table, tc.want, stored)
		}
	}

	// And the move actually happens. A row older than the logs move age (3 days)
	// but well inside its retention (30 days) must end up on the cold disk: the
	// background mover picks up a part whose move-TTL has already expired as soon
	// as it is inserted. This is the assertion that distinguishes a REAL tiering
	// rule from a merely well-formed one.
	old := time.Now().Add(-10 * day)
	if err := conn.Exec(
		ctx,
		"INSERT INTO otel.otel_logs (Timestamp, ServiceName, Body) VALUES (?, ?, ?)",
		old, "tiering-probe", "aged log line",
	); err != nil {
		t.Fatalf("insert aged log row: %v", err)
	}

	const moveDeadline = 90 * time.Second
	const movePoll = time.Second
	deadline := time.Now().Add(moveDeadline)
	var disks []string
	for time.Now().Before(deadline) {
		disks = activePartDisks(ctx, t, conn, database, "otel_logs")
		if len(disks) == 1 && disks[0] == tieredColdDisk {
			return
		}
		time.Sleep(movePoll)
	}
	t.Fatalf("aged part still on %v after %s; want it moved to %q by the TTL ... TO VOLUME rule",
		disks, moveDeadline, tieredColdDisk)
}
