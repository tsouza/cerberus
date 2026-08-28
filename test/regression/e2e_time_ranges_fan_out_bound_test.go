package regression

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// Paths this test reads. The whole point is that the three files stay in
// lock-step, so each is named once and cited in every failure message.
const (
	timeRangesSpecPath = "../e2e/playwright/iterate-time-ranges.spec.ts"
	sweepHelperPath    = "../e2e/playwright/helpers/sweep.ts"
	admitConfigPath    = "../../internal/config/config.go"
)

// tsBlockCommentRE and tsLineCommentRE strip TypeScript comments so a
// code-shape scan cannot match the spec's own prose about that shape. The
// line-comment pattern also bites inside string literals containing `//`
// (a URL, say), which is harmless here: it can only ever remove text, so
// it cannot manufacture a match that the code does not contain.
var (
	tsBlockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	tsLineCommentRE  = regexp.MustCompile(`(?m)//.*$`)
)

// stripTSComments returns src with its comments removed.
func stripTSComments(src string) string {
	return tsLineCommentRE.ReplaceAllString(tsBlockCommentRE.ReplaceAllString(src, ""), "")
}

// TestTimeRangesSweepBoundsItsFanOutBelowTheAdmissionCap pins the fix for
// tsouza/cerberus#2674.
//
// The failure it guards: iterate-time-ranges.spec.ts fired its whole
// matrix through an unbounded `await Promise.all(entries.map(...))`. The
// matrix is (eligible Prometheus targets × TIME_RANGES × STEP_SIZES), and
// the k3d dashboard's 7 aggregating/histogram targets made that 7 × 4 × 3
// = 84 simultaneous /api/v1/query_range calls. cerberus's Prom head admits
// DefaultAdmitProm concurrent requests per process and its limiter is a
// NON-blocking TryAcquire (internal/api/admit/admit.go) — overflow is not
// queued, it is shed instantly with 503 `admission control: server
// saturated`. Two tuples came back 503, the spec's own "any non-2xx other
// than the pinned 422 is a hard failure" rule fired, and
// `failOnFlakyTests` turned the pass-on-retry into a red gating
// `dashboard-shard` job (nightly run 33080842708).
//
// The spec is not runnable outside a k3d cluster, so this static pin is
// what keeps the class from recurring on an ordinary PR. It asserts:
//
//  1. The spec declares a fan-out ceiling and that ceiling fits inside
//     DefaultAdmitProm, read from internal/config/config.go rather than
//     restated here — a future cap reduction must break this test, not the
//     nightly.
//  2. The ceiling is actually referenced by the scheduling code, not just
//     declared (a declared-but-unused const is a decorative fix).
//  3. The unbounded `Promise.all(entries.map(...))` shape is gone.
//  4. The bound is not vacuous: TIME_RANGES × STEP_SIZES alone already
//     exceeds it, so the pool throttles even a one-panel dashboard.
//  5. The warmup's deliberate saturation burst (ADMIT_BURST_CONCURRENCY in
//     helpers/sweep.ts) still EXCEEDS the cap. That burst is what covers
//     the admission-shed path on purpose; bounding the assertion phase is
//     only legitimate while the saturation path stays covered elsewhere,
//     so "fixing" #2674 by shrinking the burst must fail here.
func TestTimeRangesSweepBoundsItsFanOutBelowTheAdmissionCap(t *testing.T) {
	t.Parallel()

	specBytes, err := os.ReadFile(timeRangesSpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", timeRangesSpecPath, err)
	}
	spec := string(specBytes)

	configBytes, err := os.ReadFile(admitConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", admitConfigPath, err)
	}

	// (1) The server's cap, read from the source of truth.
	admitCapRE := regexp.MustCompile(`DefaultAdmitProm\s*=\s*(\d+)`)
	capMatch := admitCapRE.FindStringSubmatch(string(configBytes))
	if capMatch == nil {
		t.Fatalf("%s no longer declares `DefaultAdmitProm = <n>` — this test's premise (a fixed per-process Prom admission cap the e2e client must stay under) is gone; re-derive the cap before dropping the assertion", admitConfigPath)
	}
	admitCap, err := strconv.Atoi(capMatch[1])
	if err != nil {
		t.Fatalf("parse DefaultAdmitProm from %s: %v", admitConfigPath, err)
	}

	// The client's ceiling.
	ceilingRE := regexp.MustCompile(`(?m)^const MATRIX_FAN_OUT_CONCURRENCY = (\d+);`)
	ceilingMatch := ceilingRE.FindStringSubmatch(spec)
	if ceilingMatch == nil {
		t.Fatalf("%s declares no `const MATRIX_FAN_OUT_CONCURRENCY = <n>;` — the matrix sweep must bound its own fan-out, or it fires (targets × ranges × steps) query_range calls at a %d-slot Prom head and fails on the 503 shed it provoked itself (#2674)", timeRangesSpecPath, admitCap)
	}
	ceiling, err := strconv.Atoi(ceilingMatch[1])
	if err != nil {
		t.Fatalf("parse MATRIX_FAN_OUT_CONCURRENCY from %s: %v", timeRangesSpecPath, err)
	}
	if ceiling < 1 {
		t.Fatalf("%s sets MATRIX_FAN_OUT_CONCURRENCY = %d — a non-positive ceiling drains nothing and the sweep would assert on zero tuples", timeRangesSpecPath, ceiling)
	}
	if ceiling > admitCap {
		t.Errorf("%s sets MATRIX_FAN_OUT_CONCURRENCY = %d, above the %d-slot Prom admission cap (DefaultAdmitProm in %s). cerberus's limiter is a non-blocking TryAcquire, so the overflow is shed with 503 `admission control: server saturated` and the spec fails on its own load (#2674)", timeRangesSpecPath, ceiling, admitCap, admitConfigPath)
	}

	// Checks (2) and (3) are about the code, not the prose. The spec
	// documents both the ceiling and the unbounded shape it replaced, so
	// scanning the raw text would match its own explanation.
	specCode := stripTSComments(spec)

	// (2) Declared AND used. A ceiling the scheduler never reads is a
	// decorative fix that leaves the unbounded burst in place.
	usageRE := regexp.MustCompile(`MATRIX_FAN_OUT_CONCURRENCY`)
	if got := len(usageRE.FindAllString(specCode, -1)); got < 2 {
		t.Errorf("%s mentions MATRIX_FAN_OUT_CONCURRENCY %d time(s) — the ceiling is declared but never read, so nothing actually bounds the sweep's fan-out (#2674)", timeRangesSpecPath, got)
	}

	// (3) The unbounded shape must be gone. `Promise.all` over a map of
	// the whole `entries` array schedules every tuple at once regardless
	// of any ceiling declared elsewhere in the file.
	unboundedRE := regexp.MustCompile(`Promise\.all\(\s*entries\.map\(`)
	if unboundedRE.MatchString(specCode) {
		t.Errorf("%s still fires the matrix through `Promise.all(entries.map(...))` — that schedules every (panel × range × step) tuple simultaneously against a %d-slot Prom head and reintroduces the 503 shed of #2674; drain `entries` through the bounded worker pool instead", timeRangesSpecPath, admitCap)
	}

	// (4) Non-vacuity: the ceiling has to actually throttle something. The
	// per-panel tuple count (ranges × steps) is the floor of what the
	// matrix produces, so if even that fits under the ceiling the pool
	// never limits anything and this whole guard is inert.
	rangeRE := regexp.MustCompile(`(?m)^\s+\{ label: '[^']*', windowSeconds: `)
	stepRE := regexp.MustCompile(`(?m)^\s+\{ label: '[^']*', stepSeconds: `)
	rangeCount := len(rangeRE.FindAllString(spec, -1))
	stepCount := len(stepRE.FindAllString(spec, -1))
	if rangeCount == 0 || stepCount == 0 {
		t.Fatalf("%s: parsed %d TIME_RANGES entr(ies) and %d STEP_SIZES entr(ies) — the matrix literals no longer match the expected shape, so this test cannot tell whether the fan-out ceiling throttles anything; fix the pattern rather than dropping the check", timeRangesSpecPath, rangeCount, stepCount)
	}
	if perPanelTuples := rangeCount * stepCount; perPanelTuples <= ceiling {
		t.Errorf("%s: a single panel target yields %d tuple(s) (%d range(s) × %d step(s)), which is not above the MATRIX_FAN_OUT_CONCURRENCY ceiling of %d — the worker pool never throttles and the guard is inert", timeRangesSpecPath, perPanelTuples, rangeCount, stepCount, ceiling)
	}

	// (5) The deliberate saturation path must survive. Bounding the
	// assertion phase is only honest while something else still drives the
	// limiter past its cap on purpose.
	sweepBytes, err := os.ReadFile(sweepHelperPath)
	if err != nil {
		t.Fatalf("read %s: %v", sweepHelperPath, err)
	}
	burstRE := regexp.MustCompile(`(?m)^const ADMIT_BURST_CONCURRENCY = (\d+);`)
	burstMatch := burstRE.FindStringSubmatch(string(sweepBytes))
	if burstMatch == nil {
		t.Fatalf("%s declares no `const ADMIT_BURST_CONCURRENCY = <n>;` — that burst is what covers the admission-shed path on purpose, and it is the reason %s may bound its own fan-out at all (#2674)", sweepHelperPath, timeRangesSpecPath)
	}
	burst, err := strconv.Atoi(burstMatch[1])
	if err != nil {
		t.Fatalf("parse ADMIT_BURST_CONCURRENCY from %s: %v", sweepHelperPath, err)
	}
	if burst <= admitCap {
		t.Errorf("%s sets ADMIT_BURST_CONCURRENCY = %d, which no longer exceeds the %d-slot Prom admission cap (DefaultAdmitProm in %s) — nothing in the e2e lane drives the limiter past its cap any more, so the shed path and the `Admission rejections` panel go uncovered", sweepHelperPath, burst, admitCap, admitConfigPath)
	}
}
