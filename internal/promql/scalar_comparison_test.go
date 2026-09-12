package promql

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Frozen direct constructor predates the shared finalizer. It deliberately does
// not use scalarBinaryValue or the sample-role projector.
func scalarComparisonPriorCanonical(inner chplan.Node, s schema.Metrics, op chplan.BinaryOp, scalar float64, scalarOnLeft, returnBool bool) (chplan.Node, error) {
	floatRowsOnly := mixedDiscriminatorFilter(inner, mixedDiscriminatorFloat)

	valueRef := chplan.Expr(&chplan.ColumnRef{Name: s.ValueColumn})
	scalarLit := chplan.Expr(&chplan.LitFloat{V: scalar})
	var predicate chplan.Expr
	if scalarOnLeft {
		predicate = &chplan.Binary{Op: op, Left: scalarLit, Right: valueRef}
	} else {
		predicate = &chplan.Binary{Op: op, Left: valueRef, Right: scalarLit}
	}

	if !returnBool {
		filtered := &chplan.Filter{Input: floatRowsOnly, Predicate: predicate}
		return &chplan.Project{
			Roles: metricRoles(s),
			Input: filtered,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
				{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
				{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
				{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
			},
		}, nil
	}

	newValue := &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{predicate}}
	return &chplan.Project{
		Roles: metricRoles(s),
		Input: floatRowsOnly,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}, nil
}

func TestScalarComparisonPredicateResolvesValueRole(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	const comparisonThreshold = 5
	const sourceValue = "source_value"
	for _, boundary := range []scalarComparisonBoundary{scalarComparisonGuarded, scalarComparisonCanonical} {
		for _, scalarOnLeft := range []bool{false, true} {
			for _, decoy := range []bool{false, true} {
				for _, projected := range []bool{false, true} {
					t.Run(fmt.Sprintf("boundary=%d/left=%t/decoy=%t/projected=%t", boundary, scalarOnLeft, decoy, projected), func(t *testing.T) {
						columns := []chplan.Column{
							{Name: "source_name", Role: chplan.RoleMetricName},
							{Name: "source_attrs", Role: chplan.RoleAttributes},
							{Name: "source_time", Role: chplan.RoleTimestamp},
							{Name: sourceValue, Role: chplan.RoleValue},
						}
						if decoy {
							columns = append(columns, chplan.Column{Name: "decoy_value", Role: chplan.RoleOpaque})
						}
						var input chplan.Node = sampleForwardTestInput(columns...)
						if projected {
							projections := make([]chplan.Projection, len(columns))
							for i, column := range columns {
								projections[i] = chplan.Projection{Expr: &chplan.ColumnRef{Name: column.Name}, Alias: column.Name}
							}
							input = &chplan.Project{Input: input, Projections: projections, Roles: columns}
						}
						plan, err := finishScalarComparison(input, nil, s, lowerCtx{}, chplan.OpGt, comparisonThreshold, scalarOnLeft, false, boundary)
						if err != nil {
							t.Fatal(err)
						}
						filtered := plan
						if boundary == scalarComparisonCanonical {
							project, ok := plan.(*chplan.Project)
							if !ok {
								t.Fatalf("canonical boundary = %T, want Project", plan)
							}
							filtered = project.Input
							if !project.Projections[0].Expr.Equal(&chplan.ColumnRef{Name: "source_name"}) ||
								!project.Projections[len(project.Projections)-1].Expr.Equal(&chplan.ColumnRef{Name: sourceValue}) {
								t.Fatal("non-bool comparison changed the original name or sample value")
							}
						} else if !plan.RowType().Equal(input.RowType()) {
							t.Fatal("guarded comparison changed the input schema")
						}
						filter, ok := filtered.(*chplan.Filter)
						if !ok || filter.Input != input {
							t.Fatalf("comparison boundary = %T, want Filter over original input", filtered)
						}
						var left, right chplan.Expr = &chplan.ColumnRef{Name: sourceValue}, &chplan.LitFloat{V: comparisonThreshold}
						if scalarOnLeft {
							left, right = right, left
						}
						want := &chplan.Binary{Op: chplan.OpGt, Left: left, Right: right}
						if !filter.Predicate.Equal(want) {
							t.Fatalf("predicate binds configured name %q instead of RoleValue %q: %#v", s.ValueColumn, sourceValue, filter.Predicate)
						}
					})
				}
			}
		}
	}
}

func TestScalarComparisonValueRefRejectsNonValueAndAmbiguousProjects(t *testing.T) {
	t.Parallel()

	input := sampleForwardTestInput(
		chplan.Column{Name: "labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: "value", Role: chplan.RoleValue},
		chplan.Column{Name: "other_value", Role: chplan.RoleValue},
	)
	project := func(columns ...chplan.Column) *chplan.Project {
		projections := make([]chplan.Projection, len(columns))
		for i, column := range columns {
			projections[i] = chplan.Projection{Expr: &chplan.ColumnRef{Name: column.Name}, Alias: column.Name}
		}
		return &chplan.Project{Input: input, Roles: columns, Projections: projections}
	}

	valid := project(
		chplan.Column{Name: "labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: "value", Role: chplan.RoleValue},
	)
	if got := scalarComparisonValueRef(valid); got.Name != "value" {
		t.Fatalf("value ref = %q, want value", got.Name)
	}

	for _, invalid := range []*chplan.Project{
		project(chplan.Column{Name: "labels", Role: chplan.RoleAttributes}),
		project(
			chplan.Column{Name: "value", Role: chplan.RoleValue},
			chplan.Column{Name: "other_value", Role: chplan.RoleValue},
		),
	} {
		capturePanic(t, func() { scalarComparisonValueRef(invalid) })
	}
}

func TestScalarComparisonPriorBoundaryEquivalence(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "labels_map", "sample_time", "sample_value"
	for _, s := range []schema.Metrics{standard, custom} {
		for _, step := range []time.Duration{0, time.Minute} {
			ctx := lowerCtx{start: at.Add(-2 * step), end: at, step: step, lowerers: RangeLowerers{}.withDefaults()}
			for _, operand := range []string{`latency_exp_hist or num_cpus`, `num_cpus or latency_exp_hist`, `sort_by_label(latency_exp_hist or num_cpus, "job")`, `sort_by_label(num_cpus or latency_exp_hist, "job")`, `num_cpus`, `{__name__=~"cpu_a|cpu_b"}`, `sum_over_time(num_cpus[5m])`, `latency_exp_hist + 1`, `num_cpus offset 1m`, `num_cpus @ 1767225600`} {
				for _, op := range []chplan.BinaryOp{chplan.OpEq, chplan.OpNe, chplan.OpLt, chplan.OpLe, chplan.OpGt, chplan.OpGe} {
					for _, left := range []bool{false, true} {
						for _, boolean := range []bool{false, true} {
							t.Run(fmt.Sprintf("%s/%s/%s/%s/left=%v/bool=%v", s.ValueColumn, step, operand, op, left, boolean), func(t *testing.T) {
								expr, err := p.ParseExpr(operand)
								if err != nil {
									t.Fatal(err)
								}
								union, direct := mixedExpHistogramSetOp(expr, s, ctx)
								var inner, got, want chplan.Node
								const scalar = 5.0
								if direct {
									inner, err = lowerMixedExpHistogramSetOp(union, s, ctx)
									if err == nil {
										got, err = lowerComparisonOverMixedExpHistogramSetOp(union, op, scalar, left, boolean, s, ctx)
									}
									if err == nil {
										want, err = scalarComparisonPriorCanonical(inner, s, op, scalar, left, boolean)
									}
								} else {
									inner, err = lowerScalarBinopOperand(expr, s, ctx)
									if err == nil {
										got, err = lowerVectorScalar(expr, s, op, scalar, left, boolean, ctx)
									}
									if err == nil {
										build := func(value chplan.Expr) chplan.Expr {
											var a, b chplan.Expr = value, &chplan.LitFloat{V: scalar}
											if left {
												a, b = b, a
											}
											return &chplan.Binary{Op: op, Left: a, Right: b}
										}
										inner = mixedRowsFloatOnly(inner)
										if !boolean {
											want = &chplan.Filter{Input: inner, Predicate: build(&chplan.ColumnRef{Name: s.ValueColumn})}
										} else {
											inner = guardNameDropCollision(inner, expr, s, ctx)
											want = projectValueOverInner(inner, s, legacySampleProjectionLayout(inner), func(refs sampleRoleRefs) chplan.Expr {
												return &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{build(refs.Value)}}
											})
										}
									}
								}
								if err != nil {
									t.Fatal(err)
								}
								assertScalarArithmeticPlansEqual(t, got, want)
							})
						}
					}
				}
			}
		}
	}
}

func TestScalarComparisonPolicyBeforeLoadAndProjection(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, site := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
		for _, mode := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedPreserve} {
			t.Run(fmt.Sprintf("%s/%d", site, mode), func(t *testing.T) {
				key := mixedWrapperKey{family: mixedComparisonFamily, site: site}
				previous := mixedOperandPolicies[key]
				mixedOperandPolicies[key] = mode
				t.Cleanup(func() { mixedOperandPolicies[key] = previous })
				if site == mixedRootAdmission {
					called := false
					plan, err := lowerComparisonRoot(func() (chplan.Node, error) {
						called = true
						return nil, errors.New("must not lower")
					})
					if called || plan != nil || err == nil {
						t.Fatalf("denied root invoked loader: %v %T %v", called, plan, err)
					}
				}
				boundary := scalarComparisonGuarded
				if site == mixedRootAdmission {
					boundary = scalarComparisonCanonical
				}
				// Malformed roles would panic in the projection callback; policy
				// rejection must happen before narrowing or role resolution.
				plan, err := finishScalarComparison(&chplan.VectorSetOp{
					Mixed:            true,
					MetricNameColumn: s.MetricNameColumn,
					AttributesColumn: s.AttributesColumn,
					TimestampColumn:  s.TimestampColumn,
					ValueColumn:      s.ValueColumn,
				}, nil, s, lowerCtx{}, chplan.OpEq, 1, false, false, boundary)
				if plan != nil || err == nil {
					t.Fatalf("denied projection continued: %T %v", plan, err)
				}
			})
		}
	}
	wantError := errors.New("histogram operand error")
	calls := 0
	_, err := lowerComparisonRoot(func() (chplan.Node, error) { calls++; return nil, wantError })
	if calls != 1 || err != wantError {
		t.Fatalf("loader error changed: calls=%d error=%v", calls, err)
	}
}

