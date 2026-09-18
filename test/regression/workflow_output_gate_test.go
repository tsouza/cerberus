package regression

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A job's `outputs:` are published the moment the step that sets them
// finishes — NOT when the job succeeds. A later step in the same job can still
// fail, and the job then reports `failure` with every output already in place.
// A downstream job whose `if:` uses `always()` (so GitHub no longer implies
// "every need succeeded") and selects itself on one of those outputs — `needs
// .plan.outputs.cardinality_selected == 'true'` — therefore runs on the
// outputs of a job that FAILED, unless it also asks for the producer's result.
//
// That is what update-golden.yml's `cardinality-leg` matrix did: `plan` writes
// its outputs in one step and can be failed by the coverage check in the next,
// and the eight legs — gated on `regenerate` being `success || skipped`, which
// `skipped` is precisely because `plan` failed — each spun up a runner, pulled
// libchdb and died looking for predecessor patches nothing had produced.
//
// The rule below is the class: in every workflow, a job running under
// `always()` that reads a need's output as a positive selector must be
// success-gated on that need. Gating may be transitive — requiring
// `needs.M.result == 'success'` where M is itself success-gated on N is
// enough, because M cannot have succeeded unless it ran, and it only ran if N
// succeeded — but it must be a CONJUNCT of the job's condition: a
// `needs.N.result == 'success'` buried inside an `||` disjunction gates
// nothing.
//
// Two shapes need no gating. The fail-open selector — `needs.N.result !=
// 'success' || needs.N.outputs.x == 'true'` — RUNS the job whenever the
// producer failed, so its outputs only decide anything for a producer that
// succeeded. And an output that IS a step outcome (`${{ steps.x.outcome }}`)
// is the opposite of a plan: it exists precisely to be read after the job
// failed, so an evidence job can run on "the harness step succeeded" even
// when a later ratchet step failed the producer.

// workflowOutputSelectorRE finds every `needs.<job>.outputs.<name> == '…'`
// positive selector in a job's `if:` expression.
var workflowOutputSelectorRE = regexp.MustCompile(`needs\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)\s*==\s*'`)

// workflowStepOutcomeRE matches a job output whose whole value is one step's
// outcome or conclusion.
var workflowStepOutcomeRE = regexp.MustCompile(`^\$\{\{\s*steps\.[A-Za-z0-9_-]+\.(outcome|conclusion)\s*\}\}$`)

// workflowNeedSuccessRE matches one whole conjunct of the shape
// `needs.<job>.result == 'success'`.
var workflowNeedSuccessRE = regexp.MustCompile(`^needs\.([A-Za-z0-9_-]+)\.result\s*==\s*'success'$`)

// workflowIfConjuncts splits a workflow `if:` expression on its TOP-LEVEL
// `&&` operators (parentheses respected), unwrapping one pair of enclosing
// parentheses per conjunct. `${{ … }}` wrappers are stripped first. A
// conjunct that still contains `||` is returned as-is, so a caller matching
// against a whole-conjunct pattern will not credit it.
func workflowIfConjuncts(expr string) []string {
	expr = strings.TrimSpace(expr)
	expr = strings.TrimPrefix(expr, "${{")
	expr = strings.TrimSuffix(expr, "}}")

	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '&':
			if depth == 0 && i+1 < len(expr) && expr[i+1] == '&' {
				out = append(out, expr[start:i])
				i++
				start = i + 1
			}
		}
	}
	out = append(out, expr[start:])

	for i, conjunct := range out {
		conjunct = strings.TrimSpace(conjunct)
		out[i] = strings.TrimSpace(unwrapEnclosingParens(conjunct))
	}
	return out
}

// unwrapEnclosingParens strips one pair of parentheses that encloses the WHOLE
// expression (`(a || b)` → `a || b`, but `(a) && (b)` is left alone).
func unwrapEnclosingParens(expr string) string {
	if !strings.HasPrefix(expr, "(") || !strings.HasSuffix(expr, ")") {
		return expr
	}
	depth := 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(expr)-1 {
				return expr // the opening paren closed before the end
			}
		}
	}
	return expr[1 : len(expr)-1]
}

