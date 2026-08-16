package regression

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	buildconstraint "go/build/constraint"
	"go/doc"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"gopkg.in/yaml.v3"
)

const (
	taggedEntrypointDirect = "direct"
	taggedEntrypointRecipe = "recipe"
	taggedEntrypointScript = "script"

	migrationExecutionScript = ".github/scripts/migration-e2e.mjs"
	migrationExecutionClaim  = "migration tier selection, execution, attestation, and aggregate"
)

type taggedTestSymbol struct {
	File       string
	PackageDir string
	Name       string
	Constraint buildconstraint.Expr
}

// taggedTestInvocation is execution evidence derived from workflow and source
// files. Registry fields are deliberately absent: metadata cannot certify that
// the command it describes exists or executes anything.
type taggedTestInvocation struct {
	Workflow       string
	Job            string
	Entrypoint     string
	EntryName      string
	Raw            string
	Tags           map[string]bool
	ExplicitTag    map[string]bool
	Packages       []string
	RunPattern     string
	CompileOnly    bool
	TestBinaryFlag map[string]string
	ContextProblem string
	Pipeline       bool
}

// taggedTestExecution is an independently discovered invocation after a
// registry lane has successfully claimed its real workflow entrypoint.
type taggedTestExecution struct {
	taggedTestInvocation
	LaneID       string
	PackageGlobs []string
}

type taggedLaneRegistry struct {
	Lanes []taggedLane `json:"lanes"`
}

type taggedLane struct {
	ID    string `json:"id"`
	Owner struct {
		Workflow string   `json:"workflow"`
		Jobs     []string `json:"jobs"`
	} `json:"owner"`
	Recipes      []string `json:"recipes"`
	Command      string   `json:"command"`
	BuildTags    []string `json:"build_tags"`
	PackageGlobs []string `json:"package_globs"`
}

type taggedWorkflow struct {
	Env      map[string]string            `yaml:"env"`
	Defaults taggedWorkflowDefaults       `yaml:"defaults"`
	Jobs     map[string]taggedWorkflowJob `yaml:"jobs"`
}

type taggedWorkflowJob struct {
	If              any                    `yaml:"if"`
	ContinueOnError any                    `yaml:"continue-on-error"`
	RunsOn          any                    `yaml:"runs-on"`
	Env             map[string]string      `yaml:"env"`
	Defaults        taggedWorkflowDefaults `yaml:"defaults"`
	Steps           []taggedWorkflowStep   `yaml:"steps"`
}

type taggedWorkflowStep struct {
	If               any               `yaml:"if"`
	ContinueOnError  any               `yaml:"continue-on-error"`
	Run              string            `yaml:"run"`
	WorkingDirectory string            `yaml:"working-directory"`
	Shell            string            `yaml:"shell"`
	Env              map[string]string `yaml:"env"`
}

type taggedWorkflowDefaults struct {
	Run taggedWorkflowRunDefaults `yaml:"run"`
}

type taggedWorkflowRunDefaults struct {
	WorkingDirectory string `yaml:"working-directory"`
	Shell            string `yaml:"shell"`
}

type taggedShellToken struct {
	Text    string
	Dynamic bool
}

type taggedStaticCommand struct {
	Executable string
	Args       []taggedShellToken
	Env        map[string]string
	Raw        string
	Unsafe     string
	Pipeline   bool
}

type taggedShellSegment struct {
	Text   string
	Before string
	After  string
}

type migrationTaggedRun struct {
	BuildTag string
	Package  string
}

// TestEveryTaggedTestHasAnExecutingLane discovers both sides of the contract.
// Tagged symbols come from the root Go module; execution comes from workflow
// run steps and the static Just recipes those steps invoke. Only afterwards is
// a registry lane allowed to claim an invocation, and it must own the exact
// workflow job, tag roster, source glob, and command/recipe/script entrypoint.
func TestEveryTaggedTestHasAnExecutingLane(t *testing.T) {
	root := repoRootForParity(t)
	symbols := discoverTaggedTestSymbols(t, root)
	invocations, discoveryProblems := discoverTaggedTestInvocations(t, root)
	executions := bindTaggedTestExecutions(invocations, readTaggedLaneRegistry(t, root))
	problems := append(discoveryProblems, taggedTestEnrollmentProblems(symbols, executions)...)
	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	t.Fatalf("tagged-test execution enrollment is incomplete:\n%s", strings.Join(uniqueStrings(problems), "\n"))
}

func discoverTaggedTestSymbols(t *testing.T, root string) []taggedTestSymbol {
	t.Helper()
	ignored := rootModuleIgnoreDirs(t, root)
	var symbols []taggedTestSymbol
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if isToolStateDir(entry.Name()) || ignoredRootModulePath(rel, ignored) {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			} else if !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") || ignoredRootModulePath(rel, ignored) {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		expr, ok, err := sourceBuildConstraint(source)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if !ok {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, source, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		packageDir := filepath.ToSlash(filepath.Dir(rel))
		for _, declaration := range parsed.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name == "TestMain" {
				continue
			}
			if isTaggedAssertionSymbol(fn.Name.Name) {
				symbols = append(symbols, taggedTestSymbol{
					File: rel, PackageDir: packageDir, Name: fn.Name.Name, Constraint: expr,
				})
			}
		}
		// Go only places examples with an Output directive in the generated
		// test binary. Documentation-only examples are not execution evidence.
		for _, example := range doc.Examples(parsed) {
			if example.Output == "" && !example.EmptyOutput {
				continue
			}
			symbols = append(symbols, taggedTestSymbol{
				File: rel, PackageDir: packageDir, Name: "Example" + example.Name, Constraint: expr,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover tagged tests: %v", err)
	}
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		return symbols[i].Name < symbols[j].Name
	})
	return symbols
}

func isToolStateDir(name string) bool {
	switch name {
	case ".git", ".claude", ".agents", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func rootModuleIgnoreDirs(t *testing.T, root string) []string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read root go.mod: %v", err)
	}
	ignored, err := parseRootModuleIgnoreDirs(string(source))
	if err != nil {
		t.Fatalf("parse root go.mod ignore directives: %v", err)
	}
	return ignored
}

func parseRootModuleIgnoreDirs(source string) ([]string, error) {
	var (
		ignored []string
		inBlock bool
	)
	add := func(raw string, line int) error {
		raw = strings.TrimSpace(strings.Trim(raw, `"`))
		clean := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(raw, "./")))
		if clean == "." || clean == "" || filepath.IsAbs(raw) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("line %d: ignore path %q is not a root-relative directory", line, raw)
		}
		ignored = append(ignored, clean)
		return nil
	}
	for index, raw := range strings.Split(source, "\n") {
		lineNumber := index + 1
		line := strings.TrimSpace(strings.SplitN(raw, "//", 2)[0])
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if err := add(line, lineNumber); err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasPrefix(line, "ignore ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "ignore "))
		if value == "(" {
			inBlock = true
			continue
		}
		if strings.ContainsAny(value, " \t") {
			return nil, fmt.Errorf("line %d: malformed ignore directive %q", lineNumber, line)
		}
		if err := add(value, lineNumber); err != nil {
			return nil, err
		}
	}
	if inBlock {
		return nil, fmt.Errorf("unterminated ignore block")
	}
	sort.Strings(ignored)
	return ignored, nil
}

