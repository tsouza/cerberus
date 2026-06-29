package ast

import (
	"testing"
	"time"
)

// Mutation-coverage tests for lexer.go: the duration-suffix scanner and the
// Prometheus-first duration parser.

// durationRHS parses `{ duration > <lit> }` and returns the right-hand Static.
func durationRHS(t *testing.T, lit string) Static {
	t.Helper()
	q := "{ duration > " + lit + " }"
	sf, ok := firstElem(t, q).(*SpansetFilter)
	if !ok {
		t.Fatalf("Parse(%q): element is not *SpansetFilter", q)
	}
	bin, ok := sf.Expression.(*BinaryOperation)
	if !ok {
		t.Fatalf("Parse(%q): expression = %T; want *BinaryOperation", q, sf.Expression)
	}
	s, ok := bin.RHS.(Static)
	if !ok {
		t.Fatalf("Parse(%q): RHS = %T; want Static", q, bin.RHS)
	}
	return s
}

// TestDurationLiteralScanning pins that a numeric literal followed by a unit
// suffix scans as a single Duration static (not a bare int). Inverting the
// suffix-rune predicate's first `&&` makes the scanner bail before consuming
// the suffix, leaving an int literal.
func TestDurationLiteralScanning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lit  string
		want time.Duration
	}{
		{"100ms", 100 * time.Millisecond},
		{"5s", 5 * time.Second},
		{"2m", 2 * time.Minute},
		{"1h", time.Hour},
		{"1.5s", 1500 * time.Millisecond},
	}
	for _, c := range cases {
		s := durationRHS(t, c.lit)
		if s.Type != TypeDuration {
			t.Errorf("%q: Type = %v; want TypeDuration", c.lit, s.Type)
			continue
		}
		d, _ := s.Duration()
		if d != c.want {
			t.Errorf("%q: Duration = %v; want %v", c.lit, d, c.want)
		}
	}
}

// TestDurationPrometheusUnits pins that the Prometheus duration grammar is
// tried first: `1w`/`1d` are valid Prometheus durations but invalid Go
// durations, so the `err == nil` short-circuit in parseDuration must accept the
// Prometheus result. Negating it forces a fall-through to time.ParseDuration,
// which fails on these units and demotes them to a bare int.
func TestDurationPrometheusUnits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lit  string
		want time.Duration
	}{
		{"1w", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
	}
	for _, c := range cases {
		s := durationRHS(t, c.lit)
		if s.Type != TypeDuration {
			t.Fatalf("%q: Type = %v; want TypeDuration", c.lit, s.Type)
		}
		d, _ := s.Duration()
		if d != c.want {
			t.Errorf("%q: Duration = %v; want %v", c.lit, d, c.want)
		}
	}
}

// TestDurationSuffixConsumesExactly pins that scanning a duration consumes
// exactly the suffix runes and no more: the token immediately after a
// space-free duration must still be lexable. Shifting the
// `for i := 0; i < consumed; i++` scanner-advance bound to `<=` swallows one
// extra rune (here the closing `}`), breaking the parse.
func TestDurationSuffixConsumesExactly(t *testing.T) {
	t.Parallel()
	// No whitespace between the duration and the closing brace, so an
	// over-consume eats the `}` and the query fails to parse.
	expr, err := Parse(`{duration>100ms}`)
	if err != nil {
		t.Fatalf("Parse(`{duration>100ms}`): %v", err)
	}
	sf, ok := expr.Pipeline.Elements[0].(*SpansetFilter)
	if !ok {
		t.Fatalf("element = %T; want *SpansetFilter", expr.Pipeline.Elements[0])
	}
	bin, ok := sf.Expression.(*BinaryOperation)
	if !ok {
		t.Fatalf("expression = %T; want *BinaryOperation", sf.Expression)
	}
	s, _ := bin.RHS.(Static)
	if d, _ := s.Duration(); d != 100*time.Millisecond {
		t.Errorf("RHS = %v; want 100ms", bin.RHS)
	}
}

// TestParentScopedAttribute pins the parent-scope two-token form
// (`parent.span.foo`) the scope-prefix probe in tryScopeAttribute produces.
func TestParentScopedAttribute(t *testing.T) {
	t.Parallel()
	bin, ok := firstElem(t, `{ parent.resource.service.name = "x" }`).(*SpansetFilter)
	if !ok {
		t.Fatalf("not a *SpansetFilter")
	}
	op, ok := bin.Expression.(*BinaryOperation)
	if !ok {
		t.Fatalf("expression = %T", bin.Expression)
	}
	attr, ok := op.LHS.(Attribute)
	if !ok {
		t.Fatalf("LHS = %T; want Attribute", op.LHS)
	}
	if !attr.Parent {
		t.Error("Parent = false; want true")
	}
	if attr.Scope != AttributeScopeResource {
		t.Errorf("Scope = %v; want resource", attr.Scope)
	}
	if attr.Name != "service.name" {
		t.Errorf("Name = %q; want service.name", attr.Name)
	}
}
