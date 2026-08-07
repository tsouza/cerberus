package loki

import (
	"bytes"
	"fmt"
	"regexp"
	"text/template"

	"github.com/buger/jsonparser"

	"github.com/tsouza/cerberus/internal/logql"
	logpattern "github.com/tsouza/cerberus/internal/logql/logpattern"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// postProcessExtract walks the parsed LogQL expression and pulls out
// the post-fetch transforms cerberus applies in Go after the SQL
// rows return:
//
//   - `| line_format "<tpl>"` — Go text/template; receives the
//     stream's labels as `.<label>` and exposes the current line as
//     `{{__line__}}` (a parameterless template func, matching Loki's
//     own templating contract). Composed left-to-right (the rightmost
//     line_format sees the output of the previous one).
//   - `| decolorize` — strip ANSI escape codes from each line.
//   - `| label_format new=old, lvl=` + "`" + `{{.severity}}` + "`" — rename and/or
//     template-set labels on the row. Subsequent line_format stages
//     see the updated label map; the streams response groups rows by
//     the final (post-format) label set.
//   - `| unpack` — only the `__error_details__` text for a `{`-prefixed
//     payload the Go JSON reader rejects (see [unpackParseDetailStep]).
//     The stage's labels, its `_entry` line replacement and its
//     `__error__` marker are all computed in SQL by the lowering's
//     unpackMergeLabels / unpackLineExpr.
//   - `| pattern "<ip> <_> <method> <path>"` — matches the line against
//     a Loki pattern expression and adds each named capture to the
//     labels map. `<_>` skips a segment.
//   - `| drop foo, bar` / `| drop foo="v"` — remove named labels (or
//     labels whose value matches the matcher) from the output, as map
//     operations reproducing Loki's DropLabels semantics.
//   - `| keep foo, bar` / `| keep foo="v"` — opposite of drop: keep
//     only the named labels (or labels whose value matches), reproducing
//     Loki's KeepLabels semantics (special error labels always kept).
//
// Lowering already returns nil-predicate no-ops for these stages so
// the SQL doesn't try to model them. Returns a transform that maps
// each (line, labels) → (line', labels'). Nil return means "no
// transform" — the caller can skip applying it and use sample's
// original labels.
func postProcessExtract(expr syntax.Expr) (lineTransform, error) {
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		return nil, nil // log-stream queries with no pipeline (rare)
	}

	var steps []lineTransform
	// dynamicLabels mirrors internal/logql/lower.go's
	// lowerPipelineWithLabels gate: once a `| pattern` stage runs, the
	// lowering skips SQL predicate generation for any downstream
	// LabelFilterExpr that tests the `__error__` / `__error_details__`
	// family specifically (see that function's doc comment for why — the
	// structured-metadata SQL fallback used for ordinary label names is
	// actively wrong for these two magic keys). Such filters have to be
	// applied here instead, once the dynamic stage's own step has
	// actually computed them.
	dynamicLabels := false
	for _, st := range pipe.MultiStages {
		switch v := st.(type) {
		case *syntax.LineFmtExpr:
			step, err := newLineFormatStep(v.Value)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
		case *syntax.DecolorizeExpr:
			steps = append(steps, decolorizeStep)
		case *syntax.LabelFmtExpr:
			step, err := newLabelFormatStep(v.Formats)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
		case *syntax.LineParserExpr:
			switch v.Op {
			case syntax.OpParserTypeUnpack:
				steps = append(steps, unpackParseDetailStep)
			case syntax.OpParserTypePattern:
				step, err := newPatternStep(v.Param)
				if err != nil {
					return nil, err
				}
				steps = append(steps, step)
				dynamicLabels = true
			}
		case *syntax.DropLabelsExpr, *syntax.KeepLabelsExpr:
			// In a multi-case type switch v keeps the type-switch
			// expression's interface type (StageExpr); newLabelProjectionStep
			// re-asserts the concrete drop/keep type to read its matchers.
			step, err := newLabelProjectionStep(v)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
		case *syntax.LabelFilterExpr:
			if dynamicLabels && logql.FiltersErrorLabel(v.LabelFilterer) {
				steps = append(steps, newLabelFilterStep(v.LabelFilterer))
			}
			// Every other label filter — one preceding any dynamic-label
			// stage, or one following it but testing an ordinary label
			// name — already has its SQL predicate applied by the
			// lowering (see internal/logql/lower.go's
			// lowerPipelineWithLabels); re-running it here would be
			// redundant (and, for the mark-stamping numeric/duration/
			// bytes kinds, would double-stamp `__error__` against the
			// labels map post-parse-stage).
		}
	}

	if len(steps) == 0 {
		return nil, nil
	}
	return composeTransforms(steps), nil
}