func ignoredRootModulePath(path string, ignored []string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	for _, prefix := range ignored {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func sourceBuildConstraint(source []byte) (buildconstraint.Expr, bool, error) {
	var (
		goBuild buildconstraint.Expr
		legacy  []buildconstraint.Expr
	)
	for _, raw := range strings.Split(string(source), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "//go:build ") {
			if goBuild != nil {
				return nil, false, fmt.Errorf("multiple //go:build lines")
			}
			expr, err := buildconstraint.Parse(line)
			if err != nil {
				return nil, false, err
			}
			goBuild = expr
			continue
		}
		if strings.HasPrefix(line, "// +build ") {
			expr, err := buildconstraint.Parse(line)
			if err != nil {
				return nil, false, err
			}
			legacy = append(legacy, expr)
			continue
		}
		if strings.HasPrefix(line, "package ") {
			break
		}
	}
	if goBuild != nil {
		return goBuild, true, nil
	}
	if len(legacy) == 0 {
		return nil, false, nil
	}
	expr := legacy[0]
	for _, next := range legacy[1:] {
		expr = &buildconstraint.AndExpr{X: expr, Y: next}
	}
	return expr, true, nil
}

func isGoTestSymbol(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	remainder := strings.TrimPrefix(name, prefix)
	if remainder == "" {
		return true
	}
	for _, first := range remainder {
		return !unicode.IsLower(first)
	}
	return false
}

func isTaggedAssertionSymbol(name string) bool {
	return name != "TestMain" && (isGoTestSymbol(name, "Test") || isGoTestSymbol(name, "Fuzz"))
}

func discoverTaggedTestInvocations(t *testing.T, root string) ([]taggedTestInvocation, []string) {
	t.Helper()
	workflows := readTaggedWorkflows(t, root)
	justfileBytes, err := os.ReadFile(filepath.Join(root, "Justfile"))
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	justfile := string(justfileBytes)
	recipeNames := taggedJustRecipeNames(justfile)
	justPipelineSafe := taggedJustPipelineIsFailClosed(justfile)
	migrationRuns, migrationErr := readMigrationTaggedRuns(root)

	var (
		invocations []taggedTestInvocation
		problems    []string
	)
	for workflowPath, workflow := range workflows {
		for jobID, job := range workflow.Jobs {
			jobExecutes, conditionProblem := taggedWorkflowIfExecutes(job.If)
			if conditionProblem != "" {
				problems = append(problems, fmt.Sprintf("%s job %q: %s", workflowPath, jobID, conditionProblem))
				continue
			}
			jobFailureGates, continueProblem := taggedWorkflowFailureGates(job.ContinueOnError)
			if continueProblem != "" {
				problems = append(problems, fmt.Sprintf("%s job %q: %s", workflowPath, jobID, continueProblem))
				continue
			}
			if !jobExecutes || !jobFailureGates {
				continue
			}
			if !taggedWorkflowRunsOnLinux(job.RunsOn) {
				continue
			}
			priorBuildContext := map[string]string{}
			for stepIndex, step := range job.Steps {
				where := fmt.Sprintf("%s job %q step %d", workflowPath, jobID, stepIndex+1)
				stepExecutes, conditionProblem := taggedWorkflowIfExecutes(step.If)
				if conditionProblem != "" {
					problems = append(problems, fmt.Sprintf("%s: %s", where, conditionProblem))
					continue
				}
				stepFailureGates, continueProblem := taggedWorkflowFailureGates(step.ContinueOnError)
				if continueProblem != "" {
					problems = append(problems, fmt.Sprintf("%s: %s", where, continueProblem))
					continue
				}
				if !stepExecutes || !stepFailureGates {
					continue
				}
				workingDirectory := taggedWorkingDirectory(workflow.Defaults, job.Defaults, step)
				pipelineSafe := taggedWorkflowPipelineIsFailClosed(workflow.Defaults, job.Defaults, step)
				githubEnvWrites := taggedGitHubEnvBuildContextWrites(step.Run)
				commands, err := taggedStaticCommands(step.Run)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: cannot parse run script: %v", where, err))
					for _, name := range githubEnvWrites {
						priorBuildContext[name] = "prior GITHUB_ENV write"
					}
					continue
				}
				for _, command := range commands {
					effectiveEnv := taggedMergedEnv(workflow.Env, job.Env, priorBuildContext, step.Env, command.Env)
					switch command.Executable {
					case "go":
						if len(command.Args) == 0 {
							continue
						}
						if command.Args[0].Dynamic {
							problems = append(problems, fmt.Sprintf("%s: dynamic Go subcommand %q cannot prove execution", where, command.Args[0].Text))
							continue
						}
						if command.Args[0].Text != "test" {
							continue
						}
						invocation, err := parseTaggedGoTestArgs(command.Args[1:])
						if err != nil {
							problems = append(problems, fmt.Sprintf("%s: unparseable go test command %q: %v", where, command.Raw, err))
							continue
						}
						if problem := taggedEvidenceContextProblem(command, effectiveEnv, workingDirectory, pipelineSafe); problem != "" {
							if len(invocation.ExplicitTag) > 0 {
								problems = append(problems, fmt.Sprintf("%s: go test command %q cannot prove execution: %s", where, command.Raw, problem))
							}
							continue
						}
						invocation.Workflow = workflowPath
						invocation.Job = jobID
						invocation.Entrypoint = taggedEntrypointDirect
						invocation.Raw = command.Raw
						invocations = append(invocations, invocation)
					case "just":
						if len(command.Args) == 0 {
							problems = append(problems, fmt.Sprintf("%s: just invocation names no recipe", where))
							continue
						}
						if command.Args[0].Dynamic {
							problems = append(problems, fmt.Sprintf("%s: dynamic Just recipe %q cannot prove execution", where, command.Args[0].Text))
							continue
						}
						recipe := command.Args[0].Text
						if !recipeNames[recipe] {
							problems = append(problems, fmt.Sprintf("%s: Justfile has no statically invoked recipe %q", where, recipe))
							continue
						}
						body := justRecipeBodyWithDeps(t, justfile, recipe)
						coverageJoin := recipe != "coverage" || taggedCoverageJoinIsFailClosed(body)
						recipeInvocations, recipeProblems := taggedGoTestsInRecipe(body, workflowPath, jobID, recipe)
						for _, invocation := range recipeInvocations {
							if problem := taggedRecipeEvidenceProblem(
								recipe, invocation, command, effectiveEnv, workingDirectory, justPipelineSafe, coverageJoin,
							); problem != "" {
								if len(invocation.ExplicitTag) > 0 {
									problems = append(problems, fmt.Sprintf("%s -> just %s cannot prove execution: %s", where, recipe, problem))
								}
								continue
							}
							invocation.ContextProblem = ""
							invocations = append(invocations, invocation)
						}
						for _, problem := range recipeProblems {
							problems = append(problems, fmt.Sprintf("%s -> just %s: %s", where, recipe, problem))
						}
					case "node":
						if !taggedCommandNamesScript(command, migrationExecutionScript) {
							continue
						}
						mode := taggedStepEnv("MODE", workflow.Env, job.Env, step.Env, command.Env)
						if mode != "run" {
							continue
						}
						if problem := taggedEvidenceContextProblem(command, effectiveEnv, workingDirectory, pipelineSafe); problem != "" {
							problems = append(problems, fmt.Sprintf("%s: migration command %q cannot prove execution: %s", where, command.Raw, problem))
							continue
						}
						if migrationErr != nil {
							problems = append(problems, fmt.Sprintf("%s: %v", where, migrationErr))
							continue
						}
						tier := taggedStepEnv("TIER", workflow.Env, job.Env, step.Env, command.Env)
						if strings.ContainsAny(tier, "$`{}") {
							problems = append(problems, fmt.Sprintf("%s: dynamic migration TIER %q cannot prove execution", where, tier))
							continue
						}
						run, ok := migrationRuns[tier]
						if !ok || jobID != "migration-"+tier {
							problems = append(problems, fmt.Sprintf("%s: MODE=run has unsupported tier/job binding %q", where, tier))
							continue
						}
						invocation := newTaggedInvocation(map[string]bool{run.BuildTag: true})
						invocation.Workflow = workflowPath
						invocation.Job = jobID
						invocation.Entrypoint = taggedEntrypointScript
						invocation.EntryName = migrationExecutionScript
						invocation.Raw = command.Raw
						invocation.Packages = []string{run.Package}
						invocations = append(invocations, invocation)
					}
				}
				for _, name := range githubEnvWrites {
					priorBuildContext[name] = "prior GITHUB_ENV write"
				}
			}
		}
	}
	return invocations, uniqueStrings(problems)
}

func readTaggedWorkflows(t *testing.T, root string) map[string]taggedWorkflow {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	workflows := map[string]taggedWorkflow{}
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var workflow taggedWorkflow
		if err := yaml.Unmarshal(source, &workflow); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		workflows[filepath.ToSlash(filepath.Join(".github/workflows", entry.Name()))] = workflow
	}
	if len(workflows) == 0 {
		t.Fatal("tagged-test enrollment parsed no workflow files")
	}
	return workflows
}

var taggedJustRecipeHeader = regexp.MustCompile(`^(_?[a-z0-9][a-z0-9_-]*)(?:\s+[^:]*)?:`)

func taggedJustRecipeNames(justfile string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(justfile, "\n") {
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
			continue
		}
		if match := taggedJustRecipeHeader.FindStringSubmatch(line); match != nil {
			names[match[1]] = true
		}
	}
	return names
}

func taggedGoTestsInRecipe(body, workflow, job, recipe string) ([]taggedTestInvocation, []string) {
	commands, err := taggedStaticCommands(body)
	if err != nil {
		return nil, []string{fmt.Sprintf("cannot parse recipe shell: %v", err)}
	}
	var (
		invocations []taggedTestInvocation
		problems    []string
	)
	for _, command := range commands {
		if command.Executable != "go" || len(command.Args) == 0 {
			continue
		}
		if command.Args[0].Dynamic {
			problems = append(problems, fmt.Sprintf("dynamic Go subcommand %q cannot prove execution", command.Args[0].Text))
			continue
		}
		if command.Args[0].Text != "test" {
			continue
		}
		invocation, err := parseTaggedGoTestArgs(command.Args[1:])
		if err != nil {
			problems = append(problems, fmt.Sprintf("unparseable go test command %q: %v", command.Raw, err))
			continue
		}
		invocation.ContextProblem = taggedEvidenceContextProblem(command, command.Env, "", false)
		invocation.Pipeline = command.Pipeline
		invocation.Workflow = workflow
		invocation.Job = job
		invocation.Entrypoint = taggedEntrypointRecipe
		invocation.EntryName = recipe
		invocation.Raw = command.Raw
		invocations = append(invocations, invocation)
	}
	return invocations, problems
}

