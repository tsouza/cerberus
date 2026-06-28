package loki

import (
	"bytes"
	"encoding/json"
	"regexp"
	"text/template"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/log/pattern"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/prometheus/prometheus/model/labels"

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
//   - `| unpack` — parses the line as a JSON object and merges each
//     string-valued key into the labels map. A special `_entry` key
//     replaces the line, restoring the original payload from
//     Promtail's `pack` stage.
//   - `| pattern "<ip> <_> <method> <path>"` — matches the line against
//     a Loki pattern expression and adds each named capture to the
//     labels map. `<_>` skips a segment.
//   - `| drop foo, bar` / `| drop foo="v"` — remove named labels (or
//     labels whose value matches the matcher) from the output. Applied
//     by handing the labels to upstream Loki's `log.DropLabels.Process`
//     so cerberus inherits its exact semantics (including the
//     special-error-label preservation).
//   - `| keep foo, bar` / `| keep foo="v"` — opposite of drop: keep
//     only the named labels (or labels whose value matches). Also
//     delegates to upstream Loki's `log.KeepLabels.Process`.
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
			// expression's interface type (StageExpr), which is exactly
			// what newLabelProjectionStep wants — Stage() lives on the
			// StageExpr interface.
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
func unpackStep(line string, _ int64, labels map[string]string) (string, map[string]string) {
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

// newLabelProjectionStep implements `| drop` and `| keep` by delegating
// to upstream Loki's `log.Stage` machinery. Both StageExprs expose a
// `Stage()` method that returns the same `log.DropLabels` / `log.KeepLabels`
// implementation Loki itself runs at query time — cerberus inherits the
// exact matcher semantics (including the special-error-label
// preservation and the bare-name vs matcher-form distinction) by
// reusing the upstream Process call. Field access on the unexported
// `dropLabels` / `keepLabels` slice is not needed: we operate on the
// LabelsBuilder shape Loki's Stage expects.
//
// Each invocation builds a fresh LabelsBuilder over the input label
// map, runs Process, and reads the surviving labels back. Labels pass
// through unchanged in shape; only the membership of the output map
// differs from the input. The line is never rewritten.
//
// Returns a FRESH labels map per row so callers can safely treat the
// original sample's labels as immutable, consistent with the other
// label-mutating steps (label_format, unpack, pattern).
func newLabelProjectionStep(stage syntax.StageExpr) (lineTransform, error) {
	st, err := stage.Stage()
	if err != nil {
		return nil, err
	}
	baseBuilder := loglib.NewBaseLabelsBuilder()
	return func(line string, _ int64, in map[string]string) (string, map[string]string) {
		// Convert the input label map into labels.Labels. Order doesn't
		// matter for Drop/Keep — both iterate via UnsortedLabels — but
		// labels.FromMap returns a canonicalised set so the hash is
		// stable across calls with the same content.
		lbs := labels.FromMap(in)
		builder := baseBuilder.ForLabels(lbs, labels.StableHash(lbs))
		builder.Reset()
		// Process returns (modifiedLine, keepRow). Drop/Keep never
		// rewrite the line or reject the row — they only mutate the
		// builder's label set. Discard both return values.
		_, _ = st.Process(0, []byte(line), builder)
		// LabelsResult().Labels() returns the surviving label set after
		// the stage applied. Read it back into a plain map[string]string
		// so the rest of the pipeline (line_format, label_format, …)
		// keeps its map-based contract.
		out := make(map[string]string, len(in))
		builder.LabelsResult().Labels().Range(func(l labels.Label) {
			out[l.Name] = l.Value
		})
		return line, out
	}, nil
}
