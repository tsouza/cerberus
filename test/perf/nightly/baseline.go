// baseline.go — the perf-nightly corpus's committed memory bounds and the
// arithmetic that derives them.
//
// Untagged, while the harness that consumes it
// (realch_perfnightly_integration_test.go) is behind `integration`, for the
// same reason test/perf/smoke's baseline.go is: the two ceilings a sentinel is
// asserted against are pure arithmetic over committed numbers, and the
// invariant that keeps the tighter of them honest — PRONG (b) must never be
// looser than PRONG (a) — is a property of the committed file, not of a
// ClickHouse container. Keeping the derivation here lets baseline_test.go
// assert it through the ordinary unit lane instead of only when somebody
// regenerates with Docker present.
package nightly

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// perfNightlyMemoryCapBytes matches CERBERUS_CH_QUERY_MAX_MEMORY's default —
// see test/perf/smoke's sentinelMemoryCapBytes for why this is the value a
// real deployment runs under, not a value tuned for this test.
const perfNightlyMemoryCapBytes int64 = 1 << 30 // 1 GiB

// nightlyMemoryCapFraction is PRONG (a)'s absolute ceiling. Same rationale
// as test/perf/smoke's sentinelMemoryCapFraction (above
// spillThreshold(cap)'s implied 0.5, below 1.0's OOM boundary), but NOT the
// same value — 0.75 (smoke's own midpoint pick) does not fit this lane's
// real measured data. Calibrated against this lane's own real
// post-#2429-fix, max-of-5 numbers: the two ExpectedStatus=200 sentinels
// measured 0.9% and 76.2% of cap; the two ExpectedStatus=422 sentinels
// (rejected BEFORE the expensive stage runs, by design) measured 25.9% and
// 27.6% at rejection. pod_status_reason_gauge's 76.2% already exceeds
// 0.75x — a plain `sum by (reason) (...)` gauge aggregation over this
// sample's real 7,024-series cardinality (855 distinct pods, only 8
// distinct `reason` values it collapses to) genuinely costs this much;
// filed as issue #2435 for its own investigation rather than silently
// absorbed into a looser fraction picked to avoid looking at it. 0.85
// keeps real, verified margin (~8 points) above that measured cost while
// staying meaningfully below ClickHouse's own 1.0 OOM boundary.
const nightlyMemoryCapFraction = 0.85

// nightlyCapCeilingBytes is PRONG (a): the ABSOLUTE, cap-relative ceiling
// every sentinel's peak memory must stay under, independent of any committed
// number.
//
// A var rather than a const, and math.Round rather than a truncating
// conversion: (1<<30)*0.85 is not itself an exact integer, so it has to go
// through runtime float64 arithmetic — Go's exact-rational constant folding
// refuses the conversion outright. (test/perf/smoke's 0.75 happens to divide
// 2^30 evenly, which is why its equivalent IS a const.)
var nightlyCapCeilingBytes = uint64(math.Round(float64(perfNightlyMemoryCapBytes) * nightlyMemoryCapFraction))

// committedCeilingBytes derives one sentinel's PRONG (b) ceiling — the number
// written into nightly-baseline.json — from its calibration-time max-of-N
// measurement and its own per-sentinel headroom multiplier.
//
// The min() is the load-bearing part: for a sentinel already close to the
// absolute ceiling (pod_status_reason_gauge measured 76.6% against a 1.5x
// headroom — 1.5x of THAT already exceeds the 1 GiB cap outright), an
// unclamped committed ceiling would sit above PRONG (a)'s own bound, making
// PRONG (b) permanently unable to fire — a looser "tighter" check is a gate
// that silently never gates. PRONG (b) must never be looser than PRONG (a).
func committedCeilingBytes(maxBytes uint64, headroom float64) uint64 {
	return min(uint64(float64(maxBytes)*headroom), nightlyCapCeilingBytes)
}

// --- baseline load/write (invariant 9: never hand-edited) -----------------

// nightlyBaselinePath is the committed bound file, a sibling of
// perf-smoke-baseline.json one level up from this package.
const nightlyBaselinePath = "../nightly-baseline.json"

// nightlyBound is one sentinel's committed bound: the calibration-time
// max-of-N measurement (kept for the diff/failure message), the
// headroom-multiplied, cap-clamped ceiling the gate actually asserts against,
// and the HTTP status the sentinel was calibrated to expect (PRONG (b) itself
// re-checks this against the sentinel's CURRENT ExpectedStatus, catching a
// baseline gone stale relative to sentinels.go rather than silently
// comparing memory across two different outcome classes).
type nightlyBound struct {
	Name           string `json:"name"`
	ExpectedStatus int    `json:"expected_status"`
	MaxOfNBytes    uint64 `json:"max_of_n_bytes"`
	CeilingBytes   uint64 `json:"ceiling_bytes"`
}

type nightlyBaseline struct {
	Sentinels []nightlyBound `json:"sentinels"`
}

func baselineFor(b nightlyBaseline, name string) (nightlyBound, bool) {
	for _, s := range b.Sentinels {
		if s.Name == name {
			return s, true
		}
	}
	return nightlyBound{}, false
}

// readNightlyBaseline parses the committed bound file. It returns the read and
// the parse error separately from any test plumbing so both the integration
// harness and the unit lane can consume the same loader.
func readNightlyBaseline() (nightlyBaseline, error) {
	buf, err := os.ReadFile(nightlyBaselinePath)
	if err != nil {
		return nightlyBaseline{}, fmt.Errorf("read baseline %s: %w", nightlyBaselinePath, err)
	}
	var b nightlyBaseline
	if err := json.Unmarshal(buf, &b); err != nil {
		return nightlyBaseline{}, fmt.Errorf("parse baseline %s: %w", nightlyBaselinePath, err)
	}
	return b, nil
}

// writeNightlyBaselineFile serialises bounds (in Sentinels order) as pretty
// JSON with a trailing newline, so the committed file diffs cleanly and is
// never hand-edited (invariant 9). Its only caller is the
// UPDATE_NIGHTLY_PERF_BASELINE=1 calibration path.
func writeNightlyBaselineFile(bounds []nightlyBound) error {
	buf, err := json.MarshalIndent(nightlyBaseline{Sentinels: bounds}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baseline: %w", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(nightlyBaselinePath, buf, 0o644); err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	return nil
}
