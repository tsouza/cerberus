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
)

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
		if strings.HasPrefix(svc.Image, grafanaLabsHubNamespace) {
			t.Errorf("%s service %q runs %s in the default profile; Grafana Labs' server images are AGPL-3.0, "+
				"so a plain `docker compose up` must not start one — give it a profile",
				rootComposePath, name, svc.Image)
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
	if repo, _, _ := strings.Cut(grafana.Image, ":"); repo != officialGrafanaRepository {
		t.Errorf("%s grafana service runs %q, want the official %s release; the project distributes no Grafana image",
			rootComposePath, grafana.Image, officialGrafanaRepository)
	}
	if grafana.Build != nil {
		t.Errorf("%s grafana service declares a build; the quickstart runs the unmodified upstream image", rootComposePath)
	}
}

// pushFlag matches a docker CLI push: `--push`, `--push=true`, or `docker push`.
var pushFlag = regexp.MustCompile(`(^|\s)--push(\s|=true|$)|docker\s+(image\s+)?push\s`)

type agplWorkflowStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

type agplWorkflowFile struct {
	Jobs map[string]struct {
		Steps []agplWorkflowStep `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestNoWorkflowPublishesTheMaintainedGrafanaImage: a workflow may build
// deploy/grafana/Dockerfile only into the local daemon, under the tag the e2e
// stacks run.
func TestNoWorkflowPublishesTheMaintainedGrafanaImage(t *testing.T) {
	t.Parallel()

	localTag := justDump(t).resolvedStringAssignments()["GRAFANA_E2E_IMAGE"]
	if localTag == "" {
		t.Errorf("%s does not define GRAFANA_E2E_IMAGE, the tag the e2e stacks run", justfilePath)
	}

	paths, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob workflows: %v (found %d)", err, len(paths))
	}
	builds := 0
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var wf agplWorkflowFile
		if err := yaml.Unmarshal(src, &wf); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, jobID := range slices.Sorted(maps.Keys(wf.Jobs)) {
			for _, step := range wf.Jobs[jobID].Steps {
				file, _ := step.With["file"].(string)
				if !strings.Contains(step.Run, maintainedGrafanaDockerfile) && file != maintainedGrafanaDockerfile {
					continue
				}
				builds++
				if pushFlag.MatchString(step.Run) {
					t.Errorf("%s job %q step %q pushes the maintained Grafana image; it carries AGPL-3.0 "+
						"software and is built only into the local daemon", path, jobID, step.Name)
				}
				if push, _ := step.With["push"].(bool); push {
					t.Errorf("%s job %q step %q sets push: true for the maintained Grafana image", path, jobID, step.Name)
				}
				if localTag != "" && step.Run != "" && !strings.Contains(step.Run, "-t "+localTag+" ") {
					t.Errorf("%s job %q step %q builds the maintained Grafana image under a tag other than %s",
						path, jobID, step.Name, localTag)
				}
			}
		}
	}
	if builds == 0 {
		t.Errorf("no workflow builds %s; the compose e2e lanes run it and must build it locally",
			maintainedGrafanaDockerfile)
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
