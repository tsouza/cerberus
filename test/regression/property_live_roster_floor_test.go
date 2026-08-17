package regression

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type propertyLiveRosterBinding struct {
	File       string
	Test       string
	Constraint string
	RosterCall string
	RunnerCall string
}

type propertyRandomRunnerBinding struct {
	File       string
	Test       string
	Constraint string
	RunnerCall string
}

var propertyLiveRosterFloor = []propertyLiveRosterBinding{
	{
		File:       "test/property/instant_window_test.go",
		Test:       "TestPromQL_InstantWindowShapeRoster",
		Constraint: "chdb",
		RosterCall: "gen.InstantWindowShapeIDs",
		RunnerCall: "property.RunShapeCases",
	},
	{
		File:       "test/property/logql_test.go",
		Test:       "TestLogQL_PropertyShapeRoster",
		Constraint: "chdb_agpl_oracle",
		RosterCall: "gen.LogQLShapeIDs",
		RunnerCall: "property.RunShapeExamples",
	},
	{
		File:       "test/property/promql_exp_histogram_test.go",
		Test:       "TestPromQL_NativeHistogramShapeRoster",
		Constraint: "chdb",
		RosterCall: "gen.ExpHistogramShapeIDs",
		RunnerCall: "property.RunShapeExamples",
	},
	{
		File:       "test/property/promql_range_test.go",
		Test:       "TestPromQL_RangePropertyShapeRoster",
		Constraint: "chdb",
		RosterCall: "gen.PromQLRangeShapeIDs",
		RunnerCall: "property.RunShapeCases",
	},
	{
		File:       "test/property/promql_test.go",
		Test:       "TestPromQL_PropertyShapeRoster",
		Constraint: "chdb",
		RosterCall: "gen.PromQLShapeIDs",
		RunnerCall: "property.RunShapeExamples",
	},
	{
		File:       "test/property/traceql_test.go",
		Test:       "TestTraceQL_PropertyShapeRoster",
		Constraint: "chdb",
		RosterCall: "gen.TraceQLShapeIDs",
		RunnerCall: "property.RunShapeExamples",
	},
}

var propertyRandomRunnerFloor = []propertyRandomRunnerBinding{
	{
		File:       "test/property/instant_window_test.go",
		Test:       "TestPromQL_InstantWindowSweep_FromScratch",
		Constraint: "chdb",
		RunnerCall: "rapid.Check",
	},
	{
		File:       "test/property/logql_test.go",
		Test:       "TestLogQL_Property",
		Constraint: "chdb_agpl_oracle",
		RunnerCall: "property.RunLogs",
	},
	{
		File:       "test/property/promql_exp_histogram_test.go",
		Test:       "TestPromQL_Property_NativeHistogram",
		Constraint: "chdb",
		RunnerCall: "property.Run",
	},
	{
		File:       "test/property/promql_range_test.go",
		Test:       "TestPromQL_RangeProperty_FromScratch",
		Constraint: "chdb",
		RunnerCall: "rapid.Check",
	},
	{
		File:       "test/property/promql_test.go",
		Test:       "TestPromQL_Property_FromScratch",
		Constraint: "chdb",
		RunnerCall: "property.Run",
	},
	{
		File:       "test/property/traceql_test.go",
		Test:       "TestTraceQL_Property",
		Constraint: "chdb",
		RunnerCall: "property.Run",
	},
}

// TestPropertyLiveRosterFloor pins executable source, not registry metadata.
// Removing a complete tagged runner would also remove it from symbol discovery;
// this independent exact inventory keeps every language's deterministic live
// differential present and wired to the intended roster and orchestration seam.
func TestPropertyLiveRosterFloor(t *testing.T) {
	root := repoRootForParity(t)
	got, discoveryProblems := discoverPropertyLiveRosterBindings(root)
	problems := append(discoveryProblems, propertyLiveRosterBindingProblems(propertyLiveRosterFloor, got)...)
	if len(problems) > 0 {
		t.Fatalf("property live-roster floor drift:\n- %s", strings.Join(problems, "\n- "))
	}
}

