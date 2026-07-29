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
// internal/schemaboot (shared with cmd/cerberus); its tests live there now.
func TestBootstrapClickHouseConfig(t *testing.T) {
	in := chclient.Config{Addr: "ch:9000", Database: "otel", Username: "u", Password: "p"}
	got := bootstrapClickHouseConfig(in, schemaApplyPool)
	if got.Database != "default" {
		t.Errorf("bootstrap Database = %q; want default", got.Database)
	}
	if got.Addr != "ch:9000" || got.Username != "u" || got.Password != "p" {
		t.Errorf("bootstrap config changed non-database fields: %+v", got)
	}
	if in.Database != "otel" {
		t.Errorf("input config mutated: %+v", in)
	}
	if got.PoolName != schemaApplyPool {
		t.Errorf("bootstrap PoolName = %q; want %q — a bootstrap pool live alongside the serving one "+
			"must name its own census", got.PoolName, schemaApplyPool)
	}
}

// TestBootstrapPoolNamesAreDistinct pins the point of naming them at all: the
// connection gauges are process-wide, the pools are not, and two live pools
// sharing a name collapse onto one series with one silently winning.
func TestBootstrapPoolNamesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for name, pool := range map[string]string{
		"version probe":      versionProbePool,
		"ts-grid probe":      tsGridProbePool,
		"schema apply":       schemaApplyPool,
		"serving (chclient)": chclient.DefaultPoolName,
	} {
		if pool == "" {
			t.Errorf("%s pool name is empty; an unlabelled census collides with every other pool", name)
			continue
		}
		if prev, dup := seen[pool]; dup {
			t.Errorf("%s and %s share the pool name %q; their censuses would overwrite each other", name, prev, pool)
			continue
		}
		seen[pool] = name
	}
}
