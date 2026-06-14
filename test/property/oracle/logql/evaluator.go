package logql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/test/property"
)

// Evaluate is the top-level entry point. It parses the query via
// Loki's syntax.ParseExpr (so the AST shape matches what cerberus's
// pipeline sees), then walks the AST under in-tree evaluation rules.
//
// Returns a [property.Outcome] in the same shape the framework's
// comparator consumes — one OutcomeRow per record that survives the
// pipeline.
//
// On parse error or any AST node the oracle doesn't support, the
// returned Outcome carries the error and an empty row set. The
// framework's CompareOutcomes treats both-erroring queries as
// agreement, so an unsupported shape doesn't fail the property; it
// just means the test doesn't exercise that shape.
func Evaluate(d property.Dataset, q property.Query) property.Outcome {
	if d.Logs == nil {
		return property.Outcome{Err: fmt.Errorf("oracle/logql: dataset has no Logs mirror")}
	}
	expr, err := syntax.ParseExpr(q.String)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("oracle/logql: parse %q: %w", q.String, err)}
	}

	records, err := applyExpr(expr, d.Logs.Records)
	if err != nil {
		return property.Outcome{Err: err}
	}

	// Group by post-pipeline label set so the property comparator
	// sees the same series identity cerberus's handler produces. The
	// handler's toStreamsWithTransform path groups by the
	// CanonicalKey of the (post-format) labels — match that here.
	//
	// Each row's labels also carry the synthesized `detected_level`,
	// resolved via cerberus's detected_level cascade (see
	// [detectedLevel] — structured-metadata `detected_level` /
	// `level` / `log.level` / `severity` / `severity_text` keys in the
	// LogAttributes map take precedence, then the dedicated
	// SeverityText column). Loki surfaces this as a stream-identity
	// dimension on EVERY row (a row with no detectable level resolves
	// to "unknown"), and cerberus's wire-wrap (Lang.ProjectSamples on
	// the log-stream branch) mirrors that — so the oracle stamps it
	// unconditionally too.
	out := property.Outcome{Rows: make([]property.OutcomeRow, 0, len(records))}
	for _, r := range records {
		labels := copyLabels(r.ResourceAttributes)
		labels["detected_level"] = detectedLevel(r)
		// TimestampMs is unix milliseconds, matching the prom-side
		// convention. Cerberus's stream-wire format uses nanos, but
		// the property runner normalises to milliseconds (the
		// generator works in nanos so we divide once here).
		out.Rows = append(out.Rows, property.OutcomeRow{
			Labels:      labels,
			TimestampMs: r.TimestampNanos / int64(1e6),
			Line:        r.Body,
		})
	}
	return out
}

// detectedLevelKeys is the structured-metadata key precedence cerberus
// scans before falling back to the SeverityText column — it must match
// [internal/logql.detectedLevelSourceExpr]'s order exactly: the
// canonical `detected_level` key first, then the allowed level fields
// (`level` / `log.level` / `severity` / `severity_text`). The first key
// present with a non-empty value wins.
var detectedLevelKeys = []string{
	"detected_level",
	"level",
	"log.level",
	"severity",
	"severity_text",
}

// detectedLevel resolves a record's synthesized `detected_level` value,
// mirroring cerberus's [internal/logql.detectedLevelExpr]: it picks the
// raw level source per [detectedLevelKeys] precedence (structured
// metadata in the LogAttributes map, then the SeverityText column) and
// normalises it via [normaliseLogLevel]. An all-empty row resolves to
// "unknown" — reference Loki stamps detected_level on every record.
func detectedLevel(r property.LogRecord) string {
	src := ""
	for _, key := range detectedLevelKeys {
		if v := r.LogAttributes[key]; v != "" {
			src = v
			break
		}
	}
	if src == "" {
		src = r.SeverityText
	}
	return normaliseLogLevel(src)
}

