package promql

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

const unknownMixedSelectorPolicy mixedOperandPolicy = 255

func TestMixedSelectorPolicyProductionSiteInventory(t *testing.T) {
	want := []string{
		"histogram_native_mixed_or_aggregate_topk.go:lowerTopKOverMixedExpHistogramSetOp:mixedTopKFamily/mixedRootAdmission",
		"lower.go:lowerLimitKInput:mixedLimitFamily/mixedOperandAdmission",
		"lower.go:lowerLimitKInput:mixedLimitFamily/mixedPlanAdmission",
		"lower.go:lowerTopK:mixedTopKFamily/mixedPlanAdmission",
		"lower.go:lowerTopKComputed:mixedTopKFamily/mixedPlanAdmission",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var got, legacy []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := goparser.ParseFile(fset, name, nil, goparser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := selectorPolicyCallee(call.Fun)
				family, site, selectorFamily := selectorPolicyExecutorKey(call)
				if callee == "executeMixedSelectorPolicy" {
					if len(call.Args) != 2 || !selectorFamily || site == "" {
						t.Errorf("%s:%s has an opaque selector executor call", name, function.Name.Name)
						return true
					}
					got = append(got, name+":"+function.Name.Name+":"+family+"/"+site)
					return true
				}
				if legacyFamily := selectorPolicyFamilyArgument(call); legacyFamily != "" {
					legacy = append(legacy, name+":"+function.Name.Name+":"+callee+"("+legacyFamily+")")
				}
				return true
			})
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selector executor sites =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if len(legacy) != 0 {
		t.Fatalf("selector families reached non-executor policy calls:\n  %s", strings.Join(legacy, "\n  "))
	}
}

func selectorPolicyExecutorKey(call *ast.CallExpr) (family, site string, selectorFamily bool) {
	if len(call.Args) > 0 {
		if ident, ok := call.Args[0].(*ast.Ident); ok {
			family = ident.Name
			selectorFamily = family == "mixedTopKFamily" || family == "mixedLimitFamily"
		}
	}
	if len(call.Args) > 1 {
		if ident, ok := call.Args[1].(*ast.Ident); ok {
			site = ident.Name
		}
	}
	return family, site, selectorFamily
}

func selectorPolicyFamilyArgument(call *ast.CallExpr) string {
	for _, argument := range call.Args {
		var family string
		ast.Inspect(argument, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				if typed.Name == "mixedTopKFamily" || typed.Name == "mixedLimitFamily" {
					family = typed.Name
					return false
				}
			case *ast.CallExpr:
				if selectorPolicyCallee(typed.Fun) == "mixedAggregateFamily" {
					family = "mixedAggregateFamily"
					return false
				}
			}
			return family == ""
		})
		if family != "" {
			return family
		}
	}
	return ""
}

func TestSelectorPolicyFamilyArgumentRecognizesLegacyForms(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   string
	}{
		{"requireMixedPlanPolicy(input, mixedTopKFamily)", "mixedTopKFamily"},
		{"requireMixedPlanPolicy(input, mixedAggregateFamily(a.Op))", "mixedAggregateFamily"},
		{"requireMixedPlanPolicy(input, mixedMathFamily)", ""},
	} {
		t.Run(tc.source, func(t *testing.T) {
			expr, err := goparser.ParseExpr(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			call, ok := expr.(*ast.CallExpr)
			if !ok {
				t.Fatalf("expression = %T, want call", expr)
			}
			if got := selectorPolicyFamilyArgument(call); got != tc.want {
				t.Fatalf("selector family = %q, want %q", got, tc.want)
			}
		})
	}
}

func selectorPolicyCallee(expr ast.Expr) string {
	switch callee := expr.(type) {
	case *ast.Ident:
		return callee.Name
	case *ast.SelectorExpr:
		return callee.Sel.Name
	default:
		return "<opaque>"
	}
}

func TestMixedSelectorPolicyExecutorModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		family    mixedWrapperFamily
		site      mixedAdmissionSite
		floatOnly bool
	}{
		{"ranked root", mixedTopKFamily, mixedRootAdmission, true},
		{"ranked plan", mixedTopKFamily, mixedPlanAdmission, true},
		{"limit operand", mixedLimitFamily, mixedOperandAdmission, false},
		{"limit plan", mixedLimitFamily, mixedPlanAdmission, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &chplan.VectorSetOp{Mixed: true}
			transform, err := executeMixedSelectorPolicy(tc.family, tc.site)
			if err != nil {
				t.Fatal(err)
			}
			got := transform(input)
			if tc.floatOnly {
				if !chplan.IsMixedFloatNarrowing(got) {
					t.Fatalf("ranked result = %#v, want mixed float narrowing", got)
				}
			} else if got != input {
				t.Fatalf("limit result = %#v, want exact loaded plan %#v", got, input)
			}
		})
	}
}

