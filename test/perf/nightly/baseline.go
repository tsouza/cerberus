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
	"slices"

	"github.com/tsouza/cerberus/internal/chopt"
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
// measurement (the highest of calibratedBound's rounds) and its own
// per-sentinel headroom multiplier.
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

// nightlyCalibrationRounds is how many max-of-N rounds a calibration measures
// each sentinel over. A gating run measures one round and asserts its
// max-of-N, so what a committed ceiling has to clear is the run-to-run
// distribution of that max-of-N, not one draw of it. The rounds share one
// server and one sample load because the spread they sample is per query —
// how a multi-threaded read's per-stream buffers happen to overlap — not per
// run (docs/performance.background.md). For
// exchangeable rounds, a fresh gating run's max-of-N lands above the highest of
// R calibration rounds with probability 1/(R+1): five rounds bring that to one
// in six before any headroom applies, and give calibratedBound a measured
// range to hold the headroom against.
const nightlyCalibrationRounds = 5

// calibratedBound derives one sentinel's committed bound from the max-of-N
// peaks of its calibration rounds (roundMaxes, one per round) and its
// per-sentinel headroom.
//
// The committed max-of-N is the HIGHEST round, so a low-tail round cannot set
// the ceiling. The run-to-run range the rounds measured (highest minus lowest)
// must then fit inside the margin the ceiling leaves above that highest round:
// the next ordinary run can land as far above the highest round as the lowest
// round sits below it, and a ceiling within that reach is one ordinary noise
// crosses. Such a calibration is refused rather than committed — the
// sentinel's headroom is narrower than its measured noise, and widening it is
// a decision about which regression the sentinel can still catch, not
// arithmetic.
//
// Fewer than nightlyCalibrationRounds rounds, or a round that measured
// nothing, is refused too: a single round has no range to measure.
func calibratedBound(name string, expectedStatus int, roundMaxes []uint64, headroom float64) (nightlyBound, error) {
	if len(roundMaxes) < nightlyCalibrationRounds {
		return nightlyBound{}, fmt.Errorf("%s: calibrated from %d max-of-N rounds, want at least %d — "+
			"one round's max-of-N is a single draw of the run-to-run distribution the ceiling has to clear",
			name, len(roundMaxes), nightlyCalibrationRounds)
	}
	lowest, highest := slices.Min(roundMaxes), slices.Max(roundMaxes)
	if lowest == 0 {
		return nightlyBound{}, fmt.Errorf("%s: a calibration round measured a peak of 0 bytes (rounds %v)", name, roundMaxes)
	}
	ceiling := committedCeilingBytes(highest, headroom)
	spread := highest - lowest
	var margin uint64
	if ceiling > highest {
		margin = ceiling - highest
	}
	if margin < spread {
		return nightlyBound{}, fmt.Errorf("%s: the calibration rounds' max-of-N span %d bytes (%d to %d, rounds %v), "+
			"more than the %d-byte margin the ceiling %d leaves above the highest round — an ordinary run can "+
			"cross that ceiling, so the sentinel's %.2fx headroom does not cover its run-to-run noise",
			name, spread, lowest, highest, roundMaxes, margin, ceiling, headroom)
	}
	return nightlyBound{Name: name, ExpectedStatus: expectedStatus, MaxOfNBytes: highest, CeilingBytes: ceiling}, nil
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
// max-of-N measurement (the highest of the calibration rounds, kept for the
// diff/failure message), the headroom-multiplied, cap-clamped ceiling the gate
// actually asserts against, and the HTTP status the sentinel was calibrated to
// expect (PRONG (b) itself re-checks this against the sentinel's CURRENT
// ExpectedStatus, catching a baseline gone stale relative to sentinels.go
// rather than silently comparing memory across two different outcome classes).
type nightlyBound struct {
	Name           string `json:"name"`
	ExpectedStatus int    `json:"expected_status"`
	MaxOfNBytes    uint64 `json:"max_of_n_bytes"`
	CeilingBytes   uint64 `json:"ceiling_bytes"`
}

// nightlyVersionBaseline is the bound set of one ClickHouse release line
// ("major.minor"): the lane runs on several servers (perf-nightly.yml's
// matrix), and the same sentinel peaks differently on each, so each server's
// bounds are calibrated on that server alone.
type nightlyVersionBaseline struct {
	ClickHouse string         `json:"clickhouse"`
	Sentinels  []nightlyBound `json:"sentinels"`
}

type nightlyBaseline struct {
	Versions []nightlyVersionBaseline `json:"versions"`
}

// forVersion returns the bound set calibrated on ClickHouse line version.
func (b nightlyBaseline) forVersion(version string) (nightlyVersionBaseline, bool) {
	for _, v := range b.Versions {
		if v.ClickHouse == version {
			return v, true
		}
	}
	return nightlyVersionBaseline{}, false
}

// withVersion returns b with version's bound set replaced by bounds (added
// when b has none), every other version's set unchanged, and the sets ordered
// by ascending server version so the committed file diffs cleanly.
func (b nightlyBaseline) withVersion(version string, bounds []nightlyBound) (nightlyBaseline, error) {
	out := nightlyBaseline{Versions: make([]nightlyVersionBaseline, 0, len(b.Versions)+1)}
	for _, v := range b.Versions {
		if v.ClickHouse != version {
			out.Versions = append(out.Versions, v)
		}
	}
	out.Versions = append(out.Versions, nightlyVersionBaseline{ClickHouse: version, Sentinels: bounds})
	var sortErr error
	slices.SortFunc(out.Versions, func(x, y nightlyVersionBaseline) int {
		vx, okx := chopt.ParseVersion(x.ClickHouse)
		vy, oky := chopt.ParseVersion(y.ClickHouse)
		if !okx || !oky {
			sortErr = fmt.Errorf("baseline names an unparseable ClickHouse version (%q, %q)", x.ClickHouse, y.ClickHouse)
			return 0
		}
		return vx.Compare(vy)
	})
	return out, sortErr
}

func baselineFor(b nightlyVersionBaseline, name string) (nightlyBound, bool) {
	for _, s := range b.Sentinels {
		if s.Name == name {
			return s, true
		}
	}
	return nightlyBound{}, false
}

// releaseLine renders a probed server version as the "major.minor" key its
// bound set is filed under.
func releaseLine(v chopt.Version) string {
	return chopt.Version{Major: v.Major, Minor: v.Minor}.String()
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

// writeNightlyBaselineFile serialises b as pretty JSON with a trailing
// newline, so the committed file diffs cleanly and is never hand-edited
// (invariant 9). Its only caller is the UPDATE_NIGHTLY_PERF_BASELINE=1
// calibration path.
func writeNightlyBaselineFile(b nightlyBaseline) error {
	buf, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baseline: %w", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(nightlyBaselinePath, buf, 0o600); err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	return nil
}
