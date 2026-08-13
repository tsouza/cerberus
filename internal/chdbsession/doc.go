// Package chdbsession owns the process-exit half of libchdb's session
// lifecycle for chdb-tagged TEST binaries.
//
// libchdb is an embedded ClickHouse loaded into the test process with
// dlopen. chdb-go caches ONE process-wide `*chdb.Session` (its package-level
// `globalSession`) and hands it to every `sql.Open("chdb", "")` in the
// binary, and the driver's own `(*conn).Close` is a no-op — so nothing on
// the Go side ever shuts the native engine down. The session, its
// connection and ClickHouse's background thread pools stay live until the
// process exits.
//
// That is fine for a plain `go test` run, because Go's `os.Exit` reaches
// the `exit_group` syscall directly and libchdb's C++ static destructors
// never run. It is NOT fine under `-race`: `os.Exit` first calls
// `runtime_beforeExit` → `runtime.racefini` → `__tsan_fini`, which — per
// the Go runtime's own comment on race.go's racefini — "will run C atexit
// functions and C++ destructors". Those destructors then tear libchdb down
// while its engine is still live, and the process dies with SIGSEGV inside
// libchdb.so AFTER every test has already passed, turning a green suite
// into a failed lane (issue #1971).
//
// [CloseForExit] supplies the missing half: it closes the cached session
// before the test binary exits, so the C++ destructors that `__tsan_fini`
// triggers run against an engine that has already been shut down cleanly.
// Wire it into a package's TestMain — see [CloseForExit]'s example — and
// that package's chdb-tagged tests become `-race`-safe.
//
// The non-chdb build of this package is a no-op, so an untagged TestMain
// can call [CloseForExit] unconditionally without dragging libchdb into
// the default CGO_ENABLED=0 lane.
package chdbsession
