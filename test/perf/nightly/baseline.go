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
// same value, and the value is PINNED TO THE COMMITTED BASELINE'S CUT: every
// ceiling_bytes in nightly-baseline.json is committedCeilingBytes(max_of_n,
// headroom) clamped to this fraction of the cap at generation time, and
// TestCommittedCeilingBytes_ClampsToTheAbsoluteCeiling refuses a file whose
// ceilings were cut against a different fraction. So the fraction only
// moves together with a baseline regeneration — never on its own, and never
// by re-measuring on an unreleased branch, which would re-cut every
// sentinel's max_of_n to whatever the branch costs today and blind the
// ratchet to exactly the regression it exists to catch.
//
// 0.85 is the fraction the committed cut was made against: at that cut
// pod_status_reason_gauge — a plain `sum by (reason) (...)` gauge
// aggregation over this sample's real 7,024-series cardinality (855
// distinct pods collapsing to 8 `reason` values) — measured 76.2% of cap,
// above smoke's 0.75, and cerberus issue #2435 was filed for it rather than
// the cost being silently absorbed. #2435 has since shipped (the
// sum()/count() fusion over a RangeLWR fan-out, PR #2452): the lane's
// max-of-5 read-outs are now a stable 66.1% across consecutive nightlies
// (runs 34055929772, 34060679663 and 34063889701 on 2026-09-06), so the
// widening no longer has a measured cost behind it — 0.75 fits again. It is
// applied at the next baseline cut, with the max_of_n values that cut
// measures, because that is the only way the fraction can move (above).
// Until then PRONG (b)'s per-sentinel headroom, not this fraction, is the
// bound that actually binds for that sentinel.
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

// exceedsCapCeiling reports whether a measured peak trips PRONG (a), the
// absolute cap-relative ceiling.
//
// The harness's PRONG (a) IS this call, so the unit lane drives the real
// decision rather than a restatement of it: a prong that started comparing the
// wrong quantity, or flipped its boundary, fails in baseline_test.go instead of
// waiting for a real regression to go unreported.
func exceedsCapCeiling(maxBytes uint64) bool {
	return maxBytes > nightlyCapCeilingBytes
}

// exceedsCommittedCeiling reports whether a measured peak trips PRONG (b), the
// committed per-sentinel ceiling. The harness's PRONG (b) IS this call — see
// exceedsCapCeiling for why that matters.
func exceedsCommittedCeiling(maxBytes uint64, bound nightlyBound) bool {
	return maxBytes > bound.CeilingBytes
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
	if err := os.WriteFile(nightlyBaselinePath, buf, 0o600); err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	return nil
}