func TestPropertyLiveRosterFloorRejectsInventorySubstitution(t *testing.T) {
	if problems := propertyLiveRosterBindingProblems(propertyLiveRosterFloor, propertyLiveRosterFloor); len(problems) > 0 {
		t.Fatalf("valid live-roster floor rejected: %v", problems)
	}

	tests := []struct {
		name   string
		mutate func([]propertyLiveRosterBinding) []propertyLiveRosterBinding
		want   string
	}{
		{
			name: "missing runner",
			mutate: func(bindings []propertyLiveRosterBinding) []propertyLiveRosterBinding {
				return bindings[1:]
			},
			want: "missing",
		},
		{
			name: "same-file roster substitution",
			mutate: func(bindings []propertyLiveRosterBinding) []propertyLiveRosterBinding {
				bindings[0].RosterCall = "gen.PromQLShapeIDs"
				return bindings
			},
			want: "wiring",
		},
		{
			name: "runner substitution",
			mutate: func(bindings []propertyLiveRosterBinding) []propertyLiveRosterBinding {
				bindings[0].RunnerCall = "property.RunShapeExamples"
				return bindings
			},
			want: "wiring",
		},
		{
			name: "unexpected runner",
			mutate: func(bindings []propertyLiveRosterBinding) []propertyLiveRosterBinding {
				return append(bindings, propertyLiveRosterBinding{
					File:       "test/property/extra_test.go",
					Test:       "TestExtraShapeRoster",
					Constraint: "chdb",
					RosterCall: "gen.ExtraShapeIDs",
					RunnerCall: "property.RunShapeExamples",
				})
			},
			want: "unexpected",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutant := tc.mutate(append([]propertyLiveRosterBinding(nil), propertyLiveRosterFloor...))
			problems := strings.Join(propertyLiveRosterBindingProblems(propertyLiveRosterFloor, mutant), "\n")
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("mutant problems = %q, want substring %q", problems, tc.want)
			}
		})
	}
}

// TestPropertyRandomRunnerFloor keeps breadth independent from the one-example
// roster floor. A language can retain deterministic shape enrollment while
// losing the randomized value and geometry search entirely; the exact runner
// inventory makes that deletion review-visible.
func TestPropertyRandomRunnerFloor(t *testing.T) {
	root := repoRootForParity(t)
	got, discoveryProblems := discoverPropertyRandomRunnerBindings(root)
	problems := append(discoveryProblems, propertyRandomRunnerBindingProblems(propertyRandomRunnerFloor, got)...)
	if len(problems) > 0 {
		t.Fatalf("property random-runner floor drift:\n- %s", strings.Join(problems, "\n- "))
	}
}

func TestPropertyRandomRunnerFloorRejectsDeletion(t *testing.T) {
	if problems := propertyRandomRunnerBindingProblems(propertyRandomRunnerFloor, propertyRandomRunnerFloor); len(problems) > 0 {
		t.Fatalf("valid random-runner floor rejected: %v", problems)
	}
	withoutOneLanguage := append([]propertyRandomRunnerBinding(nil), propertyRandomRunnerFloor[1:]...)
	problems := strings.Join(propertyRandomRunnerBindingProblems(propertyRandomRunnerFloor, withoutOneLanguage), "\n")
	if !strings.Contains(problems, "missing random-runner binding") {
		t.Fatalf("complete random-runner deletion was accepted: %q", problems)
	}
}

func discoverPropertyLiveRosterBindings(root string) ([]propertyLiveRosterBinding, []string) {
	propertyRoot := filepath.Join(root, "test", "property")
	var bindings []propertyLiveRosterBinding
	var problems []string
	err := filepath.WalkDir(propertyRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if parsed.Name.Name != "property_test" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		constraint := ""
		if expression, ok, err := sourceBuildConstraint(source); err != nil {
			problems = append(problems, fmt.Sprintf("%s has an invalid build constraint: %v", relative, err))
		} else if ok {
			constraint = expression.String()
		}

		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil || !isGoTestSymbol(function.Name.Name, "Test") {
				continue
			}
			var runnerCalls []*ast.CallExpr
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if ok && (selector.Sel.Name == "RunShapeExamples" || selector.Sel.Name == "RunShapeCases") {
					runnerCalls = append(runnerCalls, call)
				}
				return true
			})
			if len(runnerCalls) == 0 {
				continue
			}
			if len(runnerCalls) != 1 {
				problems = append(problems, fmt.Sprintf(
					"%s:%s calls %d deterministic roster runners, want exactly one",
					relative, function.Name.Name, len(runnerCalls),
				))
				continue
			}
			call := runnerCalls[0]
			rosterCall := "<missing second argument>"
			if len(call.Args) > 1 {
				if roster, ok := call.Args[1].(*ast.CallExpr); ok {
					rosterCall = propertyQualifiedName(roster.Fun)
				} else {
					rosterCall = "<not a direct roster call>"
				}
			}
			bindings = append(bindings, propertyLiveRosterBinding{
				File:       relative,
				Test:       function.Name.Name,
				Constraint: constraint,
				RosterCall: rosterCall,
				RunnerCall: propertyQualifiedName(call.Fun),
			})
		}
		return nil
	})
	if err != nil {
		problems = append(problems, fmt.Sprintf("discover property live-roster runners: %v", err))
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].File != bindings[j].File {
			return bindings[i].File < bindings[j].File
		}
		return bindings[i].Test < bindings[j].Test
	})
	return bindings, problems
}

