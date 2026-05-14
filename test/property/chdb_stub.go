//go:build !chdb

package property

import (
	"database/sql"
	"testing"
)

// openChDB on the non-chdb build emits t.Skip so the default CGO-free
// `just test` lane stays green. The full chdb-backed helpers live in
// chdb.go behind the `chdb` build tag.
//
//nolint:unused // called from chdb-tagged test code only
func openChDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Skip("property: chdb build tag not set, skipping (run with -tags chdb)")
	return nil
}

// applyDDL is a no-op stub on the non-chdb build. The caller will
// have hit the t.Skip in openChDB first, so this is unreachable in
// practice — but Go's build-tag matrix requires the symbol exist.
//
//nolint:unused // called from chdb-tagged test code only
func applyDDL(_ *testing.T, _ *sql.DB, _ string) {}

// tolerantRowsErr is the identity on the non-chdb build. Only the
// chdb-tagged code path scans rows and needs the EOF sentinel.
//
//nolint:unused // called from chdb-tagged test code only
func tolerantRowsErr(err error) error { return err }