// taggedStaticCommands recognizes only a command's executable position. It
// cannot turn comments, echo arguments, or words such as "cargo test" into Go
// execution evidence.
func taggedStaticCommands(script string) ([]taggedStaticCommand, error) {
	segments, err := taggedShellSegments(script)
	if err != nil {
		return nil, err
	}
	controlReasons, err := taggedShellControlReasons(segments)
	if err != nil {
		return nil, err
	}
	var commands []taggedStaticCommand
	priorNonGating := ""
	pipelineDisabled := false
	shellBuildContext := map[string]string{}
	for segmentIndex, segment := range segments {
		tokens, err := taggedShellFields(segment.Text)
		if err != nil {
			return nil, err
		}
		index, executable := taggedExecutable(tokens)
		inlineEnv := taggedInlineEnv(tokens)
		if index < 0 {
			taggedRememberBuildContext(shellBuildContext, inlineEnv)
			continue
		}
		inlineEnv = taggedInlineEnv(tokens[:index])
		commandEnv := taggedMergedEnv(shellBuildContext, inlineEnv)
		pipeline := segment.Before == "|" || segment.After == "|"
		unsafe := controlReasons[segmentIndex]
		if unsafe == "" && taggedUnsafeConnector(segment.Before) {
			unsafe = fmt.Sprintf("preceded by unmodeled shell connector %q", segment.Before)
		}
		if unsafe == "" && taggedUnsafeConnector(segment.After) {
			unsafe = fmt.Sprintf("followed by unmodeled shell connector %q", segment.After)
		}
		if unsafe == "" && priorNonGating != "" {
			unsafe = priorNonGating
		}
		if unsafe == "" && pipeline && pipelineDisabled {
			unsafe = "follows `set +o pipefail`, so pipeline failure would not gate the lane"
		}
		commands = append(commands, taggedStaticCommand{
			Executable: executable,
			Args:       append([]taggedShellToken(nil), tokens[index+1:]...),
			Env:        commandEnv,
			Raw:        strings.TrimSpace(segment.Text),
			Unsafe:     unsafe,
			Pipeline:   pipeline,
		})
		if executable == "exit" || executable == "return" {
			priorNonGating = fmt.Sprintf("follows %s, so reachability is not proven", executable)
		}
		if executable == "set" && taggedShellDisablesFailure(tokens[index+1:]) {
			priorNonGating = "follows `set +e`, so failure would not gate the lane"
		}
		if executable == "set" && taggedShellDisablesOption(tokens[index+1:], "pipefail") {
			pipelineDisabled = true
		}
		if executable == "export" {
			taggedRememberBuildContext(shellBuildContext, taggedInlineEnv(tokens[index+1:]))
		}
		if executable == "unset" {
			for _, token := range tokens[index+1:] {
				if contains(taggedBuildContextEnvNames, token.Text) {
					shellBuildContext[token.Text] = "unset in an earlier shell command"
				}
			}
		}
	}
	return commands, nil
}

func taggedShellSegments(script string) ([]taggedShellSegment, error) {
	var (
		segments []taggedShellSegment
		current  strings.Builder
		quote    byte
		escaped  bool
		comment  bool
		previous string
	)
	flush := func(connector string) {
		segment := strings.TrimSpace(current.String())
		if segment != "" {
			segments = append(segments, taggedShellSegment{Text: segment, Before: previous, After: connector})
		}
		current.Reset()
		previous = connector
	}
	for index := 0; index < len(script); index++ {
		char := script[index]
		if comment {
			if char == '\n' {
				comment = false
				flush("\n")
			}
			continue
		}
		if escaped {
			if char == '\n' {
				current.WriteByte(' ')
			} else {
				current.WriteByte(char)
			}
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			current.WriteByte(char)
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			current.WriteByte(char)
			continue
		}
		if char == '#' && (index == 0 || unicode.IsSpace(rune(script[index-1])) || strings.ContainsRune(";|&", rune(script[index-1]))) {
			comment = true
			continue
		}
		switch char {
		case '\n':
			flush("\n")
		case ';':
			connector := ";"
			if index+1 < len(script) && script[index+1] == ';' {
				connector = ";;"
				index++
			}
			flush(connector)
		case '|', '&':
			connector := string(char)
			if index+1 < len(script) && script[index+1] == char {
				connector += string(char)
				index++
			}
			flush(connector)
		default:
			current.WriteByte(char)
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing shell escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated shell quote")
	}
	flush("")
	return segments, nil
}

func taggedShellControlReasons(segments []taggedShellSegment) ([]string, error) {
	reasons := make([]string, len(segments))
	depth := 0
	for index, segment := range segments {
		tokens, err := taggedShellFields(segment.Text)
		if err != nil {
			return nil, err
		}
		if len(tokens) == 0 {
			continue
		}
		first := strings.TrimPrefix(tokens[0].Text, "@")
		switch first {
		case "fi", "done", "esac", "}", ")":
			if depth > 0 {
				depth--
			}
		}
		switch first {
		case "if", "for", "select", "while", "until", "case", "function", "{", "(", "[[":
			depth++
		case "!":
			reasons[index] = "contains unmodeled negated shell execution"
		default:
			if strings.HasSuffix(first, "()") || strings.Contains(first, "(){") {
				depth++
			}
		}
		if depth > 0 && reasons[index] == "" {
			reasons[index] = fmt.Sprintf("contains unmodeled shell control flow around %q", first)
		}
	}
	return reasons, nil
}

func taggedUnsafeConnector(connector string) bool {
	return connector == "&&" || connector == "||" || connector == "&" || connector == ";;"
}

func taggedInlineEnv(tokens []taggedShellToken) map[string]string {
	env := map[string]string{}
	for index, token := range tokens {
		text := token.Text
		if index == 0 {
			text = strings.TrimPrefix(text, "@")
		}
		if !taggedShellAssignment.MatchString(text) {
			continue
		}
		name, value, _ := strings.Cut(text, "=")
		env[name] = value
	}
	return env
}

func taggedShellDisablesFailure(args []taggedShellToken) bool {
	return containsText(args, "+e") || taggedShellDisablesOption(args, "errexit")
}

func taggedShellDisablesOption(args []taggedShellToken, option string) bool {
	for index, arg := range args {
		if arg.Text == "+o" && index+1 < len(args) && args[index+1].Text == option {
			return true
		}
	}
	return false
}

func containsText(tokens []taggedShellToken, value string) bool {
	for _, token := range tokens {
		if token.Text == value {
			return true
		}
	}
	return false
}

func taggedRememberBuildContext(target, source map[string]string) {
	for _, name := range taggedBuildContextEnvNames {
		if value, ok := source[name]; ok {
			target[name] = value
		}
	}
}

func taggedShellFields(command string) ([]taggedShellToken, error) {
	var (
		fields       []taggedShellToken
		current      strings.Builder
		quote        byte
		tokenStarted bool
		dynamic      bool
	)
	flush := func() {
		if tokenStarted {
			fields = append(fields, taggedShellToken{Text: current.String(), Dynamic: dynamic})
		}
		current.Reset()
		tokenStarted = false
		dynamic = false
	}
	for index := 0; index < len(command); index++ {
		char := command[index]
		if char == '\\' && quote != '\'' {
			if index+1 >= len(command) {
				return nil, fmt.Errorf("trailing shell escape")
			}
			index++
			current.WriteByte(command[index])
			tokenStarted = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
				tokenStarted = true
				continue
			}
			if quote == '"' && char == '$' && taggedDollarExpands(command, index) {
				dynamic = true
			}
			current.WriteByte(char)
			tokenStarted = true
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			tokenStarted = true
			continue
		}
		if unicode.IsSpace(rune(char)) {
			flush()
			continue
		}
		if char == '$' && taggedDollarExpands(command, index) || char == '`' ||
			char == '{' && index+1 < len(command) && command[index+1] == '{' {
			dynamic = true
		}
		current.WriteByte(char)
		tokenStarted = true
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated shell quote")
	}
	flush()
	return fields, nil
}

func taggedDollarExpands(command string, index int) bool {
	if index+1 >= len(command) {
		return false
	}
	next := rune(command[index+1])
	return next == '{' || next == '(' || next == '$' || next == '*' || next == '@' ||
		next == '#' || next == '?' || next == '!' || next == '-' || unicode.IsLetter(next) ||
		unicode.IsDigit(next) || next == '_'
}

var taggedShellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func taggedExecutable(tokens []taggedShellToken) (int, string) {
	index := 0
	for index < len(tokens) {
		text := tokens[index].Text
		if index == 0 {
			text = strings.TrimPrefix(text, "@")
		}
		if text == "then" || text == "do" || text == "else" {
			index++
			continue
		}
		if taggedShellAssignment.MatchString(text) {
			index++
			continue
		}
		if text == "env" {
			index++
			continue
		}
		if tokens[index].Dynamic {
			return -1, ""
		}
		return index, text
	}
	return -1, ""
}

func parseTaggedGoTestArgs(args []taggedShellToken) (taggedTestInvocation, error) {
	execution := newTaggedInvocation(nil)
	packageSeen := false
	for index := 0; index < len(args); index++ {
		token := args[index]
		field := token.Text
		if field == "--" {
			break
		}
		if strings.HasPrefix(field, "-") {
			name, value, hasValue := strings.Cut(field, "=")
			switch name {
			case "-c":
				execution.CompileOnly = true
			case "-list":
				valueToken, next, err := taggedFlagValue(args, index, value, hasValue)
				if err != nil {
					return taggedTestInvocation{}, err
				}
				_ = valueToken
				index = next
				execution.CompileOnly = true
			case "-tags":
				valueToken, next, err := taggedFlagValue(args, index, value, hasValue)
				if err != nil {
					return taggedTestInvocation{}, err
				}
				if valueToken.Dynamic {
					return taggedTestInvocation{}, fmt.Errorf("dynamic build tags %q", valueToken.Text)
				}
				if err := addTaggedTagList(execution.Tags, execution.ExplicitTag, valueToken.Text); err != nil {
					return taggedTestInvocation{}, err
				}
				index = next
			case "-run":
				valueToken, next, err := taggedFlagValue(args, index, value, hasValue)
				if err != nil {
					return taggedTestInvocation{}, err
				}
				if valueToken.Dynamic {
					return taggedTestInvocation{}, fmt.Errorf("dynamic -run pattern %q", valueToken.Text)
				}
				if _, err := regexp.Compile(valueToken.Text); err != nil {
					return taggedTestInvocation{}, fmt.Errorf("invalid -run pattern %q: %w", valueToken.Text, err)
				}
				execution.RunPattern = valueToken.Text
				index = next
			case "-count":
				valueToken, next, err := taggedFlagValue(args, index, value, hasValue)
				if err != nil {
					return taggedTestInvocation{}, err
				}
				if valueToken.Dynamic {
					return taggedTestInvocation{}, fmt.Errorf("dynamic -count value %q cannot certify execution", valueToken.Text)
				}
				if valueToken.Text == "0" {
					execution.CompileOnly = true
				}
				index = next
			case "-skip", "-exec":
				return taggedTestInvocation{}, fmt.Errorf("%s cannot certify tagged test execution", name)
			case "-race", "-v", "-x", "-work", "-a", "-n", "-json", "-benchmem", "-short", "-failfast", "-fullpath", "-cover":
				if hasValue {
					return taggedTestInvocation{}, fmt.Errorf("boolean flag %s has unexpected value", name)
				}
			case "-timeout", "-p", "-coverpkg", "-coverprofile", "-covermode", "-cpu", "-parallel", "-vet", "-toolexec", "-buildvcs", "-mod", "-modfile", "-overlay", "-gcflags", "-asmflags", "-ldflags", "-pkgdir", "-bench", "-benchtime", "-fuzz", "-fuzztime", "-fuzzminimizetime", "-shuffle":
				_, next, err := taggedFlagValue(args, index, value, hasValue)
				if err != nil {
					return taggedTestInvocation{}, err
				}
				index = next
			default:
				if packageSeen && hasValue {
					if token.Dynamic {
						return taggedTestInvocation{}, fmt.Errorf("dynamic test-binary flag %q", field)
					}
					if _, duplicate := execution.TestBinaryFlag[name]; duplicate {
						return taggedTestInvocation{}, fmt.Errorf("duplicate test-binary flag %q", name)
					}
					execution.TestBinaryFlag[name] = value
					continue
				}
				return taggedTestInvocation{}, fmt.Errorf("unsupported flag %q before the package list", field)
			}
			continue
		}
		if token.Dynamic {
			return taggedTestInvocation{}, fmt.Errorf("dynamic package argument %q", field)
		}
		if field != "." && !strings.HasPrefix(field, "./") {
			return taggedTestInvocation{}, fmt.Errorf("non-static package argument %q", field)
		}
		execution.Packages = append(execution.Packages, filepath.ToSlash(field))
		packageSeen = true
	}
	if len(execution.Packages) == 0 {
		execution.Packages = []string{"."}
	}
	if taggedRunMatchesNothing(execution.RunPattern) {
		execution.CompileOnly = true
	}
	return execution, nil
}

func taggedFlagValue(args []taggedShellToken, index int, inline string, hasInline bool) (taggedShellToken, int, error) {
	if hasInline {
		if inline == "" {
			return taggedShellToken{}, index, fmt.Errorf("flag %q has an empty value", args[index].Text)
		}
		return taggedShellToken{Text: inline, Dynamic: args[index].Dynamic}, index, nil
	}
	if index+1 >= len(args) || strings.HasPrefix(args[index+1].Text, "-") {
		return taggedShellToken{}, index, fmt.Errorf("flag %q has no value", args[index].Text)
	}
	return args[index+1], index + 1, nil
}

func taggedRunMatchesNothing(pattern string) bool {
	return pattern == "^$" || pattern == "^$/"
}

func newTaggedInvocation(explicit map[string]bool) taggedTestInvocation {
	tags := taggedBuildContextTags()
	copyExplicit := map[string]bool{}
	for tag := range explicit {
		tags[tag] = true
		copyExplicit[tag] = true
	}
	return taggedTestInvocation{
		Tags:           tags,
		ExplicitTag:    copyExplicit,
		TestBinaryFlag: map[string]string{},
	}
}

func addTaggedTagList(tags, explicit map[string]bool, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("empty build-tag list")
	}
	for _, tag := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if !regexp.MustCompile(`^[A-Za-z0-9_.]+$`).MatchString(tag) {
			return fmt.Errorf("invalid build tag %q", tag)
		}
		tags[tag] = true
		explicit[tag] = true
	}
	return nil
}

func taggedBuildContextTags() map[string]bool {
	context := build.Default
	tags := map[string]bool{
		context.GOOS:     true,
		context.GOARCH:   true,
		context.Compiler: true,
	}
	if context.CgoEnabled {
		tags["cgo"] = true
	}
	for _, tag := range context.ReleaseTags {
		tags[tag] = true
	}
	for _, tag := range context.ToolTags {
		tags[tag] = true
	}
	switch runtime.GOOS {
	case "android":
		tags["linux"] = true
	case "illumos":
		tags["solaris"] = true
	case "ios":
		tags["darwin"] = true
	}
	if taggedUnixGOOS[runtime.GOOS] {
		tags["unix"] = true
	}
	return tags
}

var taggedUnixGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true,
	"linux": true, "netbsd": true, "openbsd": true, "solaris": true,
}

func readMigrationTaggedRuns(root string) (map[string]migrationTaggedRun, error) {
	path := filepath.Join(root, filepath.FromSlash(migrationExecutionScript))
	sourceBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read migration execution adapter: %w", err)
	}
	source := string(sourceBytes)
	runs := map[string]migrationTaggedRun{
		"tier0": {BuildTag: "migration", Package: "./test/e2e/migration/tiers/tier0-offline/..."},
		"tier1": {BuildTag: "migration_tier1", Package: "./test/e2e/migration/tiers/tier1-dual/..."},
		"tier2": {BuildTag: "migration_tier2", Package: "./test/e2e/migration/tiers/tier2-ruler/..."},
	}
	constants := map[string]string{
		"TIER0_PACKAGE":             "./test/e2e/migration/tiers/tier0-offline/...",
		"MIGRATION_BUILD_TAG":       "migration",
		"TIER1_PACKAGE":             "./test/e2e/migration/tiers/tier1-dual/...",
		"MIGRATION_TIER1_BUILD_TAG": "migration_tier1",
		"TIER2_PACKAGE":             "./test/e2e/migration/tiers/tier2-ruler/...",
		"MIGRATION_TIER2_BUILD_TAG": "migration_tier2",
	}
	for name, value := range constants {
		declaration := regexp.MustCompile(`const\s+` + regexp.QuoteMeta(name) + `\s*=\s*['"]` + regexp.QuoteMeta(value) + `['"]\s*;`)
		if !declaration.MatchString(source) {
			return nil, fmt.Errorf("%s no longer declares %s=%q; update the enrollment adapter with the runner", migrationExecutionScript, name, value)
		}
	}
	mappings := []string{
		`tier0:\s*\{\s*pkg:\s*TIER0_PACKAGE,\s*buildTag:\s*MIGRATION_BUILD_TAG\s*\}`,
		`tier1:\s*\{\s*pkg:\s*TIER1_PACKAGE,\s*buildTag:\s*MIGRATION_TIER1_BUILD_TAG\s*\}`,
		`tier2:\s*\{\s*pkg:\s*TIER2_PACKAGE,\s*buildTag:\s*MIGRATION_TIER2_BUILD_TAG\s*\}`,
	}
	for _, mapping := range mappings {
		if !regexp.MustCompile(mapping).MatchString(source) {
			return nil, fmt.Errorf("%s RUN_TIERS mapping drifted from the validated Go package/tag contract", migrationExecutionScript)
		}
	}
	if !strings.Contains(source, "const args = ['test', `-tags=${run.buildTag}`, run.pkg, '-count=1', '-v'];") ||
		!strings.Contains(source, "spawnSync('go', args,") {
		return nil, fmt.Errorf("%s no longer executes RUN_TIERS through go test", migrationExecutionScript)
	}
	return runs, nil
}

