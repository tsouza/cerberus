package lsyntax

import (
	"strings"
	"testing"
)

// This file exists because internal/logql/lsyntax's parser.go had ZERO
// direct unit-test coverage of its own: the package's only default-tag
// test file (dotted_labels_test.go) covers the OTel dotted-label
// rewrite, and the only file that exercises parser.go/lexer.go against
// real LogQL text is oracle_agpl_test.go, which is gated behind the
// `agpl_oracle` build tag and never runs in `go test ./...`. Splitting
// `internal/logql/lsyntax` into its own gremlins legs (phase4-logql-parser
// / phase4-logql-lsyntax, see .github/workflows/mutation.yml) surfaced
// this: 51.2%/65.4% efficacy, far below the 95% bar every other phase
// clears. These tests close the gap by exercising parser.go directly
// through the public ParseExpr/ParseSampleExpr/ParseLogSelector API,
// plus a few white-box tests against parser-internal bounds-checking
// helpers that aren't reachable any other way.

// --- ParseExprWithoutValidation input-size guard (parser.go) --------------

func TestParseExpr_InputSizeGuard(t *testing.T) {
	// Exactly at the boundary: must be rejected by the explicit
	// "input size too long" guard, not merely fail later in lexing.
	atLimit := strings.Repeat("a", maxInputSize)
	_, err := ParseExpr(atLimit)
	if err == nil {
		t.Fatalf("input of length %d (== maxInputSize) should be rejected", maxInputSize)
	}
	if !strings.Contains(err.Error(), "input size too long") {
		t.Errorf("expected the size-guard error, got: %v", err)
	}

	// One under the boundary: a valid, otherwise-parseable query padded
	// with trailing whitespace out to maxInputSize-1 bytes must NOT be
	// rejected by the size guard.
	padded := `{app="x"}` + strings.Repeat(" ", maxInputSize-1-len(`{app="x"}`))
	if len(padded) != maxInputSize-1 {
		t.Fatalf("test construction bug: len(padded)=%d want %d", len(padded), maxInputSize-1)
	}
	_, err = ParseExpr(padded)
	if err != nil {
		t.Errorf("input of length maxInputSize-1 should parse (bare selector + trailing whitespace), got: %v", err)
	}
}

// --- validateExpr / validateSampleExpr error propagation ------------------

func TestParseExpr_VectorAggregationErrorPropagates(t *testing.T) {
	// mustNewVectorAggregationExpr stashes a construction error (invalid
	// topk parameter) on the node itself; validateSampleExpr's
	// *VectorAggregationExpr case must surface it via e.err, not silently
	// swallow it and recurse into e.Left (which is nil for an errored
	// node, and would panic).
	_, err := ParseExpr(`topk(0, sum(rate({app="x"}[5m])))`)
	if err == nil {
		t.Fatal("expected an error for topk with a non-positive parameter")
	}
	if !strings.Contains(err.Error(), "invalid parameter") {
		t.Errorf("expected an invalid-parameter error, got: %v", err)
	}
}

func TestParseExpr_RangeAggregationValidateErrorPropagates(t *testing.T) {
	// newRangeAggregationExpr's validate() rejects grouping on operations
	// that don't support it (count_over_time); that error must surface
	// all the way through ParseExpr's default-case Selector() path in
	// validateSampleExpr, not be dropped.
	_, err := ParseExpr(`count_over_time({app="x"}[5m]) by (host)`)
	if err == nil {
		t.Fatal("expected an error: grouping is not allowed on count_over_time")
	}
	if !strings.Contains(err.Error(), "grouping not allowed") {
		t.Errorf("expected a grouping-not-allowed error, got: %v", err)
	}
}

