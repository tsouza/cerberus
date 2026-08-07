package regression

// Pins the two strings.ReplaceAll calls that make telemetry.SanitizeForLog a
// barrier CodeQL can see.
//
// Every request-derived value cerberus logs goes through SanitizeForLog, which
// escapes the whole control-character range and is therefore correct at runtime
// however it is written. CodeQL's go/log-injection rule is narrower: it
// recognises exactly two sanitizers — a strings.ReplaceAll whose replaced
// string is "\n" or "\r", and an argument rendered with the %q directive — and
// it cannot see through a strings.Builder loop. Folding the two line
// terminators into that loop leaves the escaping intact and the analysis blind,
// so every call site is reported again as an unsanitised sink.
//
// The cost of that is not a warning: required conversation resolution means ten
// code-scanning review threads reopen across internal/api and the pull request
// cannot merge, with nothing red locally to explain why. This test is the local
// signal — it fails the moment the barrier form is refactored away, whatever
// the runtime behaviour of the replacement.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

const (
	// The file declaring the sanitizer, and the function whose body must
	// carry the barrier.
	logsafeFile = "../../internal/telemetry/logsafe.go"
	logsafeFunc = "SanitizeForLog"

	// The replaced strings go/log-injection accepts as a barrier. Any other
	// pair leaves the taint flowing.
	logsafeBarrierNewline = "\n"
	logsafeBarrierReturn  = "\r"
)

// TestSanitizeForLogKeepsTheRecognisedBarrier fails if SanitizeForLog stops
// escaping the line terminators through strings.ReplaceAll.
func TestSanitizeForLogKeepsTheRecognisedBarrier(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, logsafeFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", logsafeFile, err)
	}

	fn := findFuncDecl(file, logsafeFunc)
	if fn == nil {
		t.Fatalf("%s declares no %s — the sanitizer moved; move this pin with it", logsafeFile, logsafeFunc)
	}

	replaced := replaceAllOperands(fn)
	for _, want := range []string{logsafeBarrierNewline, logsafeBarrierReturn} {
		if !replaced[want] {
			t.Errorf("%s never calls strings.ReplaceAll(..., %q, ...): go/log-injection will report "+
				"every caller as an unsanitised log sink, blocking the merge on unresolvable "+
				"code-scanning threads", logsafeFunc, want)
		}
	}
}

// replaceAllOperands collects the string literals passed as the replaced
// operand of every strings.ReplaceAll call in fn.
func replaceAllOperands(fn *ast.FuncDecl) map[string]bool {
	const replacedArgIndex = 1 // strings.ReplaceAll(s, old, new)

	found := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) <= replacedArgIndex {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ReplaceAll" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "strings" {
			return true
		}
		lit, ok := call.Args[replacedArgIndex].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if unquoted, err := strconv.Unquote(lit.Value); err == nil {
			found[unquoted] = true
		}
		return true
	})
	return found
}