func taggedCommandNamesScript(command taggedStaticCommand, script string) bool {
	return len(command.Args) == 1 && !command.Args[0].Dynamic &&
		filepath.ToSlash(strings.TrimPrefix(command.Args[0].Text, "./")) == strings.TrimPrefix(script, "./")
}

func taggedWorkflowIfExecutes(value any) (bool, string) {
	if value == nil {
		return true, ""
	}
	switch typed := value.(type) {
	case bool:
		return typed, ""
	case string:
		literal := strings.TrimSpace(typed)
		if literal == "" {
			return true, ""
		}
		literal = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(literal, "${{"), "}}"))
		if truth, constant := taggedConstantWorkflowBool(literal); constant {
			return truth, ""
		}
		// A data-dependent condition is eligible evidence because the enrollment
		// contract asks whether the assertion has a real execution path, not
		// whether it runs on every event. Constant-false expressions are rejected
		// above, including short-circuit forms such as `false && env.ENABLED`.
		return true, ""
	default:
		return false, fmt.Sprintf("non-literal workflow if value of type %T cannot certify execution", value)
	}
}

func taggedWorkflowFailureGates(value any) (bool, string) {
	if value == nil {
		return true, ""
	}
	switch typed := value.(type) {
	case bool:
		return !typed, ""
	case string:
		literal := strings.TrimSpace(typed)
		if literal == "" {
			return true, ""
		}
		literal = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(literal, "${{"), "}}"))
		if truth, constant := taggedConstantWorkflowBool(literal); constant {
			return !truth, ""
		}
		return false, fmt.Sprintf("data-dependent continue-on-error expression %q cannot certify failure gating", typed)
	default:
		return false, fmt.Sprintf("non-literal continue-on-error value of type %T cannot certify failure gating", value)
	}
}

func taggedConstantWorkflowBool(expression string) (bool, bool) {
	parsed, err := parser.ParseExpr(expression)
	if err != nil {
		return false, false
	}
	return taggedEvalConstantBool(parsed)
}

func taggedEvalConstantBool(expression ast.Expr) (bool, bool) {
	switch expression := expression.(type) {
	case *ast.Ident:
		switch {
		case strings.EqualFold(expression.Name, "true"):
			return true, true
		case strings.EqualFold(expression.Name, "false"):
			return false, true
		default:
			return false, false
		}
	case *ast.ParenExpr:
		return taggedEvalConstantBool(expression.X)
	case *ast.UnaryExpr:
		if expression.Op != token.NOT {
			return false, false
		}
		value, constant := taggedEvalConstantBool(expression.X)
		return !value, constant
	case *ast.BinaryExpr:
		left, leftConstant := taggedEvalConstantBool(expression.X)
		right, rightConstant := taggedEvalConstantBool(expression.Y)
		switch expression.Op {
		case token.LAND:
			if leftConstant && !left || rightConstant && !right {
				return false, true
			}
			if leftConstant && rightConstant {
				return left && right, true
			}
		case token.LOR:
			if leftConstant && left || rightConstant && right {
				return true, true
			}
			if leftConstant && rightConstant {
				return left || right, true
			}
		case token.EQL:
			if leftConstant && rightConstant {
				return left == right, true
			}
		case token.NEQ:
			if leftConstant && rightConstant {
				return left != right, true
			}
		}
	}
	return false, false
}

func taggedWorkingDirectory(workflow, job taggedWorkflowDefaults, step taggedWorkflowStep) string {
	if value := strings.TrimSpace(step.WorkingDirectory); value != "" {
		return value
	}
	if value := strings.TrimSpace(job.Run.WorkingDirectory); value != "" {
		return value
	}
	return strings.TrimSpace(workflow.Run.WorkingDirectory)
}

func taggedWorkflowRunsOnLinux(runsOn any) bool {
	value, ok := runsOn.(string)
	if !ok {
		return false
	}
	return regexp.MustCompile(`^ubuntu-(?:latest|[0-9]+\.[0-9]+)$`).MatchString(strings.TrimSpace(value))
}

func taggedWorkflowPipelineIsFailClosed(workflow, job taggedWorkflowDefaults, step taggedWorkflowStep) bool {
	shell := strings.TrimSpace(step.Shell)
	if shell == "" {
		shell = strings.TrimSpace(job.Run.Shell)
	}
	if shell == "" {
		shell = strings.TrimSpace(workflow.Run.Shell)
	}
	if shell == "" || shell == "bash" {
		return true
	}
	return strings.Contains(shell, "bash") && strings.Contains(shell, "pipefail") &&
		(strings.Contains(shell, " -e") || strings.Contains(shell, " -o errexit"))
}

func taggedJustPipelineIsFailClosed(justfile string) bool {
	const required = `set shell := ["bash", "-eu", "-o", "pipefail", "-c"]`
	count := 0
	for _, line := range strings.Split(justfile, "\n") {
		if strings.TrimSpace(line) == required {
			count++
		}
	}
	return count == 1
}

func taggedCoverageJoinIsFailClosed(body string) bool {
	if !strings.Contains(body, "LANES=default+chdb;") || !strings.Contains(body, "LANES=default;") {
		return false
	}
	commands, err := taggedStaticCommands(body)
	if err != nil {
		return false
	}
	joins := 0
	for _, command := range commands {
		if taggedCommandNamesScript(command, ".github/scripts/coverage-summary.mjs") && command.Executable == "node" &&
			command.Unsafe == "" && !command.Pipeline && command.Env["COVERAGE_LANES"] == "$LANES" {
			joins++
		}
	}
	return joins == 1
}

func taggedMergedEnv(environments ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, environment := range environments {
		for name, value := range environment {
			merged[name] = value
		}
	}
	return merged
}

var taggedBuildContextEnvNames = []string{
	"CGO_ENABLED", "GOOS", "GOARCH", "GOFLAGS", "GOEXPERIMENT",
	"GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM",
}

func taggedGitHubEnvBuildContextWrites(script string) []string {
	if !strings.Contains(script, "GITHUB_ENV") {
		return nil
	}
	var names []string
	for _, name := range taggedBuildContextEnvNames {
		if regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `\s*(?:=|<<)`).MatchString(script) {
			names = append(names, name)
		}
	}
	return names
}

func taggedEvidenceContextProblem(
	command taggedStaticCommand,
	environment map[string]string,
	workingDirectory string,
	pipelineSafe bool,
) string {
	if command.Unsafe != "" {
		return command.Unsafe
	}
	for _, name := range taggedBuildContextEnvNames {
		if _, ok := environment[name]; ok {
			return fmt.Sprintf("overrides unmodeled Go build context variable %s", name)
		}
	}
	if workingDirectory != "" && filepath.Clean(workingDirectory) != "." {
		return fmt.Sprintf("runs from non-root working-directory %q", workingDirectory)
	}
	if command.Pipeline && !pipelineSafe {
		return "uses a pipeline without a proven errexit and pipefail shell"
	}
	return ""
}

func taggedRecipeEvidenceProblem(
	recipe string,
	invocation taggedTestInvocation,
	command taggedStaticCommand,
	environment map[string]string,
	workingDirectory string,
	pipelineSafe bool,
	coverageJoin bool,
) string {
	if problem := taggedEvidenceContextProblem(command, environment, workingDirectory, pipelineSafe); problem != "" {
		return problem
	}
	if invocation.Pipeline && !pipelineSafe {
		return "recipe uses a pipeline without the pinned fail-closed Just shell"
	}
	contextProblem := invocation.ContextProblem
	if invocation.Pipeline && pipelineSafe && strings.Contains(contextProblem, "pipeline without") {
		contextProblem = ""
	}
	if contextProblem == "" {
		return ""
	}
	// The measured coverage recipe intentionally keeps its chDB half optional
	// for local use. Its CI caller turns that branch into fail-closed evidence:
	// coverage-summary rejects any result that did not execute both named lanes.
	if recipe == "coverage" && coverageJoin && environment["COVERAGE_REQUIRE_LANES"] == "default+chdb" &&
		strings.Contains(contextProblem, "unmodeled shell control flow") &&
		invocation.ExplicitTag["chdb"] && invocation.ExplicitTag["agpl_oracle"] &&
		invocation.ExplicitTag["chdb_agpl_oracle"] {
		return ""
	}
	return contextProblem
}