func TestScalarComparisonMissingAuthorityKeepsComputedControl(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	for _, site := range []mixedAdmissionSite{mixedRootAdmission, mixedPlanAdmission} {
		t.Run(string(site), func(t *testing.T) {
			key := mixedWrapperKey{family: mixedComparisonFamily, site: site}
			old := mixedOperandPolicies[key]
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = old })
			query := `(latency_exp_hist or num_cpus) > 2`
			if site == mixedPlanAdmission {
				query = `sort_by_label(latency_exp_hist or num_cpus, "job") > bool 2`
			}
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Lower(context.Background(), expr, s); err == nil {
				t.Fatal("missing scalar-comparison authority admitted literal comparison")
			}
			expr, err = p.ParseExpr(`sort_by_label(latency_exp_hist or num_cpus, "job") > scalar(vector(2))`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Lower(context.Background(), expr, s); err != nil {
				t.Fatalf("computed scalar control changed: %v", err)
			}
		})
	}
}

func TestScalarComparisonUnknownBoundaryFailsBeforeInput(t *testing.T) {
	const unknownBoundary scalarComparisonBoundary = 255
	plan, err := finishScalarComparison(nil, nil, schema.DefaultOTelMetrics(), lowerCtx{}, chplan.OpEq, 1, false, false, unknownBoundary)
	if plan != nil || err == nil {
		t.Fatalf("unknown comparison boundary continued: plan=%T error=%v", plan, err)
	}
}

func TestScalarComparisonNonMixedIndependentOfPlanPolicy(t *testing.T) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	for _, query := range []string{`num_cpus > 1`, `num_cpus > bool 1`, `latency_exp_hist > 1`, `latency_exp_hist > bool 1`} {
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			want, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatal(err)
			}
			key := mixedWrapperKey{family: mixedComparisonFamily, site: mixedPlanAdmission}
			old := mixedOperandPolicies[key]
			delete(mixedOperandPolicies, key)
			t.Cleanup(func() { mixedOperandPolicies[key] = old })
			got, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatal(err)
			}
			assertScalarArithmeticPlansEqual(t, got, want)
		})
	}
}
