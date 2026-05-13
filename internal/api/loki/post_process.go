package loki

import (
	"bytes"
	"encoding/json"
	"regexp"
	"text/template"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/log/pattern"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
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
//   - `| unpack` — parses the line as a JSON object and merges each
//     string-valued key into the labels map. A special `_entry` key
//     replaces the line, restoring the original payload from
//     Promtail's `pack` stage.
//   - `| pattern "<ip> <_> <method> <path>"` — matches the line against
//     a Loki pattern expression and adds each named capture to the
//     labels map. `<_>` skips a segment.
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
				steps = append(steps, unpackStep)
			case syntax.OpParserTypePattern:
				step, err := newPatternStep(v.Param)
				if err != nil {
					return nil, err
				}
				steps = append(steps, step)
			}
		}
	}

	if len(steps) == 0 {
		return nil, nil
	}
	return composeTransforms(steps), nil
}

// lineTransform is the per-row transform shape: takes the current
// line + the stream's labels and returns the new line + new labels.
// Transforms that don't modify labels (line_format, decolorize)
// return the input map reference unchanged; transforms that DO
// modify labels (label_format) return a fresh map so callers can
// safely treat the original sample's labels as immutable.
type lineTransform func(line string, labels map[string]string) (string, map[string]string)

// composeTransforms left-to-right composes the per-stage transforms
// so the next stage sees the previous stage's output line AND output
// labels. A `| label_format` followed by a `| line_format` template
// thus sees the renamed labels in the template's dot map.
func composeTransforms(steps []lineTransform) lineTransform {
	if len(steps) == 1 {
		return steps[0]
	}
	return func(line string, labels map[string]string) (string, map[string]string) {
		for _, s := range steps {
			line, labels = s(line, labels)
		}
		return line, labels
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
	var currentLine string
	funcs := templateFuncs()
	funcs["__line__"] = func() string { return currentLine }
	// __timestamp__ stub — Loki exposes this as a func too. Not wired
	// through Sample.Timestamp yet; revisit if a user template
	// references it.
	funcs["__timestamp__"] = func() string { return "" }
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
	return func(line string, labels map[string]string) (string, map[string]string) {
		currentLine = line
		ctx := make(map[string]any, len(labels))
		for k, v := range labels {
			ctx[k] = v
		}
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, ctx); err != nil {
			return line, labels
		}
		return buf.String(), labels
	}, nil
}

// decolorizeStep strips ANSI escape sequences from each line. Matches
// Loki's `| decolorize` semantics. Labels pass through unchanged.
func decolorizeStep(line string, labels map[string]string) (string, map[string]string) {
	return ansiEscape.ReplaceAllString(line, ""), labels
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
func newLabelFormatStep(formats []loglib.LabelFmt) (lineTransform, error) {
	// Pre-parse all template Formats so per-row execution is cheap.
	type compiled struct {
		dst    string
		src    string
		rename bool
		tpl    *template.Template
	}
	steps := make([]compiled, 0, len(formats))
	for _, f := range formats {
		c := compiled{dst: f.Name, src: f.Value, rename: f.Rename}
		if !f.Rename {
			// Loki uses `Option("missingkey=zero")` so a missing label
			// renders as `<no value>`; cerberus mirrors that — silent
			// rather than error, same as line_format.
			tpl, err := template.New("label_format").
				Option("missingkey=zero").
				Funcs(templateFuncs()).
				Parse(f.Value) //nolint:gosec // G708: user-template input is the feature
			if err != nil {
				return nil, err
			}
			c.tpl = tpl
		}
		steps = append(steps, c)
	}
	return func(line string, labels map[string]string) (string, map[string]string) {
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
		return line, out
	}, nil
}

// unpackStep implements `| unpack`. The line is expected to be a JSON
// object emitted by Promtail's `pack` stage: each string-valued key
// becomes a label, and the special `_entry` key replaces the line.
//
// Non-object payloads and JSON-decode errors leave the line and labels
// unchanged — matching Loki's silent-on-malformed-JSON contract.
// Non-string values (numbers, arrays, nested objects) are skipped at
// the label level but the `_entry` rewrite still applies.
//
// Returns a FRESH labels map so callers can treat the input as
// immutable, consistent with newLabelFormatStep.
func unpackStep(line string, labels map[string]string) (string, map[string]string) {
	if len(line) == 0 || line[0] != '{' {
		return line, labels
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return line, labels
	}
	out := make(map[string]string, len(labels)+len(raw))
	for k, v := range labels {
		out[k] = v
	}
	newLine := line
	for k, v := range raw {
		// Skip non-string values (Loki's unpack only extracts strings).
		if len(v) == 0 || v[0] != '"' {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		if k == logqlmodel.PackedEntryKey {
			newLine = s
			continue
		}
		// Don't shadow stream labels — Loki appends a duplicate suffix
		// in that case. Mirror that behavior so cerberus's row output
		// matches Loki's.
		if _, ok := labels[k]; ok {
			out[k+duplicateSuffix] = s
			continue
		}
		out[k] = s
	}
	return newLine, out
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
	m, err := pattern.New(p)
	if err != nil {
		return nil, err
	}
	names := m.Names()
	return func(line string, lbs map[string]string) (string, map[string]string) {
		caps := m.Matches([]byte(line))
		if len(caps) == 0 {
			return line, lbs
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
		return line, out
	}, nil
}
