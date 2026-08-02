package loki

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"text/template"

	"github.com/buger/jsonparser"

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
//   - `| unpack` — parses the line as a JSON object emitted by
//     Promtail's `pack` stage. The special `_entry` key replaces the
//     line with the original payload, and the remaining string-valued
//     keys are merged into the labels map — but only if `_entry` was
//     present. A payload that is not a JSON object, or that fails to
//     parse, stamps the `__error__` / `__error_details__` pair.
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
		case *syntax.DropLabelsExpr, *syntax.KeepLabelsExpr:
			// In a multi-case type switch v keeps the type-switch
			// expression's interface type (StageExpr); newLabelProjectionStep
			// re-asserts the concrete drop/keep type to read its matchers.
			step, err := newLabelProjectionStep(v)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
		}
	}

	if len(steps) == 0 {
		return nil, nil
	}
	return composeTransforms(steps), nil
}

// lineTransform is the per-row transform shape: takes the current
// line, the row's nanosecond timestamp, and the stream's labels and
// returns the new line + new labels. The timestamp is threaded through
// so `| line_format` / `| label_format` templates can expose
// `{{__timestamp__}}` (as a time.Time, matching Loki's
// AddLineAndTimestampFunctions). Transforms that don't read the
// timestamp ignore it.
//
// Transforms that don't modify labels (line_format, decolorize)
// return the input map reference unchanged; transforms that DO
// modify labels (label_format) return a fresh map so callers can
// safely treat the original sample's labels as immutable.
type lineTransform func(line string, ts int64, labels map[string]string) (string, map[string]string)

// composeTransforms left-to-right composes the per-stage transforms
// so the next stage sees the previous stage's output line AND output
// labels. A `| label_format` followed by a `| line_format` template
// thus sees the renamed labels in the template's dot map.
func composeTransforms(steps []lineTransform) lineTransform {
	if len(steps) == 1 {
		return steps[0]
	}
	return func(line string, ts int64, labels map[string]string) (string, map[string]string) {
		for _, s := range steps {
			line, labels = s(line, ts, labels)
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
	return func(line string, ts int64, labels map[string]string) (string, map[string]string) {
		currentLine = line
		currentTs = ts
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
func decolorizeStep(line string, _ int64, labels map[string]string) (string, map[string]string) {
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
	return func(line string, ts int64, labels map[string]string) (string, map[string]string) {
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
		return line, out
	}, nil
}

// errUnexpectedJSONObject reproduces Loki's sentinel byte for byte. The
// text reaches callers through `__error_details__`, so it is part of the
// wire contract rather than an internal diagnostic: upstream builds it as
// `fmt.Errorf("expecting json object(%d), but it is not", jsoniter.ObjectValue)`
// and json-iterator's ObjectValue ordinal is 6.
var errUnexpectedJSONObject = errors.New("expecting json object(6), but it is not")

// jsonParserErr is the `__error__` value Loki stamps for every failure of
// a JSON-family parser stage, `| unpack` included.
const jsonParserErr = "JSONParserErr"

// unpackStep implements `| unpack`. The line is expected to be a JSON
// object emitted by Promtail's `pack` stage: each string-valued key
// becomes a label, and the special `_entry` key replaces the line.
//
// Three rules here are easy to get backwards, and all three are what
// Loki's UnpackParser actually does (pkg/logql/log/parser.go):
//
//   - A payload that is not a JSON object, or one that fails to parse,
//     is an ERROR, not a silent pass-through. Loki stamps
//     `__error__="JSONParserErr"` plus an `__error_details__` message and
//     returns the ORIGINAL line. Swallowing it inverts the meaning of a
//     `| __error__=""` filter, which is the usual way callers ask for
//     "only lines that parsed" (issue #1447).
//   - Labels are collected into a buffer and flushed only if a top-level
//     string `_entry` key was found. A well-formed object without one is
//     a complete no-op: original line, and NO extracted labels.
//   - Non-string values (numbers, booleans, arrays, nested objects) are
//     skipped, and an empty line is not an error.
//
// Returns a FRESH labels map so callers can treat the input as
// immutable, consistent with newLabelFormatStep.
func unpackStep(line string, _ int64, labels map[string]string) (string, map[string]string) {
	if len(line) == 0 {
		return line, labels
	}
	if line[0] != '{' {
		return line, withParserError(labels, errUnexpectedJSONObject)
	}

	// Buffered as alternating key/value pairs so nothing is committed
	// before the `_entry` gate below, matching upstream's lbsBuffer.
	var buf []string
	newLine := line
	isPacked := false
	err := jsonparser.ObjectEach([]byte(line),
		func(key, value []byte, typ jsonparser.ValueType, _ int) error {
			if typ != jsonparser.String {
				return nil
			}
			k := string(key)
			if k == syntax.PackedEntryKey {
				var stackbuf [unescapeStackBufSize]byte
				unescaped, uerr := jsonparser.Unescape(value, stackbuf[:])
				if uerr != nil {
					return uerr
				}
				newLine = string(unescaped)
				isPacked = true
				return nil
			}
			// Don't shadow a stream label — Loki appends a duplicate
			// suffix to the RAW key, then sanitises the result.
			if _, ok := labels[k]; ok {
				k += duplicateSuffix
			}
			buf = append(buf, sanitizeLabelKey(k), unescapeJSONString(value))
			return nil
		})
	if err != nil {
		return line, withParserError(labels, err)
	}
	if !isPacked {
		return line, labels
	}

	out := make(map[string]string, len(labels)+len(buf)/2)
	for k, v := range labels {
		out[k] = v
	}
	for i := 0; i < len(buf); i += 2 {
		out[buf[i]] = buf[i+1]
	}
	return newLine, out
}

// withParserError returns a copy of labels carrying the `__error__` /
// `__error_details__` pair Loki's addErrLabel stamps. The copy keeps the
// step's caller-immutable contract; the extracted labels are deliberately
// dropped, because upstream discards its buffer on the error path.
func withParserError(labels map[string]string, err error) map[string]string {
	out := make(map[string]string, len(labels)+2)
	for k, v := range labels {
		out[k] = v
	}
	out[syntax.ErrorLabel] = jsonParserErr
	out[syntax.ErrorDetailsLabel] = err.Error()
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
	return func(line string, _ int64, lbs map[string]string) (string, map[string]string) {
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
		return func(line string, _ int64, in map[string]string) (string, map[string]string) {
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
			return line, out
		}, nil
	case *syntax.KeepLabelsExpr:
		matchers := s.Matchers()
		return func(line string, _ int64, in map[string]string) (string, map[string]string) {
			out := copyLabelMap(in)
			if len(matchers) == 0 {
				return line, out
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
			return line, out
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
