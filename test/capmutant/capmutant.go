// Package capmutant adjudicates the `ARITHMETIC_BASE` mutants gremlins emits
// on a `make([]T, 0, <hint>)` capacity argument.
//
// "An `ARITHMETIC_BASE` mutant on a `make` capacity argument is equivalent" is
// the single most attractive wrong claim in the mutation lane's adjudication
// history, and `docs/test-strategy.md`'s "When a capacity mutant is equivalent"
// states the rule it is wrong about: such a mutant survives only where the
// slice never escapes its builder, OR the finished slice no longer reports the
// hint. Where the slice DOES escape to a surface a test can reach and the
// appends do not grow past the pre-allocation, `cap` reads the mutated
// arithmetic straight back and a plain assertion kills the substitution.
//
// [AssertKilled] is that assertion, plus the proof that it discriminates.
// A `cap` assertion on its own is a claim about arithmetic nobody re-ran: it
// passes on a mutant whose shifted hint happens to land back on the true
// capacity through `append`'s growth schedule, and prose cannot rule that out
// for itself. So the caller hands over the hint's operator positions and a
// replay of the builder's append sequence, and every mutant gremlins emits on
// that hint is required to move the finished capacity.
//
// Two hints in `internal/logql/detected_level.go`'s `detectedLevelSourceExpr`
// are the worked instances the rule was derived from, and they earn opposite
// verdicts under the same mutator: `keys := make([]string, 0,
// len(allowedLevelFields)+1)` never leaves the function, while `args :=
// make([]chplan.Expr, 0, len(keys)*2+1)` becomes the returned `FuncCall`'s
// exported `Args`.
package capmutant

import "testing"

// Substitution is the `ARITHMETIC_BASE` operator table, and it is a MAP rather
// than a set of alternatives on purpose: gremlins rewrites each arithmetic
// token to exactly ONE other token, so a hint spelling P arithmetic operators
// yields exactly P mutants and a completed run reports exactly P verdicts on
// its line. The table mirrors `tokenMutations[mutator.ArithmeticBase]` in the
// pinned `tsouza/gremlins` fork (`internal/engine/mappings.go`), which
// `.github/workflows/mutation.yml` installs by version.
//
// Enumerating the four other operators per position instead would adjudicate
// mutants no run emits — and the verdicts differ: `internal/promql/
// schema_lookup.go`'s `len(pairs)*2` is unkillable under the `/` gremlins
// actually writes and killable under a `+` it never writes. An
// over-approximation there reports a kill the lane cannot collect.
var Substitution = map[string]string{
	"+": "-",
	"-": "+",
	"*": "/",
	"/": "*",
	"%": "*",
}

// mulPrecedence reports whether op binds tighter than `+` and `-`, i.e.
// whether Go's precedence rules group it first. [Eval] needs this because a
// mutation can MOVE a multiplicative operator: rewriting the second `*` of
// `a*b*c` to `/` keeps the grouping, but rewriting the `-` of `a-b*c`
// would not, and answering a different arithmetic than the compiler does would
// adjudicate a mutant nobody compiled.
func mulPrecedence(op string) bool { return op == "*" || op == "/" || op == "%" }

// Apply evaluates `a op b` for the operators `ARITHMETIC_BASE` can produce and
// reports whether the result is defined. `/` and `%` by zero are NOT: the
// mutant that produces them panics before `make` is ever reached, which kills
// it in any test that exercises the builder at all. Reporting that rather than
// panicking here is what lets [AssertKilled] count such a mutant as killed
// instead of crashing the enumeration.
func Apply(t testing.TB, a int, op string, b int) (int, bool) {
	t.Helper()

	switch op {
	case "+":
		return a + b, true
	case "-":
		return a - b, true
	case "*":
		return a * b, true
	case "/":
		if b == 0 {
			return 0, false
		}
		return a / b, true
	case "%":
		if b == 0 {
			return 0, false
		}
		return a % b, true
	}
	t.Fatalf("capmutant: unknown arithmetic operator %q", op)
	return 0, false
}

// Eval evaluates `operands[0] ops[0] operands[1] … ops[n-1] operands[n]` under
// Go's own operator precedence, and reports whether the result is defined.
//
// A hint written with parentheses — `(n+1)*2` — is evaluated as one Eval call
// per parenthesised group by its caller, because the parentheses fix a grouping
// no operator substitution can move.
func Eval(t testing.TB, operands []int, ops []string) (int, bool) {
	t.Helper()

	if len(ops) != len(operands)-1 {
		t.Fatalf("capmutant: %d operands need %d operators, got %d",
			len(operands), len(operands)-1, len(ops))
	}

	// Pass one collapses every multiplicative operator, leaving a list of
	// terms joined only by `+` and `-`; pass two folds those left to right.
	terms := []int{operands[0]}
	var addOps []string
	for i, op := range ops {
		if mulPrecedence(op) {
			v, ok := Apply(t, terms[len(terms)-1], op, operands[i+1])
			if !ok {
				return 0, false
			}
			terms[len(terms)-1] = v
			continue
		}
		terms = append(terms, operands[i+1])
		addOps = append(addOps, op)
	}

	acc := terms[0]
	for i, op := range addOps {
		v, ok := Apply(t, acc, op, terms[i+1])
		if !ok {
			return 0, false
		}
		acc = v
	}
	return acc, true
}

// Position names one arithmetic operator position of a capacity hint, with the
// operator the unmutated source spells there. The list of them IS the mutant
// set under adjudication, so it has to mirror the cited construct operator for
// operator: a position left out is a mutant nobody enumerated, and a position
// invented is a verdict on a mutant no run reports.
type Position struct {
	// Name describes the position the way a reader of the source would — "the
	// `*2`", "the trailing `+1`" — so a survivor is reported by name.
	Name string
	// Op is the operator the unmutated hint spells at this position.
	Op string
}

