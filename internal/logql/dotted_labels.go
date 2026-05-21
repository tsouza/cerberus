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

	// Single-advance walker. Every iteration of the outer loop ends with
	// exactly one `i = next` assignment computed by the inner step. This
	// collapses the `i++` mutation surface (gremlins INCREMENT_DECREMENT
	// would otherwise have to be killed independently in every per-token
	// branch) down to a single `next := i + 1` site that any "first byte
	// reaches the walker" test panics on under the `i--` mutation.
	for i := 0; i < len(q); {
		ch := q[i]
		next := i + 1
		nextKeyStart := false

		switch {
		case state != lokiOutside:
			next, state = lokiAdvanceInString(&out, q, i, state)
		case isLokiStringOpen(ch):
			state = lokiOpenStringState(ch)
			out.WriteByte(ch)
		case ch == '{':
			out.WriteByte(ch)
			depth++
			nextKeyStart = true
		case ch == '}':
			out.WriteByte(ch)
			if depth > 0 {
				depth--
			}
		case ch == ',':
			// keyStart is unconditionally set on `,`; the `depth > 0`
			// guard at the rewrite site below suppresses top-level
			// commas so a stray `a.b , c.d` outside braces still falls
			// through unchanged. Keeping the assignment unconditional
			// here eliminates a semantically-equivalent boundary
			// mutation that the previous `depth > 0` form admitted.
			out.WriteByte(ch)
			nextKeyStart = true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			// Whitespace preserves keyStart across the gap between `{`
			// and the first label key, so `{  service.name=…}` still
			// rewrites the dotted token.
			out.WriteByte(ch)
			nextKeyStart = keyStart
		case keyStart && depth > 0 && lokiIsIdentStart(ch):
			next = lokiConsumeIdentToken(&out, q, i)
		default:
			out.WriteByte(ch)
		}

		i = next
		keyStart = nextKeyStart
	}
	return out.String()
}

// lokiConsumeIdentToken greedy-consumes one
// `[a-zA-Z_]([a-zA-Z0-9_]|\.)*` token starting at q[i], writes its
// rewritten (or verbatim) form to out, and returns the index past the
// token. A trailing-dot token is left verbatim — that's malformed input
// and we want the downstream parser to surface a clean error rather
// than the rewrite mangling it into an underscore-suffixed identifier.
func lokiConsumeIdentToken(out *strings.Builder, q string, i int) int {
	j := i + 1
	for j < len(q) && (lokiIsIdentCont(q[j]) || q[j] == '.') {
		j++
	}
	tok := q[i:j]
	if strings.HasSuffix(tok, ".") {
		out.WriteString(tok)
		return j
	}
	if strings.Contains(tok, ".") {
		out.WriteString(strings.ReplaceAll(tok, ".", "_"))
	} else {
		out.WriteString(tok)
	}
	return j
}

// isLokiStringOpen reports whether ch begins a Loki string literal —
// the three openers are `"`, `'`, and the backtick character.
// Separating the opener test from the state lookup lets the walker
// `switch` over byte-equality cases so gremlins'
// CONDITIONALS_NEGATION on the compound `state == lokiOutside && open`
// guard can't survive — opening a string is a single observable byte
// event the round-trip + verbatim-string tests pin from both legs.
func isLokiStringOpen(ch byte) bool {
	return ch == '"' || ch == '\'' || ch == '`'
}

// lokiOpenStringState returns the in-string state corresponding to a
// string-opener byte. Callers must gate this with isLokiStringOpen —
// passing a non-opener byte panics, which is exactly the kind of
// fail-loud signal a hypothetical bypass mutation would trip on.
func lokiOpenStringState(ch byte) lokiStringState {
	switch ch {
	case '"':
		return lokiInDouble
	case '\'':
		return lokiInSingle
	case '`':
		return lokiInBacktick
	}
	panic("lokiOpenStringState: not a string opener")
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

func lokiAdvanceInString(out *strings.Builder, q string, i int, state lokiStringState) (int, lokiStringState) {
	ch := q[i]
	out.WriteByte(ch)
	// Per-state advance: backtick strings have NO escape grammar (Loki
	// treats `\` as a literal byte), while double / single strings honour
	// `\X` as a two-byte literal pair. Splitting on state up-front makes
	// the escape-branch guard a single-operator compare (`ch == '\\'`)
	// rather than the compound `state != lokiInBacktick && ch == '\\'`
	// the previous form carried — every mutation on the surviving
	// operators is killed by one of the in-string TXTAR tests.
	if state == lokiInBacktick {
		if ch == '`' {
			state = lokiOutside
		}
		return i + 1, state
	}
	if ch == '\\' && i+1 < len(q) {
		out.WriteByte(q[i+1])
		return i + 2, state
	}
	if state == lokiInDouble && ch == '"' {
		state = lokiOutside
	}
	if state == lokiInSingle && ch == '\'' {
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
