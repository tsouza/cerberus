//go:build chdb

// Package chclient_test — chDB engine probe.
//
// This test answers a single, gating question: can the chdb-go
// database/sql driver round-trip a `Map(String, String)` column out of
// the engine into a Go value that cerberus can use?
//
// A large slice of cerberus's TXTAR fixtures emits Map subscript syntax
// against `Attributes` / `ResourceAttributes`. Any plan to adopt chDB
// as a semantic-assertion layer on top of today's text goldens depends
// on Map round-trip working — directly or via a shim.
//
// The probe answers two questions in order:
//
//  1. **Native scan**: does `rows.Scan(..., &m, ...)` with `m` of type
//     `map[string]string` work? (We expect not — chdb-go uses Parquet
//     as the wire format and the parquet-go library panics on
//     parquet's MAP logical type.)
//  2. **JSON shim**: does the same scan work if we wrap the column in
//     `toJSONString(Attributes)` server-side and decode in Go?
//
// The test asserts (2) — that's the path the downstream runner can
// stand up on. (1) is recorded as a `t.Log` observation, not a hard
// failure, because we already know native Map scan is unsupported in
// chdb-go v1.11.0 and the *purpose* of the probe is to confirm the
// shim works around it. If a future chdb-go ever lights up native
// scan, the runner can drop the shim — but the runner doesn't depend
// on that day arriving.
//
// The test is gated behind `//go:build chdb` so it never enters the
// required `check` lane (which runs `CGO_ENABLED=0`). It runs only in
// the dedicated `chdb` workflow after `just chdb-install` has placed
// `libchdb.so` at `/usr/local/lib`.
package chclient_test

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// chdbEOFSentinel is the spurious end-of-iteration error the chdb-go
// parquet driver returns instead of io.EOF when a row buffer runs out.
// See chdb/driver/parquet.go in chdb-go v1.11.0: `return fmt.Errorf("empty row")`.
// Surface this on rows.Err() and we have to ignore it; treat any other
// error as a real failure.
const chdbEOFSentinel = "empty row"

func tolerantRowsErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

// TestChDBProbe creates an OTel-metrics-gauge-shaped table inside an
// ephemeral chDB session, inserts two rows with Map(String, String)
// literals, and exercises both round-trip paths.
func TestChDBProbe(t *testing.T) {
	// Empty DSN -> chdb-go creates a temporary on-disk session that is
	// torn down with the connection. There is no `:memory:` flavor in
	// the driver today; the temp dir behaves equivalently.
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	for _, ddl := range []string{
		`CREATE DATABASE IF NOT EXISTS probe`,
		`CREATE TABLE probe.metrics (
			MetricName String,
			Attributes Map(String, String),
			TimeUnix   DateTime64(9),
			Value      Float64
		) ENGINE = MergeTree() ORDER BY MetricName`,
		`INSERT INTO probe.metrics VALUES
			('cpu', map('host', 'a', 'region', 'us'), now64(9), 0.5),
			('cpu', map('host', 'b', 'region', 'eu'), now64(9), 0.7)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("exec %q: %v", ddl, err)
		}
	}

	expected := []map[string]string{
		{"host": "a", "region": "us"},
		{"host": "b", "region": "eu"},
	}

	// --- Path A: native Map(String, String) -> map[string]string scan.
	//
	// We expect this NOT to work today. Log the exact failure mode so
	// downstream readers don't have to re-discover it. If chdb-go ever
	// adds proper Map handling this t.Log line will start reading
	// "supported" and Path B becomes optional.
	t.Run("native_map_scan", func(t *testing.T) {
		rows, err := db.Query(`
			SELECT MetricName, Attributes, Value
			FROM probe.metrics
			ORDER BY Value ASC
		`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()

		var scanErr error
		for rows.Next() {
			var (
				name   string
				labels map[string]string
				value  float64
			)
			if err := rows.Scan(&name, &labels, &value); err != nil {
				scanErr = err
				break
			}
		}
		switch {
		case scanErr != nil:
			t.Logf("native Map scan: NOT supported (chdb-go v1.11.0). Driver error: %v", scanErr)
		case tolerantRowsErr(rows.Err()) != nil:
			t.Logf("native Map scan: NOT supported (chdb-go v1.11.0). rows.Err: %v", rows.Err())
		default:
			t.Logf("native Map scan: supported (chdb-go behavior changed — Path B shim is optional)")
		}
	})

	// --- Path B: toJSONString(Attributes) shim -> json.Unmarshal.
	//
	// This is the path the downstream runner must rely on. The shim
	// emits a deterministic JSON object whose keys reflect the Map's
	// CH insertion order — fixture authors must therefore not depend
	// on key ordering and the runner must compare via map equality
	// (which we do here via reflect.DeepEqual).
	t.Run("json_shim_scan", func(t *testing.T) {
		rows, err := db.Query(`
			SELECT MetricName, toJSONString(Attributes) AS Attrs, TimeUnix, Value
			FROM probe.metrics
			ORDER BY Value ASC
		`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()

		var got []map[string]string
		for rows.Next() {
			var (
				name      string
				attrsJSON string
				ts        time.Time
				value     float64
			)
			if err := rows.Scan(&name, &attrsJSON, &ts, &value); err != nil {
				t.Fatalf("scan (json shim): %v", err)
			}
			labels := map[string]string{}
			if err := json.Unmarshal([]byte(attrsJSON), &labels); err != nil {
				t.Fatalf("unmarshal %q: %v", attrsJSON, err)
			}
			got = append(got, labels)
			if name != "cpu" {
				t.Errorf("MetricName: got %q, want %q", name, "cpu")
			}
			if ts.IsZero() {
				t.Errorf("TimeUnix zero — DateTime64(9) scan failed")
			}
			_ = value
		}
		// Ignore chdb-go's spurious "empty row" sentinel that fires at
		// end-of-iteration in place of io.EOF (see chdbEOFSentinel).
		if err := tolerantRowsErr(rows.Err()); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("json shim mismatch:\n got = %#v\nwant = %#v", got, expected)
		}
		t.Logf("toJSONString shim: supported. Map scan path is unblocked via this route.")
	})
}

