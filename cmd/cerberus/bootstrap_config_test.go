package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestBootstrapClickHouseConfig pins that the bootstrap connection rebinds to
// ClickHouse's always-present `default` database (so CREATE DATABASE works
// even when the configured target database doesn't exist yet) while leaving
// the rest of the connection config untouched.
//
// The runtime-config → ddl.Config mapping that used to live here moved to
// internal/schemaboot (shared with cmd/migrate); its tests live there now.
func TestBootstrapClickHouseConfig(t *testing.T) {
	in := chclient.Config{Addr: "ch:9000", Database: "otel", Username: "u", Password: "p"}
	got := bootstrapClickHouseConfig(in)
	if got.Database != "default" {
		t.Errorf("bootstrap Database = %q; want default", got.Database)
	}
	if got.Addr != "ch:9000" || got.Username != "u" || got.Password != "p" {
		t.Errorf("bootstrap config changed non-database fields: %+v", got)
	}
	if in.Database != "otel" {
		t.Errorf("input config mutated: %+v", in)
	}
}
