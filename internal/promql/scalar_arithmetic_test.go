package promql

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

// This constructor pins the two projection contracts before scalar arithmetic
// shared its kernel. It intentionally does not call the new finalizer or value
// builder: changes to either must still preserve the old aliases and topology.
func scalarArithmeticPriorProjection(inner chplan.Node, arg parser.Expr, s schema.Metrics, ctx lowerCtx, op chplan.BinaryOp, scalar float64, left, direct bool) chplan.Node {
	build := func(value chplan.Expr) chplan.Expr {
		literal := &chplan.LitFloat{V: scalar}
		if left {
			return &chplan.Binary{Op: op, Left: literal, Right: value}
		}
		return &chplan.Binary{Op: op, Left: value, Right: literal}
	}
	if direct {
		return &chplan.Project{Roles: metricRoles(s), Input: mixedDiscriminatorFilter(inner, mixedDiscriminatorFloat), Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: build(&chplan.ColumnRef{Name: s.ValueColumn}), Alias: s.ValueColumn},
		}}
	}
	inner = mixedRowsFloatOnly(inner)
	inner = guardNameDropCollision(inner, arg, s, ctx)
	return projectValueOverInner(inner, s, legacySampleProjectionLayout(inner), func(refs sampleRoleRefs) chplan.Expr { return build(refs.Value) })
}

func assertScalarArithmeticPlansEqual(t *testing.T, got, want chplan.Node) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("arithmetic plan differs from prior projection: got %T, want %T", got, want)
	}
	for _, optimize := range []bool{false, true} {
		a, b := got, want
		if optimize {
			a = spec.AssertScanTimeBoundAccepts(t, chplan.CloneNode(got))
			b = spec.AssertScanTimeBoundAccepts(t, chplan.CloneNode(want))
			if !a.Equal(b) {
				t.Fatal("optimized arithmetic plans differ")
			}
		}
		aSQL, aArgs, err := chsql.Emit(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		bSQL, bArgs, err := chsql.Emit(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		if aSQL != bSQL || fmt.Sprintf("%#v", aArgs) != fmt.Sprintf("%#v", bArgs) {
			t.Fatalf("SQL/args changed (optimized=%v)\ngot %s %v\nwant %s %v", optimize, aSQL, aArgs, bSQL, bArgs)
		}
	}
}

func TestScalarArithmeticPriorBoundaryEquivalence(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "labels_map", "sample_time", "sample_value"
	for _, s := range []schema.Metrics{standard, custom} {
		for _, step := range []time.Duration{0, time.Minute} {
			ctx := lowerCtx{start: at.Add(-2 * step), end: at, step: step, lowerers: RangeLowerers{}.withDefaults()}
			for _, operand := range []string{
				`latency_exp_hist or num_cpus`, `num_cpus or latency_exp_hist`,
				`sort_by_label(latency_exp_hist or num_cpus, "job")`,
				`sort_by_label(num_cpus or latency_exp_hist, "job")`,
				`num_cpus`, `{__name__=~"cpu_a|cpu_b"}`, `sum_over_time(num_cpus[5m])`,
				`latency_exp_hist offset 1m or num_cpus`,
				`latency_exp_hist or num_cpus offset 1m`,
				`num_cpus offset 1m`, `num_cpus @ 1767225600`,
				`sum_over_time(num_cpus[5m] offset 1m)`,
				`sum_over_time(num_cpus[5m] @ 1767225600)`,
				`latency_exp_hist + 1`,
			} {
				for _, op := range []chplan.BinaryOp{chplan.OpAdd, chplan.OpSub, chplan.OpMod, chplan.OpPow, chplan.OpAtan2, chplan.OpDiv} {
					for _, left := range []bool{false, true} {
						if mixedScalarBinaryFamily(op, left) != mixedArithmeticFamily {
							continue // Histogram scaling is a different family.
						}
						t.Run(fmt.Sprintf("%s/%s/%s/%s/left=%v", s.ValueColumn, step, operand, op, left), func(t *testing.T) {
							expr, err := p.ParseExpr(operand)
							if err != nil {
								t.Fatal(err)
							}
							setOp, direct := mixedExpHistogramSetOp(expr, s, ctx)
							var inner, got chplan.Node
							const scalar = -3.0
							if direct {
								inner, err = lowerMixedExpHistogramSetOp(setOp, s, ctx)
								if err == nil {
									got, err = lowerArithmeticOverMixedExpHistogramSetOp(setOp, op, scalar, left, s, ctx)
								}
							} else {
								inner, err = lowerScalarBinopOperand(expr, s, ctx)
								if err == nil {
									got, err = lowerVectorScalar(expr, s, op, scalar, left, false, ctx)
								}
							}
							if err != nil {
								t.Fatal(err)
							}
							want := scalarArithmeticPriorProjection(inner, expr, s, ctx, op, scalar, left, direct)
							assertScalarArithmeticPlansEqual(t, got, want)
						})
					}
				}
			}
		}
	}
}

func TestScalarArithmeticPolicyBeforeLoadAndProjection(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, site := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
		for _, mode := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedPreserve} {
			t.Run(fmt.Sprintf("%s/%d", site, mode), func(t *testing.T) {
				key := mixedWrapperKey{family: mixedArithmeticFamily, site: site}
				previous := mixedOperandPolicies[key]
				mixedOperandPolicies[key] = mode
				t.Cleanup(func() { mixedOperandPolicies[key] = previous })
				if site == mixedRootAdmission {
					called := false
					plan, err := lowerArithmeticRoot(func() (chplan.Node, error) {
						called = true
						return nil, errors.New("must not lower")
					})
					if called || plan != nil || err == nil {
						t.Fatalf("denied root invoked loader: %v %T %v", called, plan, err)
					}
				}
				boundary := scalarArithmeticGuarded
				if site == mixedRootAdmission {
					boundary = scalarArithmeticCanonical
				}
				// Malformed roles would panic in the projection callback; policy
				// rejection must happen before narrowing or role resolution.
				plan, err := finishScalarArithmetic(&chplan.VectorSetOp{Mixed: true}, nil, s, lowerCtx{}, chplan.OpAdd, 1, false, boundary)
				if plan != nil || err == nil {
					t.Fatalf("denied projection continued: %T %v", plan, err)
				}
			})
		}
	}
	wantError := errors.New("histogram operand error")
	calls := 0
	_, err := lowerArithmeticRoot(func() (chplan.Node, error) { calls++; return nil, wantError })
	if calls != 1 || err != wantError {
		t.Fatalf("loader error changed: calls=%d error=%v", calls, err)
	}
}

