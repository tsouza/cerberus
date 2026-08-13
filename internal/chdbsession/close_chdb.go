//go:build chdb

package chdbsession

import (
	"sync"

	"github.com/chdb-io/chdb-go/chdb"
)

// closeOnce guards against a TestMain that calls CloseForExit on more than
// one path. chdb.Session.Close drops chdb-go's cached globalSession and
// removes the session's temp directory, so a second call would build a
// BRAND NEW session (see below) purely to tear it down again.
var closeOnce sync.Once

// CloseForExit shuts the process-wide chDB session down. Call it from a
// test package's TestMain, after m.Run has returned and before os.Exit:
//
//	func TestMain(m *testing.M) {
//		code := m.Run()
//		chdbsession.CloseForExit()
//		os.Exit(code)
//	}
//
// Without it, a `-race` build of a chdb-tagged test binary segfaults inside
// libchdb.so during runtime.racefini — after the suite has already passed.
// The package doc has the full mechanism.
//
// It must run AFTER the last test: closing the session invalidates every
// *sql.DB in the binary, and chdb-go would silently mint a replacement
// session (with an empty temp directory) for the next query.
//
// chdb-go exposes no "is there a session?" predicate — chdb.NewSession
// returns the cached globalSession when one exists and CREATES one when
// none does. So in the rare case of a chdb-tagged binary that ran no chDB
// test at all (`go test -tags chdb -run TestSomethingElse`), this opens a
// session just to close it. That costs one chdb_connect and one temp
// directory at process exit and is the price of not reaching into
// chdb-go's unexported package state, which invariant 11 forbids.
func CloseForExit() {
	closeOnce.Do(func() {
		session, err := chdb.NewSession()
		// A session that cannot even be opened has no native state to tear
		// down, which is precisely the state CloseForExit wants to reach.
		if err != nil || session == nil {
			return
		}
		session.Close()
	})
}
