// baseline.go — the perf-smoke corpus's committed memory bounds and the
// arithmetic that derives them.
//
// This file is deliberately UNTAGGED while the harness that consumes it
// (realch_perfsmoke_integration_test.go) is behind `integration`. The two
// ceilings a sentinel is asserted against are pure arithmetic over committed
// numbers, and the invariant that keeps the tighter of them honest — PRONG (b)
// must never be looser than PRONG (a) — is a property of the committed file,
// not of a ClickHouse container. Keeping the derivation here lets
// sentinels_test.go assert it on every PR through the ordinary unit lane,
// instead of only when somebody happens to regenerate the baseline with Docker
// present. Issue #2906 is exactly the failure that costs: the clamp below was
// missing for the whole life of this corpus and nothing could observe it.
package smoke

import (
	"encoding/json"
	"fmt"
	"os"
)

// --- calibrated constants -------------------------------------------------
//
// Every number below was measured, not guessed, against a real
// testcontainers ClickHouse on this branch. See each constant's own comment
// for the specific run that produced it; the PR description carries the full
// calibration log.

// sentinelMemoryCapBytes is the max_memory_usage cap the whole test session
// runs under. 1 GiB matches config's default CERBERUS_CH_QUERY_MAX_MEMORY —
// the cap a real deployment runs under by default — so
// spillThreshold(cap) (spill.go) resolves to the exact 512 MiB the #2364
// incident's own postmortem cites, and Sentinel 2's seed cardinality is
// calibrated to reliably cross it at this specific cap.
const sentinelMemoryCapBytes int64 = 1 << 30 // 1 GiB

// sentinelMemoryCapFraction bounds the ABSOLUTE, cap-relative ceiling every
// sentinel's peak memory must stay under: strictly above spillCapDenominator's
// implied 0.5 (spill.go — below that fraction spill should already have
// engaged and kept every query comfortably under half the cap) and strictly
// below 1.0 (ClickHouse's own MEMORY_LIMIT_EXCEEDED boundary). 0.75 sits at
// the midpoint of that (0.5, 1.0) admissible band. Calibration against a real
// testcontainers ClickHouse measured every sentinel comfortably under this
// line: Sentinel 1 (native histogram) ~205 MiB (19% of the 1 GiB cap),
// Sentinel 2 (spill) ~650 MiB (61%) at its calibrated 10,000-series
// cardinality, Sentinel 3 (compare) ~682 MiB (64%) at its calibrated
// 40,000-trace cardinality — every one comfortably under the 0.75x/805 MiB
// line with real margin, so 0.75 is not tuned tight to the measured data; it
// is the band midpoint the task asked for, validated rather than picked
// blind.
const sentinelMemoryCapFraction = 0.75

// sentinelBaselineHeadroom multiplies each sentinel's calibrated max-of-N
// measurement to derive its committed per-sentinel ceiling in
// perf-smoke-baseline.json — the NOMINAL multiple, before
// committedCeilingBytes clamps it to sentinelCapCeilingBytes; a sentinel close
// to the absolute ceiling ends up with less. 1.5x mirrors
// scale_wall_pin_chdb_test.go's
// scanAmplificationHeadroom (its low-variance prong): real-CH memory_usage
// across the calibration repeats varied by well under 2% run-to-run for
// every sentinel (e.g. Sentinel 3's five repeats at its calibrated scale all
// landed within 4 bytes of each other, 682,475,556-682,475,660), far tighter
// than scale-wall's WALL-clock prong (which uses 2.5x precisely because wall
// is noisier), so the tighter 1.5x is the deliberate choice for a memory
// metric.
const sentinelBaselineHeadroom = 1.5

// sentinelCapCeilingBytes is PRONG (a): the ABSOLUTE, cap-relative ceiling
// every sentinel's peak memory must stay under, independent of any committed
// number. 0.75 divides 2^30 evenly, so this constant expression is exact and
// needs none of the math.Round test/perf/nightly's 0.85 fraction does.
const sentinelCapCeilingBytes = uint64(float64(sentinelMemoryCapBytes) * sentinelMemoryCapFraction)

