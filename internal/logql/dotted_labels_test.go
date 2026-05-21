package logql

import (
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

// TestNormalizeLokiDottedLabels pins the rewrite contract at the unit
// layer. The wired sites (Lang.Parse, selectorMatchers, tail) call
// this before handing the query to the upstream LogQL parser, so the
// rewrite is the source of truth for whether
// `{service.name="api"}`-shape queries round-trip through cerberus.
func TestNormalizeLokiDottedLabels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// no rewrite — query contains no dotted identifiers
		{
			name: "noop_no_dots",
			in:   `{service_name="api"}`,
			want: `{service_name="api"}`,
		},
		{
			name: "noop_empty",
			in:   ``,
			want: ``,
		},
		{
			name: "noop_numeric_literal_in_duration",
			in:   `rate({service_name="api"}[5m])`,
			want: `rate({service_name="api"}[5m])`,
		},
		// bare dotted label key
		{
			name: "bare_dotted_label",
			in:   `{service.name="api"}`,
			want: `{service_name="api"}`,
		},
		// multiple dotted keys in a single selector
		{
			name: "multi_dotted_labels",
			in:   `{service.name="api", http.method="GET"}`,
			want: `{service_name="api", http_method="GET"}`,
		},
		// dotted key with regex matcher
		{
			name: "regex_matcher",
			in:   `{service.name=~"api|web"}`,
			want: `{service_name=~"api|web"}`,
		},
		// dotted key with negated matchers
		{
			name: "negated_matchers",
			in:   `{service.name!="api", http.status!~"5.."}`,
			want: `{service_name!="api", http_status!~"5.."}`,
		},
		// dotted key followed by a pipeline stage
		{
			name: "with_pipeline_json",
			in:   `{service.name="api"} | json`,
			want: `{service_name="api"} | json`,
		},
		// dotted key inside a range-vector / metric query
		{
			name: "inside_metric_query",
			in:   `rate({service.name="api"}[5m])`,
			want: `rate({service_name="api"}[5m])`,
		},
		{
			name: "inside_sum_by",
			in:   `sum by (service_name) (rate({service.name="api"}[5m]))`,
			want: `sum by (service_name) (rate({service_name="api"}[5m]))`,
		},
		// dotted name inside a string literal must NOT be rewritten
		{
			name: "preserve_string_value",
			in:   `{service_name="my.api.service"}`,
			want: `{service_name="my.api.service"}`,
		},
		{
			name: "preserve_string_value_with_dotted_key",
			in:   `{service.name="my.api.service"}`,
			want: `{service_name="my.api.service"}`,
		},
		{
			name: "preserve_string_value_escaped_quote",
			in:   `{service_name="my.api.\"escaped\".service"}`,
			want: `{service_name="my.api.\"escaped\".service"}`,
		},
		// backtick string literal (Loki supports `…`) — body untouched
		{
			name: "preserve_backtick_string",
			in:   "{service_name=`my.api.service`}",
			want: "{service_name=`my.api.service`}",
		},
		// nested k8s.* / multi-segment dotted keys
		{
			name: "multi_segment_dotted_key",
			in:   `{k8s.pod.name="cerberus-0"}`,
			want: `{k8s_pod_name="cerberus-0"}`,
		},
		// pipeline-stage labels are left alone (they don't sit inside `{...}`)
		{
			name: "pipeline_keep_label_untouched",
			in:   `{service.name="api"} | json foo.bar="$.payload.foo"`,
			want: `{service_name="api"} | json foo.bar="$.payload.foo"`,
		},
		// `| label_format` outside the brace is similarly untouched
		{
			name: "label_format_outside_braces",
			in:   `{service.name="api"} | label_format new=old`,
			want: `{service_name="api"} | label_format new=old`,
		},
		// whitespace between `{` and key
		{
			name: "whitespace_after_open_brace",
			in:   `{  service.name="api"  }`,
			want: `{  service_name="api"  }`,
		},
		// dotted key followed by underscore-only key (mixed)
		{
			name: "mixed_dotted_and_normal",
			in:   `{service.name="api", env="prod"}`,
			want: `{service_name="api", env="prod"}`,
		},
		// already-underscored key with a `.` in the value stays untouched
		{
			name: "already_normal_key_with_dotted_value",
			in:   `{service_name="my.svc"}`,
			want: `{service_name="my.svc"}`,
		},
		// short dotted-key input (< 16 bytes) — exercises the
		// `strings.Builder.Grow(len(q) + 16)` path with an input where
		// `len(q) - 16` would be negative. An ARITHMETIC_BASE mutation
		// that flips `+` to `-` panics at Grow, so this case pins the
		// growth-hint arithmetic.
		{
			name: "short_input_under_grow_pad",
			in:   `{a.b=""}`,
			want: `{a_b=""}`,
		},
		// top-level dotted token MUST round-trip verbatim. The rewrite
		// only fires inside `{ … }` stream-selector braces; a bare
		// `a.b` outside braces is some other construct (function
		// invocation, range duration, …) and must not be molested.
		// Pins the depth-guard at the rewrite site against
		// INVERT_LOGICAL (`&&` → `||`) and CONDITIONALS_BOUNDARY
		// (`>` → `>=`) mutations on the `keyStart && depth > 0` check.
		{
			name: "noop_top_level_dotted_token",
			in:   `a.b="c"`,
			want: `a.b="c"`,
		},
		// stray closing brace BEFORE the selector. The walker MUST
		// guard the `depth--` decrement with `depth > 0` so an
		// unmatched `}` doesn't push depth negative — otherwise the
		// next `{` brings depth back up to 0 (not 1) and the rewrite
		// inside is silently disabled. Pins the close-brace decrement
		// guard against CONDITIONALS_BOUNDARY / CONDITIONALS_NEGATION
		// on `if depth > 0`.
		{
			name: "stray_close_before_selector",
			in:   `}{a.b="c"}`,
			want: `}{a_b="c"}`,
		},
		// unclosed brace with a dotted token at end-of-string. The
		// token-consume loop MUST stop at `j < len(q)` strictly — a
		// CONDITIONALS_BOUNDARY mutation flipping `<` to `<=` would
		// dereference past the end of the input and panic. Output is
		// the rewritten token with no trailing close-brace (matching
		// the input shape so the downstream parser surfaces a clean
		// `unexpected EOF` instead of a corrupted query).
		{
			name: "unclosed_brace_token_at_end",
			in:   `{a.b`,
			want: `{a_b`,
		},
		// escape-in-double-string with a subsequent dotted token. The
		// double-quoted string must honour `\"` so the closing-quote
		// detector skips past the escaped quote; if the escape branch
		// were inverted to fire only on backtick strings (a
		// CONDITIONALS_NEGATION mutation on the
		// `state != lokiInBacktick` predicate), the synthetic close
		// would land the walker outside the string mid-content, and
		// the next `b.c` would wrongly rewrite. The byte output here
		// pins both legs.
		{
			name: "escape_in_double_string_preserves_trailing_key",
			in:   `{a="\",b.c="d"}`,
			want: `{a="\",b.c="d"}`,
		},
		// backtick string containing a backslash followed by the
		// closing backtick. Loki's backtick-string grammar treats `\`
		// as a LITERAL byte (no escape interpretation), so the closing
		// backtick MUST end the string and a subsequent `b.c=...`
		// matcher MUST still be rewritten.
		//
		// Pins the FIRST `&&` (col 29) in
		// `state != lokiInBacktick && ch == '\\' && i+1 < len(q)` at
		// lokiAdvanceInString. An INVERT_LOGICAL mutant `&&` → `||`
		// makes the escape branch fire even inside backtick strings,
		// which would consume the closing backtick as the "escaped
		// char" and leave the walker permanently inside the string —
		// the trailing `b.c` would then NOT be rewritten.
		{
			name: "backtick_with_backslash_then_dotted_key",
			in:   "{a=`x\\`, b.c=\"y\"}",
			want: "{a=`x\\`, b_c=\"y\"}",
		},
		// double-quoted string with a single-byte body that does NOT
		// contain a backslash. The escape branch MUST stay dormant so
		// the closing `"` at the next position ends the string and a
		// subsequent `c.d=...` matcher MUST be rewritten.
		//
		// Pins the SECOND `&&` (col 43) in
		// `state != lokiInBacktick && ch == '\\' && i+1 < len(q)`. An
		// INVERT_LOGICAL mutant turning the second `&&` into `||`
		// expands the predicate to
		// `(state != lokiInBacktick && ch == '\\') || i+1 < len(q)` —
		// which fires the escape path on EVERY non-final byte
		// regardless of backslash presence. With a single-byte body
		// the mutant swallows the closing `"` as the "escaped byte",
		// leaving the walker permanently inside the string and
		// skipping the trailing `c.d` rewrite.
		{
			name: "double_string_one_byte_body_then_dotted_key",
			in:   `{a="b",c.d="y"}`,
			want: `{a="b",c_d="y"}`,
		},
		// trailing top-level dotted token AFTER a closed selector.
		// The `}` MUST decrement depth back to 0 (not increment); if
		// the decrement were flipped to an increment, the subsequent
		// `,` would land at depth > 0 and the trailing `x.y` would
		// be wrongly rewritten. Pins the close-brace
		// INCREMENT_DECREMENT (`depth--` → `depth++`) and the
		// CONDITIONALS_NEGATION on the close-brace guard.
		{
			name: "trailing_top_level_after_selector",
			in:   `{a.b="c"},x.y="z"`,
			want: `{a_b="c"},x.y="z"`,
		},
		// Trailing backslash inside a double-quoted string at end-of-
		// input. The escape-branch guard `i+1 < len(q)` in
		// lokiAdvanceInString MUST stay strict-less-than: a
		// CONDITIONALS_BOUNDARY mutant flipping `<` to `<=` would
		// admit `i+1 == len(q)` and then dereference `q[i+1]` past
		// the end of the input, panicking the test. Original output
		// preserves the unterminated string verbatim so the upstream
		// parser surfaces a clean error. The `a.` prefix is there
		// just to clear the early-return ContainsRune('.') fast path
		// so the walker actually runs.
		{
			name: "trailing_backslash_in_double_string_eof",
			in:   `{a.b="x\`,
			want: `{a_b="x\`,
		},
		// Backslash inside a double-quoted string followed by a `"`
		// then a top-level dotted token. The escape branch MUST fire
		// (consume the `"` as the escaped byte) so the string
		// continues — a CONDITIONALS_NEGATION mutant flipping `<` to
		// `>=` makes the escape predicate uniformly false for normal
		// (i.e., non-EOF) positions, dropping the escape consume.
		// With the mutant the second `"` closes the string early and
		// `c.d` at top level inside braces gets rewritten — distinct
		// output. The leading dotted key fires the early-return-guard
		// path so the walker enters the loop.
		{
			name: "escape_in_double_string_then_dotted_label",
			in:   `{a.b="x\"y",c.d="z"}`,
			want: `{a_b="x\"y",c_d="z"}`,
		},
		// Bottom-of-loop fall-through advance: a single non-special
		// character `=` outside any selector. The fall-through `i++`
		// at the end of the for-loop body MUST advance: an
		// INCREMENT_DECREMENT mutant flipping `i++` to `i--` lands
		// `i` at -1 on the next iteration and panics on `q[-1]`.
		// The `.` is required to clear the ContainsRune('.') early-
		// return; the `=` lands the walker at the bottom fall-through
		// path (not whitespace, not `{`/`}`/`,`, not string-open, not
		// ident-at-keystart-with-depth>0).
		{
			name: "bottom_fallthrough_advance_top_level",
			in:   `=.`,
			want: `=.`,
		},
		// Whitespace-inside-selector advance: the whitespace case
		// MUST advance `i` past the space. An INCREMENT_DECREMENT
		// mutant flipping the whitespace-case `i++` to `i--` sends
		// `i` back to `{`, depth re-increments, and the walker
		// loops without progress (or, on a leading-space input,
		// lands at `q[-1]` and panics). The trailing `.` guarantees
		// the ContainsRune early-return path is bypassed; the
		// leading-space case puts the whitespace branch at i=0 so
		// the mutant's underflow is the simplest possible panic.
		{
			name: "leading_whitespace_top_level_with_dot",
			in:   ` .`,
			want: ` .`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeLokiDottedLabels(tc.in)
			if got != tc.want {
				t.Errorf("normalizeLokiDottedLabels(%q):\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeLokiDottedLabels_Idempotent pins idempotency — running
// the rewrite twice produces the same result as running it once. Once
// the dotted key has been collapsed to `service_name`, a second pass
// has nothing to do.
func TestNormalizeLokiDottedLabels_Idempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		`{service.name="api"}`,
		`{service.name="api", http.method="GET"}`,
		`rate({service.name="api"}[5m])`,
		`sum by (service_name) (rate({service.name="api"}[5m]))`,
		`{service.name="api"} | json | duration > 1s`,
	}
	for _, in := range inputs {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			first := normalizeLokiDottedLabels(in)
			second := normalizeLokiDottedLabels(first)
			if first != second {
				t.Errorf("not idempotent:\n  in:     %q\n  first:  %q\n  second: %q", in, first, second)
			}
		})
	}
}

// TestNormalizeLokiDottedLabels_ParserRoundtrip is the load-bearing
// invariant for this PR: every shape the task spec enumerates as a
// rejection case must parse cleanly AFTER the rewrite. If the upstream
// LogQL parser ever changes its label-key grammar to accept dotted
// identifiers natively, the rewrite becomes a no-op for these inputs;
// either way the contract — "cerberus accepts `{service.name=…}`" —
// holds.
func TestNormalizeLokiDottedLabels_ParserRoundtrip(t *testing.T) {
	t.Parallel()

	queries := []string{
		`{service.name="api"}`,
		`{service.name="api", http.method="GET"}`,
		`{service.name="api"} | json`,
		`{service.name=~"api|web"}`,
		`rate({service.name="api"}[5m])`,
		`sum by (service_name) (rate({service.name="api"}[5m]))`,
		`{k8s.pod.name="cerberus-0"}`,
		`{service.name="api"} | label_format new=old`,
	}
	for _, q := range queries {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			rewritten := normalizeLokiDottedLabels(q)
			if _, err := syntax.ParseExpr(rewritten); err != nil {
				t.Errorf("parser rejected rewritten query:\n  in:        %q\n  rewritten: %q\n  err:       %v", q, rewritten, err)
			}
		})
	}
}

// TestNormalizeLokiDottedLabels_MatcherSemantics confirms the rewrite
// preserves the original Loki matcher contract: the matcher's Name
// field reaches the SelectorPredicate lowering as the underscored form
// (`service_name`), which the OTel-CH ResourceAttributes map mirrors
// alongside the dotted form. Combined with the parser-roundtrip test
// above, this pins both legs of the round-trip: parser accepts AND the
// resulting matcher targets the right row.
func TestNormalizeLokiDottedLabels_MatcherSemantics(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(normalizeLokiDottedLabels(`{service.name="api", http.method="GET"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel, ok := expr.(syntax.LogSelectorExpr)
	if !ok {
		t.Fatalf("not a log-selector expr: %T", expr)
	}
	matchers := sel.Matchers()
	if len(matchers) != 2 {
		t.Fatalf("want 2 matchers, got %d", len(matchers))
	}
	wantNames := map[string]bool{"service_name": false, "http_method": false}
	for _, m := range matchers {
		if _, ok := wantNames[m.Name]; !ok {
			t.Errorf("unexpected matcher name %q", m.Name)
			continue
		}
		wantNames[m.Name] = true
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("missing matcher %q", name)
		}
	}
}