func taggedStepEnv(name string, environments ...map[string]string) string {
	value := ""
	for _, environment := range environments {
		if next, exists := environment[name]; exists {
			value = next
		}
	}
	return strings.TrimSpace(value)
}

func readTaggedLaneRegistry(t *testing.T, root string) taggedLaneRegistry {
	t.Helper()
	path := filepath.Join(root, ".github", "ci-lanes.json")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CI lane registry: %v", err)
	}
	var registry taggedLaneRegistry
	if err := json.Unmarshal(source, &registry); err != nil {
		t.Fatalf("parse CI lane registry: %v", err)
	}
	if len(registry.Lanes) == 0 {
		t.Fatal("CI lane registry contains no lanes")
	}
	return registry
}

func bindTaggedTestExecutions(invocations []taggedTestInvocation, registry taggedLaneRegistry) []taggedTestExecution {
	var executions []taggedTestExecution
	for _, invocation := range invocations {
		for _, lane := range registry.Lanes {
			if !taggedLaneOwnsInvocation(lane, invocation) {
				continue
			}
			executions = append(executions, taggedTestExecution{
				taggedTestInvocation: invocation,
				LaneID:               lane.ID,
				PackageGlobs:         append([]string(nil), lane.PackageGlobs...),
			})
		}
	}
	return executions
}

func taggedLaneOwnsInvocation(lane taggedLane, invocation taggedTestInvocation) bool {
	if lane.ID == "" || lane.Owner.Workflow != invocation.Workflow || !contains(lane.Owner.Jobs, invocation.Job) ||
		!taggedLaneRosterContains(lane.BuildTags, invocation.ExplicitTag) {
		return false
	}
	switch invocation.Entrypoint {
	case taggedEntrypointRecipe:
		return contains(lane.Recipes, invocation.EntryName)
	case taggedEntrypointDirect:
		if lane.Command == migrationExecutionClaim && lane.Owner.Workflow == ".github/workflows/migration-e2e.yml" {
			return true
		}
		return taggedLaneCommandClaimsDirect(lane.Command, invocation)
	case taggedEntrypointScript:
		return invocation.EntryName == migrationExecutionScript &&
			lane.Owner.Workflow == ".github/workflows/migration-e2e.yml" &&
			lane.Command == migrationExecutionClaim
	default:
		return false
	}
}

func taggedLaneRosterContains(roster []string, actual map[string]bool) bool {
	registered := map[string]bool{}
	for _, tag := range roster {
		registered[tag] = true
	}
	for tag := range actual {
		if !registered[tag] {
			return false
		}
	}
	return true
}

func taggedLaneCommandClaimsDirect(command string, invocation taggedTestInvocation) bool {
	commands, err := taggedStaticCommands(command)
	if err != nil {
		return false
	}
	for _, candidate := range commands {
		if candidate.Executable != "go" || len(candidate.Args) == 0 || candidate.Args[0].Text != "test" ||
			taggedEvidenceContextProblem(candidate, candidate.Env, "", false) != "" {
			continue
		}
		claim, err := parseTaggedGoTestArgs(candidate.Args[1:])
		if err != nil || claim.CompileOnly != invocation.CompileOnly || claim.RunPattern != invocation.RunPattern ||
			!boolMapEqual(claim.ExplicitTag, invocation.ExplicitTag) || !sameNormalizedStrings(claim.Packages, invocation.Packages) ||
			!taggedTestBinaryBudgetCovers(claim.TestBinaryFlag, invocation.TestBinaryFlag) {
			continue
		}
		return true
	}
	return false
}

func taggedTestBinaryBudgetCovers(claim, invocation map[string]string) bool {
	requiredRaw, claimed := claim["-rapid.checks"]
	if !claimed {
		return true
	}
	required, err := strconv.Atoi(requiredRaw)
	if err != nil || required <= 0 {
		return false
	}
	actual, err := strconv.Atoi(invocation["-rapid.checks"])
	return err == nil && actual >= required
}

func taggedTestEnrollmentProblems(symbols []taggedTestSymbol, executions []taggedTestExecution) []string {
	if len(symbols) == 0 {
		return []string{"tagged test discovery found zero Test/Fuzz/Example symbols"}
	}
	var problems []string
	for _, symbol := range symbols {
		covered := false
		for _, execution := range executions {
			if taggedExecutionCovers(symbol, execution) {
				covered = true
				break
			}
		}
		if !covered {
			problems = append(problems, fmt.Sprintf(
				"%s:%s (%s) has no registered workflow execution with matching tags, package, -run, and entrypoint",
				symbol.File, symbol.Name, symbol.Constraint,
			))
		}
	}
	return problems
}

func taggedExecutionCovers(symbol taggedTestSymbol, execution taggedTestExecution) bool {
	if execution.CompileOnly || execution.ContextProblem != "" || execution.Workflow == "" || execution.Job == "" || execution.LaneID == "" ||
		!symbol.Constraint.Eval(func(tag string) bool { return execution.Tags[tag] }) ||
		!packagePatternsCover(execution.Packages, symbol.PackageDir) ||
		!registryGlobsCover(execution.PackageGlobs, symbol.File) {
		return false
	}
	if execution.RunPattern == "" {
		return true
	}
	match, err := regexp.MatchString(execution.RunPattern, symbol.Name)
	return err == nil && match
}

func packagePatternsCover(patterns []string, packageDir string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSuffix(filepath.ToSlash(strings.TrimPrefix(pattern, "./")), "/")
		switch {
		case pattern == "...":
			return true
		case strings.HasSuffix(pattern, "/..."):
			prefix := strings.TrimSuffix(pattern, "/...")
			if packageDir == prefix || strings.HasPrefix(packageDir, prefix+"/") {
				return true
			}
		case pattern == packageDir || pattern == "" && packageDir == ".":
			return true
		}
	}
	return false
}

func registryGlobsCover(globs []string, file string) bool {
	for _, glob := range globs {
		glob = filepath.ToSlash(strings.TrimPrefix(glob, "./"))
		if glob == "**" {
			return true
		}
		if strings.HasSuffix(glob, "/**") {
			prefix := strings.TrimSuffix(glob, "/**")
			if file == prefix || strings.HasPrefix(file, prefix+"/") {
				return true
			}
			continue
		}
		if glob == file {
			return true
		}
	}
	return false
}

func boolMapEqual(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if !right[key] {
			return false
		}
	}
	return true
}