func discoverPropertyRandomRunnerBindings(root string) ([]propertyRandomRunnerBinding, []string) {
	propertyRoot := filepath.Join(root, "test", "property")
	var bindings []propertyRandomRunnerBinding
	var problems []string
	err := filepath.WalkDir(propertyRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if parsed.Name.Name != "property_test" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		constraint := ""
		if expression, ok, err := sourceBuildConstraint(source); err != nil {
			problems = append(problems, fmt.Sprintf("%s has an invalid build constraint: %v", relative, err))
		} else if ok {
			constraint = expression.String()
		}

		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil || !isGoTestSymbol(function.Name.Name, "Test") {
				continue
			}
			var runnerCalls []*ast.CallExpr
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := propertyQualifiedName(call.Fun)
				if name == "property.Run" || name == "property.RunLogs" || name == "rapid.Check" {
					runnerCalls = append(runnerCalls, call)
				}
				return true
			})
			if len(runnerCalls) == 0 {
				continue
			}
			if len(runnerCalls) != 1 {
				problems = append(problems, fmt.Sprintf(
					"%s:%s calls %d randomized property runners, want exactly one",
					relative, function.Name.Name, len(runnerCalls),
				))
				continue
			}
			bindings = append(bindings, propertyRandomRunnerBinding{
				File:       relative,
				Test:       function.Name.Name,
				Constraint: constraint,
				RunnerCall: propertyQualifiedName(runnerCalls[0].Fun),
			})
		}
		return nil
	})
	if err != nil {
		problems = append(problems, fmt.Sprintf("discover property random runners: %v", err))
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].File != bindings[j].File {
			return bindings[i].File < bindings[j].File
		}
		return bindings[i].Test < bindings[j].Test
	})
	return bindings, problems
}

func propertyQualifiedName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.SelectorExpr:
		prefix := propertyQualifiedName(expression.X)
		if prefix == "" {
			return expression.Sel.Name
		}
		return prefix + "." + expression.Sel.Name
	default:
		return ""
	}
}

func propertyLiveRosterBindingProblems(want, got []propertyLiveRosterBinding) []string {
	wantByKey := make(map[string]propertyLiveRosterBinding, len(want))
	gotByKey := make(map[string]propertyLiveRosterBinding, len(got))
	var problems []string
	for _, binding := range want {
		key := binding.File + ":" + binding.Test
		if _, duplicate := wantByKey[key]; duplicate {
			problems = append(problems, "duplicate expected live-roster binding "+key)
		}
		wantByKey[key] = binding
	}
	for _, binding := range got {
		key := binding.File + ":" + binding.Test
		if _, duplicate := gotByKey[key]; duplicate {
			problems = append(problems, "duplicate discovered live-roster binding "+key)
		}
		gotByKey[key] = binding
	}
	for key, expected := range wantByKey {
		actual, ok := gotByKey[key]
		if !ok {
			problems = append(problems, "missing live-roster binding "+key)
			continue
		}
		if actual != expected {
			problems = append(problems, fmt.Sprintf(
				"%s wiring = constraint %q, roster %q, runner %q; want %q, %q, %q",
				key,
				actual.Constraint, actual.RosterCall, actual.RunnerCall,
				expected.Constraint, expected.RosterCall, expected.RunnerCall,
			))
		}
	}
	for key := range gotByKey {
		if _, ok := wantByKey[key]; !ok {
			problems = append(problems, "unexpected live-roster binding "+key)
		}
	}
	sort.Strings(problems)
	return problems
}

