package resourcefixture

import (
	"os"
	"testing"

	"github.com/tsouza/cerberus/internal/chdbsession"
)

// TestMain shuts the process-wide chDB session down before the test binary
// exits — chdbsession.CloseForExit is a no-op in the default, non-chdb
// build, and this file carries no build tag so the untagged
// resource_config_test.go in this same package keeps running under a
// plain `go test`. See internal/api/tempo/testmain_test.go for the fuller
// rationale (cerberus issue #1971: os.Exit under -race runs
// runtime.racefini, which segfaults against a still-live embedded
// ClickHouse session).
func TestMain(m *testing.M) {
	code := m.Run()
	chdbsession.CloseForExit()
	os.Exit(code)
}
