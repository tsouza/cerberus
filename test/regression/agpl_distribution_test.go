package regression

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Cerberus is Apache-2.0, and the project distributes no AGPL software. Grafana
// and its Drilldown apps are AGPL-3.0. The maintained Grafana image
// (deploy/grafana) is built on the host that runs an e2e lane and never pushed
// to a registry; the root quickstart runs the official Grafana release, and
// only behind its opt-in `grafana` profile, so a plain `docker compose up`
// starts no AGPL image.

const (
	maintainedGrafanaDockerfile = "deploy/grafana/Dockerfile"
	maintainedGrafanaOverride   = "../../test/e2e/grafana/compose/maintained-grafana.yml"
	k3sGrafanaManifest          = "../../test/e2e/k3s/grafana.yaml"
	quickstartGrafanaProfile    = "grafana"
	officialGrafanaRepository   = "grafana/grafana"
	// grafanaLabsHubNamespace is the Docker Hub namespace of Grafana Labs'
	// server images (grafana, loki, tempo, mimir, pyroscope), all AGPL-3.0.
	grafanaLabsHubNamespace = "grafana/"
	// publishedGrafanaRepository and publishedGrafanaTagPrefix name the
	// maintained Grafana image this repository once published to GHCR.
	publishedGrafanaRepository   = "ghcr.io/tsouza/cerberus"
	publishedGrafanaTagPrefix    = "grafana-"
	e2eWorkflowPath              = "../../.github/workflows/e2e.yml"
	rootComposeFileName          = "docker-compose.yml"
	maintainedGrafanaOverrideRel = "test/e2e/grafana/compose/maintained-grafana.yml"
)

// normaliseImageRef returns an image reference's repository with Docker Hub's
// implicit parts spelled out the short way: `docker.io/`, `index.docker.io/`
// and `registry-1.docker.io/` hosts and the `library/` namespace are dropped,
// so `docker.io/grafana/grafana` and `grafana/grafana` compare equal. Any other
// registry host is kept. The tag is returned separately; a digest is dropped.
func normaliseImageRef(ref string) (repo, tag string) {
	ref, _, _ = strings.Cut(strings.TrimSpace(ref), "@")
	repo = ref
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	for _, host := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		repo = strings.TrimPrefix(repo, host)
	}
	repo = strings.TrimPrefix(repo, "library/")
	return repo, tag
}

// agplImage reports why an image reference is a known AGPL-3.0 image, or "".
func agplImage(ref string) string {
	repo, tag := normaliseImageRef(ref)
	switch {
	case strings.HasPrefix(repo, grafanaLabsHubNamespace):
		return "a Grafana Labs server image (AGPL-3.0)"
	case repo == publishedGrafanaRepository && strings.HasPrefix(tag, publishedGrafanaTagPrefix):
		return "the maintained Grafana image (Grafana and its Drilldown apps are AGPL-3.0)"
	default:
		return ""
	}
}

func TestAGPLImageRecognisesRegistrySpellings(t *testing.T) {
	t.Parallel()

	for ref, agpl := range map[string]bool{
		"grafana/grafana:12.2.9":                        true,
		"docker.io/grafana/grafana:12.2.9":              true,
		"index.docker.io/grafana/loki:3.7.1":            true,
		"registry-1.docker.io/grafana/tempo@sha256:abc": true,
		"ghcr.io/tsouza/cerberus:grafana-1a2b3c4":       true,
		"ghcr.io/tsouza/cerberus:v1.21.0":               false,
		"example.org/team/app:1.0":                      false,
		"docker.io/library/app:2":                       false,
		"localhost:5000/cerberus:e2e":                   false,
	} {
		if got := agplImage(ref) != ""; got != agpl {
			t.Errorf("agplImage(%q) flagged=%v, want %v", ref, got, agpl)
		}
	}
}

