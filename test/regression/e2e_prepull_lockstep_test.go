package regression

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The k3d e2e lanes pull every external image on the HOST daemon and import the
// result into the cluster, so the cluster's containerd never reaches a registry.
// That is the only reason those lanes survive Docker Hub's anonymous per-IP
// quota: a host pull authenticates and reaches the GHCR mirror first
// (`lib/registry.mjs`), while an in-cluster containerd pull does neither.
//
// The mechanism covers exactly the images someone remembered to list. An image
// the manifests ask for and the pre-pull omits is not pre-pulled, not imported,
// and pulled by containerd at pod start — anonymously, un-mirrored, and at the
// worst possible moment, since a rate-limited pod sits in `ImagePullBackOff`
// until the deployment wait expires and the whole lane goes red.
//
// It had already failed silently. #1263 moved the k3d ClickHouse to
// `26.6-alpine` because upstream 26.5 carried a topK-prefilter regression, and
// left `E2E_EXTERNAL_IMAGES` on `26.5-alpine`. For ten days the pre-pull warmed
// a tag no manifest asked for while the one image it was written for — the
// biggest, slowest pull in the lane — went to Docker Hub on every run. Nothing
// went red, because nothing was looking (issue #1461).
//
// The comment that was supposed to prevent this said the two sides "MUST stay in
// lock-step". A comment is not a gate, and this is the gate. It runs in both
// directions on purpose: an uncovered manifest image is the exposure, and a
// stale pre-pull entry is how the list stops describing the lane — the exact
// state that made the real gap hard to see.
//
// Floor for the manifest scan. There are 7 distinct refs across the two manifest
// trees today; the floor sits below that so a move that leaves this guard
// scanning an empty set fails instead of passing vacuously.
const minManifestImageRefs = 5

// manifestImage matches a Kubernetes/Helm-values `image:` whose value is a ref
// on the same line. The scalar form is the requirement: `cerberus-values.yaml`
// spells the chart's image as an `image:` BLOCK with `repository:` / `tag:`
// children, and that one is built locally and loaded with `pullPolicy: Never`,
// so it is not an external pull and must not be read as one.
var manifestImage = regexp.MustCompile(`^\s*-?\s*image:\s*["']?([^"'\s#]+)["']?\s*$`)

// manifestTrees are the directories holding the manifests the k3d lanes apply,
// and prePullPins the Justfile lists whose union must account for them.
//
// The check is over the UNION rather than per-lane, and that is a deliberate
// scope, not an oversight. A directory is not a lane: `cerberus-values-bwc.yaml`
// lives under test/e2e/k3s but only the bwc lane passes it to Helm, and the bwc
// lane in turn excludes test/e2e/k3s/clickhouse.yaml through its kustomization.
// Recovering the true per-lane file set means interpreting `kubectl apply -f`,
// `kubectl kustomize`, and `helm --values` out of two recipes plus a
// kustomization that reaches across trees — machinery that can under-scan
// silently, which is the exact defect being fixed.
//
// The union is the right scope anyway, because it is the SILENT half. An image
// in neither list is pulled anonymously from Docker Hub by the cluster's own
// containerd and usually succeeds, so it shows up as nothing at all until a
// rate-limit window closes on it — the ten-day gap above. An image listed but
// pre-pulled by the other lane fails loudly at pod start instead
// (`ImagePullBackOff` against `pullPolicy: Never`, then a rollout timeout), so
// it cannot hide.
var (
	manifestTrees = []string{"../../test/e2e/k3s", "../../test/e2e/k3s-bwc"}
	prePullPins   = []string{"E2E_EXTERNAL_IMAGES", "E2E_BWC_IMAGES"}
)

// prePulledImages is the union of the pre-pull lists.
func prePulledImages(t *testing.T) map[string]bool {
	t.Helper()

	pins := justfilePins(t)
	union := map[string]bool{}
	for _, pin := range prePullPins {
		set, ok := pins[pin]
		if !ok {
			t.Fatalf("%s no longer defines %s, so this guard cannot tell what the k3d lanes pre-pull",
				justfilePath, pin)
		}
		for ref := range set {
			union[ref] = true
		}
	}
	return union
}

