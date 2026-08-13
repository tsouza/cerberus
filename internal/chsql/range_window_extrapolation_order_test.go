package chsql

import (
	"strings"
	"testing"
)

// fiveMinutesInSeconds is the `[5m]` range these tests extrapolate over, in
// the seconds Prom's `ms.Range.Seconds()` yields. It is the divisor `rate`
// applies and `increase` / `delta` do not, so it is what tells the three kinds
// apart in the assertions below.
const fiveMinutesInSeconds = 300.0

// fiveMinutesInNanos is the same range in the nanoseconds the fused path's
// window-start arithmetic takes.
const fiveMinutesInNanos int64 = 300_000_000_000

// TestExtrapolatedValueExpr_DividesFactorNotProduct pins WHERE the
// extrapolation correction's divisions land, which is a separate fact from
// whether they happen.
//
// Reference Prometheus (promql/functions.go, `extrapolatedRate`) builds the
// whole factor first and applies it in ONE multiplication:
//
//	factor := extrapolateToInterval / sampledInterval
//	if isRate { factor /= ms.Range.Seconds() }
//	resultFloat *= factor
//
// Cerberus multiplied by the undivided numerator and divided the product
// afterwards. That is the same real number and a different float64 — (a*b)/c
// and a*(b/c) disagree by an ulp on most inputs — so cerberus's answer drifted
// an ulp off reference on `rate`, `increase` AND `delta` over an ordinary
// float counter. The compat harness compares float samples under an epsilon
// and the txtar goldens compare cerberus against its own arithmetic, so
// neither layer could see it; the identical inversion on the histogram-valued
// path (#2090), whose bucket counts are compared against reference with no
// epsilon, is what made the class visible.
//
// The assertion reads the whole rendered shape rather than looking for a
// division somewhere in the text, because "a `/ sampled_interval` exists"
// passes on BOTH orders — it is exactly the assertion that would have let the
// bug through.
func TestExtrapolatedValueExpr_DividesFactorNotProduct(t *testing.T) {
	// Sentinel operands so the assertions read the SHAPE rather than the
	// mid-layer alias spellings the materialized path happens to use.
	const (
		cd = "cd" // counter_delta
		si = "si" // sampled_interval
		fv = "fv" // first_val
		lv = "lv" // last_val
		ds = "ds" // duration_to_start
		de = "de" // duration_to_end
	)

	// clampedStart is the counter zero-crossing clamp the rate / increase
	// kinds wrap duration_to_start in; delta (a gauge) reads it bare.
	const clampedStart = "if(" + cd + " > 0 AND " + fv + " >= 0, least(" +
		ds + ", " + si + " * " + fv + " / " + cd + "), " + ds + ")"

	tests := []struct {
		name string
		kind extrapolationKind
		// raw is the `resultFloat` the factor scales.
		raw string
		// factor is the single scalar reference multiplies by, INCLUDING
		// rate's own `/ range` — that this division is inside the factor
		// rather than outside the product is the whole point of the fix.
		factor string
	}{
		{
			name:   "rate divides the factor by the range",
			kind:   extrapolationKindRate,
			raw:    cd,
			factor: "(" + si + " + " + clampedStart + " + " + de + ") / " + si + " / 300",
		},
		{
			name:   "increase applies the same factor without the range division",
			kind:   extrapolationKindIncrease,
			raw:    cd,
			factor: "(" + si + " + " + clampedStart + " + " + de + ") / " + si,
		},
		{
			name:   "delta applies the same factor to the gauge difference",
			kind:   extrapolationKindDelta,
			raw:    "(" + lv + " - " + fv + ")",
			factor: "(" + si + " + " + ds + " + " + de + ") / " + si,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderFragToSQL(extrapolatedValueExpr(
				tt.kind, fiveMinutesInSeconds,
				BareIdent(cd), BareIdent(si), BareIdent(fv), BareIdent(lv), BareIdent(ds), BareIdent(de),
			))

			// The outer `if` is Prom's `len(samples.Floats) > 1` guard; the
			// parens around the factor are what keep the divisions inside it.
			want := "if(" + si + " > 0, " + tt.raw + " * (" + tt.factor + "), nan)"
			// inverted is the shape that shipped: the SAME terms with the
			// divisions trailing the product rather than building the factor,
			// which differs only by those parens and only by an ulp.
			inverted := "if(" + si + " > 0, " + tt.raw + " * " + tt.factor + ", nan)"

			if got == inverted {
				t.Fatalf("extrapolatedValueExpr(%v) divides the PRODUCT of the raw result and the "+
					"factor rather than the factor itself — reference computes `factor /= range` and "+
					"then `resultFloat *= factor`, and the two orders differ by an ulp in float64:\n  %s",
					tt.kind, got)
			}
			if got != want {
				t.Fatalf("extrapolatedValueExpr(%v) rendered\n  %s\nwant\n  %s", tt.kind, got, want)
			}
		})
	}
}

