package testsql

import "testing"

// TestPromoteCreateTable pins the rewrite to the bare `CREATE TABLE` form:
// every other spelling already states its own collision semantics, and
// promoting `CREATE TABLE IF NOT EXISTS` produced
// `CREATE OR REPLACE TABLE IF NOT EXISTS`, which ClickHouse rejects as a
// syntax error.
func TestPromoteCreateTable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"CREATE TABLE t (a Int8) ENGINE = Memory", "CREATE OR REPLACE TABLE t (a Int8) ENGINE = Memory"},
		{"-- seed\n  create table t (a Int8)", "-- seed\n  CREATE OR REPLACE TABLE t (a Int8)"},
		{"CREATE TABLE IF NOT EXISTS t (a Int8)", "CREATE TABLE IF NOT EXISTS t (a Int8)"},
		{"create table if not exists t (a Int8)", "create table if not exists t (a Int8)"},
		{"CREATE OR REPLACE TABLE t (a Int8)", "CREATE OR REPLACE TABLE t (a Int8)"},
		{"CREATE TEMPORARY TABLE t (a Int8)", "CREATE TEMPORARY TABLE t (a Int8)"},
		{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (1)"},
	}
	for _, c := range cases {
		if got := PromoteCreateTable(c.in); got != c.want {
			t.Errorf("PromoteCreateTable(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