// workflowIfUsesAlways reports whether an `if:` expression removes GitHub's
// implicit `success()` by calling `always()`.
func workflowIfUsesAlways(expr string) bool {
	return strings.Contains(expr, "always()")
}

// successGatedNeeds returns the needs a job's condition requires to have
// succeeded as explicit conjuncts.
func successGatedNeeds(expr string) []string {
	var out []string
	for _, conjunct := range workflowIfConjuncts(expr) {
		if m := workflowNeedSuccessRE.FindStringSubmatch(conjunct); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// jobRunsOnProducerFailure reports whether a condition runs the job whenever
// `need` did not succeed (`needs.<need>.result != 'success'`), which makes a
// selector on that need's outputs decisive only for a successful producer.
func jobRunsOnProducerFailure(expr, need string) bool {
	return regexp.MustCompile(`needs\.` + regexp.QuoteMeta(need) + `\.result\s*!=\s*'success'`).MatchString(expr)
}

type workflowGateJob struct {
	needs   []string
	ifExpr  string
	outputs map[string]string
}

// outputIsStepOutcome reports whether a job publishes `name` as one of its
// steps' outcome — a value meant to be read whatever the job's own result.
func outputIsStepOutcome(job workflowGateJob, name string) bool {
	return workflowStepOutcomeRE.MatchString(strings.TrimSpace(job.outputs[name]))
}

// jobIsSuccessGatedOn reports whether `job` can only run if `need` succeeded:
// implicitly (no `always()`, and `need` is among its needs), explicitly (a
// `needs.<need>.result == 'success'` conjunct), or transitively through a
// need that is itself success-gated on `need`.
func jobIsSuccessGatedOn(jobs map[string]workflowGateJob, jobID, need string, visiting map[string]bool) bool {
	if visiting[jobID] {
		return false
	}
	visiting[jobID] = true
	defer delete(visiting, jobID)

	job := jobs[jobID]
	inNeeds := false
	for _, n := range job.needs {
		if n == need {
			inNeeds = true
		}
	}
	if !workflowIfUsesAlways(job.ifExpr) {
		if inNeeds {
			return true
		}
		for _, n := range job.needs {
			if jobIsSuccessGatedOn(jobs, n, need, visiting) {
				return true
			}
		}
		return false
	}
	for _, gated := range successGatedNeeds(job.ifExpr) {
		if gated == need {
			return true
		}
		if jobIsSuccessGatedOn(jobs, gated, need, visiting) {
			return true
		}
	}
	return false
}

func TestAlwaysJobsSelectingOnAnOutputRequireTheProducersSuccess(t *testing.T) {
	t.Parallel()

	for workflowPath, workflow := range readCILaneWorkflows(t) {
		jobs := map[string]workflowGateJob{}
		for jobID, job := range workflow.Jobs {
			jobs[jobID] = workflowGateJob{
				needs:   ciLaneNeeds(t, workflowPath, jobID, job.Needs),
				ifExpr:  ciLaneScalarValue(job.If),
				outputs: job.Outputs,
			}
		}
		jobIDs := make([]string, 0, len(jobs))
		for jobID := range jobs {
			jobIDs = append(jobIDs, jobID)
		}
		sort.Strings(jobIDs)

		for _, jobID := range jobIDs {
			job := jobs[jobID]
			if !workflowIfUsesAlways(job.ifExpr) {
				continue
			}
			producers := map[string]bool{}
			for _, m := range workflowOutputSelectorRE.FindAllStringSubmatch(job.ifExpr, -1) {
				producer, output := m[1], m[2]
				if outputIsStepOutcome(jobs[producer], output) {
					continue
				}
				producers[producer] = true
			}
			for producer := range producers {
				if jobIsSuccessGatedOn(jobs, jobID, producer, map[string]bool{}) ||
					jobRunsOnProducerFailure(job.ifExpr, producer) {
					continue
				}
				t.Errorf("%s job %q runs under always() and selects itself on an output of %q without "+
					"requiring `needs.%s.result == 'success'` (directly, or through a need that is itself "+
					"success-gated on it). Outputs are published when the step that sets them finishes, "+
					"not when the job succeeds, so a later step failing %q leaves every output in place "+
					"and this job runs on a failed producer's plan. Condition:\n%s",
					workflowPath, jobID, producer, producer, producer, job.ifExpr)
			}
		}
	}
}

func TestWorkflowIfConjuncts(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		expr      string
		wantGated []string
	}{
		"explicit conjunct": {
			expr:      "always() && needs.plan.result == 'success' && needs.plan.outputs.x == 'true'",
			wantGated: []string{"plan"},
		},
		"expression wrapper": {
			expr:      "${{ always() && needs.plan.result == 'success' }}",
			wantGated: []string{"plan"},
		},
		"parenthesised conjunct": {
			expr:      "always() && (needs.plan.result == 'success')",
			wantGated: []string{"plan"},
		},
		"disjunction does not gate": {
			expr:      "always() && (needs.regenerate.result == 'success' || needs.regenerate.result == 'skipped')",
			wantGated: nil,
		},
		"multiline": {
			expr:      "always() &&\n  needs.gate.outputs.app_publish == 'true' &&\n  needs.preflight.result == 'success'\n",
			wantGated: []string{"preflight"},
		},
		"skipped is not success": {
			expr:      "always() && needs.plan.result == 'skipped'",
			wantGated: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := successGatedNeeds(test.expr)
			if strings.Join(got, ",") != strings.Join(test.wantGated, ",") {
				t.Errorf("successGatedNeeds(%q) = %v, want %v", test.expr, got, test.wantGated)
			}
		})
	}
}

