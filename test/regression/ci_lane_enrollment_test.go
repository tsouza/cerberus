package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	mutationLaneID = "quality.mutation"
	propertyLaneID = "quality.property"
)

var queryHeads = []string{"logql", "promql", "traceql"}

var queryHeadAPIPackages = map[string]string{
	"logql":   "loki",
	"promql":  "prom",
	"traceql": "tempo",
}

type ciEnrollmentLane struct {
	ID           string
	Layers       []string
	OracleClass  string
	Recipes      []string
	Command      string
	BuildTags    []string
	PackageGlobs []string
	Substrate    string
	RiskDomains  []string
	MergePosture string
	MainPosture  string
	Determinism  string
}

type taggedTestFile struct {
	Path string
	Tag  string
}

func TestCILaneTaggedTestEnrollment(t *testing.T) {
	t.Parallel()

	negativeControl := []ciEnrollmentLane{{
		BuildTags:    []string{"chdb"},
		PackageGlobs: []string{"test/property/**"},
	}}
	if !taggedTestEnrolled(negativeControl, taggedTestFile{Path: "test/property/promql_test.go", Tag: "chdb"}) {
		t.Fatal("tagged-test enrollment negative control rejected its matching lane")
	}
	if taggedTestEnrolled(negativeControl, taggedTestFile{Path: "test/property/promql_test.go", Tag: "integration"}) {
		t.Fatal("tagged-test enrollment ignored a missing build tag")
	}
	if taggedTestEnrolled(negativeControl, taggedTestFile{Path: "internal/promql/lower_test.go", Tag: "chdb"}) {
		t.Fatal("tagged-test enrollment ignored a missing path claim")
	}

	lanes := readCIEnrollmentLanes(t)
	tagged := rootModuleTaggedTests(t)
	if len(tagged) == 0 {
		t.Fatal("root-module tagged-test inventory is empty")
	}
	for _, file := range tagged {
		if taggedTestEnrolled(lanes, file) {
			continue
		}
		t.Errorf("%s uses build tag %q but no registered lane claims both that tag and path; "+
			"add the real executing lane's build_tags/package_globs instead of leaving the test compiled by no CI lane",
			file.Path, file.Tag)
	}
}

func TestCILaneMutationPackageEnrollment(t *testing.T) {
	t.Parallel()

	if !laneClaimsMutationScope([]string{"internal/promql/**"}, "internal/promql") {
		t.Fatal("mutation enrollment negative control rejected a recursive package claim")
	}
	if laneClaimsMutationScope([]string{"internal/promql/**"}, "internal/logql") {
		t.Fatal("mutation enrollment ignored a missing matrix scope")
	}

	lane := requireCIEnrollmentLane(t, readCIEnrollmentLanes(t), mutationLaneID)
	scopes := mutationPackageScopes(t)
	for _, scope := range scopes {
		if laneClaimsMutationScope(lane.PackageGlobs, scope) {
			continue
		}
		t.Errorf("%s emits mutation scope %q but %s does not claim it in package_globs; "+
			"impact selection could omit a package the full mutation matrix measures",
			mutationMatrixScript, scope, mutationLaneID)
	}

	for _, glob := range lane.PackageGlobs {
		if !strings.HasPrefix(glob, "internal/") {
			continue
		}
		backed := false
		for _, scope := range scopes {
			if registryGlobMatches(glob, scope+"/enrollment.go") {
				backed = true
				break
			}
		}
		if !backed {
			t.Errorf("%s claims mutation package glob %q, but %s emits no matching scope; "+
				"the selector can route changes to work the matrix never executes",
				mutationLaneID, glob, mutationMatrixScript)
		}
	}
}

