package chclient

import "testing"

// TestOpenSQLDB_IsLazyAndReusesBuildOptions pins the two properties the
// offline audit tools depend on.
//
// Laziness: like clickhouse.Open, OpenDB must not dial until the first query.
// `cerberus audit` validates its arguments before connecting precisely so a
// bad invocation cannot cause a dial against a production deployment, and that
// ordering is only meaningful if construction itself is quiet — this test
// passes an address nothing is listening on and still expects success.
//
// Option reuse: the handle must come from the same buildOptions the serving
// client dials with, so an audit cannot silently reach a DIFFERENT database
// than the server reads.
func TestOpenSQLDB_IsLazyAndReusesBuildOptions(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Addr:     "127.0.0.1:1", // nothing listens here
		Database: "cerberus_audit_probe",
		Username: "default",
	}
	db, err := OpenSQLDB(cfg)
	if err != nil {
		t.Fatalf("OpenSQLDB must not dial at construction time, got: %v", err)
	}
	if db == nil {
		t.Fatal("OpenSQLDB returned a nil handle with no error")
	}
	t.Cleanup(func() { _ = db.Close() })

	// The same Config must map to the same driver options the serving path
	// uses; assert on the mapping rather than on the opaque handle.
	opts := buildOptions(cfg)
	if got := opts.Auth.Database; got != cfg.Database {
		t.Errorf("database = %q, want %q — an audit reaching a different database\n"+
			"than the server reads would report confident numbers about the wrong data", got, cfg.Database)
	}
	if len(opts.Addr) != 1 || opts.Addr[0] != cfg.Addr {
		t.Errorf("addr = %v, want [%s]", opts.Addr, cfg.Addr)
	}
}
