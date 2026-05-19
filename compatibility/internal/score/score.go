// Package score builds the per-head compatibility "score JSON" that the
// three compatibility harnesses (compatibility/{prometheus,loki,tempo})
// emit alongside their human-readable reports.
//
// The score JSON is a shields.io endpoint-badge payload
// (https://shields.io/badges/endpoint-badge): label / message / color
// plus a few cerberus-specific bookkeeping fields the badge service
// ignores but downstream tooling (dashboard, trend graphs) can consume.
//
// Centralising the shape + the color thresholds in one place keeps the
// three drivers honest: a head can't accidentally drift the color cutoff
// or rename a field and break the badge URL. Drivers call
// score.Compute(passed, total) to derive the struct, then either
// score.Write(path, s) for the convenience io path or marshal it
// themselves if they already own an *os.File.
//
// Per task #68 (the broader "compat is informational" workstream): the
// three drivers report-only — they accumulate per-case results, write
// this score JSON, and exit 0 even when parity has drifted. Only HARD
// errors (driver-wide panic, compile failure, harness can't reach the
// upstream backend) escalate to a non-zero rc. The score percent gives
// the at-a-glance signal that PR-level CI no longer provides.
package score

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Score is the shields.io endpoint-badge envelope plus the cerberus
// bookkeeping fields. The JSON tag set matches the shields contract
// for the label/message/color fields; the extra fields are tolerated
// by shields (it ignores unknown keys) and surface in downstream tools.
type Score struct {
	// SchemaVersion is the shields.io endpoint-badge schema version.
	// Pinned at 1 — bump in lock-step with shields if/when they ship
	// a v2 contract.
	SchemaVersion int `json:"schemaVersion"`

	// Label is the badge's left-hand text (e.g. "TraceQL compat").
	Label string `json:"label"`

	// Message is the badge's right-hand text — by convention the
	// percent value with one decimal of precision and a "%" suffix.
	Message string `json:"message"`

	// Color is the shields.io badge color — one of brightgreen / green
	// / yellowgreen / yellow / orange / red, chosen via threshold bands.
	Color string `json:"color"`

	// Passed counts cases that fully agreed with the reference backend.
	Passed int `json:"passed"`

	// Total counts cases attempted (passed + per-case parity failures).
	// Driver-wide hard errors (panic, compose-up failure) are NOT here
	// because the driver wouldn't have written the score JSON in that
	// path; per-case failures (HTTP 5xx, value mismatch, schema diff,
	// expected-failure marker) all contribute to total.
	Total int `json:"total"`

	// Percent is the passed/total ratio expressed as a percentage,
	// rounded to two decimal places. Set to 0 when total == 0
	// (defensive: an empty corpus would otherwise divide by zero, and
	// "100% of nothing" is a misleading signal).
	Percent float64 `json:"percent"`
}

// Compute derives a Score from the (passed, total) pair, picking the
// color band per the project-wide thresholds. The thresholds match the
// "shields.io traffic-light" convention plus an explicit 100% case
// (brightgreen) so a fully-passing run is visually distinct from a "just
// barely" pass.
//
//   - 100%        -> brightgreen  (deliberately separate from >=95;
//     brightgreen reads as "perfect")
//   - [95, 100)%  -> green        (operationally green; small known gaps)
//   - [80, 95)%   -> yellowgreen  (mostly green; nontrivial gaps)
//   - [60, 80)%   -> yellow       (mixed; substantial parity work)
//   - [40, 60)%   -> orange       (early-mover signal)
//   - [0, 40)%    -> red          (parity is essentially aspirational)
//
// The empty-corpus case (total == 0) defaults to red so an
// accidentally-blank run reads as "do not ship" rather than "perfect".
func Compute(label string, passed, total int) Score {
	var pct float64
	if total > 0 {
		pct = float64(passed) / float64(total) * 100
	}
	pct = roundTwo(pct)

	color := colorFor(pct, total)
	return Score{
		SchemaVersion: 1,
		Label:         label,
		Message:       formatMessage(pct),
		Color:         color,
		Passed:        passed,
		Total:         total,
		Percent:       pct,
	}
}

// colorFor returns the shields color band for a percent value.
// Threshold bands are inclusive on the lower edge and exclusive on the
// upper edge except for the 100% case, which is exact-match for
// brightgreen so a one-case drop reads as "green" rather than
// "brightgreen". Empty corpora are forced to red.
func colorFor(pct float64, total int) string {
	if total == 0 {
		return "red"
	}
	switch {
	case pct >= 100:
		return "brightgreen"
	case pct >= 95:
		return "green"
	case pct >= 80:
		return "yellowgreen"
	case pct >= 60:
		return "yellow"
	case pct >= 40:
		return "orange"
	default:
		return "red"
	}
}

// formatMessage renders the percent value for the shields message slot.
// Two decimal precision keeps the badge readable while still showing
// movement when the corpus grows or shrinks by a single case.
func formatMessage(pct float64) string {
	// %g would drop trailing zeros (95.00 -> 95); we want a stable
	// width so the badge SVG layout doesn't jiggle from run to run.
	return fmt.Sprintf("%.2f%%", pct)
}

// roundTwo rounds a float to two decimal places. The percent field on
// the JSON output is documented as "two decimals"; using a dedicated
// helper means the message + the numeric fields agree on the rounding.
func roundTwo(f float64) float64 {
	// Multiply, round half-away-from-zero, divide. Avoids the
	// math.Round / fmt printing drift that a naive %0.2f -> ParseFloat
	// roundtrip can introduce on exact .005 boundaries.
	if f >= 0 {
		return float64(int64(f*100+0.5)) / 100
	}
	return float64(int64(f*100-0.5)) / 100
}

// Write marshals the Score to path as indented JSON, creating the
// parent directory as needed. The two-space indent matches what the
// tempo and loki JSON reports already emit, so a casual tail of the
// reports/ directory stays visually consistent.
func Write(path string, s Score) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir score dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal score: %w", err)
	}
	// Append a trailing newline so the file is POSIX-text-clean
	// (matters for `cat` / editor diffs / git's "no newline at end
	// of file" warning).
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write score: %w", err)
	}
	return nil
}