// TestExtrapolatedValue_BothPathsMultiplyOnce pins the operand order on BOTH
// callers of extrapolatedValueExpr — the materialized range-window path
// (operands = mid-layer column aliases) and the fused subquery path (operands
// = inline slice-derived expressions). The two are documented as emitting
// byte-identical arithmetic, so an assertion on only one of them would leave
// the other free to drift.
//
// The property asserted is structural rather than textual: the value branch of
// the `sampled_interval > 0` guard must hold exactly ONE top-level operator
// and it must be the multiplication (reference's single `resultFloat *=
// factor`). The inverted shape has three top-level operators for `rate`
// (`* / /`) and two for `increase` / `delta` (`* /`), so it cannot satisfy
// this. The multiplication's right operand is then checked to carry the
// divisions, which is where the ulp difference actually lives.
func TestExtrapolatedValue_BothPathsMultiplyOnce(t *testing.T) {
	kinds := []struct {
		name string
		kind extrapolationKind
		// wantFactorDivisions is how many top-level `/` the factor holds:
		// `/ sampled_interval` for every kind, plus `/ range` for rate.
		wantFactorDivisions int
	}{
		{name: "rate", kind: extrapolationKindRate, wantFactorDivisions: 2},
		{name: "increase", kind: extrapolationKindIncrease, wantFactorDivisions: 1},
		{name: "delta", kind: extrapolationKindDelta, wantFactorDivisions: 1},
	}

	e := &emitter{}
	paths := []struct {
		name string
		frag func(kind extrapolationKind) Frag
	}{
		{
			name: "materialized",
			frag: func(kind extrapolationKind) Frag {
				return extrapolatedValueFrag(kind, fiveMinutesInSeconds)
			},
		},
		{
			name: "fused",
			frag: func(kind extrapolationKind) Frag {
				return e.fusedExtrapolatedValueFrag(
					BareIdent("w"), BareIdent("a"), kind, fiveMinutesInNanos, fiveMinutesInSeconds, nil,
				)
			},
		},
	}

	for _, p := range paths {
		for _, k := range kinds {
			t.Run(p.name+"/"+k.name, func(t *testing.T) {
				value := guardedValueBranch(t, renderFragToSQL(p.frag(k.kind)))

				ops := topLevelOps(value)
				if len(ops) != 1 || ops[0] != "*" {
					t.Fatalf("value branch has top-level operators %v, want exactly one `*` — "+
						"reference applies the extrapolation factor in a SINGLE multiplication "+
						"(`resultFloat *= factor`), so a trailing division means the PRODUCT is being "+
						"divided, which is an ulp off reference.\nvalue: %s", ops, value)
				}

				_, factor := splitAtTopLevelOp(value, "*")
				if !strings.HasPrefix(factor, "(") || !strings.HasSuffix(factor, ")") {
					t.Fatalf("the multiplication's right operand is not a parenthesised factor — "+
						"without the parens the divisions below would trail the product instead: %s", factor)
				}

				divisions := 0
				for _, op := range topLevelOps(strings.TrimSuffix(strings.TrimPrefix(factor, "("), ")")) {
					if op == "/" {
						divisions++
					}
				}
				if divisions != k.wantFactorDivisions {
					t.Fatalf("factor holds %d top-level divisions, want %d — `/ sampled_interval` for "+
						"every kind, plus `/ range` for rate only.\nfactor: %s",
						divisions, k.wantFactorDivisions, factor)
				}
			})
		}
	}
}

// guardedValueBranch returns the `<value>` argument of the rendered
// `if(<sampled_interval > 0>, <value>, nan)` guard extrapolatedValueExpr wraps
// every kind in.
func guardedValueBranch(t *testing.T, sql string) string {
	t.Helper()

	body, ok := strings.CutPrefix(sql, "if(")
	if !ok || !strings.HasSuffix(body, ", nan)") {
		t.Fatalf("rendered value is not the `if(<guard>, <value>, nan)` shape: %s", sql)
	}
	body = strings.TrimSuffix(body, ")")

	// <cond>, <value>, nan.
	const guardArity = 3
	args := topLevelSplit(body)
	if len(args) != guardArity {
		t.Fatalf("guard has %d top-level arguments, want %d (<cond>, <value>, nan): %s",
			len(args), guardArity, sql)
	}
	return args[1]
}

// topLevelSplit splits a rendered argument list on the commas sitting at
// nesting depth zero, ignoring commas inside nested calls, subscripts and
// string literals.
func topLevelSplit(s string) []string {
	var parts []string
	start := 0
	forEachTopLevelByte(s, func(i int) {
		if s[i] == ',' {
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	})
	return append(parts, strings.TrimSpace(s[start:]))
}

// topLevelOps returns, in order, the binary arithmetic operators of s that sit
// at nesting depth zero. It is what distinguishes `raw * (num / si / range)`
// (one operator) from `raw * (num) / si / range` (three).
func topLevelOps(s string) []string {
	var ops []string
	forEachTopLevelByte(s, func(i int) {
		if op, ok := binaryOpAt(s, i); ok {
			ops = append(ops, op)
		}
	})
	return ops
}

// splitAtTopLevelOp splits s around its first depth-zero occurrence of the
// binary operator op, returning the two operands.
func splitAtTopLevelOp(s, op string) (left, right string) {
	at := -1
	forEachTopLevelByte(s, func(i int) {
		if at >= 0 {
			return
		}
		if got, ok := binaryOpAt(s, i); ok && got == op {
			at = i
		}
	})
	if at < 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:at]), strings.TrimSpace(s[at+1:])
}

// binaryOpAt reports whether index i of s holds a rendered binary arithmetic
// operator. binOp renders `<l> <op> <r>`, so the single surrounding spaces are
// what keeps a unary minus (Neg renders `-<f>`, no space) out.
func binaryOpAt(s string, i int) (string, bool) {
	if i == 0 || i+1 >= len(s) || s[i-1] != ' ' || s[i+1] != ' ' {
		return "", false
	}
	switch s[i] {
	case '*', '/', '+', '-', '%':
		return string(s[i]), true
	}
	return "", false
}

// forEachTopLevelByte walks s byte by byte, tracking parenthesis / bracket
// nesting and single-quoted string literals, and calls visit for each index
// that sits outside every nesting level and outside every literal.
func forEachTopLevelByte(s string, visit func(i int)) {
	depth, inString := 0, false
	for i := range len(s) {
		c := s[i]
		switch {
		case inString:
			if c == '\'' {
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case depth == 0:
			visit(i)
		}
	}
}