func TestParseExpr_UnwrapRequirement(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantErr string // substring; empty means "must succeed"
	}{
		{
			name:  "count_over_time without unwrap succeeds",
			query: `count_over_time({app="x"}[5m])`,
		},
		{
			name:    "count_over_time with unwrap is rejected",
			query:   `count_over_time({app="x"} | unwrap bytes [5m])`,
			wantErr: "invalid aggregation count_over_time with unwrap",
		},
		{
			name:  "avg_over_time with unwrap succeeds",
			query: `avg_over_time({app="x"} | unwrap bytes [5m])`,
		},
		{
			name:    "avg_over_time without unwrap is rejected",
			query:   `avg_over_time({app="x"}[5m])`,
			wantErr: "invalid aggregation avg_over_time without unwrap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseExpr(tc.query)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got success", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// --- parseUnary: signed numeric literals -----------------------------------

func TestParseExpr_SignedLiteral(t *testing.T) {
	cases := []struct {
		query string
		want  float64
	}{
		{"-5", -5},
		{"+5", 5},
	}
	for _, tc := range cases {
		e, err := ParseExpr(tc.query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): unexpected error: %v", tc.query, err)
		}
		lit, ok := e.(*LiteralExpr)
		if !ok {
			t.Fatalf("ParseExpr(%q): got %T, want *LiteralExpr", tc.query, e)
		}
		got, err := lit.Value()
		if err != nil {
			t.Fatalf("ParseExpr(%q): Value(): %v", tc.query, err)
		}
		if got != tc.want {
			t.Errorf("ParseExpr(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// --- parseExpr: operator precedence and associativity ----------------------

func TestParseExpr_PrecedenceAndAssociativity(t *testing.T) {
	// All-literal binary expressions constant-fold to a single
	// LiteralExpr (mustNewBinOpExpr reduces two literal legs), so the
	// resulting numeric value pins both precedence and associativity
	// unambiguously: a wrong precedence table or a left/right-assoc bug
	// changes the computed number.
	cases := []struct {
		query string
		want  float64
	}{
		// '*' binds tighter than '+': 2 + (3*4), not (2+3)*4.
		{"2 + 3 * 4", 14},
		// '-' is left-associative: (1-2)-3 = -4, not 1-(2-3) = 2.
		{"1 - 2 - 3", -4},
		// '^' is right-associative: 2^(3^2) = 2^9 = 512, not (2^3)^2 = 64.
		{"2 ^ 3 ^ 2", 512},
	}
	for _, tc := range cases {
		e, err := ParseExpr(tc.query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): unexpected error: %v", tc.query, err)
		}
		lit, ok := e.(*LiteralExpr)
		if !ok {
			t.Fatalf("ParseExpr(%q): got %T, want a constant-folded *LiteralExpr", tc.query, e)
		}
		got, err := lit.Value()
		if err != nil {
			t.Fatalf("ParseExpr(%q): Value(): %v", tc.query, err)
		}
		if got != tc.want {
			t.Errorf("ParseExpr(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// --- parseRangeAggregation / parseVectorAggregation: optional leading ------
// --- numeric parameter must not be confused with an arbitrary token --------

func TestParseExpr_RangeAggregationRejectsNonNumericParam(t *testing.T) {
	// The `<number> ','` lookahead in parseRangeAggregation must require
	// BOTH a number token AND a following comma; a lone identifier
	// sitting where the comma-separated parameter goes must not be
	// silently consumed as the parameter, swallowing the comma with it.
	// If it were, parsing would incorrectly proceed straight into the log
	// range and fail later with an "invalid parameter" error instead of
	// the correct syntax error over the malformed selector.
	_, err := ParseExpr(`quantile_over_time(x, {app="y"}[5m])`)
	if err == nil {
		t.Fatal("expected a syntax error for a non-numeric range-aggregation parameter")
	}
	if !strings.Contains(err.Error(), "expected '{'") {
		t.Errorf("expected a syntax error about the missing selector, got: %v", err)
	}
}

func TestParseExpr_VectorAggregationRejectsNonNumericParam(t *testing.T) {
	// Same lookahead, mirrored in parseVectorAggregation.
	_, err := ParseExpr(`topk(x, sum(rate({app="y"}[5m])))`)
	if err == nil {
		t.Fatal("expected a syntax error for a non-numeric vector-aggregation parameter")
	}
	if !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("expected a syntax error about the unexpected token, got: %v", err)
	}
}

// --- parseVectorAggregation: grouping may appear before OR after the -------
// --- argument list, but never both -----------------------------------------

func TestParseExpr_VectorAggregationGrouping(t *testing.T) {
	// Grouping before the parens.
	e, err := ParseExpr(`sum by (a) (rate({app="x"}[5m]))`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	va, ok := e.(*VectorAggregationExpr)
	if !ok {
		t.Fatalf("got %T, want *VectorAggregationExpr", e)
	}
	if va.Grouping == nil || len(va.Grouping.Groups) != 1 || va.Grouping.Groups[0] != "a" {
		t.Errorf("grouping before parens: got %+v, want Groups=[a]", va.Grouping)
	}

	// Grouping after the parens (the alternative, equally valid form).
	e, err = ParseExpr(`sum(rate({app="x"}[5m])) by (host)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	va, ok = e.(*VectorAggregationExpr)
	if !ok {
		t.Fatalf("got %T, want *VectorAggregationExpr", e)
	}
	if va.Grouping == nil || len(va.Grouping.Groups) != 1 || va.Grouping.Groups[0] != "host" {
		t.Errorf("grouping after parens: got %+v, want Groups=[host]", va.Grouping)
	}

	// No grouping at all: mustNewVectorAggregationExpr must default
	// Grouping to a non-nil, empty *Grouping (never leave it nil).
	e, err = ParseExpr(`sum(rate({app="x"}[5m]))`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	va, ok = e.(*VectorAggregationExpr)
	if !ok {
		t.Fatalf("got %T, want *VectorAggregationExpr", e)
	}
	if va.Grouping == nil {
		t.Fatal("expected a default (non-nil) Grouping when none is specified")
	}
	if len(va.Grouping.Groups) != 0 {
		t.Errorf("expected an empty default Grouping, got %+v", va.Grouping)
	}

	// Grouping BOTH before and after the parens is not valid grammar:
	// once the before-parens grouping is set, the after-parens clause
	// must be left unconsumed, producing a trailing-tokens error. The
	// error text must name the actual offending token ("by"), which
	// pins parser.tokenText's `t.str != ""` branch (a keyword token
	// always carries its text in .str; tokenKindName's fallback ("token")
	// is only reached for tokens that don't).
	_, err = ParseExpr(`sum by (a) (rate({app="x"}[5m])) by (b)`)
	if err == nil {
		t.Fatal("expected an error: grouping specified both before and after the parens")
	}
	if !strings.Contains(err.Error(), `trailing tokens near "by"`) {
		t.Errorf(`expected an error naming the trailing "by" token, got: %v`, err)
	}
}

// --- parseRangeAggregation: a genuine numeric parameter must be ------------
// --- recognized (not just a malformed one rejected) ------------------------

func TestParseExpr_RangeAggregationAcceptsNumericParam(t *testing.T) {
	// Complements TestParseExpr_RangeAggregationRejectsNonNumericParam:
	// that test pins the `&&` in the `at(tkNumber) && peek(1)==tkComma`
	// lookahead; this one pins the `==` itself, which only diverges from
	// `!=` when a real, well-formed "<number>," prefix is present.
	e, err := ParseExpr(`quantile_over_time(0.95, {app="x"} | unwrap bytes [5m])`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ra, ok := e.(*RangeAggregationExpr)
	if !ok {
		t.Fatalf("got %T, want *RangeAggregationExpr", e)
	}
	if ra.Params == nil || *ra.Params != 0.95 {
		t.Errorf("Params = %v, want 0.95", ra.Params)
	}
}

// --- parseBinOpModifier: on(...) / ignoring(...) / bool ---------------------

func TestParseExpr_BinOpModifier(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantOn     bool
		wantBool   bool
		wantLabels []string
	}{
		{
			name:       "on",
			query:      `count_over_time({app="x"}[5m]) + on(host) count_over_time({app="y"}[5m])`,
			wantOn:     true,
			wantLabels: []string{"host"},
		},
		{
			name:       "ignoring",
			query:      `count_over_time({app="x"}[5m]) + ignoring(host) count_over_time({app="y"}[5m])`,
			wantOn:     false,
			wantLabels: []string{"host"},
		},
		{
			name:       "bool on",
			query:      `count_over_time({app="x"}[5m]) + bool on(host) count_over_time({app="y"}[5m])`,
			wantOn:     true,
			wantBool:   true,
			wantLabels: []string{"host"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			bo, ok := e.(*BinOpExpr)
			if !ok {
				t.Fatalf("got %T, want *BinOpExpr", e)
			}
			if bo.Opts.ReturnBool != tc.wantBool {
				t.Errorf("ReturnBool = %v, want %v", bo.Opts.ReturnBool, tc.wantBool)
			}
			if bo.Opts.VectorMatching.On != tc.wantOn {
				t.Errorf("VectorMatching.On = %v, want %v", bo.Opts.VectorMatching.On, tc.wantOn)
			}
			if strings.Join(bo.Opts.VectorMatching.MatchingLabels, ",") != strings.Join(tc.wantLabels, ",") {
				t.Errorf("MatchingLabels = %v, want %v", bo.Opts.VectorMatching.MatchingLabels, tc.wantLabels)
			}
		})
	}
}

// --- parseUnwrap: trailing `| <labelFilter>` post-filters (white-box) ------

func TestParseExpr_UnwrapPostFilters(t *testing.T) {
	// Both accepted lookahead shapes (`| <identifier>...` and
	// `| (...)`) must be recognized and folded into PostFilters.
	cases := []struct {
		name  string
		query string
	}{
		{"identifier-led filter", `avg_over_time({app="x"} | unwrap bytes | foo > 5 [5m])`},
		{"paren-led filter", `avg_over_time({app="x"} | unwrap bytes | (foo > 5) [5m])`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			ra, ok := e.(*RangeAggregationExpr)
			if !ok {
				t.Fatalf("got %T, want *RangeAggregationExpr", e)
			}
			if got := len(ra.Left.Unwrap.PostFilters); got != 1 {
				t.Errorf("PostFilters = %d, want 1", got)
			}
		})
	}
}

// --- parseSignedFloat: signed numeric values inside label filters ---------

func TestParseExpr_SignedLabelFilterValue(t *testing.T) {
	cases := []struct {
		query string
		want  float64
	}{
		{`avg_over_time({app="x"} | unwrap bytes | foo > -5 [5m])`, -5},
		{`avg_over_time({app="x"} | unwrap bytes | foo > +5 [5m])`, 5},
	}
	for _, tc := range cases {
		e, err := ParseExpr(tc.query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): unexpected error: %v", tc.query, err)
		}
		ra, ok := e.(*RangeAggregationExpr)
		if !ok {
			t.Fatalf("ParseExpr(%q): got %T, want *RangeAggregationExpr", tc.query, e)
		}
		if len(ra.Left.Unwrap.PostFilters) != 1 {
			t.Fatalf("ParseExpr(%q): PostFilters = %d, want 1", tc.query, len(ra.Left.Unwrap.PostFilters))
		}
		nf, ok := ra.Left.Unwrap.PostFilters[0].(*NumericLabelFilter)
		if !ok {
			t.Fatalf("ParseExpr(%q): PostFilter is %T, want *NumericLabelFilter", tc.query, ra.Left.Unwrap.PostFilters[0])
		}
		if nf.Value != tc.want {
			t.Errorf("ParseExpr(%q): Value = %v, want %v", tc.query, nf.Value, tc.want)
		}
	}
}

// --- parseGroupModifier: group_left(...) / group_right(...) ---------------

func TestParseExpr_GroupModifier(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantCard   VectorMatchCardinality
		wantLabels []string
	}{
		{
			name:       "group_left with include list",
			query:      `count_over_time({app="x"}[5m]) + on(host) group_left(extra) count_over_time({app="y"}[5m])`,
			wantCard:   CardManyToOne,
			wantLabels: []string{"extra"},
		},
		{
			name:       "group_right with include list",
			query:      `count_over_time({app="x"}[5m]) + on(host) group_right(extra) count_over_time({app="y"}[5m])`,
			wantCard:   CardOneToMany,
			wantLabels: []string{"extra"},
		},
		{
			name:     "no group modifier at all",
			query:    `count_over_time({app="x"}[5m]) + on(host) count_over_time({app="y"}[5m])`,
			wantCard: CardOneToOne,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			bo, ok := e.(*BinOpExpr)
			if !ok {
				t.Fatalf("got %T, want *BinOpExpr", e)
			}
			vm := bo.Opts.VectorMatching
			if vm.Card != tc.wantCard {
				t.Errorf("Card = %v, want %v", vm.Card, tc.wantCard)
			}
			if strings.Join(vm.Include, ",") != strings.Join(tc.wantLabels, ",") {
				t.Errorf("Include = %v, want %v", vm.Include, tc.wantLabels)
			}
		})
	}
}

// --- validateSampleExpr: *BinOpExpr error propagation ----------------------

func TestParseExpr_BinOpErrorPropagates(t *testing.T) {
	// mustNewBinOpExpr rejects a literal leg on a logical/set operator
	// ("and"/"or"/"unless") by stashing the error directly on the
	// resulting BinOpExpr; validateSampleExpr's *BinOpExpr case must
	// surface it via e.err rather than recursing into the (nil) legs.
	_, err := ParseExpr(`1 and count_over_time({app="x"}[5m])`)
	if err == nil {
		t.Fatal("expected an error: literal leg on a logical/set binary operator")
	}
	if !strings.Contains(err.Error(), "unexpected literal") {
		t.Errorf("expected an unexpected-literal error, got: %v", err)
	}
}

func TestParseExpr_BinOpLeftLegErrorPropagates(t *testing.T) {
	// When the BinOpExpr node itself constructs cleanly but its LEFT leg
	// carries a validation error (e.g. an invalid grouping on a nested
	// range aggregation), validateSampleExpr must recurse into
	// e.SampleExpr and surface that error, not silently return success
	// (or fall through to validating e.RHS instead).
	_, err := ParseExpr(`count_over_time({app="x"}[5m]) by (host) + count_over_time({app="y"}[5m])`)
	if err == nil {
		t.Fatal("expected an error: grouping is not allowed on count_over_time")
	}
	if !strings.Contains(err.Error(), "grouping not allowed") {
		t.Errorf("expected a grouping-not-allowed error, got: %v", err)
	}
}

// --- mustNewVectorAggregationExpr: non-integer topk/bottomk parameter -----

func TestParseExpr_VectorAggregationNonIntegerParam(t *testing.T) {
	// "0.5" lexes as a single tkNumber, so it passes the
	// at(tkNumber)+comma lookahead and reaches strconv.Atoi, which
	// rejects it for not being an integer. That must surface as its own
	// "invalid parameter" error, not fall through to the (unrelated)
	// "must be greater than 0" check using Atoi's zero-value result.
	_, err := ParseExpr(`topk(0.5, sum(rate({app="x"}[5m])))`)
	if err == nil {
		t.Fatal("expected an error for a non-integer topk parameter")
	}
	if !strings.Contains(err.Error(), "invalid parameter topk(0.5") {
		t.Errorf("expected an invalid-parameter error naming the value, got: %v", err)
	}
	if strings.Contains(err.Error(), "must be greater than 0") {
		t.Errorf("got the wrong error variant (should not have reached the >0 check): %v", err)
	}
}

// --- mustNewBinOpExpr: constant folding requires BOTH legs to be ----------
// --- literals, not just one -------------------------------------------------

func TestParseExpr_BinOpFoldsOnlyWhenBothLegsAreLiterals(t *testing.T) {
	// The right leg here is a literal (5) but the left is not
	// (a RangeAggregationExpr); the result must stay a *BinOpExpr, never
	// get folded into a *LiteralExpr using the non-literal leg's
	// (zero-value) numeric value.
	e, err := ParseExpr(`count_over_time({app="x"}[5m]) + 5`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := e.(*BinOpExpr); !ok {
		t.Errorf("got %T, want *BinOpExpr (must not constant-fold with a non-literal leg)", e)
	}
}

func TestParserUnwrapPostFilterLookaheadBoundary(t *testing.T) {
	// White-box: parseUnwrap's trailing-filter loop requires the token
	// after '|' to be an identifier or '(' before consuming the pipe. A
	// pipe followed by anything else (here, a bare number) must be left
	// entirely unconsumed for the caller to deal with, not swallowed.
	toks, err := lex(`| unwrap bytes | 5`)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	p := &parser{toks: toks}
	u := p.parseUnwrap()
	if len(u.PostFilters) != 0 {
		t.Errorf("PostFilters = %d, want 0 (lookahead token is neither identifier nor '(')", len(u.PostFilters))
	}
	if p.cur().kind != tkPipe {
		t.Errorf("parseUnwrap consumed the trailing '|' it should have left for the caller; cur=%v", p.cur().kind)
	}
}

// --- parseLogRange: bare selector vs. selector-with-pipeline shape ---------

func TestParseExpr_LogRangeLeftShape(t *testing.T) {
	// No pipeline stages: Left must stay the bare *MatchersExpr the
	// selector parsed into, not get wrapped in an (empty) *PipelineExpr.
	e, err := ParseExpr(`count_over_time({app="x"}[5m])`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ra, ok := e.(*RangeAggregationExpr)
	if !ok {
		t.Fatalf("got %T, want *RangeAggregationExpr", e)
	}
	if _, ok := ra.Left.Left.(*MatchersExpr); !ok {
		t.Errorf("no pipeline stages: got %T, want *MatchersExpr", ra.Left.Left)
	}

	// With a pipeline stage: Left must be wrapped in a *PipelineExpr.
	e, err = ParseExpr(`count_over_time({app="x"} | json [5m])`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ra, ok = e.(*RangeAggregationExpr)
	if !ok {
		t.Fatalf("got %T, want *RangeAggregationExpr", e)
	}
	if _, ok := ra.Left.Left.(*PipelineExpr); !ok {
		t.Errorf("with a pipeline stage: got %T, want *PipelineExpr", ra.Left.Left)
	}
}

// --- parser.peek / parser.advance bounds checking (white-box) --------------

func TestParserPeekAdvanceBounds(t *testing.T) {
	toks, err := lex("a b")
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	// "a", "b", EOF.
	if len(toks) != 3 {
		t.Fatalf("test construction bug: len(toks)=%d, want 3", len(toks))
	}
	lastKind := toks[len(toks)-1].kind

	p := &parser{toks: toks}
	if got := p.peek(0); got.kind != toks[0].kind {
		t.Errorf("peek(0) = %v, want %v", got.kind, toks[0].kind)
	}
	if got := p.peek(1); got.kind != toks[1].kind {
		t.Errorf("peek(1) = %v, want %v", got.kind, toks[1].kind)
	}
	// Exactly at the token-count boundary: must clamp to the last token.
	if got := p.peek(len(toks)); got.kind != lastKind {
		t.Errorf("peek(len(toks)) = %v, want clamp to %v", got.kind, lastKind)
	}
	// Well past the boundary: must still clamp, not index out of range.
	if got := p.peek(len(toks) + 5); got.kind != lastKind {
		t.Errorf("peek(len(toks)+5) = %v, want clamp to %v", got.kind, lastKind)
	}

	// advance() must never move pos past the last valid token index, no
	// matter how many times it's called.
	for i := 0; i < len(toks)+3; i++ {
		p.advance()
	}
	if want := len(toks) - 1; p.pos != want {
		t.Errorf("pos after over-advancing = %d, want clamp to %d", p.pos, want)
	}
	if p.cur().kind != lastKind {
		t.Errorf("cur() after over-advancing = %v, want %v", p.cur().kind, lastKind)
	}
}
