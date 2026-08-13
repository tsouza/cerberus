package optcorpus

import (
	"os"
	"testing"

	"github.com/tsouza/cerberus/internal/chdbsession"
)

// TestMain shuts the process-wide chDB session down before this binary
// exits. Under `-race`, os.Exit runs runtime.racefini, which runs libchdb's
// C++ static destructors against a still-live embedded ClickHouse and
// segfaults AFTER every test has already passed (#1971).
// chdbsession.CloseForExit is a no-op in the default, non-chdb build.
func TestMain(m *testing.M) {
	code := m.Run()
	chdbsession.CloseForExit()
	os.Exit(code)
}