func propertyRandomRunnerBindingProblems(want, got []propertyRandomRunnerBinding) []string {
	wantByKey := make(map[string]propertyRandomRunnerBinding, len(want))
	gotByKey := make(map[string]propertyRandomRunnerBinding, len(got))
	var problems []string
	for _, binding := range want {
		key := binding.File + ":" + binding.Test
		if _, duplicate := wantByKey[key]; duplicate {
			problems = append(problems, "duplicate expected random-runner binding "+key)
		}
		wantByKey[key] = binding
	}
	for _, binding := range got {
		key := binding.File + ":" + binding.Test
		if _, duplicate := gotByKey[key]; duplicate {
			problems = append(problems, "duplicate discovered random-runner binding "+key)
		}
		gotByKey[key] = binding
	}
	for key, expected := range wantByKey {
		actual, ok := gotByKey[key]
		if !ok {
			problems = append(problems, "missing random-runner binding "+key)
			continue
		}
		if actual != expected {
			problems = append(problems, fmt.Sprintf(
				"%s wiring = constraint %q, runner %q; want %q, %q",
				key, actual.Constraint, actual.RunnerCall, expected.Constraint, expected.RunnerCall,
			))
		}
	}
	for key := range gotByKey {
		if _, ok := wantByKey[key]; !ok {
			problems = append(problems, "unexpected random-runner binding "+key)
		}
	}
	sort.Strings(problems)
	return problems
}

// propertyFanoutScript is the composite go-test invocation
// TestPropertyWorkflowPinsCompositeExecutionAndRapidBudget proves the
// property job's fan-out step actually executes.
//
// The "Run property tests" step no longer embeds `go test ...
// ./test/property/...` directly in property.yml: TestPromQL_Property_
// NativeHistogram alone grew too close to (and past) the job's 20-minute
// go test -timeout after #2165 widened its query-shape space, so a single
// serial `go test ./test/property/...` line is no longer a shape this
// job's execution can safely take — it now fans the histogram sweep, the
// histogram roster, and the rest of the package out across several
// concurrent processes via this script (see the script's own header for
// the full account, including why t.Parallel() inside one process is not
// a safe alternative: chdb-go v1.12.0 keeps a single process-wide
// globalSession).
//
// property-fanout.test.mjs (run under the required `check` lane's `node
// --test`) is what proves the fanned-out legs still cover the full
// ./test/property/... surface at the full -rapid.checks=500 depth with no
// test dropped, narrowed, or duplicated — the equivalent of what this file
// used to check directly against a single `-rapid.checks=500` flag. The
// Go-side check below is scoped to the workflow WIRING: that property.yml
// still invokes this exact script, with the composite build-tag set,
// rather than silently reverting to a raw (and now unsafe) inline command.
const propertyFanoutScript = ".github/scripts/property-fanout.mjs"

// propertyFanoutTags is the literal TAGS env value property-fanout.mjs
// requires (see the script's own "TAGS is required" guard): chdb for the
// property-test entry points, agpl_oracle for the LogQL from-scratch
// oracle, and chdb_agpl_oracle for logql_test.go's own single-term build
// constraint (see property.yml's tag-composition comment and #1520).
const propertyFanoutTags = "chdb,agpl_oracle,chdb_agpl_oracle"

func TestPropertyWorkflowPinsCompositeExecutionAndRapidBudget(t *testing.T) {
	workflows := readTaggedWorkflows(t, repoRootForParity(t))
	workflow, ok := workflows[".github/workflows/property.yml"]
	if !ok {
		t.Fatal("property workflow is missing")
	}
	if problems := propertyWorkflowExecutionProblems(workflow); len(problems) > 0 {
		t.Fatalf("property workflow execution drift:\n- %s", strings.Join(problems, "\n- "))
	}
}

