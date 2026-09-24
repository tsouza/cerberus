package regression

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/config"
)

// TestChartQueryTimeoutDefaultMatchesBinary holds the chart's copy of the
// binary's default query timeout equal to the binary's own. The bundled
// ClickHouse shutdown wait follows cerberus's query timeout, and falls back to
// this copy when no timeout is set; a binary default that moved alone would
// leave every such release draining for the old budget.
func TestChartQueryTimeoutDefaultMatchesBinary(t *testing.T) {
	const helperPath = "../../deploy/helm/cerberus/templates/clickhouse/_helpers.tpl"
	helper, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read %s: %v", helperPath, err)
	}
	m := regexp.MustCompile(`(?s)define "cerberus\.queryTimeoutDefaultSeconds" -\}\}\s*(\d+)\s*\{\{- end`).FindSubmatch(helper)
	if m == nil {
		t.Fatalf("%s defines no cerberus.queryTimeoutDefaultSeconds", helperPath)
	}
	chartSeconds, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("cerberus.queryTimeoutDefaultSeconds %q: %v", m[1], err)
	}

	t.Setenv("CERBERUS_QUERY_TIMEOUT", "")
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if want := cfg.ClickHouse.QueryTimeout; time.Duration(chartSeconds)*time.Second != want {
		t.Fatalf("chart cerberus.queryTimeoutDefaultSeconds = %ds, binary default query timeout = %s", chartSeconds, want)
	}
}
