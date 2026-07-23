package migrateverify

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// IssuesURL is where an operator reports a divergence that looks like a cerberus
// bug. It is surfaced in the failing text report's bug-report guidance and is a
// named constant so the report and any docs stay in lock-step.
const IssuesURL = "https://github.com/tsouza/cerberus/issues"

// VerifyReportVersion is the schema version stamped into every --report JSON
// diagnostic. Bump it when the on-disk shape changes so a consumer can refuse a
// diagnostic it does not understand.
const VerifyReportVersion = 1

// hotspotFuncs are the PromQL functions whose cerberus results are most likely to
// be computed via an EXPERIMENTAL ClickHouse path (native timeSeries*ToGrid /
// experimental time-series aggregates, exponential-histogram quantiles) that can
// deviate from real Prometheus. A divergence on one of these is not proof of an
// experimental path — verify sees only two HTTP backends and cannot introspect
// cerberus config — so it is surfaced as a CANDIDATE cause, never a detection.
//
// deriv/predict_linear are included: their native path
// (timeSeriesDerivToGrid / timeSeriesPredictLinearToGrid) fits the per-window
// least-squares regression on a WHOLE-SECOND timestamp axis, which also
// quantises the aggregate's window membership. On sub-second-offset samples
// straddling a window boundary that can shift a sample between grid windows
// relative to real Prometheus (raw-ts membership) — see regressionMembershipNote.
var hotspotFuncs = []string{"rate", "irate", "increase", "histogram_quantile", "deriv", "predict_linear"}

// regressionHotspotFuncs are the subset of hotspotFuncs whose experimental
// native path carries the additional whole-second window-membership quirk, so a
// divergence on them gets a MORE SPECIFIC candidate-cause note than the generic
// experimental-CH one (see regressionMembershipNote).
var regressionHotspotFuncs = []string{"deriv", "predict_linear"}

// Attribution candidate categories. Each is a HINT about what MIGHT explain a
// divergence, never a claim that verify detected the cause: verify only compares
// two HTTP responses and cannot see either backend's internals.
const (
	// AttribCerberusBug: the divergence may be a genuine cerberus defect. Always a
	// candidate on any divergence — the honest default.
	AttribCerberusBug = "cerberus-bug"
	// AttribExperimentalCHFeature: cerberus may have computed a hotspot function via
	// an experimental ClickHouse path that deviates from Prometheus.
	AttribExperimentalCHFeature = "experimental-ch-feature"
	// AttribIngestArtifact: a series/point present on only one side may reflect an
	// ingestion difference between the two backends, not a compute bug.
	AttribIngestArtifact = "ingest-artifact"
	// AttribDataWindowGap: a coverage gap (a step or series present on one side
	// only) may reflect a data-availability window difference, not a compute bug.
	AttribDataWindowGap = "data-window-gap"
	// AttribDialectSemantics: a value difference may stem from a query-language
	// semantic difference between the reference and cerberus, not a bug.
	AttribDialectSemantics = "dialect-semantics"
)

// Per-candidate operator notes. Kept as consts so the text and JSON reports carry
// identical wording and the honesty framing ("candidate", "may") is not lost in a
// hand-edit.
const (
	cerberusBugNote = "a divergence may indicate a genuine cerberus bug; " +
		"if the other candidates below are ruled out, report it."
	experimentalCHNote = "cerberus may compute this via an experimental CH path; " +
		"re-run cerberus with experimental features disabled to isolate a cerberus bug " +
		"from an experimental-feature deviation."
	regressionMembershipNote = "deriv/predict_linear have an experimental native CH " +
		"path (timeSeriesDerivToGrid / timeSeriesPredictLinearToGrid) that fits the " +
		"regression on a whole-second timestamp axis, which also quantises window " +
		"membership: a sub-second-offset sample straddling a range-window boundary can " +
		"land in a different window than real Prometheus, shifting the slope/forecast. " +
		"This is a KNOWN limitation of that path (default-off, CERBERUS_EXPERIMENTAL_TS_GRID_RANGE); " +
		"re-run with it disabled — the portable fan-out uses raw-ts membership and " +
		"should match Prometheus — before reporting a bug."
	ingestArtifactNote = "a series present on only one backend may reflect an ingestion " +
		"difference (relabeling, scrape timing) rather than a compute bug."
	dataWindowGapNote = "a missing step or series may reflect a data-availability window " +
		"difference between the two backends rather than a compute bug."
	dialectSemanticsNote = "a value difference may stem from a query-language semantic " +
		"difference between the reference and cerberus rather than a bug."
)

// ExperimentalNote is the one-time general framing printed in the report header
// and stamped into the JSON diagnostic. It states plainly that verify cannot
// introspect cerberus config, so any experimental-feature attribution is a
// candidate hint, not a detection.
const ExperimentalNote = "cerberus may use EXPERIMENTAL ClickHouse features " +
	"(native timeSeries*ToGrid / experimental time-series aggregates, exponential " +
	"histograms) that can deviate calculations from real Prometheus. verify sees only " +
	"two HTTP backends and CANNOT introspect cerberus config, so every attribution " +
	"below is a CANDIDATE-CAUSE HINT, never a claim of detection."

// AttributionCandidate is one candidate explanation for a divergence: a category
// plus an operator-facing note. It is a hint, not a detection (see ExperimentalNote).
type AttributionCandidate struct {
	Category string `json:"category"`
	Note     string `json:"note"`
}

// isIdentChar reports whether c can appear inside a PromQL identifier (metric,
// label, or function name, plus ':' for recording-rule names). Used to enforce
// call-token boundaries so "irate" is not matched as "rate" and "foo:rate" is not
// matched as the rate() function.
func isIdentChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == ':':
		return true
	default:
		return false
	}
}

