package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// outOfLoaderEnvFiles are the env-parsing files that live outside
// internal/config because their packages may not import it
// (.go-arch-lint.yml), each with the document its keys must be spelled
// out in: the rendered configuration document, or — for the solver's
// tuning knobs, which the configuration document delegates by reference —
// docs/solver.md. internal/config's own TestEnvDocsCoverAllKeys pins that
// every LOADER key is documented; nothing pinned the keys these files
// resolve, and one of them (the fold-cost ceiling) shipped undocumented.
// A key counts only through its exported `Env*` constant: a retired key
// kept solely to warn an operator still setting it is not configuration.
var outOfLoaderEnvFiles = []struct {
	path         string
	alsoAllowDoc string
}{
	{path: "internal/engine/resource_bound_env.go"},
	{path: "internal/promql/resource_bounds_env.go"},
	{path: "internal/actuals/config_env.go"},
	{path: "internal/solver/config_env.go", alsoAllowDoc: "docs/solver.md"},
}

var exportedEnvConst = regexp.MustCompile(`\bEnv[A-Za-z0-9]+\s*=\s*"(CERBERUS_[A-Z0-9_]+)"`)

func TestConfigDocsCoverOutOfLoaderEnvKeys(t *testing.T) {
	rendered, err := cfgdocRender()
	if err != nil {
		t.Fatalf("cfgdocRender: %v", err)
	}
	root := filepath.Join("..", "..")
	var missing []string
	seen := 0
	for _, file := range outOfLoaderEnvFiles {
		src, err := os.ReadFile(filepath.Join(root, file.path))
		if err != nil {
			t.Fatalf("read %s: %v", file.path, err)
		}
		docs := rendered
		if file.alsoAllowDoc != "" {
			extra, err := os.ReadFile(filepath.Join(root, file.alsoAllowDoc))
			if err != nil {
				t.Fatalf("read %s: %v", file.alsoAllowDoc, err)
			}
			docs += "\n" + string(extra)
		}
		keys := map[string]bool{}
		for _, m := range exportedEnvConst.FindAllStringSubmatch(string(src), -1) {
			keys[m[1]] = true
		}
		if len(keys) == 0 {
			t.Fatalf("%s declares no exported Env* key; the roster above is stale", file.path)
		}
		for key := range keys {
			seen++
			if !strings.Contains(docs, "`"+key+"`") {
				missing = append(missing, file.path+": "+key)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d of %d out-of-loader env key(s) are not documented in docs/configuration.md "+
			"(add them to the generator's prose in cmd_configdocs.go):\n  %s",
			len(missing), seen, strings.Join(missing, "\n  "))
	}
}