func TestPropertyWorkflowRapidBudgetRejectsHollowCommands(t *testing.T) {
	valid := "node " + propertyFanoutScript
	validEnv := map[string]string{"TAGS": propertyFanoutTags}
	if problems := propertyWorkflowExecutionProblems(propertyWorkflowWithStep(valid, validEnv)); len(problems) > 0 {
		t.Fatalf("valid property fan-out command rejected: %v", problems)
	}
	tests := []struct {
		name    string
		command string
		env     map[string]string
		want    string
	}{
		{
			name:    "reverted to the old raw go test line",
			command: "go test -tags chdb,agpl_oracle,chdb_agpl_oracle ./test/property/... -rapid.checks=500",
			env:     validEnv,
			want:    "want exactly one",
		},
		{
			name:    "missing TAGS entirely",
			command: valid,
			env:     nil,
			want:    "declares no TAGS env var",
		},
		{
			name:    "TAGS drops a required tag",
			command: valid,
			env:     map[string]string{"TAGS": "chdb"},
			want:    "TAGS = ",
		},
		{
			name:    "TAGS is a dynamic expression rather than the literal set",
			command: valid,
			env:     map[string]string{"TAGS": "${{ env.SOMETHING }}"},
			want:    "TAGS = ",
		},
		{
			name:    "extra argument narrows or redirects the script",
			command: valid + " --dry-run",
			env:     validEnv,
			want:    "want exactly the script path",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := strings.Join(propertyWorkflowExecutionProblems(propertyWorkflowWithStep(tc.command, tc.env)), "\n")
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("command problems = %q, want substring %q", problems, tc.want)
			}
		})
	}

	t.Run("duplicate invocation", func(t *testing.T) {
		workflow := taggedWorkflow{Jobs: map[string]taggedWorkflowJob{
			"property": {
				RunsOn: "ubuntu-latest",
				Steps: []taggedWorkflowStep{
					{Run: valid, Env: validEnv},
					{Run: valid, Env: validEnv},
				},
			},
		}}
		problems := strings.Join(propertyWorkflowExecutionProblems(workflow), "\n")
		if !strings.Contains(problems, "want exactly one") {
			t.Fatalf("command problems = %q, want substring %q", problems, "want exactly one")
		}
	})
}

func propertyWorkflowWithStep(command string, env map[string]string) taggedWorkflow {
	workflow := taggedWorkflow{Jobs: map[string]taggedWorkflowJob{}}
	workflow.Jobs["property"] = taggedWorkflowJob{
		RunsOn: "ubuntu-latest",
		Steps:  []taggedWorkflowStep{{Run: command, Env: env}},
	}
	return workflow
}

func propertyWorkflowExecutionProblems(workflow taggedWorkflow) []string {
	job, ok := workflow.Jobs["property"]
	if !ok {
		return []string{`property workflow has no "property" job`}
	}
	type candidate struct {
		command taggedStaticCommand
		stepEnv map[string]string
	}
	var candidates []candidate
	var problems []string
	for index, step := range job.Steps {
		commands, err := taggedStaticCommands(step.Run)
		if err != nil {
			problems = append(problems, fmt.Sprintf("property step %d cannot be parsed: %v", index+1, err))
			continue
		}
		for _, command := range commands {
			if command.Executable != "node" || len(command.Args) == 0 {
				continue
			}
			if command.Args[0].Text == propertyFanoutScript {
				candidates = append(candidates, candidate{command: command, stepEnv: step.Env})
			}
		}
	}
	if len(candidates) != 1 {
		problems = append(problems, fmt.Sprintf(
			"property job has %d node %s commands, want exactly one",
			len(candidates), propertyFanoutScript,
		))
		return problems
	}
	found := candidates[0]
	command := found.command
	if problem := taggedEvidenceContextProblem(command, command.Env, "", false); problem != "" {
		problems = append(problems, "property fan-out command cannot prove execution: "+problem)
	}
	if command.Args[0].Dynamic {
		problems = append(problems, "property fan-out script path is dynamic, not a literal path")
	}
	if len(command.Args) != 1 {
		problems = append(problems, fmt.Sprintf(
			"property fan-out command has %d argument(s), want exactly the script path",
			len(command.Args),
		))
	}
	tags, ok := found.stepEnv["TAGS"]
	switch {
	case !ok:
		problems = append(problems, "property fan-out step declares no TAGS env var")
	case tags != propertyFanoutTags:
		problems = append(problems, fmt.Sprintf("property fan-out TAGS = %q, want %q", tags, propertyFanoutTags))
	}
	return problems
}