// isHotspotExpr reports whether expr invokes any hotspot function. It is a robust
// token check (not a substring match): the function name must sit on an
// identifier boundary and be followed — modulo whitespace — by '(', so it matches
// a genuine call and not a metric named "rate_total" or a "foo:rate" recording
// rule.
func isHotspotExpr(expr string) bool {
	for _, fn := range hotspotFuncs {
		if containsCall(expr, fn) {
			return true
		}
	}
	return false
}

// isRegressionHotspotExpr reports whether expr invokes deriv/predict_linear — the
// hotspot subset whose experimental native path carries the whole-second
// window-membership quirk, warranting the more specific regressionMembershipNote.
func isRegressionHotspotExpr(expr string) bool {
	for _, fn := range regressionHotspotFuncs {
		if containsCall(expr, fn) {
			return true
		}
	}
	return false
}

// containsCall reports whether fn appears in expr as a function call: bounded on
// the left by a non-identifier char (or start of string) and followed on the
// right — after optional whitespace — by '('.
func containsCall(expr, fn string) bool {
	for from := 0; ; {
		i := strings.Index(expr[from:], fn)
		if i < 0 {
			return false
		}
		i += from
		from = i + len(fn)
		if i > 0 && isIdentChar(expr[i-1]) {
			continue // part of a longer identifier, e.g. "irate" for "rate"
		}
		j := from
		for j < len(expr) && (expr[j] == ' ' || expr[j] == '\t') {
			j++
		}
		if j < len(expr) && expr[j] == '(' {
			return true
		}
	}
}

// attributeDivergence builds the candidate-cause list for a diverging query. It
// always lists cerberus-bug (the honest default), adds experimental-ch-feature
// for hotspot exprs, and splits the remaining hint by the divergence shape: a
// coverage gap points at data-window / ingest differences, a value difference at
// a dialect-semantics difference. Every entry is a candidate, never a detection.
func attributeDivergence(expr string, fd *FirstDiff) []AttributionCandidate {
	cands := []AttributionCandidate{{Category: AttribCerberusBug, Note: cerberusBugNote}}
	if isHotspotExpr(expr) {
		// deriv/predict_linear get the sub-second window-membership note; the rest
		// get the generic experimental-CH note. Both are the same category — a
		// candidate experimental-feature deviation — just worded to the actual path.
		note := experimentalCHNote
		if isRegressionHotspotExpr(expr) {
			note = regressionMembershipNote
		}
		cands = append(cands, AttributionCandidate{Category: AttribExperimentalCHFeature, Note: note})
	}
	if fd != nil && isCoverageGap(fd.Reason) {
		cands = append(cands,
			AttributionCandidate{Category: AttribDataWindowGap, Note: dataWindowGapNote},
			AttributionCandidate{Category: AttribIngestArtifact, Note: ingestArtifactNote})
	} else {
		cands = append(cands, AttributionCandidate{Category: AttribDialectSemantics, Note: dialectSemanticsNote})
	}
	return cands
}

// isCoverageGap reports whether a first-diff reason describes a series/step that
// exists on only one backend (a coverage gap), as opposed to a value that differs
// where both backends have data.
func isCoverageGap(reason string) bool {
	return strings.Contains(reason, "present in") || strings.Contains(reason, "no sample")
}

// VerifyReportParams is the resolved run context stamped into the JSON diagnostic:
// the two backend URLs, the exact query_range window (resolved to RFC3339 so a
// relative -1h/now input is captured as the concrete instant that was queried),
// the step, the tolerance, and the corpus path.
type VerifyReportParams struct {
	RefURL      string  `json:"ref_url"`
	CerberusURL string  `json:"cerberus_url"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	Step        string  `json:"step"`
	Tolerance   float64 `json:"tolerance"`
	Corpus      string  `json:"corpus"`
}

// VerifyReport is the versioned, self-describing --report diagnostic: the full
// parity result plus the context needed to reproduce and triage it. It is a
// superset of the in-memory Report, adding the schema/tool version, the generation
// timestamp, the resolved run params, and the general experimental-feature note.
type VerifyReport struct {
	SchemaVersion  int                   `json:"schema_version"`
	ToolVersion    string                `json:"tool_version"`
	GeneratedAt    string                `json:"generated_at"`
	Note           string                `json:"note"`
	Params         VerifyReportParams    `json:"params"`
	Summary        Summary               `json:"summary"`
	Results        []QueryResult         `json:"results"`
	OutOfScope     []OutOfScopeEntry     `json:"out_of_scope,omitempty"`
	HarvestSkipped []HarvestSkippedEntry `json:"harvest_skipped,omitempty"`
}

// NewVerifyReport assembles the JSON diagnostic from a completed parity run.
// generatedAt is passed in (not read from the clock) so the marshaling is
// deterministic and testable; the caller pins it to the run's wall-clock.
func NewVerifyReport(rep Report, params VerifyReportParams, toolVersion string, generatedAt time.Time) VerifyReport {
	return VerifyReport{
		SchemaVersion:  VerifyReportVersion,
		ToolVersion:    toolVersion,
		GeneratedAt:    generatedAt.UTC().Format(time.RFC3339),
		Note:           ExperimentalNote,
		Params:         params,
		Summary:        rep.Summary,
		Results:        rep.Results,
		OutOfScope:     rep.OutOfScope,
		HarvestSkipped: rep.HarvestSkipped,
	}
}

// WriteJSON renders the diagnostic as deterministically-marshaled, indented JSON
// with a trailing newline. Field order is fixed by the struct definition and the
// per-query results are in corpus order, so re-running verify on the same inputs
// produces byte-identical diagnostics (modulo the generation timestamp).
func (vr VerifyReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(vr); err != nil {
		return fmt.Errorf("encode verify report: %w", err)
	}
	return nil
}