// newLabelFilterStep builds the Go-side transform for a `| label op
// value` filter that follows a `| pattern` stage (see
// postProcessExtract's dynamicLabels gate). internal/logql/lower.go's
// lowerPipelineWithLabels skips SQL lowering for these filters
// entirely — the labels they test may only exist once the preceding
// dynamic stage's own step (newPatternStep's closure, which runs
// earlier in the same composed pipeline) has produced them — so this
// is the only place they're actually evaluated.
//
// Returns keep == false to drop the row; [composeTransforms]
// short-circuits the remaining steps when that happens.
func newLabelFilterStep(lf syntax.LabelFilterer) lineTransform {
	return func(line string, _ int64, labels map[string]string) (string, map[string]string, bool) {
		keep, labels := labelFiltererEval(lf, labels)
		return line, labels, keep
	}
}

// lineTransform is the per-row transform shape: takes the current
// line, the row's nanosecond timestamp, and the stream's labels and
// returns the new line + new labels + whether the row survives (false
// means "drop this row entirely" — see [newLabelFilterStep], the only
// step that ever returns false; every other step keeps every row). The
// timestamp is threaded through so `| line_format` / `| label_format`
// templates can expose `{{__timestamp__}}` (as a time.Time, matching
// Loki's AddLineAndTimestampFunctions). Transforms that don't read the
// timestamp ignore it.
//
// Transforms that don't modify labels (line_format, decolorize)
// return the input map reference unchanged; transforms that DO
// modify labels (label_format) return a fresh map so callers can
// safely treat the original sample's labels as immutable.
type lineTransform func(line string, ts int64, labels map[string]string) (string, map[string]string, bool)

// composeTransforms left-to-right composes the per-stage transforms
// so the next stage sees the previous stage's output line AND output
// labels. A `| label_format` followed by a `| line_format` template
// thus sees the renamed labels in the template's dot map. A step that
// drops the row (keep == false) short-circuits the remaining steps —
// there's no row left for them to act on.
func composeTransforms(steps []lineTransform) lineTransform {
	if len(steps) == 1 {
		return steps[0]
	}
	return func(line string, ts int64, labels map[string]string) (string, map[string]string, bool) {
		var keep bool
		for _, s := range steps {
			line, labels, keep = s(line, ts, labels)
			if !keep {
				return line, labels, false
			}
		}
		return line, labels, true
	}
}

// newLineFormatStep parses a `| line_format` template and returns a
// per-row transform. The template can reference labels as `.<name>`
// and the current line via the parameterless `{{__line__}}` function
// — Loki's contract.
//
// On a runtime template error (e.g., a referenced label is missing)
// the transform returns the input line unchanged — matching Loki's
// silent-fallback semantics. Parse-time errors surface as a query
// error so the user knows their template is broken.
//
// The returned closure captures `currentLine` so `{{__line__}}` can
// read the line for each call. The transform is single-goroutine by
// construction (postProcessExtract returns a fresh transform per
// request, and toStreamsWithTransform applies it sequentially over
// samples), so no synchronization is needed. Labels pass through
// unchanged.
func newLineFormatStep(src string) (lineTransform, error) {
	var (
		currentLine string
		currentTs   int64
	)
	// AddLineAndTimestampFunctions returns the FULL Loki funcmap (sprig
	// allow-list + Loki-native funcs) with `__line__` / `__timestamp__`
	// bound to the capture closures. `__timestamp__` returns a
	// time.Time(time.Unix(0, ns)) so `{{ __timestamp__ | date "..." }}`
	// stays at parity.
	funcs := templateFuncs(
		func() string { return currentLine },
		func() int64 { return currentTs },
	)
	// Parsing a user-supplied template is the documented contract for
	// `| line_format` — Loki accepts the same input and we mirror its
	// semantics. The template runs against the streams response (label
	// values + line text) only, never against server state. The
	// per-execution funcmap above and the empty default context bound
	// `{{...}}` access to the request payload.
	tpl, err := template.New("line_format").Funcs(funcs).Parse(src) //nolint:gosec // G708: user-template input is the feature
	if err != nil {
		return nil, err
	}
	return func(line string, ts int64, labels map[string]string) (string, map[string]string, bool) {
		currentLine = line
		currentTs = ts
		ctx := make(map[string]any, len(labels))
		for k, v := range labels {
			ctx[k] = v
		}
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, ctx); err != nil {
			return line, labels, true
		}
		return buf.String(), labels, true
	}, nil
}

// decolorizeStep strips ANSI escape sequences from each line. Matches
// Loki's `| decolorize` semantics. Labels pass through unchanged.
func decolorizeStep(line string, _ int64, labels map[string]string) (string, map[string]string, bool) {
	return ansiEscape.ReplaceAllString(line, ""), labels, true
}