func TestScalarArithmeticRecognizerBoundary(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{`(latency_exp_hist or num_cpus) + (1 + 2)`, true},
		{`(1 + 2) / (latency_exp_hist or num_cpus)`, true},
		{`(latency_exp_hist or num_cpus) * 2`, false},
		{`(latency_exp_hist or num_cpus) / 2`, false},
		{`(latency_exp_hist or num_cpus) > 2`, false},
		{`(latency_exp_hist or num_cpus) > bool 2`, false},
		{`(latency_exp_hist or num_cpus) + scalar(vector(2))`, false},
		{`(latency_exp_hist or num_cpus) + time()`, false},
		{`abs((latency_exp_hist or num_cpus) + 2)`, false},
	} {
		expr, err := p.ParseExpr(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, _, ok := arithmeticOverMixedExpHistogramSetOp(expr, s, lowerCtx{})
		if ok != tc.want {
			t.Errorf("recognition %s = %v, want %v", tc.query, ok, tc.want)
		}
	}
	// A removed row must deny real dispatch, not only the executor helper.
	for _, tc := range []struct {
		query string
		site  mixedAdmissionSite
	}{
		{`(latency_exp_hist or num_cpus) + 2`, mixedRootAdmission},
		{`sort_by_label(latency_exp_hist or num_cpus, "job") + 2`, mixedPlanAdmission},
	} {
		t.Run(tc.query, func(t *testing.T) {
			key := mixedWrapperKey{family: mixedArithmeticFamily, site: tc.site}
			old := mixedOperandPolicies[key]
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = old })
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Lower(context.Background(), expr, s)
			if err == nil || !strings.Contains(err.Error(), "mixed operand is not admitted for scalar-arithmetic at "+string(tc.site)) {
				t.Fatalf("actual dispatch bypassed policy: %v", err)
			}
		})
	}
}