// Hint is one `make(..., 0, <expr>)` capacity argument under adjudication.
type Hint struct {
	// Construct cites the capacity argument in the construct form
	// `.github/scripts/verify-code-citations.mjs` resolves — the source file's
	// base name, a colon, and the backticked expression, scoped to a function
	// name when the expression repeats in the file. It is quoted in every
	// failure this file reports, so a survivor names the source site rather
	// than a test-local variable.
	Construct string

	// Positions are the hint's arithmetic operator positions, in the order
	// Eval receives them.
	Positions []Position

	// Eval evaluates the hint with ops[i] substituted into Positions[i], and
	// reports whether the result is defined. [Eval] is the workhorse.
	Eval func(t testing.TB, ops []string) (int, bool)

	// Observe builds the real value the way production does and reads back the
	// length and capacity of the slice the hint sized, through the escape
	// surface the calling test's note names. This is the half that makes the
	// adjudication a kill: it is what fails on the mutated binary.
	Observe func(t *testing.T) (length, capacity int)

	// Build replays the builder's append sequence against a slice
	// pre-allocated with `hint`, reporting the finished length and capacity.
	//
	// It MUST append the builder's REAL element type in the builder's real
	// grouping. Go rounds a growing slice's capacity up to an allocator size
	// class measured in BYTES and the growth schedule depends on the order and
	// grouping of the appends, so a replay that appends a different type, or
	// the same total in one call, answers a question nobody asked.
	// [AssertKilled] cross-checks the replay against Observe rather than
	// trusting that, so a replay that has drifted from its builder is reported
	// instead of quietly standing in for it.
	Build func(hint int) (length, capacity int)
}

// AssertKilled adjudicates h: it asserts that the real value's capacity reads
// the unmutated hint back, and that every `ARITHMETIC_BASE` mutant gremlins
// emits in the hint moves that capacity — so the assertion is a kill for the
// whole mutant set rather than for the subset that happens to shift it.
//
// A mutant whose hint goes negative, or whose `/` divides by zero, panics
// inside `make` or before it and so dies in Observe itself rather than in the
// capacity comparison. Those are counted and reported separately, because a
// run where every mutant fell into that bucket would mean the capacity
// comparison never ran and the note above the test should say so.
func AssertKilled(t *testing.T, h Hint) {
	t.Helper()

	if len(h.Positions) == 0 {
		t.Fatalf("capmutant: %s enumerates no operator positions, so it "+
			"adjudicates no mutant at all", h.Construct)
	}

	orig := make([]string, len(h.Positions))
	for i, p := range h.Positions {
		if _, ok := Substitution[p.Op]; !ok {
			t.Fatalf("capmutant: %s names operator %q at %s, which ARITHMETIC_BASE "+
				"does not rewrite", h.Construct, p.Op, p.Name)
		}
		orig[i] = p.Op
	}
	trueHint, ok := h.Eval(t, orig)
	if !ok {
		t.Fatalf("capmutant: %s does not evaluate under its own operators — the "+
			"unmutated hint divides by zero, so the operands this test drives the "+
			"builder with cannot adjudicate anything", h.Construct)
	}

	obsLen, obsCap := h.Observe(t)

	// The kill. `make([]T, 0, N)` hands back a slice whose cap is exactly N,
	// and a builder that does not grow past the pre-allocation still reports it
	// when it is done — so cap reads the hint's arithmetic straight back.
	if obsCap != trueHint {
		t.Fatalf("cap of the slice %s sizes = %d; want %d (its own hint). The appends "+
			"have grown past the pre-allocation, so cap no longer reads the hint "+
			"back and asserting it has stopped being a capacity kill",
			h.Construct, obsCap, trueHint)
	}

	// …and the replay below has to be the builder this test just observed, or
	// every mutant verdict it produces describes a slice nobody builds.
	repLen, repCap := h.Build(trueHint)
	if repLen != obsLen || repCap != obsCap {
		t.Fatalf("the append replay for %s finishes at len %d / cap %d, but the real "+
			"builder finishes at len %d / cap %d — the replay has drifted from the "+
			"builder it stands in for", h.Construct, repLen, repCap, obsLen, obsCap)
	}

	distinguished, panicking := 0, 0
	for i, pos := range h.Positions {
		op := Substitution[pos.Op]

		ops := append([]string(nil), orig...)
		ops[i] = op
		hint, defined := h.Eval(t, ops)
		if !defined || hint < 0 {
			// `make` panics on a negative capacity, and a `/` by zero panics
			// before it is reached: either way the mutant dies inside Observe.
			panicking++
			continue
		}
		distinguished++

		if _, got := h.Build(hint); got == obsCap {
			t.Errorf("rewriting %s of %s to %q gives capacity hint %d, and the finished "+
				"slice still ends at cap %d — identical to the unmutated build, so "+
				"asserting cap does NOT kill this mutant", pos.Name, h.Construct, op, hint, got)
		}
	}

	// Anti-vacuity: the enumeration has to cover one mutant per operator
	// position, which is the number of verdicts a completed run reports on this
	// hint's line.
	if want := len(h.Positions); distinguished+panicking != want {
		t.Fatalf("adjudicated %d mutants of %s, want %d (one per arithmetic operator "+
			"position) — the enumeration is not covering the mutant set it claims to",
			distinguished+panicking, h.Construct, want)
	}
	t.Logf("%s: %d mutants adjudicated, %d distinguished by capacity, %d killed by a "+
		"panicking make() capacity", h.Construct, distinguished+panicking, distinguished, panicking)
}
