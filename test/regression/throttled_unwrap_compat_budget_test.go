package regression

import (
	"os"
	"regexp"
	"testing"
)

// The Loki compatibility lane's driver budget, as the lane's runner script
// defaults it, and as test/chserver's throttled unwrap probe copies it.
const (
	lokiCompatRunnerScript = "../../.github/scripts/run-loki-compatibility.mjs"
	throttledUnwrapProbe   = "../../test/chserver/throttled_unwrap_integration_test.go"
)

var (
	runnerParallelism = regexp.MustCompile(`(?m)^const PARALLELISM = process\.env\.DRIVER_PARALLELISM \|\| '(\d+)';$`)
	runnerTimeout     = regexp.MustCompile(`(?m)^const TIMEOUT = process\.env\.DRIVER_TIMEOUT \|\| '(\d+)s';$`)
	probeParallelism  = regexp.MustCompile(`(?m)^\tcompatDriverParallelism = (\d+)$`)
	probeTimeout      = regexp.MustCompile(`(?m)^\tcompatRequestTimeout\s+= (\d+) \* time\.Second$`)
)

// TestThrottledUnwrapProbeMirrorsTheLokiCompatBudget requires the throttled
// unwrap probe to send as many requests at once, and to give each as long, as
// the Loki compatibility lane's driver does by default. A probe whose budget
// drifted from the lane's would keep passing while the lane timed out.
func TestThrottledUnwrapProbeMirrorsTheLokiCompatBudget(t *testing.T) {
	t.Parallel()
	runner := readBudgetSource(t, lokiCompatRunnerScript)
	probe := readBudgetSource(t, throttledUnwrapProbe)
	for _, c := range []struct {
		what          string
		lane, inProbe *regexp.Regexp
	}{
		{"requests at once", runnerParallelism, probeParallelism},
		{"request timeout (s)", runnerTimeout, probeTimeout},
	} {
		lane := budgetCapture(t, lokiCompatRunnerScript, c.lane, runner)
		got := budgetCapture(t, throttledUnwrapProbe, c.inProbe, probe)
		if got != lane {
			t.Errorf("throttled unwrap probe %s = %s; the Loki compatibility lane's driver uses %s", c.what, got, lane)
		}
	}
}

func readBudgetSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// budgetCapture returns re's one capture in source, failing when re does not
// match exactly once.
func budgetCapture(t *testing.T, path string, re *regexp.Regexp, source string) string {
	t.Helper()
	m := re.FindAllStringSubmatch(source, -1)
	if len(m) != 1 {
		t.Fatalf("%s: %d matches of %s; want 1", path, len(m), re)
	}
	return m[0][1]
}
