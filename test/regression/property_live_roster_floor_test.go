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
	"strconv"
	"strings"
	"testing"
)

const propertyRapidChecksFloor = 500

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
	valid := "go test -tags chdb,agpl_oracle,chdb_agpl_oracle ./test/property/... -rapid.checks=500"
	if problems := propertyWorkflowExecutionProblems(propertyWorkflowWithCommand(valid)); len(problems) > 0 {
		t.Fatalf("valid property command rejected: %v", problems)
	}
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "missing budget", command: "go test -tags chdb,agpl_oracle,chdb_agpl_oracle ./test/property/...", want: "want exactly one"},
		{name: "reduced budget", command: "go test -tags chdb,agpl_oracle,chdb_agpl_oracle ./test/property/... -rapid.checks=1", want: "want 500"},
		{name: "budget before package", command: "go test -tags chdb,agpl_oracle,chdb_agpl_oracle -rapid.checks=500 ./test/property/...", want: "after the package list"},
		{name: "dynamic budget", command: "go test -tags chdb,agpl_oracle,chdb_agpl_oracle ./test/property/... -rapid.checks=$RAPID_CHECKS", want: "literal"},
		{name: "duplicate budget", command: "go test -tags chdb,agpl_oracle,chdb_agpl_oracle ./test/property/... -rapid.checks=500 -rapid.checks=500", want: "want exactly one"},
		{name: "incomplete tags", command: "go test -tags chdb ./test/property/... -rapid.checks=500", want: "build tags"},
		{name: "compile only", command: "go test -tags chdb,agpl_oracle,chdb_agpl_oracle -run '^$' ./test/property/... -rapid.checks=500", want: "compile-only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := strings.Join(propertyWorkflowExecutionProblems(propertyWorkflowWithCommand(tc.command)), "\n")
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("command problems = %q, want substring %q", problems, tc.want)
			}
		})
	}
}

func propertyWorkflowWithCommand(command string) taggedWorkflow {
	workflow := taggedWorkflow{Jobs: map[string]taggedWorkflowJob{}}
	workflow.Jobs["property"] = taggedWorkflowJob{
		RunsOn: "ubuntu-latest",
		Steps:  []taggedWorkflowStep{{Run: command}},
	}
	return workflow
}

func propertyWorkflowExecutionProblems(workflow taggedWorkflow) []string {
	job, ok := workflow.Jobs["property"]
	if !ok {
		return []string{`property workflow has no "property" job`}
	}
	var candidates []taggedStaticCommand
	var problems []string
	for index, step := range job.Steps {
		commands, err := taggedStaticCommands(step.Run)
		if err != nil {
			problems = append(problems, fmt.Sprintf("property step %d cannot be parsed: %v", index+1, err))
			continue
		}
		for _, command := range commands {
			if command.Executable != "go" || len(command.Args) == 0 || command.Args[0].Text != "test" {
				continue
			}
			for _, argument := range command.Args[1:] {
				if argument.Text == "./test/property/..." {
					candidates = append(candidates, command)
					break
				}
			}
		}
	}
	if len(candidates) != 1 {
		problems = append(problems, fmt.Sprintf(
			"property job has %d go test commands for ./test/property/..., want exactly one",
			len(candidates),
		))
		return problems
	}
	command := candidates[0]
	if problem := taggedEvidenceContextProblem(command, command.Env, "", false); problem != "" {
		problems = append(problems, "property go test command cannot prove execution: "+problem)
	}
	claim, err := parseTaggedGoTestArgs(command.Args[1:])
	if problem := propertyRapidChecksProblem(command.Args[1:]); problem != "" {
		problems = append(problems, problem)
	}
	if err != nil {
		problems = append(problems, "property go test command is invalid: "+err.Error())
		return problems
	}
	wantTags := map[string]bool{"agpl_oracle": true, "chdb": true, "chdb_agpl_oracle": true}
	if !boolMapEqual(claim.ExplicitTag, wantTags) {
		problems = append(problems, fmt.Sprintf("property go test build tags = %v, want exactly %v", sortedBoolKeys(claim.ExplicitTag), sortedBoolKeys(wantTags)))
	}
	if !sameNormalizedStrings(claim.Packages, []string{"./test/property/..."}) {
		problems = append(problems, fmt.Sprintf("property go test packages = %v, want only ./test/property/...", claim.Packages))
	}
	if claim.CompileOnly {
		problems = append(problems, "property go test command is compile-only")
	}
	if claim.RunPattern != "" {
		problems = append(problems, fmt.Sprintf("property go test command narrows execution with -run %q", claim.RunPattern))
	}
	return problems
}

func propertyRapidChecksProblem(arguments []taggedShellToken) string {
	packageIndex := -1
	for index, argument := range arguments {
		if argument.Text == "./test/property/..." {
			if packageIndex >= 0 {
				return "property go test command repeats ./test/property/..."
			}
			packageIndex = index
		}
	}
	if packageIndex < 0 {
		return "property go test command is missing ./test/property/..."
	}
	type occurrence struct {
		index int
		value taggedShellToken
	}
	var occurrences []occurrence
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		name, inline, hasInline := strings.Cut(argument.Text, "=")
		if name != "-rapid.checks" {
			continue
		}
		value, next, err := taggedFlagValue(arguments, index, inline, hasInline)
		if err != nil {
			return "property -rapid.checks is invalid: " + err.Error()
		}
		occurrences = append(occurrences, occurrence{index: index, value: value})
		index = next
	}
	if len(occurrences) != 1 {
		return fmt.Sprintf("property go test command has %d -rapid.checks flags, want exactly one", len(occurrences))
	}
	checks := occurrences[0]
	if checks.index < packageIndex {
		return "property -rapid.checks must appear after the package list"
	}
	if checks.value.Dynamic {
		return "property -rapid.checks must be a literal integer"
	}
	value, err := strconv.Atoi(checks.value.Text)
	if err != nil {
		return fmt.Sprintf("property -rapid.checks value %q is not an integer", checks.value.Text)
	}
	if value != propertyRapidChecksFloor {
		return fmt.Sprintf("property -rapid.checks = %d, want %d", value, propertyRapidChecksFloor)
	}
	return ""
}

func sortedBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, enabled := range values {
		if enabled {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
