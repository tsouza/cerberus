package loki

import (
	"bytes"
	"regexp"
	"text/template"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

// postProcessExtract walks the parsed LogQL expression and pulls out
// the post-fetch transforms cerberus applies in Go after the SQL
// rows return:
//
//   - `| line_format "<tpl>"` — Go text/template; receives the
//     stream's labels as `.<label>` and the original line as
//     `__line__`. Composed left-to-right (the rightmost line_format
//     sees the output of the previous one).
//   - `| decolorize` — strip ANSI escape codes from each line.
//
// Lowering already returns nil-predicate no-ops for these stages so
// the SQL doesn't try to model them. Returns a transform that maps
// each (line, labels) → new line. Nil return means "no transform"
// — the caller can skip applying it.
func postProcessExtract(expr syntax.Expr) (lineTransform, error) {
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		return nil, nil // log-stream queries with no pipeline (rare)
	}

	var steps []lineTransform
	for _, st := range pipe.MultiStages {
		switch v := st.(type) {
		case *syntax.LineFmtExpr:
			tpl, err := template.New("line_format").
				Funcs(lineFormatFuncs).
				Parse(v.Value)
			if err != nil {
				return nil, err
			}
			steps = append(steps, lineFormatStep(tpl))
		case *syntax.DecolorizeExpr:
			steps = append(steps, decolorizeStep)
		}
	}

	if len(steps) == 0 {
		return nil, nil
	}
	return composeTransforms(steps), nil
}

// lineTransform is the per-line transform shape: takes the current
// line + the stream's labels and returns the new line.
type lineTransform func(line string, labels map[string]string) string

// composeTransforms left-to-right composes the per-stage transforms
// so the next stage sees the previous stage's output line.
func composeTransforms(steps []lineTransform) lineTransform {
	if len(steps) == 1 {
		return steps[0]
	}
	return func(line string, labels map[string]string) string {
		for _, s := range steps {
			line = s(line, labels)
		}
		return line
	}
}

// lineFormatStep returns the transform for a single `| line_format`
// template. Each call merges the stream's labels into the template
// dot value and injects the current line as `__line__`. On a runtime
// template error (e.g., a referenced label is missing) the function
// returns the input line unchanged — silently failing matches Loki's
// own behaviour.
func lineFormatStep(tpl *template.Template) lineTransform {
	return func(line string, labels map[string]string) string {
		ctx := make(map[string]any, len(labels)+1)
		for k, v := range labels {
			ctx[k] = v
		}
		ctx["__line__"] = line

		var buf bytes.Buffer
		if err := tpl.Execute(&buf, ctx); err != nil {
			return line
		}
		return buf.String()
	}
}

// decolorizeStep strips ANSI escape sequences from each line. Matches
// Loki's `| decolorize` semantics.
func decolorizeStep(line string, _ map[string]string) string {
	return ansiEscape.ReplaceAllString(line, "")
}

// ansiEscape matches CSI (Control Sequence Introducer) sequences —
// the most common form: `ESC [ <params> <intermediate> <final>`. Loki
// uses a similar regex (see github.com/grafana/loki/pkg/logql/log).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// lineFormatFuncs are the template funcs cerberus exposes inside
// `| line_format`. A subset of Loki's full set; the rest fail at
// template-parse time with a clear error pointing the user at the
// gap.
//
// Currently exposed: none (just label-access via `.<name>`). Loki
// also offers `regexReplaceAll`, `lower`, `upper`, `trim`, etc. —
// add as use-cases surface. Failing at parse-time is intentional:
// silent omission would render garbage lines.
var lineFormatFuncs = template.FuncMap{}
