//go:build chdb

package chdbsession

import (
	"os"
	"time"
)

// ServerTimezone is the server timezone every chdb-tagged binary that links
// this package gives the embedded ClickHouse.
const ServerTimezone = "UTC"

// timezoneEnv is the variable ClickHouse resolves its implicit server
// timezone from when its config names none, which chDB's never does.
const timezoneEnv = "TZ"

// init pins the embedded ClickHouse's server timezone to [ServerTimezone].
//
// chDB has no server config: its implicit server timezone is the host's,
// read through TZ (else /etc/localtime) the first time a session resolves a
// timezone, and fixed for the life of the process. A seed literal without a
// zone, such as `toDateTime64('2026-05-14 12:00:00', 9)`, is read in that
// zone, so on a non-UTC host every such seed lands at a different instant
// than on a UTC CI runner and the same test gives a different verdict.
//
// Setting TZ before any session exists makes every chdb-tagged binary behave
// like a ClickHouse server configured for UTC, whatever the host. It is a
// process-level default rather than a `SET session_timezone` on one
// session, so it survives chdb-go's session rebuilds (CloseForExit, the spec
// runner's per-fixture teardown), and a query that names its own
// `SETTINGS session_timezone` still overrides it: a test that checks
// behaviour under a non-UTC server keeps seeing one.
//
// Go's own time.Local is resolved from the host BEFORE TZ changes, so the
// pin reaches only the embedded server: Go code under test that leans on
// the local zone still behaves as it would on the host.
//
// Every chdb-tagged test package links this package through its TestMain's
// [CloseForExit] call (enforced by test/regression's
// TestChDBTaggedPackagesCloseSessionOnExit), and a package's init runs
// before the init of any package that imports it and before any test runs.
func init() {
	_ = time.Local.String()
	if err := os.Setenv(timezoneEnv, ServerTimezone); err != nil {
		panic("chdbsession: pin the embedded ClickHouse server timezone: " + err.Error())
	}
}