// committedCeilingBytes derives one sentinel's PRONG (b) ceiling — the number
// written into perf-smoke-baseline.json — from its calibration-time max-of-N
// measurement.
//
// The min() is the load-bearing part, and it mirrors test/perf/nightly's own
// calibration path rather than inventing a second mechanism: for a sentinel
// already close to the absolute ceiling, an unclamped
// maxBytes*sentinelBaselineHeadroom sits ABOVE PRONG (a)'s own bound, and a
// ceiling above PRONG (a)'s can never fire — PRONG (a) rejects first, every
// time. A looser "tighter" check is a gate that silently never gates. PRONG
// (b) must never be looser than PRONG (a).
//
// Issue #2906: the smoke lane shipped without this clamp, so
// spill_high_cardinality_groupby (committed 977,277,601) and
// compare_memory_bound (committed 1,023,624,673) both carried ceilings above
// the 805,306,368-byte absolute one and gated nothing at all — on the lane
// that runs on every PR through the required strict-scan job.
func committedCeilingBytes(maxBytes uint64) uint64 {
	return min(uint64(float64(maxBytes)*sentinelBaselineHeadroom), sentinelCapCeilingBytes)
}

// exceedsCapCeiling reports whether a measured peak trips PRONG (a), the
// absolute cap-relative ceiling.
//
// The harness's PRONG (a) IS this call, so the unit lane drives the real
// decision rather than a restatement of it: a prong that started comparing the
// wrong quantity, or flipped its boundary, fails in baseline_test.go instead of
// waiting for a real regression to go unreported.
func exceedsCapCeiling(maxBytes uint64) bool {
	return maxBytes > sentinelCapCeilingBytes
}

// exceedsCommittedCeiling reports whether a measured peak trips PRONG (b), the
// committed per-sentinel ceiling. The harness's PRONG (b) IS this call — see
// exceedsCapCeiling for why that matters.
func exceedsCommittedCeiling(maxBytes uint64, bound sentinelBound) bool {
	return maxBytes > bound.CeilingBytes
}

// --- baseline load/write (invariant 9: never hand-edited) -----------------

// perfSmokeBaselinePath is the committed bound file, a sibling of
// scale-wall-baseline.json one level up from this package.
const perfSmokeBaselinePath = "../perf-smoke-baseline.json"

// sentinelBound is one sentinel's committed bound: the calibration-time
// max-of-N measurement (kept for the diff/failure message) and the
// headroom-multiplied, cap-clamped ceiling the gate actually asserts against.
type sentinelBound struct {
	Name         string `json:"name"`
	MaxOfNBytes  uint64 `json:"max_of_n_bytes"`
	CeilingBytes uint64 `json:"ceiling_bytes"`
}

type perfSmokeBaseline struct {
	Sentinels []sentinelBound `json:"sentinels"`
}

func baselineFor(b perfSmokeBaseline, name string) (sentinelBound, bool) {
	for _, s := range b.Sentinels {
		if s.Name == name {
			return s, true
		}
	}
	return sentinelBound{}, false
}

// readPerfSmokeBaseline parses the committed bound file. It returns the read
// and the parse error separately from any test plumbing so both the
// integration harness and the unit lane can consume the same loader.
func readPerfSmokeBaseline() (perfSmokeBaseline, error) {
	buf, err := os.ReadFile(perfSmokeBaselinePath)
	if err != nil {
		return perfSmokeBaseline{}, fmt.Errorf("read baseline %s: %w", perfSmokeBaselinePath, err)
	}
	var b perfSmokeBaseline
	if err := json.Unmarshal(buf, &b); err != nil {
		return perfSmokeBaseline{}, fmt.Errorf("parse baseline %s: %w", perfSmokeBaselinePath, err)
	}
	return b, nil
}

// writePerfSmokeBaselineFile serialises bounds (in Sentinels order) as pretty
// JSON with a trailing newline, so the committed file diffs cleanly and is
// never hand-edited (invariant 9). Its only caller is the
// UPDATE_PERF_SMOKE_BASELINE=1 calibration path.
func writePerfSmokeBaselineFile(bounds []sentinelBound) error {
	buf, err := json.MarshalIndent(perfSmokeBaseline{Sentinels: bounds}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baseline: %w", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(perfSmokeBaselinePath, buf, 0o600); err != nil {
		return fmt.Errorf("write baseline: %w", err)
	}
	return nil
}