func sameNormalizedStrings(left, right []string) bool {
	normalize := func(values []string) []string {
		out := make([]string, len(values))
		for index, value := range values {
			out[index] = filepath.ToSlash(strings.TrimSuffix(value, "/"))
		}
		sort.Strings(out)
		return out
	}
	left = normalize(left)
	right = normalize(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func TestTaggedTestEnrollmentNegativeControls(t *testing.T) {
	expr, err := buildconstraint.Parse("//go:build tagged")
	if err != nil {
		t.Fatal(err)
	}
	symbol := taggedTestSymbol{
		File: "internal/example/tagged_test.go", PackageDir: "internal/example",
		Name: "TestTagged", Constraint: expr,
	}
	invocation := newTaggedInvocation(map[string]bool{"tagged": true})
	invocation.Workflow = ".github/workflows/tagged.yml"
	invocation.Job = "test"
	invocation.Entrypoint = taggedEntrypointDirect
	invocation.Raw = "go test -tags tagged -run '^TestTagged$' ./internal/example/..."
	invocation.Packages = []string{"./internal/example/..."}
	invocation.RunPattern = "^TestTagged$"
	lane := taggedLane{
		ID: "tagged", Recipes: []string{"tagged-recipe"},
		Command:   "go test -tags tagged -run '^TestTagged$' ./internal/example/...",
		BuildTags: []string{"tagged", "extra"}, PackageGlobs: []string{"internal/example/**"},
	}
	lane.Owner.Workflow = invocation.Workflow
	lane.Owner.Jobs = []string{invocation.Job}
	registry := taggedLaneRegistry{Lanes: []taggedLane{lane}}
	assertCovered := func(t *testing.T, candidate taggedTestInvocation, candidateRegistry taggedLaneRegistry) {
		t.Helper()
		executions := bindTaggedTestExecutions([]taggedTestInvocation{candidate}, candidateRegistry)
		if problems := taggedTestEnrollmentProblems([]taggedTestSymbol{symbol}, executions); len(problems) != 0 {
			t.Fatalf("valid execution rejected: %v", problems)
		}
	}
	assertRejected := func(t *testing.T, candidate taggedTestInvocation, candidateRegistry taggedLaneRegistry) {
		t.Helper()
		executions := bindTaggedTestExecutions([]taggedTestInvocation{candidate}, candidateRegistry)
		if problems := taggedTestEnrollmentProblems([]taggedTestSymbol{symbol}, executions); len(problems) == 0 {
			t.Fatal("negative control was accepted")
		}
	}
	assertCovered(t, invocation, registry) // lane tag rosters may be a strict superset.

	t.Run("wrong invocation tag", func(t *testing.T) {
		candidate := invocation
		candidate.Tags = taggedBuildContextTags()
		candidate.Tags["other"] = true
		candidate.ExplicitTag = map[string]bool{"other": true}
		assertRejected(t, candidate, registry)
	})
	t.Run("extra unregistered tag", func(t *testing.T) {
		candidate := invocation
		candidate.Tags = taggedBuildContextTags()
		candidate.Tags["tagged"] = true
		candidate.Tags["wrong"] = true
		candidate.ExplicitTag = map[string]bool{"tagged": true, "wrong": true}
		assertRejected(t, candidate, registry)
	})
	t.Run("wrong package", func(t *testing.T) {
		candidate := invocation
		candidate.Packages = []string{"./internal/other/..."}
		assertRejected(t, candidate, registry)
	})
	t.Run("excluding run", func(t *testing.T) {
		candidate := invocation
		candidate.RunPattern = "^TestOther$"
		assertRejected(t, candidate, registry)
	})
	t.Run("compile only", func(t *testing.T) {
		candidate := invocation
		candidate.CompileOnly = true
		assertRejected(t, candidate, registry)
	})
	t.Run("missing workflow", func(t *testing.T) {
		candidate := invocation
		candidate.Workflow = ".github/workflows/missing.yml"
		assertRejected(t, candidate, registry)
	})
	t.Run("missing job", func(t *testing.T) {
		candidate := invocation
		candidate.Job = "missing"
		assertRejected(t, candidate, registry)
	})
	t.Run("registry package mismatch", func(t *testing.T) {
		changed := lane
		changed.PackageGlobs = []string{"internal/other/**"}
		assertRejected(t, invocation, taggedLaneRegistry{Lanes: []taggedLane{changed}})
	})
	t.Run("direct command claim mismatch", func(t *testing.T) {
		changed := lane
		changed.Command = "go test -tags tagged ./internal/other/..."
		assertRejected(t, invocation, taggedLaneRegistry{Lanes: []taggedLane{changed}})
	})
	t.Run("recipe claim", func(t *testing.T) {
		candidate := invocation
		candidate.Entrypoint = taggedEntrypointRecipe
		candidate.EntryName = "tagged-recipe"
		assertCovered(t, candidate, registry)
		changed := lane
		changed.Recipes = []string{"other-recipe"}
		assertRejected(t, candidate, taggedLaneRegistry{Lanes: []taggedLane{changed}})
	})
	t.Run("script claim", func(t *testing.T) {
		candidate := invocation
		candidate.Workflow = ".github/workflows/migration-e2e.yml"
		candidate.Job = "migration-tier1"
		candidate.Entrypoint = taggedEntrypointScript
		candidate.EntryName = migrationExecutionScript
		changed := lane
		changed.Owner.Workflow = candidate.Workflow
		changed.Owner.Jobs = []string{candidate.Job}
		changed.Command = migrationExecutionClaim
		assertCovered(t, candidate, taggedLaneRegistry{Lanes: []taggedLane{changed}})
		changed.Command = "unrelated execution"
		assertRejected(t, candidate, taggedLaneRegistry{Lanes: []taggedLane{changed}})
	})

	t.Run("empty universe", func(t *testing.T) {
		if problems := taggedTestEnrollmentProblems(nil, nil); len(problems) != 1 {
			t.Fatalf("empty discovery universe problems = %v", problems)
		}
	})
	t.Run("TestMain excluded", func(t *testing.T) {
		if isTaggedAssertionSymbol("TestMain") {
			t.Fatal("TestMain was classified as an assertion symbol")
		}
	})
	t.Run("tool state excluded", func(t *testing.T) {
		for _, name := range []string{".git", ".claude", ".agents", "node_modules"} {
			if !isToolStateDir(name) {
				t.Fatalf("tool-state directory %q was not excluded", name)
			}
		}
	})
	t.Run("legacy build constraint", func(t *testing.T) {
		parsed, ok, err := sourceBuildConstraint([]byte("// +build tagged\n\npackage p\n"))
		if err != nil || !ok || !parsed.Eval(func(tag string) bool { return tag == "tagged" }) {
			t.Fatalf("legacy constraint parse = %v, %v, %v", parsed, ok, err)
		}
	})
	t.Run("root ignore block", func(t *testing.T) {
		ignored, err := parseRootModuleIgnoreDirs("ignore (\n  ./vendor-one\n  ./vendor-two\n)\n")
		if err != nil || !ignoredRootModulePath("vendor-two/pkg/x.go", ignored) {
			t.Fatalf("ignore parse = %v, %v", ignored, err)
		}
	})

	parserControls := []struct {
		name    string
		command string
		wantErr bool
		compile bool
	}{
		{name: "dynamic tags", command: `go test -tags "$TAGS" ./internal/example/...`, wantErr: true},
		{name: "dynamic package", command: `go test -tags tagged "$PKG"`, wantErr: true},
		{name: "missing tag value", command: `go test -tags`, wantErr: true},
		{name: "invalid run regex", command: `go test -tags tagged -run '[' ./internal/example/...`, wantErr: true},
		{name: "skip cannot certify execution", command: `go test -tags tagged -skip TestTagged ./internal/example/...`, wantErr: true},
		{name: "exec wrapper cannot certify execution", command: `go test -tags tagged -exec ./wrapper ./internal/example/...`, wantErr: true},
		{name: "dynamic count cannot certify execution", command: `go test -tags tagged -count "$COUNT" ./internal/example/...`, wantErr: true},
		{name: "compile build", command: `go test -c -tags tagged ./internal/example/...`, compile: true},
		{name: "compile empty run", command: `go test -tags tagged -run '^$' ./internal/example/...`, compile: true},
	}
	for _, control := range parserControls {
		t.Run(control.name, func(t *testing.T) {
			commands, err := taggedStaticCommands(control.command)
			if err != nil || len(commands) != 1 {
				t.Fatalf("static command parse = %v, %v", commands, err)
			}
			parsed, parseErr := parseTaggedGoTestArgs(commands[0].Args[1:])
			if control.wantErr {
				if parseErr == nil {
					t.Fatalf("dynamic/unparseable command accepted: %#v", parsed)
				}
				return
			}
			if parseErr != nil || parsed.CompileOnly != control.compile {
				t.Fatalf("command classification = %#v, %v", parsed, parseErr)
			}
		})
	}

	t.Run("comments and prose are not commands", func(t *testing.T) {
		commands, err := taggedStaticCommands(strings.Join([]string{
			"# go test -tags tagged ./internal/example/...",
			`echo "go test -tags tagged ./internal/example/..."`,
			"cargo test -tags tagged ./internal/example/...",
			`echo "just tagged-recipe"`,
		}, "\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, command := range commands {
			if command.Executable == "go" || command.Executable == "just" {
				t.Fatalf("prose counted as execution: %#v", command)
			}
		}
	})
	t.Run("dynamic Go subcommand fails closed", func(t *testing.T) {
		invocations, problems := taggedGoTestsInRecipe(`go "$SUBCOMMAND" -tags tagged ./internal/example/...`, "workflow.yml", "test", "recipe")
		if len(invocations) != 0 || len(problems) == 0 {
			t.Fatalf("dynamic subcommand evidence=%v problems=%v", invocations, problems)
		}
	})
	t.Run("vet is not execution evidence", func(t *testing.T) {
		invocations, problems := taggedGoTestsInRecipe(`go vet -tags tagged ./internal/example/...`, "workflow.yml", "test", "recipe")
		if len(invocations) != 0 || len(problems) != 0 {
			t.Fatalf("vet evidence=%v problems=%v", invocations, problems)
		}
	})

	for _, control := range []struct {
		name   string
		script string
	}{
		{name: "and unreachable", script: `false && go test -tags tagged ./internal/example/...`},
		{name: "or unreachable", script: `true || go test -tags tagged ./internal/example/...`},
		{name: "failure neutralized", script: `go test -tags tagged ./internal/example/... || true`},
		{name: "backgrounded", script: `go test -tags tagged ./internal/example/... &`},
		{name: "if conditional", script: `if true; then go test -tags tagged ./internal/example/...; fi`},
		{name: "loop conditional", script: `for value in one; do go test -tags tagged ./internal/example/...; done`},
		{name: "case conditional", script: `case x in x) go test -tags tagged ./internal/example/...;; esac`},
		{name: "function body", script: "run() { go test -tags tagged ./internal/example/...; }\nrun"},
		{name: "after exit", script: "exit 0\ngo test -tags tagged ./internal/example/..."},
		{name: "after return", script: "return 0\ngo test -tags tagged ./internal/example/..."},
		{name: "errexit disabled", script: "set +e\ngo test -tags tagged ./internal/example/..."},
		{name: "pipefail disabled", script: "set +o pipefail\ngo test -tags tagged ./internal/example/... | tee test.log"},
		{name: "exported build context", script: "export GOOS=windows\ngo test -tags tagged ./internal/example/..."},
		{name: "negated", script: `! go test -tags tagged ./internal/example/...`},
	} {
		t.Run(control.name, func(t *testing.T) {
			invocations, _ := taggedGoTestsInRecipe(control.script, "workflow.yml", "test", "recipe")
			for _, candidate := range invocations {
				if candidate.ContextProblem == "" {
					t.Fatalf("unmodeled shell control flow counted as execution: %#v", invocations)
				}
			}
		})
	}

	t.Run("pipeline remains gating evidence", func(t *testing.T) {
		invocations, problems := taggedGoTestsInRecipe(
			`go test -tags tagged ./internal/example/... | tee test.log`, "workflow.yml", "test", "recipe",
		)
		if len(invocations) != 1 || len(problems) != 0 {
			t.Fatalf("pipeline evidence=%v problems=%v", invocations, problems)
		}
		if problem := taggedRecipeEvidenceProblem(
			"recipe", invocations[0], taggedStaticCommand{}, nil, "", true, true,
		); problem != "" {
			t.Fatalf("fail-closed pipeline rejected: %q", problem)
		}
		if problem := taggedRecipeEvidenceProblem(
			"recipe", invocations[0], taggedStaticCommand{}, nil, "", false, true,
		); !strings.Contains(problem, "pipeline") {
			t.Fatalf("pipeline without pipefail accepted: %q", problem)
		}
		validJust := "set shell := [\"bash\", \"-eu\", \"-o\", \"pipefail\", \"-c\"]\n"
		if !taggedJustPipelineIsFailClosed(validJust) || taggedJustPipelineIsFailClosed(strings.Replace(validJust, "pipefail", "nounset", 1)) {
			t.Fatal("Just pipeline shell contract was not pinned")
		}
		if taggedWorkflowPipelineIsFailClosed(taggedWorkflowDefaults{}, taggedWorkflowDefaults{}, taggedWorkflowStep{Shell: "sh"}) {
			t.Fatal("custom workflow shell without pipefail was accepted")
		}
	})

	t.Run("workflow controls cannot certify execution", func(t *testing.T) {
		if executes, problem := taggedWorkflowIfExecutes(true); !executes || problem != "" {
			t.Fatalf("literal true workflow if rejected: executes=%v problem=%q", executes, problem)
		}
		if executes, problem := taggedWorkflowIfExecutes("${{ false }}"); executes || problem != "" {
			t.Fatalf("literal false workflow if accepted: executes=%v problem=%q", executes, problem)
		}
		if executes, problem := taggedWorkflowIfExecutes("${{ github.event_name == 'pull_request' }}"); !executes || problem != "" {
			t.Fatalf("data-dependent workflow path rejected: executes=%v problem=%q", executes, problem)
		}
		if executes, problem := taggedWorkflowIfExecutes("${{ false && env.ENABLED }}"); executes || problem != "" {
			t.Fatalf("constant-false workflow expression accepted: executes=%v problem=%q", executes, problem)
		}
		if gates, problem := taggedWorkflowFailureGates(false); !gates || problem != "" {
			t.Fatalf("literal false continue-on-error rejected: gates=%v problem=%q", gates, problem)
		}
		if gates, problem := taggedWorkflowFailureGates("${{ true }}"); gates || problem != "" {
			t.Fatalf("literal true continue-on-error accepted: gates=%v problem=%q", gates, problem)
		}
		if gates, problem := taggedWorkflowFailureGates("${{ matrix.allow_failure }}"); gates || !strings.Contains(problem, "data-dependent") {
			t.Fatalf("dynamic continue-on-error accepted: gates=%v problem=%q", gates, problem)
		}
		if !taggedWorkflowRunsOnLinux("ubuntu-latest") || taggedWorkflowRunsOnLinux("windows-latest") ||
			taggedWorkflowRunsOnLinux("${{ matrix.os }}") {
			t.Fatal("runner build context was not bound to static Linux")
		}
	})

	t.Run("property rapid budget cannot fall below registry claim", func(t *testing.T) {
		claim := map[string]string{"-rapid.checks": "500"}
		for _, invocation := range []map[string]string{
			{"-rapid.checks": "500"},
			{"-rapid.checks": "750"},
		} {
			if !taggedTestBinaryBudgetCovers(claim, invocation) {
				t.Fatalf("sufficient rapid budget rejected: claim=%v invocation=%v", claim, invocation)
			}
		}
		for _, invocation := range []map[string]string{
			nil,
			{"-rapid.checks": "499"},
			{"-rapid.checks": "dynamic"},
		} {
			if taggedTestBinaryBudgetCovers(claim, invocation) {
				t.Fatalf("insufficient rapid budget accepted: claim=%v invocation=%v", claim, invocation)
			}
		}
		commands, err := taggedStaticCommands(
			`go test -tags tagged ./internal/example/... -rapid.checks=500 -rapid.shrinktime=5m`,
		)
		if err != nil || len(commands) != 1 {
			t.Fatalf("rapid command parse = %#v, %v", commands, err)
		}
		invocation, err := parseTaggedGoTestArgs(commands[0].Args[1:])
		if err != nil || invocation.TestBinaryFlag["-rapid.checks"] != "500" || invocation.TestBinaryFlag["-rapid.shrinktime"] != "5m" {
			t.Fatalf("rapid test-binary flags = %#v, %v", invocation.TestBinaryFlag, err)
		}
	})

	t.Run("working directory and build context drift fail closed", func(t *testing.T) {
		commands, err := taggedStaticCommands(`CGO_ENABLED=0 go test -tags tagged ./internal/example/...`)
		if err != nil || len(commands) != 1 {
			t.Fatalf("command parse = %#v, %v", commands, err)
		}
		if problem := taggedEvidenceContextProblem(commands[0], commands[0].Env, "", false); !strings.Contains(problem, "CGO_ENABLED") {
			t.Fatalf("inline environment override problem = %q", problem)
		}
		for _, name := range taggedBuildContextEnvNames {
			if problem := taggedEvidenceContextProblem(taggedStaticCommand{}, map[string]string{name: "changed"}, "", false); !strings.Contains(problem, name) {
				t.Fatalf("%s override problem = %q", name, problem)
			}
		}
		if problem := taggedEvidenceContextProblem(taggedStaticCommand{}, nil, "test/oracle", false); !strings.Contains(problem, "non-root") {
			t.Fatalf("working-directory problem = %q", problem)
		}
		if problem := taggedEvidenceContextProblem(taggedStaticCommand{}, nil, "./", false); problem != "" {
			t.Fatalf("root working-directory rejected: %q", problem)
		}
		writes := taggedGitHubEnvBuildContextWrites(`echo "GOOS=windows" >> "$GITHUB_ENV"`)
		if !contains(writes, "GOOS") || len(taggedGitHubEnvBuildContextWrites(`echo "GOOS=windows"`)) != 0 {
			t.Fatalf("prior GITHUB_ENV writes = %v", writes)
		}
		poisoned := map[string]string{}
		for _, name := range writes {
			poisoned[name] = "prior GITHUB_ENV write"
		}
		if problem := taggedEvidenceContextProblem(taggedStaticCommand{}, poisoned, "", false); !strings.Contains(problem, "GOOS") {
			t.Fatalf("prior GITHUB_ENV write still certified execution: %q", problem)
		}
	})

	t.Run("coverage conditional requires its fail-closed join", func(t *testing.T) {
		join := strings.Join([]string{
			"LANES=default+chdb;",
			"LANES=default;",
			`COVERAGE_LANES="$LANES" node .github/scripts/coverage-summary.mjs`,
		}, "\n")
		if !taggedCoverageJoinIsFailClosed(join) || taggedCoverageJoinIsFailClosed(strings.Replace(join, "coverage-summary.mjs", "echo", 1)) ||
			taggedCoverageJoinIsFailClosed(join+" || true") {
			t.Fatal("coverage summary join contract was not pinned")
		}
		candidate := invocation
		candidate.ExplicitTag = map[string]bool{"chdb": true, "agpl_oracle": true, "chdb_agpl_oracle": true}
		candidate.ContextProblem = `contains unmodeled shell control flow around "then"`
		candidate.Pipeline = true
		environment := map[string]string{"COVERAGE_REQUIRE_LANES": "default+chdb"}
		if problem := taggedRecipeEvidenceProblem(
			"coverage", candidate, taggedStaticCommand{}, environment, "", true, true,
		); problem != "" {
			t.Fatalf("valid coverage join rejected: %q", problem)
		}
		if problem := taggedRecipeEvidenceProblem(
			"coverage", candidate, taggedStaticCommand{}, environment, "", true, false,
		); problem == "" {
			t.Fatal("coverage evidence survived removal of its summary join")
		}
	})
}