// ansiEscape matches CSI (Control Sequence Introducer) sequences —
// the most common form: `ESC [ <params> <intermediate> <final>`. Loki
// uses a similar regex (see github.com/grafana/loki/pkg/logql/log).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// newLabelFormatStep returns the transform for a single `| label_format`
// stage. Each `LabelFmt` is either a Rename (copy old→new, drop old) or a
// Template (set Name to the rendered Value template).
//
// Renames where source doesn't exist are silently skipped — matches
// Loki's `lbs.GetWithCategory` early-return path. Renames where Name
// equals Value are no-ops.
//
// Template errors at execute-time are swallowed (the target label is
// left unset); Loki's own implementation sets an error label, but
// cerberus mirrors the silent semantics it uses for line_format.
// Parse-time errors surface as a query error.
//
// Returns a FRESH labels map per row so callers can safely treat the
// original sample's labels as immutable (a shared reference from a
// previous step is also fine — we always allocate).
func newLabelFormatStep(formats []syntax.LabelFmt) (lineTransform, error) {
	// Pre-parse all template Formats so per-row execution is cheap.
	type compiled struct {
		dst    string
		src    string
		rename bool
		tpl    *template.Template
	}
	// Capture closures shared across every template Format in this
	// stage, updated per-row before Execute so `__line__` /
	// `__timestamp__` read the current sample — matching line_format and
	// upstream Loki's LabelsFormatter.
	var (
		currentLine string
		currentTs   int64
	)
	funcs := templateFuncs(
		func() string { return currentLine },
		func() int64 { return currentTs },
	)
	steps := make([]compiled, 0, len(formats))
	for _, f := range formats {
		c := compiled{dst: f.Name, src: f.Value, rename: f.Rename}
		if !f.Rename {
			// Loki uses `Option("missingkey=zero")` so a missing label
			// renders as `<no value>`; cerberus mirrors that — silent
			// rather than error, same as line_format.
			tpl, err := template.New("label_format").
				Option("missingkey=zero").
				Funcs(funcs).
				Parse(f.Value) //nolint:gosec // G708: user-template input is the feature
			if err != nil {
				return nil, err
			}
			c.tpl = tpl
		}
		steps = append(steps, c)
	}
	return func(line string, ts int64, labels map[string]string) (string, map[string]string, bool) {
		currentLine = line
		currentTs = ts
		// Copy the input labels into a fresh map; mutations stay scoped
		// to this row's result.
		out := make(map[string]string, len(labels))
		for k, v := range labels {
			out[k] = v
		}
		// Build a template context (map[string]any) once per row from
		// the *input* labels — Loki templates see the pre-format label
		// set, matching their `lbs.IntoMap(m)` pattern.
		var ctx map[string]any
		for _, c := range steps {
			if c.rename {
				if v, ok := out[c.src]; ok {
					out[c.dst] = v
					if c.dst != c.src {
						delete(out, c.src)
					}
				}
				continue
			}
			if ctx == nil {
				ctx = make(map[string]any, len(labels))
				for k, v := range labels {
					ctx[k] = v
				}
			}
			var buf bytes.Buffer
			if err := c.tpl.Execute(&buf, ctx); err != nil {
				continue
			}
			out[c.dst] = buf.String()
		}
		return line, out, true
	}, nil
}

// unpackParseDetailStep supplies the one part of `| unpack` that cannot
// be computed in SQL: the `__error_details__` text for a `{`-prefixed
// payload the Go JSON reader rejects.
//
// Everything else about the stage — which members become labels, the
// `_entry` gate that arms the extraction, the `_entry` line replacement,
// and the `__error__` marker itself — is modelled in the lowering
// (internal/logql/lower.go's unpackMergeLabels / unpackLineExpr), so it
// reaches metric-mode aggregation, which never runs a Go-side pass at
// all. This step deliberately does NOT recompute any of it: a second
// answer to "what does unpack extract" is exactly how the SQL side and
// the Go side drifted apart in the first place.
//
// The detail text is buger/jsonparser's own parse-position message —
// `{"a":"b"` yields "Value is array, but can't find closing ']' symbol",
// `{` yields "Malformed JSON error" — so it is a property of that
// reader's state machine rather than of the input, and no ClickHouse
// expression derives it. The SQL side stamps `__error__` for the same
// row and leaves the detail empty; this step fills it in on the paths
// that have a per-row Go pass.
//
// Returns a FRESH labels map so callers can treat the input as
// immutable, consistent with newLabelFormatStep.
func unpackParseDetailStep(line string, _ int64, labels map[string]string) (string, map[string]string, bool) {
	if labels[syntax.ErrorLabel] != logql.JSONParserErrValue || labels[syntax.ErrorDetailsLabel] != "" {
		return line, labels, true
	}
	err := jsonparser.ObjectEach([]byte(line),
		func(_, _ []byte, _ jsonparser.ValueType, _ int) error { return nil })
	if err == nil {
		// ClickHouse's isValidJSON is stricter than jsonparser — it
		// rejects a trailing comma that jsonparser accepts — so the SQL
		// side can mark a row this reader parses happily. Upstream's
		// verdict is this reader's, so the marker comes back off.
		return line, withoutParserError(labels), true
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	out[syntax.ErrorDetailsLabel] = err.Error()
	return line, out, true
}

// withoutParserError returns a copy of labels with the JSON parser's
// error pair removed.
func withoutParserError(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k == syntax.ErrorLabel || k == syntax.ErrorDetailsLabel {
			continue
		}
		out[k] = v
	}
	return out
}

