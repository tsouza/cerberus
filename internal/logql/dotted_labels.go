package logql

import "strings"

// NormalizeDottedLabels is the exported entry point for the LogQL
// stream-selector dotted-label rewrite. Handlers OUTSIDE the logql
// package (e.g. internal/api/loki/index_stats.go's selectorMatchers,
// internal/api/loki/tail.go's stream parser) call this before handing
// the query string to syntax.ParseExpr. Inside the logql package,
// Lang.Parse routes through the package-private form for symmetry
// with the PromQL head.
func NormalizeDottedLabels(q string) string {
	return normalizeLokiDottedLabels(q)
}

// normalizeLokiDottedLabels rewrites OTel-style dotted label names in
// LogQL stream selectors to the underscore form so the upstream LogQL
// parser — which restricts label keys to `[a-zA-Z_][a-zA-Z0-9_]*` —
// accepts them.
//
// Example: `{service.name="api"}` becomes `{service_name="api"}`.
//
// Why this is needed: OpenTelemetry's semantic conventions emit dotted
// resource attribute names (`service.name`, `k8s.pod.name`,
// `http.method`, …). Reference Loki has no syntactic support for
// dotted identifiers in stream-selector position; a query like
// `{service.name="api"}` is rejected with `parse error … syntax error:
// unexpected '.'`. Grafana's Loki datasource happily round-trips dotted
// resource-attribute names through cerberus's `/loki/api/v1/labels`
// endpoint (the OTel-CH schema mirrors both the dotted and underscored
// form on every event — see the `dropOTelDottedLabels` response-shaper
// in internal/api/loki/handler.go), so without this rewrite a user can
// SEE the dotted attribute in the label picker, click it, and watch
// the query 400-parse-error.
//
// The rewrite is conservative: only the identifier-position tokens
// inside `{ … }` braces are touched. String literals (`"…"`, `'…'`,
// backticks), label values, pipeline stages, and the surrounding LogQL
// expression are passed through verbatim. The transformation is the
// same `'.' → '_'` map the OTel-CH receiver applies when it writes
// the underscored sibling onto each row, so the rewritten matcher
// targets the same column the schema already mirrors.
//
// The walker tracks:
//   - whether we're inside `"…"` / `'…'` / “ `…` “ strings (skip
//     rewrites so a string literal `"a.b.c"` isn't molested)
//   - whether we're inside a `{ … }` stream-selector group (rewrites
//     only fire here — function names, range durations, pipeline
//     stages, etc. are passed through)
//   - whether the current rune-position is a label-key start (after
//     `{` or `,`, possibly with surrounding whitespace, BEFORE the
//     operator)
//
// At a label-key start position we greedy-consume
// `[a-zA-Z_]([a-zA-Z0-9_]|\.)*` and `'.' → '_'` the consumed token.
// Tokens without a `.` are passed through unchanged, matching the
// underscored canonical form Loki users already type.
//
// Pipeline-stage label keys (e.g. `| service.name="api"` after a
// `| label_format` stage) are NOT touched — the rewrite scope ends at
// the closing `}` of the stream selector. Cerberus's
// schema-stored attributes only need rewriting at the selector layer;
// pipeline stages reference parsed labels that already exist in the
// row's label-set in normalised form.
func normalizeLokiDottedLabels(q string) string {
	if !strings.ContainsRune(q, '.') {
		return q
	}
	var out strings.Builder
	out.Grow(len(q) + 16)

	state := lokiOutside
	depth := 0       // `{` nesting depth — rewrite only fires when depth > 0
	keyStart := true // next non-space rune at this position starts a label key

	i := 0
	for i < len(q) {
		ch := q[i]
		if state != lokiOutside {
			i, state = lokiAdvanceInString(&out, q, i, state)
			keyStart = false
			continue
		}

		if next, ok := lokiOpenString(ch); ok {
			state = next
			out.WriteByte(ch)
			i++
			keyStart = false
			continue
		}

		switch ch {
		case '{':
			out.WriteByte(ch)
			depth++
			keyStart = true
			i++
			continue
		case '}':
			out.WriteByte(ch)
			if depth > 0 {
				depth--
			}
			keyStart = false
			i++
			continue
		case ',':
			out.WriteByte(ch)
			keyStart = depth > 0
			i++
			continue
		case ' ', '\t', '\n', '\r':
			out.WriteByte(ch)
			i++
			// keyStart is preserved across whitespace inside braces
			continue
		}

		if keyStart && depth > 0 && lokiIsIdentStart(ch) {
			j := i + 1
			for j < len(q) && (lokiIsIdentCont(q[j]) || q[j] == '.') {
				j++
			}
			tok := q[i:j]
			// Trailing dot would be malformed input; leave verbatim so
			// the parser surfaces a clean error.
			if strings.HasSuffix(tok, ".") {
				out.WriteString(tok)
				i = j
				keyStart = false
				continue
			}
			if strings.Contains(tok, ".") {
				out.WriteString(strings.ReplaceAll(tok, ".", "_"))
			} else {
				out.WriteString(tok)
			}
			i = j
			keyStart = false
			continue
		}

		out.WriteByte(ch)
		keyStart = false
		i++
	}
	return out.String()
}

// lokiStringState mirrors stringState in internal/api/prom/dotted_names.go —
// duplicated here so the LogQL rewrite has no cross-package coupling
// and the two heads can evolve independently.
type lokiStringState int

const (
	lokiOutside lokiStringState = iota
	lokiInDouble
	lokiInSingle
	lokiInBacktick
)

func lokiOpenString(ch byte) (lokiStringState, bool) {
	switch ch {
	case '"':
		return lokiInDouble, true
	case '\'':
		return lokiInSingle, true
	case '`':
		return lokiInBacktick, true
	}
	return lokiOutside, false
}

func lokiAdvanceInString(out *strings.Builder, q string, i int, state lokiStringState) (int, lokiStringState) {
	ch := q[i]
	out.WriteByte(ch)
	if state != lokiInBacktick && ch == '\\' && i+1 < len(q) {
		out.WriteByte(q[i+1])
		return i + 2, state
	}
	switch {
	case state == lokiInDouble && ch == '"',
		state == lokiInSingle && ch == '\'',
		state == lokiInBacktick && ch == '`':
		state = lokiOutside
	}
	return i + 1, state
}

func lokiIsIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func lokiIsIdentCont(b byte) bool {
	return lokiIsIdentStart(b) || (b >= '0' && b <= '9')
}