// normaliseLogLevel mirrors cerberus's [internal/logql.normaliseLevelExpr]
// — maps the case-insensitive forms upstream Loki accepts onto the
// canonical lowercase level strings. Empty input returns "unknown":
// reference Loki's distributor stamps `detected_level="unknown"` on
// every record with no detectable level, and cerberus's
// `normaliseLevelExpr` mirrors that with its leading empty → "unknown"
// branch. Non-empty inputs outside the known vocabulary pass through
// lowercased, matching upstream `normalizeLogLevel`'s default branch.
func normaliseLogLevel(severity string) string {
	switch strings.ToLower(severity) {
	case "":
		return "unknown"
	case "trace", "trc":
		return "trace"
	case "debug", "dbg":
		return "debug"
	case "info", "inf", "information":
		return "info"
	case "warn", "wrn", "warning":
		return "warn"
	case "error", "err":
		return "error"
	case "critical":
		return "critical"
	case "fatal":
		return "fatal"
	default:
		return strings.ToLower(severity)
	}
}

// applyExpr walks the parsed LogQL expression and returns the records
// that survive the pipeline. The pipeline order is:
//
//  1. Stream-selector matchers filter records that lack the matched
//     ResourceAttribute pair.
//  2. Each pipeline stage runs over the surviving records left-to-
//     right. Filter stages drop records; format stages mutate the
//     per-record label set (and, for `| line_format`, the body — not
//     yet implemented).
//
// Returns a fresh slice; callers can mutate the result without
// aliasing back into the dataset.
func applyExpr(expr syntax.Expr, records []property.LogRecord) ([]property.LogRecord, error) {
	switch v := expr.(type) {
	case *syntax.MatchersExpr:
		return applyMatchers(v.Mts, records), nil
	case *syntax.PipelineExpr:
		filtered := applyMatchers(v.Left.Mts, records)
		return applyStages(v.MultiStages, filtered)
	default:
		return nil, fmt.Errorf("oracle/logql: unsupported expression %T (metric-form queries are out of scope for the MVP)", expr)
	}
}