type agplComposeService struct {
	Image      string   `yaml:"image"`
	Profiles   []string `yaml:"profiles"`
	PullPolicy string   `yaml:"pull_policy"`
	Build      any      `yaml:"build"`
}

type agplComposeModel struct {
	Services map[string]agplComposeService `yaml:"services"`
}

func readComposeModel(t *testing.T, path string) agplComposeModel {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var model agplComposeModel
	if err := yaml.Unmarshal(src, &model); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(model.Services) == 0 {
		t.Fatalf("%s declares no services; this guard would inspect nothing", path)
	}
	return model
}

// buildsMaintainedGrafana reports whether a compose build section builds the
// maintained Grafana Dockerfile.
func buildsMaintainedGrafana(build any) bool {
	section, ok := build.(map[string]any)
	if !ok {
		return false
	}
	dockerfile, _ := section["dockerfile"].(string)
	return filepath.ToSlash(filepath.Clean(dockerfile)) == maintainedGrafanaDockerfile
}

// TestQuickstartDefaultProfileStartsNoAGPLImage: every service a plain
// `docker compose up` starts is free of Grafana Labs' AGPL images, and the
// Grafana service is the official release behind the opt-in profile.
func TestQuickstartDefaultProfileStartsNoAGPLImage(t *testing.T) {
	t.Parallel()

	model := readComposeModel(t, rootComposePath)
	for _, name := range slices.Sorted(maps.Keys(model.Services)) {
		svc := model.Services[name]
		if len(svc.Profiles) > 0 {
			continue
		}
		if why := agplImage(svc.Image); why != "" {
			t.Errorf("%s service %q runs %s, %s, in the default profile; a plain `docker compose up` "+
				"must not start an AGPL image — give it a profile",
				rootComposePath, name, svc.Image, why)
		}
		if buildsMaintainedGrafana(svc.Build) {
			t.Errorf("%s service %q builds %s in the default profile", rootComposePath, name, maintainedGrafanaDockerfile)
		}
	}

	grafana, ok := model.Services["grafana"]
	if !ok {
		t.Fatalf("%s has no grafana service; the README quickstart opens Grafana", rootComposePath)
	}
	if !slices.Equal(grafana.Profiles, []string{quickstartGrafanaProfile}) {
		t.Errorf("%s grafana service profiles are %v, want [%s]: Grafana is AGPL-3.0 and is started only on request",
			rootComposePath, grafana.Profiles, quickstartGrafanaProfile)
	}
	if repo, _ := normaliseImageRef(grafana.Image); repo != officialGrafanaRepository {
		t.Errorf("%s grafana service runs %q, want the official %s release; the project distributes no Grafana image",
			rootComposePath, grafana.Image, officialGrafanaRepository)
	}
	if grafana.Build != nil {
		t.Errorf("%s grafana service declares a build; the quickstart runs the unmodified upstream image", rootComposePath)
	}
}

// pushFlag matches a docker CLI push: `--push`, `--push=true`, or `docker push`.
var pushFlag = regexp.MustCompile(`(^|\s)--push(\s|=true|$)|docker\s+(image\s+)?push\b`)

// retagCommand matches a docker CLI retag, the step before a push to a registry.
var retagCommand = regexp.MustCompile(`docker\s+(image\s+)?tag\b`)

type agplWorkflowStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

type agplWorkflowFile struct {
	Jobs map[string]struct {
		Env   map[string]string  `yaml:"env"`
		Steps []agplWorkflowStep `yaml:"steps"`
	} `yaml:"jobs"`
}

// shellLines splits a script into logical lines, joining `\`-continued ones.
func shellLines(src string) []string {
	return strings.Split(regexp.MustCompile(`\\\r?\n`).ReplaceAllString(src, " "), "\n")
}