func TestCILanePropertyShapeEnrollment(t *testing.T) {
	t.Parallel()

	if missing := missingQueryHeads(map[string][]taggedTestFile{
		"promql":  {{Path: "promql_test.go", Tag: "chdb"}},
		"traceql": {{Path: "traceql_test.go", Tag: "chdb"}},
	}); !slices.Equal(missing, []string{"logql"}) {
		t.Fatalf("property-shape negative control did not expose the missing LogQL head: %v", missing)
	}

	shapes := propertyRunnerShapes(t)
	if missing := missingQueryHeads(shapes); len(missing) > 0 {
		t.Fatalf("property runner has no live from-scratch shape for head(s) %v", missing)
	}

	lane := requireCIEnrollmentLane(t, readCIEnrollmentLanes(t), propertyLaneID)
	if lane.MainPosture != "always" || lane.Determinism != "seeded" {
		t.Errorf("%s must remain an always-on main lane with seeded evidence; got main=%q determinism=%q",
			propertyLaneID, lane.MainPosture, lane.Determinism)
	}
	if !strings.Contains(lane.Command, "./test/property/...") ||
		!strings.Contains(lane.Command, "rapid.checks=500") {
		t.Errorf("%s command no longer runs the full property tree at the registered deep-sweep count: %q",
			propertyLaneID, lane.Command)
	}

	for _, head := range queryHeads {
		if !slices.Contains(lane.RiskDomains, head) {
			t.Errorf("%s has a live %s property shape but omits %q from risk_domains",
				propertyLaneID, head, head)
		}
		if !laneClaimsMutationScope(lane.PackageGlobs, "internal/"+head) {
			t.Errorf("%s has a live %s property shape but no internal/%s/** impact claim",
				propertyLaneID, head, head)
		}
		for _, shape := range shapes[head] {
			if !slices.Contains(lane.BuildTags, shape.Tag) {
				t.Errorf("%s property runner %s needs tag %q, absent from the lane's build_tags",
					head, shape.Path, shape.Tag)
			}
			if !pathClaimed(lane.PackageGlobs, shape.Path) {
				t.Errorf("%s property runner %s is outside the lane's package_globs", head, shape.Path)
			}
		}
	}
}

func TestCILaneLiveParityEnrollment(t *testing.T) {
	t.Parallel()

	negativeControl := map[string][]string{
		"promql":  {"compatibility.prometheus"},
		"traceql": {"compatibility.tempo"},
	}
	if missing := missingQueryHeadsByLane(negativeControl); !slices.Equal(missing, []string{"logql"}) {
		t.Fatalf("live-parity negative control did not expose the missing LogQL head: %v", missing)
	}

	byHead := map[string][]string{}
	for _, lane := range readCIEnrollmentLanes(t) {
		if lane.Substrate != "reference-stack" || lane.OracleClass != "reference" ||
			lane.MergePosture != "always" || lane.MainPosture != "always" ||
			!slices.Contains(lane.Layers, "6d") {
			continue
		}
		if len(lane.Recipes) == 0 && strings.TrimSpace(lane.Command) == "" {
			t.Errorf("live reference lane %s declares no executable recipe or command", lane.ID)
		}
		for _, head := range queryHeads {
			if slices.Contains(lane.RiskDomains, head) {
				byHead[head] = append(byHead[head], lane.ID)
			}
		}
	}
	if missing := missingQueryHeadsByLane(byHead); len(missing) > 0 {
		t.Fatalf("no always-on layer-6d live reference-stack parity lane for head(s) %v", missing)
	}
}

func readCIEnrollmentLanes(t *testing.T) []ciEnrollmentLane {
	t.Helper()

	registry := readCILaneRegistry(t)
	lanes := make([]ciEnrollmentLane, 0, len(registry.Lanes))
	for _, lane := range registry.Lanes {
		lanes = append(lanes, ciEnrollmentLane{
			ID:           lane.ID,
			Layers:       slices.Clone(lane.Layers),
			OracleClass:  lane.OracleClass,
			Recipes:      slices.Clone(lane.Recipes),
			Command:      lane.Command,
			BuildTags:    slices.Clone(lane.BuildTags),
			PackageGlobs: slices.Clone(lane.PackageGlobs),
			Substrate:    lane.Substrate,
			RiskDomains:  slices.Clone(lane.RiskDomains),
			MergePosture: lane.MergePosture,
			MainPosture:  lane.MainPosture,
			Determinism:  lane.Determinism,
		})
	}
	return lanes
}

