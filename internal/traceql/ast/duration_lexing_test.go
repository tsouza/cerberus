package ast

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestNumberWithDurationSuffix pins how a number followed directly by a
// duration suffix lexes: the suffix runs over digits, '.', and the unit
// letters n s u m h µ d w y; a suffix that completes a duration makes one
// duration token and is consumed, while one that does not leaves the
// number alone and lexes the suffix as what follows it.
func TestNumberWithDurationSuffix(t *testing.T) {
	t.Parallel()

	render := func(toks []token) string {
		var out strings.Builder
		for _, tk := range toks {
			switch tk.kind {
			case tokDuration:
				fmt.Fprintf(&out, "D(%v) ", tk.d)
			case tokInteger:
				fmt.Fprintf(&out, "I(%d) ", tk.i)
			case tokFloat:
				fmt.Fprintf(&out, "F(%v) ", tk.f)
			case tokEOF:
				out.WriteString("EOF")
			default:
				fmt.Fprintf(&out, "K%d(%s) ", tk.kind, tk.str)
			}
		}
		return out.String()
	}
	for _, c := range []struct{ in, want string }{
		{"100ms", "D(100ms) EOF"},
		{"1.5h", "D(1h30m0s) EOF"},
		{"2µs", "D(2µs) EOF"},
		{"3us", "D(3µs) EOF"},
		{"7ns", "D(7ns) EOF"},
		{"1d", fmt.Sprintf("D(%v) EOF", 24*time.Hour)},
		{"1w", fmt.Sprintf("D(%v) EOF", 7*24*time.Hour)},
		{"1y", fmt.Sprintf("D(%v) EOF", 365*24*time.Hour)},
		{"1h30m", "D(1h30m0s) EOF"},
		{"1m1.5s", "D(1m1.5s) EOF"},
		{"1h0.5m", "D(1h0m30s) EOF"},
		{"1m}", fmt.Sprintf("D(1m0s) K%d() EOF", tokCloseBrace)},
		{"5mx", fmt.Sprintf("D(5m0s) K%d(x) EOF", tokIdentifier)},
		{"5hh", fmt.Sprintf("I(5) K%d(hh) EOF", tokIdentifier)},
		{"5 ms", fmt.Sprintf("I(5) K%d(ms) EOF", tokIdentifier)},
		{"5..s", fmt.Sprintf("F(5) K%d() K%d(s) K%d() EOF", tokDot, tokIdentifier, tokEndAttribute)},
		{"42", "I(42) EOF"},
		{"4.5", "F(4.5) EOF"},
	} {
		toks, err := tokenize(c.in)
		got := ""
		if err == nil {
			got = render(toks)
		}
		if got != c.want {
			t.Errorf("tokenize(%q) = %s (err %v), want %s", c.in, got, err, c.want)
		}
	}
}

// TestDurationRuneSet pins the unit letters a duration suffix may hold.
func TestDurationRuneSet(t *testing.T) {
	t.Parallel()

	for r := range rune(0x300) {
		want := false
		for _, u := range "nsumhµdwy" {
			want = want || r == u
		}
		if got := isDurationRune(r); got != want {
			t.Errorf("isDurationRune(%q) = %v, want %v", r, got, want)
		}
	}
}

// TestImpliedTypeAndAggregateText pins the type a unary operation implies
// (boolean for the existence operators, the operand's type otherwise) and
// the text of an aggregate with and without an operand.
func TestImpliedTypeAndAggregateText(t *testing.T) {
	t.Parallel()

	operand := NewStaticInt(3)
	for _, c := range []struct {
		op   Operator
		want StaticType
	}{
		{OpExists, TypeBoolean},
		{OpNotExists, TypeBoolean},
		{OpSub, TypeInt},
		{OpNot, TypeInt},
	} {
		if got := (UnaryOperation{Op: c.op, Expression: operand}).impliedType(); got != c.want {
			t.Errorf("%v implies %v, want %v", c.op, got, c.want)
		}
	}
	if got := (UnaryOperation{Op: OpSub, Expression: NewStaticString("x")}).impliedType(); got != TypeString {
		t.Errorf("-string implies %v, want TypeString", got)
	}

	if got, want := newAggregate(AggregateCount, nil).String(), "count()"; got != want {
		t.Errorf("aggregate without operand = %q, want %q", got, want)
	}
	if got, want := newAggregate(AggregateAvg, NewStaticInt(1)).String(), "avg(1)"; got != want {
		t.Errorf("aggregate with operand = %q, want %q", got, want)
	}
}