// TestNoWorkflowPublishesTheMaintainedGrafanaImage: the maintained Grafana
// image is built only into the local daemon, under the tag the e2e stacks run.
// No workflow, recipe or CI script pushes it or retags it, and no workflow
// step builds it with push enabled.
func TestNoWorkflowPublishesTheMaintainedGrafanaImage(t *testing.T) {
	t.Parallel()

	localTag := justDump(t).resolvedStringAssignments()["GRAFANA_E2E_IMAGE"]
	if localTag == "" {
		t.Fatalf("%s does not define GRAFANA_E2E_IMAGE, the tag the e2e stacks run", justfilePath)
	}
	localRepo, _ := normaliseImageRef(localTag)
	// A line refers to the maintained image when it names its Dockerfile, its
	// tag's repository, or the Just variable holding the tag.
	markers := []string{maintainedGrafanaDockerfile, localRepo, "GRAFANA_E2E_IMAGE"}
	mentions := func(line string) bool {
		return slices.ContainsFunc(markers, func(m string) bool { return strings.Contains(line, m) })
	}

	var files []string
	for _, pattern := range []string{
		"../../.github/workflows/*.yml", "../../.github/scripts/*.mjs", "../../.github/scripts/lib/*.mjs",
		"../../just/*.just", justfilePath,
	} {
		matched, err := filepath.Glob(pattern)
		if err != nil || len(matched) == 0 {
			t.Fatalf("glob %s: %v (found %d)", pattern, err, len(matched))
		}
		files = append(files, matched...)
	}

	builds, mentioned := 0, 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range shellLines(string(src)) {
			if !mentions(line) {
				continue
			}
			mentioned++
			if pushFlag.MatchString(line) || retagCommand.MatchString(line) {
				t.Errorf("%s:%d pushes or retags the maintained Grafana image; it carries AGPL-3.0 software "+
					"and is built only into the local daemon:\n%s", path, i+1, strings.TrimSpace(line))
			}
			if strings.Contains(line, maintainedGrafanaDockerfile) && regexp.MustCompile(`docker\s+(buildx\s+)?build\b`).MatchString(line) {
				builds++
				if !strings.Contains(line, "-t "+localTag+" ") && !strings.Contains(line, "-t {{GRAFANA_E2E_IMAGE}} ") {
					t.Errorf("%s:%d builds the maintained Grafana image under a tag other than %s", path, i+1, localTag)
				}
			}
		}
		if !strings.HasSuffix(path, ".yml") {
			continue
		}
		var wf agplWorkflowFile
		if err := yaml.Unmarshal(src, &wf); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, jobID := range slices.Sorted(maps.Keys(wf.Jobs)) {
			for _, step := range wf.Jobs[jobID].Steps {
				file, _ := step.With["file"].(string)
				if file != maintainedGrafanaDockerfile {
					continue
				}
				builds++
				if push, _ := step.With["push"].(bool); push {
					t.Errorf("%s job %q step %q sets push: true for the maintained Grafana image", path, jobID, step.Name)
				}
			}
		}
	}
	if builds == 0 || mentioned == 0 {
		t.Errorf("nothing builds %s (builds=%d, mentions=%d); the e2e lanes run it and must build it locally — "+
			"this guard would inspect nothing", maintainedGrafanaDockerfile, builds, mentioned)
	}
}

