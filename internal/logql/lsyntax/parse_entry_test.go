package lsyntax

import (
	"errors"
	"strings"
	"testing"
)

// TestParseEntryPoints_ValidationRules pins what each of the three parse
// entry points accepts. ParseExpr runs every validation rule;
// ParseExprAllowingEmptyCompatibleMatchers relaxes the empty-compatible
// matcher rule alone; ParseExprWithoutValidation runs none.
func TestParseEntryPoints_ValidationRules(t *testing.T) {
	t.Parallel()

	type verdicts struct{ strict, permissive, unvalidated bool }
	cases := []struct {
		query string
		want  verdicts // true = accepted
	}{
		{`{app="x"}`, verdicts{true, true, true}},
		{`{app=~".*"}`, verdicts{false, true, true}},
		{`avg_over_time({app="x"}[5m])`, verdicts{false, false, true}},
		{`avg_over_time({app=~".*"}[5m])`, verdicts{false, false, true}},
		{`count_over_time({app="x"}[5m]) by (host)`, verdicts{false, false, true}},
		{`1 + avg_over_time({app="x"}[5m])`, verdicts{false, false, true}},
		{`avg_over_time({app="x"}[5m]) + 1`, verdicts{false, false, true}},
		{`1 + count_over_time({app="x"}[5m])`, verdicts{true, true, true}},
		{`{app="x"`, verdicts{false, false, false}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			_, strictErr := ParseExpr(tc.query)
			_, permissiveErr := ParseExprAllowingEmptyCompatibleMatchers(tc.query)
			_, rawErr := ParseExprWithoutValidation(tc.query)
			got := verdicts{strictErr == nil, permissiveErr == nil, rawErr == nil}
			if got != tc.want {
				t.Errorf("accepted (strict, permissive, unvalidated) = %+v, want %+v\n errors: %v | %v | %v",
					got, tc.want, strictErr, permissiveErr, rawErr)
			}
		})
	}
}

// TestParseEntryPoints_ErrorsAreParseErrors pins that every rejection a
// parse entry point returns is a ParseError carrying the reference's
// message, whether it is raised by the size guard, the lexer, the parser
// or the validation walk, and that no expression accompanies it.
func TestParseEntryPoints_ErrorsAreParseErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		query   string
		wantMsg string
	}{
		{strings.Repeat("a", maxInputSize), "parse error : input size too long (131072 > 131072)"},
		{`{app="x"} )`, `parse error : syntax error: unexpected trailing tokens near ")"`},
		{`{app=~"("}`, "error parsing regexp"},
		{`{app=~".*"}`, errEmptyCompatibleMatcherRejected},
		{`topk(0, sum(rate({app="x"}[5m])))`, "invalid parameter"},
	}
	for _, tc := range cases {
		t.Run(tc.wantMsg, func(t *testing.T) {
			t.Parallel()
			e, err := ParseExpr(tc.query)
			if err == nil {
				t.Fatalf("ParseExpr accepted %q", tc.query)
			}
			if e != nil {
				t.Errorf("ParseExpr returned an expression alongside error %v", err)
			}
			if !errors.Is(err, ErrParse) {
				t.Errorf("error %v (%T) is not a ParseError", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

// TestPanicToParseError pins how a value the parser panics with becomes
// the returned error: a ParseError is returned as is, any other error is
// wrapped with its message, and any other value is wrapped with its
// formatted form.
func TestPanicToParseError(t *testing.T) {
	t.Parallel()

	pe := NewParseError("boom", 2, 3)
	if got := asParseError(pe); got != error(pe) {
		t.Errorf("asParseError(ParseError) = %v, want it unchanged", got)
	}
	if got, want := asParseError(errors.New("plain")).Error(), "parse error : plain"; got != want {
		t.Errorf("asParseError(error) = %q, want %q", got, want)
	}
	if got, want := asParseError(42).Error(), "parse error : 42"; got != want {
		t.Errorf("asParseError(42) = %q, want %q", got, want)
	}
}
