package loki_test

import (
	"os"
	"testing"

	"github.com/tsouza/cerberus/internal/chdbsession"
)

// TestMain exists to shut the process-wide chDB session down before the
// test binary exits. Under `-race`, os.Exit runs runtime.racefini, which
// runs libchdb's C++ static destructors against a still-live embedded
// ClickHouse and segfaults AFTER every test has passed (#1971).
// chdbsession.CloseForExit is a no-op in the default, non-chdb build.
func TestMain(m *testing.M) {
	code := m.Run()
	chdbsession.CloseForExit()
	os.Exit(code)
}