func TestScalarArithmeticLiteralBitsAndPolarity(t *testing.T) {
	value := &chplan.ColumnRef{Name: "opaque_value"}
	for _, scalar := range []float64{0, math.Copysign(0, -1), math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, left := range []bool{false, true} {
			got := scalarBinaryValue(value, chplan.OpSub, scalar, left).(*chplan.Binary)
			vector, literal := got.Left, got.Right
			if left {
				vector, literal = got.Right, got.Left
			}
			if vector != value || got.Op != chplan.OpSub {
				t.Fatalf("operand polarity changed: %#v", got)
			}
			if bits := math.Float64bits(literal.(*chplan.LitFloat).V); bits != math.Float64bits(scalar) {
				t.Fatalf("scalar bits changed: got %x, want %x", bits, math.Float64bits(scalar))
			}
		}
	}
}

func TestScalarArithmeticHistogramOnlyKeepsDroppingRecognizer(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, step := range []time.Duration{0, time.Minute} {
		ctx := lowerCtx{start: at.Add(-2 * step), end: at, step: step, lowerers: RangeLowerers{}.withDefaults()}
		for _, op := range []chplan.BinaryOp{chplan.OpAdd, chplan.OpSub, chplan.OpMod, chplan.OpPow, chplan.OpAtan2, chplan.OpDiv} {
			for _, left := range []bool{false, true} {
				if mixedScalarBinaryFamily(op, left) != mixedArithmeticFamily {
					continue
				}
				query := "latency_exp_hist " + string(op) + " (-3)"
				if left {
					query = "(-3) " + string(op) + " latency_exp_hist"
				}
				t.Run(fmt.Sprintf("%s/%s", step, query), func(t *testing.T) {
					expr, err := p.ParseExpr(query)
					if err != nil {
						t.Fatal(err)
					}
					histSide, matched := expHistogramDroppingScalarBinop(expr, s, ctx)
					if !matched {
						t.Fatal("existing histogram drop recognizer did not match")
					}
					// Root admission uses the histogram-preserving empty Filter,
					// not the float envelope the nested dropping helper adds.
					want, err := lowerExpHistogramScalarBinop(histSide, "", nil, s, ctx, true)
					if err != nil {
						t.Fatal(err)
					}
					got, err := LowerAtRange(context.Background(), expr, s, ctx.start, ctx.end, step)
					if err != nil {
						t.Fatal(err)
					}
					assertScalarArithmeticPlansEqual(t, got, want)
				})
			}
		}
	}
}

func TestScalarArithmeticUnknownBoundaryFailsBeforeInput(t *testing.T) {
	const unknownBoundary scalarArithmeticBoundary = 255
	plan, err := finishScalarArithmetic(nil, nil, schema.DefaultOTelMetrics(), lowerCtx{}, chplan.OpAdd, 1, false, unknownBoundary)
	if plan != nil || err == nil || !strings.Contains(err.Error(), "unknown scalar arithmetic projection boundary") {
		t.Fatalf("unknown boundary continued: plan=%T error=%v", plan, err)
	}
}

func TestScalarArithmeticOrdinaryFloatIndependentOfMixedPolicy(t *testing.T) {
	for _, site := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
		key := mixedWrapperKey{family: mixedArithmeticFamily, site: site}
		old := mixedOperandPolicies[key]
		delete(mixedOperandPolicies, key)
		t.Cleanup(func() { mixedOperandPolicies[key] = old })
	}
	s := schema.DefaultOTelMetrics()
	inner := sampleForwardTestInput(metricRoles(s)...)
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	arg, err := p.ParseExpr("num_cpus")
	if err != nil {
		t.Fatal(err)
	}
	for _, left := range []bool{false, true} {
		got, err := finishScalarArithmetic(inner, arg, s, lowerCtx{}, chplan.OpSub, 1, left, scalarArithmeticGuarded)
		if err != nil {
			t.Fatalf("ordinary float inherited mixed denial: %v", err)
		}
		want := scalarArithmeticPriorProjection(inner, arg, s, lowerCtx{}, chplan.OpSub, 1, left, false)
		if !got.Equal(want) {
			t.Fatal("ordinary float projection changed while mixed policies were denied")
		}
	}
}