func TestMixedSelectorRootPolicyPrecedesBoolValidation(t *testing.T) {
	key := mixedWrapperKey{family: mixedTopKFamily, site: mixedRootAdmission}
	wrong := mixedPreserve
	for _, tc := range []struct {
		name     string
		injected *mixedOperandPolicy
	}{
		{name: "missing"},
		{name: "wrong", injected: &wrong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior, existed := mixedOperandPolicies[key]
			if tc.injected == nil {
				delete(mixedOperandPolicies, key)
			} else {
				mixedOperandPolicies[key] = *tc.injected
			}
			t.Cleanup(func() {
				if existed {
					mixedOperandPolicies[key] = prior
				} else {
					delete(mixedOperandPolicies, key)
				}
			})

			_, err := lowerTopKOverMixedExpHistogramSetOp(
				&promparser.AggregateExpr{Op: promparser.TOPK},
				&promparser.BinaryExpr{ReturnBool: true},
				schema.Metrics{},
				lowerCtx{},
			)
			if err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted for topk-bottomk at root") {
				t.Fatalf("root policy did not reject before bool validation: %v", err)
			}
		})
	}
}

func TestMixedSelectorPolicyExecutorRejectsEveryWrongMode(t *testing.T) {
	for _, key := range []mixedWrapperKey{
		{family: mixedTopKFamily, site: mixedRootAdmission},
		{family: mixedTopKFamily, site: mixedPlanAdmission},
		{family: mixedLimitFamily, site: mixedOperandAdmission},
		{family: mixedLimitFamily, site: mixedPlanAdmission},
	} {
		expected := mixedFloatOnly
		if key.family == mixedLimitFamily {
			expected = mixedPreserve
		}
		for _, mode := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedFloatOnly, mixedPreserve, mixedPolicyClosed, unknownMixedSelectorPolicy} {
			if mode == expected {
				continue
			}
			t.Run(string(key.family)+"/"+string(key.site)+"/mode", func(t *testing.T) {
				assertMixedSelectorPolicyRejected(t, key, &mode)
			})
		}
		t.Run(string(key.family)+"/"+string(key.site)+"/missing", func(t *testing.T) {
			assertMixedSelectorPolicyRejected(t, key, nil)
		})
	}
}

func TestMixedSelectorPolicyExecutorRejectsInventedPairsDespiteTableInjection(t *testing.T) {
	for _, tc := range []struct {
		key  mixedWrapperKey
		mode mixedOperandPolicy
	}{
		{mixedWrapperKey{family: mixedTopKFamily, site: mixedOperandAdmission}, mixedFloatOnly},
		{mixedWrapperKey{family: mixedLimitFamily, site: mixedRootAdmission}, mixedPreserve},
		{mixedWrapperKey{family: mixedTopKFamily, site: "invented-site"}, mixedFloatOnly},
		{mixedWrapperKey{family: "invented-family", site: mixedPlanAdmission}, mixedPreserve},
		// This is a real, valid table row for another family. Selector policy
		// remains closed even without test-only table injection.
		{mixedWrapperKey{family: mixedMathFamily, site: mixedRootAdmission}, mixedFloatOnly},
	} {
		t.Run(string(tc.key.family)+"/"+string(tc.key.site), func(t *testing.T) {
			assertMixedSelectorPolicyRejected(t, tc.key, &tc.mode)
		})
	}
}

func assertMixedSelectorPolicyRejected(t *testing.T, key mixedWrapperKey, injected *mixedOperandPolicy) {
	t.Helper()
	prior, existed := mixedOperandPolicies[key]
	if injected == nil {
		delete(mixedOperandPolicies, key)
	} else {
		mixedOperandPolicies[key] = *injected
	}
	t.Cleanup(func() {
		if existed {
			mixedOperandPolicies[key] = prior
		} else {
			delete(mixedOperandPolicies, key)
		}
	})
	transform, err := executeMixedSelectorPolicy(key.family, key.site)
	if transform != nil || err == nil {
		t.Fatalf("rejected policy transform=%v err=%v", transform, err)
	}
}