// duplicateSuffix matches Loki's `_extracted` suffix appended to
// parser-extracted labels that would otherwise shadow a stream label.
// See `loglib.duplicateSuffix` (unexported, kept in sync by name).
const duplicateSuffix = "_extracted"

// newPatternStep implements `| pattern "<ip> <_> <method> <path>"`.
// The pattern parser is taken straight from upstream Loki so cerberus
// matches Loki's named-capture semantics (including `<_>` skips and
// the trailing-anchor / inter-literal boundaries).
//
// Each named capture is added to the labels map. Captures that would
// shadow a stream label get the `_extracted` suffix, mirroring Loki's
// disambiguation contract.
//
// A pattern that fails to match (Matches returns nil) leaves the line
// and labels unchanged — Loki's silent-fallback semantics.
func newPatternStep(p string) (lineTransform, error) {
	m, err := logpattern.New(p)
	if err != nil {
		return nil, err
	}
	names := m.Names()
	return func(line string, _ int64, lbs map[string]string) (string, map[string]string, bool) {
		caps := m.Matches([]byte(line))
		if len(caps) == 0 {
			return line, lbs, true
		}
		out := make(map[string]string, len(lbs)+len(names))
		for k, v := range lbs {
			out[k] = v
		}
		for i, c := range caps {
			if i >= len(names) {
				break
			}
			name := names[i]
			if _, ok := lbs[name]; ok {
				name += duplicateSuffix
			}
			out[name] = string(c)
		}
		return line, out, true
	}, nil
}

// newLabelProjectionStep implements `| drop` and `| keep` as map
// operations over a row's label set, reproducing upstream Loki's
// DropLabels / KeepLabels semantics (pkg/logql/log/{drop,keep}_labels.go):
//
//   - `| drop`: a bare entry deletes the named label; a matcher entry
//     (`| drop x="v"`) deletes the label only when it is present and its
//     value matches. The `__error__` / `__error_details__` keys are
//     ordinary map keys here, so the same rules apply to them.
//   - `| keep`: an empty keep list keeps everything; otherwise every
//     non-special label is dropped unless it matches a keep entry (bare
//     name, or matcher name+value). The special `__error__` family is
//     always retained.
//
// Each invocation returns a FRESH labels map so callers can treat the
// original sample's labels as immutable, consistent with the other
// label-mutating steps. The line is never rewritten.
func newLabelProjectionStep(stage syntax.StageExpr) (lineTransform, error) {
	switch s := stage.(type) {
	case *syntax.DropLabelsExpr:
		matchers := s.Matchers()
		return func(line string, _ int64, in map[string]string) (string, map[string]string, bool) {
			out := copyLabelMap(in)
			for _, d := range matchers {
				if d.Matcher != nil {
					if v, ok := out[d.Matcher.Name]; ok && d.Matcher.Matches(v) {
						delete(out, d.Matcher.Name)
					}
					continue
				}
				delete(out, d.Name)
			}
			return line, out, true
		}, nil
	case *syntax.KeepLabelsExpr:
		matchers := s.Matchers()
		return func(line string, _ int64, in map[string]string) (string, map[string]string, bool) {
			out := copyLabelMap(in)
			if len(matchers) == 0 {
				return line, out, true
			}
			for name, val := range in {
				if isSpecialLabel(name) {
					continue
				}
				keep := false
				for _, k := range matchers {
					if k.Matcher != nil && k.Matcher.Name == name && k.Matcher.Matches(val) {
						keep = true
						break
					}
					if k.Name == name {
						keep = true
						break
					}
				}
				if !keep {
					delete(out, name)
				}
			}
			return line, out, true
		}, nil
	default:
		return nil, fmt.Errorf("loki: unsupported label projection stage %T", stage)
	}
}

// isSpecialLabel reports whether name is one of Loki's reserved error
// labels, which `| keep` always retains.
func isSpecialLabel(name string) bool {
	switch name {
	case syntax.ErrorLabel, syntax.ErrorDetailsLabel, syntax.PreserveErrorLabel:
		return true
	}
	return false
}

// copyLabelMap returns a shallow copy of in so per-row mutations stay
// scoped to the result.
func copyLabelMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
