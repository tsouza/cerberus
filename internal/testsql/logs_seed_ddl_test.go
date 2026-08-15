package testsql

import (
	"strings"
	"testing"
)

func TestBackfillLogsColumns_AddsWireProjectionColumns(t *testing.T) {
	ddl := "CREATE TABLE otel_logs (\n" +
		"    Timestamp DateTime64(9),\n" +
		"    Body String,\n" +
		"    ResourceAttributes Map(String, String)\n" +
		") ENGINE = Memory"
	insert := "INSERT INTO otel_logs VALUES (now64(9), 'line', map('job', 'api'))"
	got := BackfillLogsColumns([]string{ddl, insert})
	if len(got) != 2 {
		t.Fatalf("BackfillLogsColumns returned %d statements, want 2", len(got))
	}
	for _, column := range []string{"SeverityText LowCardinality(String)", "LogAttributes Map(String, String)"} {
		if !strings.Contains(got[0], column) {
			t.Errorf("backfilled DDL missing %q:\n%s", column, got[0])
		}
	}
	if !strings.Contains(got[1], "INSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES") {
		t.Errorf("positional INSERT did not retain the original column list:\n%s", got[1])
	}
}

func TestBackfillLogsColumns_NormalizesWireColumnsToDefault(t *testing.T) {
	ddl := "CREATE TABLE otel_logs (\n" +
		"    Body String,\n" +
		"    ResourceAttributes Map(String, String) MATERIALIZED map('job', 'api')\n" +
		") ENGINE = Memory"
	got := BackfillLogsColumns([]string{ddl})
	if len(got) != 1 {
		t.Fatalf("BackfillLogsColumns returned %d statements, want 1", len(got))
	}
	if strings.Contains(got[0], "ResourceAttributes Map(String, String) MATERIALIZED") {
		t.Fatalf("wire-read column remained MATERIALIZED and invisible to SELECT *:\n%s", got[0])
	}
	if !strings.Contains(got[0], "ResourceAttributes Map(String, String) DEFAULT map('job', 'api')") {
		t.Fatalf("wire-read column did not preserve its expression as DEFAULT:\n%s", got[0])
	}
}

func TestBackfillLogsColumns_LeavesOtherTablesUntouched(t *testing.T) {
	ddl := "CREATE TABLE helper (Timestamp DateTime64(9)) ENGINE = Memory"
	got := BackfillLogsColumns([]string{ddl})
	if len(got) != 1 || got[0] != ddl {
		t.Fatalf("BackfillLogsColumns(helper) = %q, want unchanged %q", got, ddl)
	}
}