// applyMatchers keeps only records whose ResourceAttributes satisfy
// every matcher. Missing labels match the empty string — same
// convention Loki uses for absent labels.
func applyMatchers(matchers []*labels.Matcher, records []property.LogRecord) []property.LogRecord {
	if len(matchers) == 0 {
		return append([]property.LogRecord(nil), records...)
	}
	out := make([]property.LogRecord, 0, len(records))
	for _, r := range records {
		if !matchesAll(matchers, r.ResourceAttributes) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// matchesAll reports whether attrs satisfies every matcher. An
// absent label in attrs is treated as the empty string (Loki's
// convention).
func matchesAll(matchers []*labels.Matcher, attrs map[string]string) bool {
	for _, m := range matchers {
		val := attrs[m.Name]
		if !m.Matches(val) {
			return false
		}
	}
	return true
}

// applyStages runs each pipeline stage over the records, returning
// the records that survive.
func applyStages(stages syntax.MultiStageExpr, records []property.LogRecord) ([]property.LogRecord, error) {
	current := records
	for _, stage := range stages {
		next, err := applyStage(stage, current)
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

// applyStage dispatches to the per-stage handler. Unsupported stages
// surface as a Loki-shaped error so the property framework treats
// both-side errors as agreement.
func applyStage(stage syntax.StageExpr, records []property.LogRecord) ([]property.LogRecord, error) {
	switch st := stage.(type) {
	case *syntax.LineFilterExpr:
		return applyLineFilter(st, records)
	case *syntax.LabelFmtExpr:
		return applyLabelFmt(st, records), nil
	default:
		return nil, fmt.Errorf("oracle/logql: unsupported pipeline stage %T (generator should not produce this shape)", stage)
	}
}

// applyLineFilter drops records whose Body fails the filter. The
// chain handling mirrors Loki's `LineFilterExpr.Left` walk: older
// chained filters live on `Left` and AND into the head clause; `Or`
// alternates OR with the head clause.
//
// The semantic spec is the upstream Loki source:
//
//	|=  → substring contains, positive
//	!=  → substring contains, negated
//	|~  → regex match, positive
//	!~  → regex match, negated
//
// Substring uses strings.Contains for byte-exact case-sensitivity,
// matching Loki's filter_test.go expectations.
func applyLineFilter(f *syntax.LineFilterExpr, records []property.LogRecord) ([]property.LogRecord, error) {
	pred, err := lineFilterPredicate(f)
	if err != nil {
		return nil, err
	}
	out := make([]property.LogRecord, 0, len(records))
	for _, r := range records {
		if pred(r.Body) {
			out = append(out, r)
		}
	}
	return out, nil
}

// lineFilterPredicate compiles a LineFilterExpr (with optional Left /
// Or chain) into a single line-acceptance predicate.
func lineFilterPredicate(f *syntax.LineFilterExpr) (func(string) bool, error) {
	headPred, err := singleLineFilter(f.LineFilter)
	if err != nil {
		return nil, err
	}
	current := headPred
	// Or alternates: OR with the head clause.
	for or := f.Or; or != nil; or = or.Or {
		next, err := singleLineFilter(or.LineFilter)
		if err != nil {
			return nil, err
		}
		prev := current
		nextPred := next
		current = func(line string) bool {
			return prev(line) || nextPred(line)
		}
	}
	// Older filters in the same pipeline (the `Left` chain): each
	// must also accept the line. The chain is built right-to-left
	// in Loki, so we recurse and AND.
	if f.Left != nil {
		left, err := lineFilterPredicate(f.Left)
		if err != nil {
			return nil, err
		}
		prev := current
		current = func(line string) bool {
			return left(line) && prev(line)
		}
	}
	return current, nil
}

// singleLineFilter compiles one LineFilter into a predicate. The
// `ip(...)` function form is flagged by lf.Op (the parser stores the
// function name there); plain filters dispatch on the match type,
// including the `|>` / `!>` pattern forms.
func singleLineFilter(lf syntax.LineFilter) (func(string) bool, error) {
	if lf.Op == syntax.OpFilterIP {
		return ipLineFilterPredicate(lf.Match, lf.Ty)
	}
	switch lf.Ty {
	case loglib.LineMatchEqual:
		return func(line string) bool { return strings.Contains(line, lf.Match) }, nil
	case loglib.LineMatchNotEqual:
		return func(line string) bool { return !strings.Contains(line, lf.Match) }, nil
	case loglib.LineMatchRegexp:
		re, err := regexp.Compile(lf.Match)
		if err != nil {
			return nil, fmt.Errorf("oracle/logql: compile regex %q: %w", lf.Match, err)
		}
		return func(line string) bool { return re.MatchString(line) }, nil
	case loglib.LineMatchNotRegexp:
		re, err := regexp.Compile(lf.Match)
		if err != nil {
			return nil, fmt.Errorf("oracle/logql: compile regex %q: %w", lf.Match, err)
		}
		return func(line string) bool { return !re.MatchString(line) }, nil
	case loglib.LineMatchPattern:
		return patternLinePredicate(lf.Match, false)
	case loglib.LineMatchNotPattern:
		return patternLinePredicate(lf.Match, true)
	}
	return nil, fmt.Errorf("oracle/logql: unsupported line-match type %s", lf.Ty)
}

// applyLabelFmt applies a `| label_format` stage. Each LabelFmt is
// either a Rename (copy src → dst, drop src if dst != src) or a
// Template (set dst to the rendered Value).
//
// Template support is OUT OF SCOPE for the MVP — the generator only
// produces rename shapes. A template-mode LabelFmt slips through as a
// no-op (preserves the input labels) so future generator widenings
// don't have to coordinate with this oracle.
//
// Rename semantics match Loki's `loglib.LabelsFormatter` exactly:
// missing source labels skip the rename silently; same-name renames
// are no-ops.
func applyLabelFmt(e *syntax.LabelFmtExpr, records []property.LogRecord) []property.LogRecord {
	out := make([]property.LogRecord, 0, len(records))
	for _, r := range records {
		newLabels := copyLabels(r.ResourceAttributes)
		for _, f := range e.Formats {
			if !f.Rename {
				continue // template mode: oracle treats as no-op (MVP)
			}
			if v, ok := newLabels[f.Value]; ok {
				newLabels[f.Name] = v
				if f.Name != f.Value {
					delete(newLabels, f.Value)
				}
			}
		}
		out = append(out, property.LogRecord{
			Body:               r.Body,
			SeverityText:       r.SeverityText,
			ResourceAttributes: newLabels,
			LogAttributes:      r.LogAttributes,
			TimestampNanos:     r.TimestampNanos,
		})
	}
	return out
}

// copyLabels returns a deep-enough copy of in so callers can mutate
// without aliasing into the caller's map.
func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sortedKeys returns m's keys in sorted order. Used for deterministic
// labelKey output in failure logs.
//
// Reserved for future test-helper use; kept here so the oracle's
// helper surface is colocated.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// _ silences the unused-function lint on sortedKeys.
var _ = sortedKeys
