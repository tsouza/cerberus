package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This pins #3187. Cerberus is Apache-2.0 (see LICENSE, and the badge in
// README.md), and the release image says so — `Dockerfile` carries
// `org.opencontainers.image.licenses="Apache-2.0"`. Two other images in the
// tree declared `"MIT"`:
//
//   - Dockerfile.local, the image behind the compose stack, the histogram
//     bench, the Tempo compat harness and the migration lane — i.e. every
//     image a developer or a CI lane actually runs locally;
//   - compatibility/tempo/driver/Dockerfile.
//
// A wrong licence label is not cosmetic. It is a machine-readable assertion
// about redistribution terms, and it is the field a consumer's scanner reads;
// an image labelled MIT invites reuse under terms this project never granted.
//
// The label block had drifted twice by the time anyone looked, which is the
// argument for pinning it here rather than fixing the two files and moving on:
// there is no reason to expect a third copy to be written correctly either.
//
// Scope is every Dockerfile this repository AUTHORS. Vendored upstream sources
// under `compatibility/*/upstream/` are excluded — they are another project's
// files mirrored verbatim, carry that project's licence, and rewriting them
// would be a lie rather than a fix. That is the same structural boundary
// `forbid-skip`'s pathspecs already draw, not a tolerance list: nothing under
// that prefix is ours to label.
const wantImageLicense = `org.opencontainers.image.licenses="Apache-2.0"`

var (
	anyOCILabel  = regexp.MustCompile(`org\.opencontainers\.image\.[a-z]+`)
	licenseLabel = regexp.MustCompile(`org\.opencontainers\.image\.licenses\s*=\s*"([^"]*)"`)
)

// dockerfilesAuthoredHere walks the tree for Dockerfiles this repo owns.
func dockerfilesAuthoredHere(t *testing.T) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored upstream trees and build output are not ours to label.
			switch d.Name() {
			case ".git", "node_modules", "upstream", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		// `Dockerfile`, `Dockerfile.local`, … but not `Dockerfile.dockerignore`.
		if name != "Dockerfile" && !strings.HasPrefix(name, "Dockerfile.") {
			return nil
		}
		if strings.HasSuffix(name, ".dockerignore") {
			return nil
		}
		found = append(found, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for Dockerfiles: %v", repoRoot, err)
	}
	return found
}

func TestEveryAuthoredImageDeclaresTheProjectLicense(t *testing.T) {
	files := dockerfilesAuthoredHere(t)

	// Anti-vacuity: this test is a scan, and a scan that matched nothing would
	// report success having examined nothing — the exact shape #3182 catalogues.
	// The tree has at least the release image and the local image.
	if len(files) < 2 {
		t.Fatalf("found %d Dockerfile(s) under %s — the scan is broken, not the tree", len(files), repoRoot)
	}

	checked := 0
	for _, path := range files {
		body := readFileString(t, path)
		// Only images that describe themselves with OCI labels at all are in
		// scope; a Dockerfile with no label block makes no licence claim to be
		// wrong. Every one that DOES make claims must make the right one.
		if !anyOCILabel.MatchString(body) {
			continue
		}
		checked++

		m := licenseLabel.FindAllStringSubmatch(body, -1)
		if len(m) == 0 {
			t.Errorf(
				"%s declares OCI image labels but no licence label — an image that describes "+
					"itself must state the terms it ships under. Add `LABEL %s`.",
				path, wantImageLicense,
			)
			continue
		}
		for _, got := range m {
			if got[1] != "Apache-2.0" {
				t.Errorf(
					"%s labels the image licenses=%q, but this project is Apache-2.0 (see LICENSE). "+
						"The label is a machine-readable redistribution claim, so a wrong value "+
						"invites reuse under terms this project never granted.",
					path, got[1],
				)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no Dockerfile in the tree carries OCI image labels — the scan is broken, not the tree")
	}
}
