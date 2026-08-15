package traceql

import (
	"os"
	"testing"

	"github.com/tsouza/cerberus/internal/chdbsession"
)

// TestMain closes the process-wide chDB session before the Go runtime tears
// down libchdb's C++ statics. See test/integration/promql/testmain_test.go.
func TestMain(m *testing.M) {
	code := m.Run()
	chdbsession.CloseForExit()
	os.Exit(code)
}