func requireCIEnrollmentLane(t *testing.T, lanes []ciEnrollmentLane, id string) ciEnrollmentLane {
	t.Helper()

	var matches []ciEnrollmentLane
	for _, lane := range lanes {
		if lane.ID == id {
			matches = append(matches, lane)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("registry contains %d lane(s) named %q, want exactly one", len(matches), id)
	}
	return matches[0]
}

func taggedTestEnrolled(lanes []ciEnrollmentLane, file taggedTestFile) bool {
	for _, lane := range lanes {
		if slices.Contains(lane.BuildTags, file.Tag) && pathClaimed(lane.PackageGlobs, file.Path) {
			return true
		}
	}
	return false
}

func rootModuleTaggedTests(t *testing.T) []taggedTestFile {
	t.Helper()

	ignored := ignoredModulePrefixes(t)
	var tagged []taggedTestFile
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "." {
				return nil
			}
			if strings.HasPrefix(entry.Name(), ".") || skippedDirs[entry.Name()] ||
				skippedDirs[rel] || pathHasAnyPrefix(rel, ignored) {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") || pathHasAnyPrefix(rel, ignored) {
			return nil
		}
		tag, positive := positiveBuildTag(t, path, rel)
		if positive {
			tagged = append(tagged, taggedTestFile{Path: rel, Tag: tag})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk root-module tagged tests: %v", err)
	}
	slices.SortFunc(tagged, func(a, b taggedTestFile) int {
		return strings.Compare(a.Path, b.Path)
	})
	return tagged
}

func ignoredModulePrefixes(t *testing.T) []string {
	t.Helper()

	var prefixes []string
	inBlock := false
	for _, line := range strings.Split(readFileString(t, repoRoot+"/go.mod"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "ignore (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case strings.HasPrefix(line, "ignore "):
			prefixes = append(prefixes, strings.TrimPrefix(strings.TrimPrefix(line, "ignore "), "./"))
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			prefixes = append(prefixes, strings.TrimPrefix(strings.Fields(line)[0], "./"))
		}
	}
	return prefixes
}

func positiveBuildTag(t *testing.T, path, rel string) (string, bool) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	match := buildConstraintLine.FindSubmatch(data)
	if match == nil {
		return "", false
	}
	parts := singleTermConstraint.FindStringSubmatch(strings.TrimSpace(string(match[1])))
	if parts == nil {
		t.Fatalf("%s has a non-single-term build constraint; TestEveryBuildConstraintIsASingleTerm owns the detailed error", rel)
	}
	return parts[2], parts[1] != "!"
}

func mutationPackageScopes(t *testing.T) []string {
	t.Helper()

	seen := map[string]bool{}
	for _, leg := range mutationLegs(t) {
		scope := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(leg.Scope)), "./")
		seen[scope] = true
	}
	scopes := make([]string, 0, len(seen))
	for scope := range seen {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	return scopes
}

func laneClaimsMutationScope(globs []string, scope string) bool {
	return pathClaimed(globs, strings.TrimSuffix(scope, "/")+"/enrollment.go")
}

func propertyRunnerShapes(t *testing.T) map[string][]taggedTestFile {
	t.Helper()

	const propertyDir = "test/property"
	shapes := map[string][]taggedTestFile{}
	err := filepath.WalkDir(filepath.Join(repoRoot, propertyDir), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		aliases := map[string]string{}
		var head string
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			alias := filepath.Base(importPath)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			aliases[alias] = importPath
			for _, candidate := range queryHeads {
				if importPath == "github.com/tsouza/cerberus/internal/api/"+queryHeadAPIPackages[candidate] {
					head = candidate
				}
			}
		}

		runsProperty := false
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Run" && selector.Sel.Name != "RunLogs") {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if ok && aliases[ident.Name] == "github.com/tsouza/cerberus/test/property" {
				runsProperty = true
			}
			return true
		})
		if !runsProperty {
			return nil
		}
		if head == "" {
			t.Errorf("property runner %s imports no query-head HTTP API", rel)
			return nil
		}
		tag, positive := positiveBuildTag(t, path, rel)
		if !positive {
			t.Errorf("property runner %s has no positive single-term build tag", rel)
			return nil
		}
		shapes[head] = append(shapes[head], taggedTestFile{Path: rel, Tag: tag})
		return nil
	})
	if err != nil {
		t.Fatalf("scan property runner shapes: %v", err)
	}
	return shapes
}

func missingQueryHeads(shapes map[string][]taggedTestFile) []string {
	missing := make([]string, 0, len(queryHeads))
	for _, head := range queryHeads {
		if len(shapes[head]) == 0 {
			missing = append(missing, head)
		}
	}
	return missing
}

func missingQueryHeadsByLane(byHead map[string][]string) []string {
	missing := make([]string, 0, len(queryHeads))
	for _, head := range queryHeads {
		if len(byHead[head]) == 0 {
			missing = append(missing, head)
		}
	}
	return missing
}

func pathClaimed(globs []string, path string) bool {
	for _, glob := range globs {
		if registryGlobMatches(glob, path) {
			return true
		}
	}
	return false
}

func registryGlobMatches(glob, path string) bool {
	glob = filepath.ToSlash(filepath.Clean(glob))
	path = filepath.ToSlash(filepath.Clean(path))
	if strings.HasSuffix(glob, "/**") {
		return pathHasPrefix(path, strings.TrimSuffix(glob, "/**"))
	}
	if strings.ContainsAny(glob, "*?[") {
		return false
	}
	return glob == path
}

func pathHasAnyPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if pathHasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func pathHasPrefix(path, prefix string) bool {
	prefix = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(prefix)), "/")
	path = filepath.ToSlash(filepath.Clean(path))
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