func TestOutputIsStepOutcome(t *testing.T) {
	t.Parallel()

	job := workflowGateJob{outputs: map[string]string{
		"harness": "${{ steps.compat.outcome }}",
		"spaced":  "${{steps.fanout.conclusion}}",
		"plan":    "${{ steps.plan.outputs.matrix }}",
		"mixed":   "${{ steps.compat.outcome }}-${{ steps.plan.outputs.x }}",
	}}
	for name, want := range map[string]bool{
		"harness": true,
		"spaced":  true,
		"plan":    false,
		"mixed":   false,
		"absent":  false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := outputIsStepOutcome(job, name); got != want {
				t.Errorf("outputIsStepOutcome(%q) = %v, want %v", name, got, want)
			}
		})
	}
}

func TestJobIsSuccessGatedOnTransitively(t *testing.T) {
	t.Parallel()

	jobs := map[string]workflowGateJob{
		"plan":     {},
		"implicit": {needs: []string{"plan"}},
		"explicit": {needs: []string{"plan"}, ifExpr: "always() && needs.plan.result == 'success'"},
		"via":      {needs: []string{"plan", "implicit"}, ifExpr: "always() && needs.implicit.result == 'success'"},
		"loose": {
			needs:  []string{"plan", "implicit"},
			ifExpr: "always() && (needs.implicit.result == 'success' || needs.implicit.result == 'skipped')",
		},
		"cycle-a": {needs: []string{"cycle-b"}, ifExpr: "always() && needs.cycle-b.result == 'success'"},
		"cycle-b": {needs: []string{"cycle-a"}, ifExpr: "always() && needs.cycle-a.result == 'success'"},
	}
	for jobID, want := range map[string]bool{
		"implicit": true,
		"explicit": true,
		"via":      true,
		"loose":    false,
		"cycle-a":  false,
	} {
		t.Run(jobID, func(t *testing.T) {
			t.Parallel()
			if got := jobIsSuccessGatedOn(jobs, jobID, "plan", map[string]bool{}); got != want {
				t.Errorf("jobIsSuccessGatedOn(%q, plan) = %v, want %v", jobID, got, want)
			}
		})
	}
}
