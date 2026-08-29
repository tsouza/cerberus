package regression

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// compatHarnessScriptPath drives the prometheus differential harness for
// every lane (compatibility/prometheus, -forced-route, -floor).
const compatHarnessScriptPath = "../../compatibility/prometheus/scripts/run-prometheus-compatibility.sh"

// compatComparerRelPath is the vendored upstream file the harness build-time
// patches before compiling promql-compliance-tester.
const compatComparerRelPath = "upstream/promql/comparer/comparer.go"

// The patch must both target the file the tester actually builds from and
// widen the deadline PAST the hardcoded 10s upstream ships — pinned as two
// substrings rather than one exact block so a harmless reflow of the
// surrounding comment cannot break this test, while the load-bearing pieces
// (which file, and that the replacement second count exceeds 10) still can.
const (
	compatComparerTimeoutOriginal = "10*time.Second"
	compatComparerPatchMarker     = `COMPARER="$ROOT_DIR/upstream/promql/comparer/comparer.go"`
)

// TestPrometheusCompatHarnessWidensCompareTimeout (#2707) pins the fix for
// two resets() cases over exponential (native) histograms —
// resets(demo_latency_exp_hist[5m]) and
// resets(demo_shifting_latency_exp_hist[5m]) — that flipped between
// REGRESSED and passing on an otherwise unchanged tree.
//
// Root cause, established by direct measurement (see the harness script's
// own doc comment): promql-compliance-tester's comparer.go wraps EACH
// comparison's reference+test QueryRange pair, run sequentially against one
// shared context, in a hardcoded `context.WithTimeout(..., 10*time.Second)`
// with no flag to raise it. Below ClickHouse 25.9 (#1500) every ts_grid_*
// native-rate feature resolves OFF, so the floor lane deliberately measures
// cerberus's un-optimized per-row fallback SQL for resets()/changes() over
// an exponential histogram — genuinely slower than the native path every
// other lane exercises, not incorrect — and that fallback alone already
// costs 9-13s on an otherwise idle host. A shared 10s ref+test budget was
// always going to flip on nothing but runner-speed noise: not a semantic
// divergence (every captured report carried an EMPTY diff), not seed
// nondeterminism (the fixture anchors to a fixed wall-clock timestamp), and
// not a capability-floor race (a prior, unrelated readiness fix was
// misattributed to this before direct measurement ruled it out).
//
// Lowering -query-parallelism further cannot fix this: that knob bounds how
// many comparisons QUEUE, not how long any single one is given once it
// runs, so it cannot buy back a budget a single unqueued call has already
// spent. Only widening the deadline itself gives the floor lane's
// legitimately-slower fallback SQL room to answer before being mistaken for
// a divergence — invariant 7 forbids parking the two cases in an
// allow-list, and a gate that decides on runner speed cannot mean what that
// invariant needs it to mean.
//
// This does not weaken what the gate proves: Compare() still returns the
// moment both calls do, so a genuine semantic divergence (a real diff, or a
// hard error) is caught exactly as fast as before. Only the amount of time
// a legitimately slow floor-lane answer is given before being mistaken for
// one changes.
func TestPrometheusCompatHarnessWidensCompareTimeout(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile(compatHarnessScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", compatHarnessScriptPath, err)
	}
	body := string(script)

	if !strings.Contains(body, compatComparerPatchMarker) {
		t.Fatalf("%s no longer resolves COMPARER to %s; the compare-timeout patch below "+
			"targets that variable, so this pin can't tell whether it still hits the right file",
			compatHarnessScriptPath, compatComparerRelPath)
	}

	if !strings.Contains(body, compatComparerTimeoutOriginal) {
		t.Fatalf("%s no longer names the upstream literal %q it is meant to replace in %s. "+
			"Without a build-time patch, promql-compliance-tester ships a single 10s deadline "+
			"covering BOTH the reference and test QueryRange calls, which resets()/changes() over "+
			"an exponential histogram already sits at or past on the floor lane (ClickHouse < 25.9, "+
			"#1500) alone — a legitimately slow answer gets reported as a REGRESSED case at random, "+
			"the exact tsouza/cerberus#2707 flip.",
			compatHarnessScriptPath, compatComparerTimeoutOriginal, compatComparerRelPath)
	}

	replacement := findReplacementTimeoutSeconds(t, body)
	const compatComparerMinTimeoutSeconds = 10
	if replacement <= compatComparerMinTimeoutSeconds {
		t.Fatalf("%s patches comparer.go's ref+test compare timeout to %ds, which is not wider "+
			"than the %ds upstream already ships. The patch must give the floor lane's measured "+
			"9-13s exponential-histogram resets()/changes() fallback (and the sibling "+
			"histogram_quantile/count_values family documented above it in the script) real "+
			"headroom, not just restate the same deadline that already flips at random.",
			compatHarnessScriptPath, replacement, compatComparerMinTimeoutSeconds)
	}
}

// findReplacementTimeoutSeconds extracts the N in "N*time.Second" from the
// script's own COMPAT_COMPARE_TIMEOUT_SECONDS assignment, so this test fails
// loudly — rather than silently passing on a stale marker — if that
// assignment is ever renamed or removed without updating this pin.
func findReplacementTimeoutSeconds(t *testing.T, scriptBody string) int {
	t.Helper()

	const assignPrefix = "COMPAT_COMPARE_TIMEOUT_SECONDS="
	idx := strings.Index(scriptBody, assignPrefix)
	if idx < 0 {
		t.Fatalf("%s no longer assigns %s; the compare-timeout patch this test pins reads its "+
			"replacement value from that variable", compatHarnessScriptPath, assignPrefix)
	}
	rest := scriptBody[idx+len(assignPrefix):]
	end := strings.IndexByte(rest, '\n')
	if end < 0 {
		t.Fatalf("%s: %s assignment runs off the end of the file", compatHarnessScriptPath, assignPrefix)
	}
	value := strings.TrimSpace(rest[:end])

	seconds, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("%s: %s%q did not parse as an integer: %v", compatHarnessScriptPath, assignPrefix, value, err)
	}
	return seconds
}