// manifestImages is every externally-pulled image ref across all manifest trees,
// mapped to the sites naming it.
func manifestImages(t *testing.T) map[string][]string {
	t.Helper()

	all := map[string][]string{}
	for _, dir := range manifestTrees {
		for ref, sites := range manifestImagesUnder(t, dir) {
			all[ref] = append(all[ref], sites...)
		}
	}
	return all
}

// justfilePins reads every `NAME := "a b c"` pin out of the Justfile as a set of
// space-separated refs, which is the shape the pre-pull lists use.
func justfilePins(t *testing.T) map[string]map[string]bool {
	t.Helper()

	src, err := os.ReadFile(justfilePath)
	if err != nil {
		t.Fatalf("read %s: %v", justfilePath, err)
	}

	pins := map[string]map[string]bool{}
	for _, m := range justAssignment.FindAllStringSubmatch(string(src), -1) {
		set := map[string]bool{}
		for _, ref := range strings.Fields(m[2]) {
			set[ref] = true
		}
		pins[m[1]] = set
	}
	return pins
}

// manifestImagesUnder collects every externally-pulled image ref in a manifest
// tree, mapped to the files that name it.
func manifestImagesUnder(t *testing.T, dir string) map[string][]string {
	t.Helper()

	refs := map[string][]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(d.Name()); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(src), "\n") {
			if isProse(line) {
				continue
			}
			m := manifestImage.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// A templated ref is resolved by whatever renders it, not by the
			// pre-pull, and `dockerHubRef` rejects it for the same reason the
			// mirror inventory does.
			if strings.Contains(m[1], "{{") || strings.Contains(m[1], "${") {
				continue
			}
			refs[m[1]] = append(refs[m[1]], fmt.Sprintf("%s:%d", path, i+1))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return refs
}

// TestE2EPrePullCoversEveryManifestImage is the forward direction: every image a
// k3d manifest asks for must be pre-pulled and imported, or the cluster pulls it
// itself.
func TestE2EPrePullCoversEveryManifestImage(t *testing.T) {
	t.Parallel()

	prePulled := prePulledImages(t)
	found := manifestImages(t)

	for _, ref := range sortedKeys(found) {
		if prePulled[ref] {
			continue
		}
		t.Errorf("the k3d manifests ask for %s (%s) but no pre-pull list names it (%s), so the host never "+
			"pulls it and k3d never imports it — the cluster's own containerd fetches it at pod start, "+
			"anonymously and bypassing the GHCR mirror, which is the entire exposure the pre-pull exists to "+
			"remove. Add it to whichever list the lane using it imports.",
			ref, strings.Join(dedupe(found[ref]), ", "), strings.Join(prePullPins, " / "))
	}

	if len(found) < minManifestImageRefs {
		t.Errorf("found only %d distinct image refs across %s (expected at least %d) — the scanner is "+
			"matching a shape the manifests no longer use, so this guard is asserting nothing",
			len(found), describeTrees(), minManifestImageRefs)
	}
}

// TestE2EPrePullListsNameOnlyManifestImages is the reverse direction. A pin no
// manifest asks for costs a pull and an import on every e2e run, and — the
// reason it is an error rather than a wart — it makes the list stop describing
// the lane, which is precisely why the real gap went unnoticed for ten days: the
// stale `26.5-alpine` entry made the list LOOK like it covered ClickHouse.
func TestE2EPrePullListsNameOnlyManifestImages(t *testing.T) {
	t.Parallel()

	pins := justfilePins(t)
	named := manifestImages(t)

	for _, pin := range prePullPins {
		set, ok := pins[pin]
		if !ok {
			t.Fatalf("%s no longer defines %s", justfilePath, pin)
		}
		for _, ref := range sortedSet(set) {
			if _, ok := named[ref]; ok {
				continue
			}
			t.Errorf("%s pre-pulls and imports %s, but no manifest under %s asks for it any more. Every e2e "+
				"run pays for that pull, and a list naming images the lanes do not use is how a MISSING one "+
				"hides. Drop the entry, or bump it to the tag the manifests moved to.",
				pin, ref, describeTrees())
		}
	}
}

func describeTrees() string {
	dirs := make([]string, 0, len(manifestTrees))
	for _, dir := range manifestTrees {
		dirs = append(dirs, strings.TrimPrefix(dir, "../../"))
	}
	sort.Strings(dirs)
	return strings.Join(dirs, " / ")
}