// TestE2EComposeJobsLayerTheMaintainedGrafana: every e2e.yml job that starts
// the compose stack's grafana profile layers the maintained-Grafana override on
// the root compose file, so the compose lanes test the patched image the k3d
// lanes run, not the quickstart's stock one.
func TestE2EComposeJobsLayerTheMaintainedGrafana(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(e2eWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", e2eWorkflowPath, err)
	}
	var wf agplWorkflowFile
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", e2eWorkflowPath, err)
	}
	if _, err := os.Stat("../../" + maintainedGrafanaOverrideRel); err != nil {
		t.Fatalf("%s: %v", maintainedGrafanaOverrideRel, err)
	}
	jobs := 0
	for _, jobID := range slices.Sorted(maps.Keys(wf.Jobs)) {
		env := wf.Jobs[jobID].Env
		if !strings.Contains(env["COMPOSE_PROFILES"], quickstartGrafanaProfile) {
			continue
		}
		jobs++
		files := strings.Split(env["COMPOSE_FILE"], ":")
		if len(files) < 2 || files[0] != rootComposeFileName || !slices.Contains(files[1:], maintainedGrafanaOverrideRel) {
			t.Errorf("%s job %q starts the grafana profile with COMPOSE_FILE %q; want %s first and %s layered on it",
				e2eWorkflowPath, jobID, env["COMPOSE_FILE"], rootComposeFileName, maintainedGrafanaOverrideRel)
		}
	}
	if jobs == 0 {
		t.Errorf("no %s job sets COMPOSE_PROFILES with %q; the compose e2e lanes must start Grafana", e2eWorkflowPath, quickstartGrafanaProfile)
	}
}

// TestE2EStacksRunTheLocallyBuiltGrafana: the compose override and the k3d
// manifest name the locally built tag and never pull it.
func TestE2EStacksRunTheLocallyBuiltGrafana(t *testing.T) {
	t.Parallel()

	localTag := justDump(t).resolvedStringAssignments()["GRAFANA_E2E_IMAGE"]
	if localTag == "" {
		t.Fatalf("%s no longer defines GRAFANA_E2E_IMAGE", justfilePath)
	}

	override := readComposeModel(t, maintainedGrafanaOverride).Services["grafana"]
	if override.Image != localTag || override.PullPolicy != "never" {
		t.Errorf("%s grafana service is image %q pull_policy %q, want %q pull_policy never",
			maintainedGrafanaOverride, override.Image, override.PullPolicy, localTag)
	}

	src, err := os.ReadFile(k3sGrafanaManifest)
	if err != nil {
		t.Fatalf("read %s: %v", k3sGrafanaManifest, err)
	}
	manifest := string(src)
	if !regexp.MustCompile(`(?m)^\s*image:\s*`+regexp.QuoteMeta(localTag)+`\s*$`).MatchString(manifest) ||
		!regexp.MustCompile(`(?m)^\s*imagePullPolicy:\s*Never\s*$`).MatchString(manifest) {
		t.Errorf("%s must run image %s with imagePullPolicy: Never", k3sGrafanaManifest, localTag)
	}
}

// TestInHouseTempopbMatchesTheUpstreamPin: internal/thirdparty/tempopb was
// synced from the grafana/tempo commit the repository tracks, the same one
// TestForkVersionSkew holds the reference image and go.mod to.
func TestInHouseTempopbMatchesTheUpstreamPin(t *testing.T) {
	t.Parallel()

	pin, err := os.ReadFile("../../compatibility/tempo/UPSTREAM_VERSION")
	if err != nil {
		t.Fatalf("read compatibility/tempo/UPSTREAM_VERSION: %v", err)
	}
	pinned := versionFileField(t, string(pin), "upstream_commit")

	source, err := os.ReadFile("../../internal/thirdparty/tempopb/SOURCE")
	if err != nil {
		t.Fatalf("read internal/thirdparty/tempopb/SOURCE: %v", err)
	}
	mod, version, ok := strings.Cut(strings.TrimSpace(string(source)), "@")
	if !ok || mod != "github.com/grafana/tempo/pkg/tempopb" {
		t.Fatalf("internal/thirdparty/tempopb/SOURCE is %q, want github.com/grafana/tempo/pkg/tempopb@<version>", source)
	}
	if synced := pseudoVersionCommit(t, version); !commitPrefixMatch(synced, pinned) {
		t.Errorf("internal/thirdparty/tempopb was synced from commit %s, but compatibility/tempo/UPSTREAM_VERSION "+
			"pins %s; run `just tempopb-sync`", synced, pinned)
	}
}
