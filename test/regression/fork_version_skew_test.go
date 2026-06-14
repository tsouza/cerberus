package regression

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestForkVersionSkew pins the correspondence between the upstream version
// each tsouza/* parser fork is based on (recorded as the `require` line in
// go.mod that the fork's `replace` substitutes for) and the reference
// backend container the matching compatibility harness diffs cerberus
// against.
//
// Why this matters (the fork-vs-reference parity story; see
// docs/upstream-forks.md §"Version-skew gate"):
//
// The compat lanes — compatibility/{prometheus,loki,tempo} — are the source
// of truth for all three heads. They run cerberus's emitted SQL against a
// *reference backend container* and fail on any diff. The sharded-pushdown
// solver's "route A as parity oracle" premise leans on those lanes: route A
// is trusted because it matches the reference engine. But route A parses
// each query with a tsouza/* fork of that same engine's parser. If a fork
// drifts to a different upstream version than the reference container the
// compat lane runs, the lane is comparing cerberus-with-parser-vX against
// reference-engine-vY — and any parity claim it certifies is unsound for the
// grammar / semantics that changed between vX and vY.
//
// This test makes that skew a `check`-lane failure instead of a silent
// divergence that only surfaces as a confusing compat diff (or, worse, a
// false-green compat run).
//
// Granularity: the two semver heads (prometheus, loki) are checked at
// MAJOR.MINOR — that's the grain at which the query-language grammar and
// evaluation semantics that parity rests on actually change; upstream cuts
// patch releases for bug fixes that don't move the language, and the
// reference image routinely lags the Go library by a patch (loki: go.mod
// pins v3.7.1, the reference image is grafana/loki:3.7.0 — same 3.7 grammar).
// Tempo is pinned by commit on both sides, so it is checked at the exact
// commit prefix, which is both available and meaningful.
//
// If this test fails, EITHER bump the compat reference image to match the
// fork's upstream base, OR bump the go.mod fork require to match the image —
// whichever direction is correct for the change you're making. Do not relax
// the assertion to make the skew disappear.
func TestForkVersionSkew(t *testing.T) {
	t.Parallel()

	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	t.Run("prometheus", func(t *testing.T) {
		t.Parallel()
		// go.mod pins the Prometheus library version. Since the v3.0.0
		// release, the Go module reports v0.(300+MINOR).PATCH for release
		// v3.MINOR.PATCH (e.g. v0.311.3 <-> v3.11.3). Lower the library
		// version into release coordinates, then compare MAJOR.MINOR
		// against the reference container.
		lib := requireVersion(t, goMod, "github.com/prometheus/prometheus")
		relMajor, relMinor := prometheusLibToRelease(t, lib)

		img := composeImageTag(
			t,
			"../../compatibility/prometheus/docker-compose.yml",
			regexp.MustCompile(`image:\s*prom/prometheus:v(\d+)\.(\d+)\.\d+`),
		)
		imgMajor, imgMinor := img[0], img[1]

		if relMajor != imgMajor || relMinor != imgMinor {
			t.Fatalf("prometheus version skew: go.mod fork base resolves to release v%s.%s.x "+
				"(library %s), but compatibility/prometheus reference image is "+
				"prom/prometheus:v%s.%s.x. The compat lane would diff cerberus's "+
				"v%s.%s parser against a v%s.%s reference engine — parity claims "+
				"are unsound. Align the two (see docs/upstream-forks.md § version-skew gate).",
				relMajor, relMinor, lib, imgMajor, imgMinor,
				relMajor, relMinor, imgMajor, imgMinor)
		}
	})

	t.Run("loki", func(t *testing.T) {
		t.Parallel()
		// go.mod pins github.com/grafana/loki/v3 vMAJOR.MINOR.PATCH and the
		// reference image is grafana/loki:MAJOR.MINOR.PATCH (no leading v).
		lib := requireVersion(t, goMod, "github.com/grafana/loki/v3")
		libMajor, libMinor := semverMajorMinor(t, lib)

		img := composeImageTag(
			t,
			"../../compatibility/loki/docker-compose.yml",
			regexp.MustCompile(`image:\s*grafana/loki:(\d+)\.(\d+)\.\d+`),
		)
		imgMajor, imgMinor := img[0], img[1]

		if libMajor != imgMajor || libMinor != imgMinor {
			t.Fatalf("loki version skew: go.mod fork base is %s (v%s.%s.x), but "+
				"compatibility/loki reference image is grafana/loki:%s.%s.x. The "+
				"compat lane would diff cerberus's v%s.%s parser against a v%s.%s "+
				"reference engine — parity claims are unsound. Align the two "+
				"(see docs/upstream-forks.md § version-skew gate).",
				lib, libMajor, libMinor, imgMajor, imgMinor,
				libMajor, libMinor, imgMajor, imgMinor)
		}
	})

	t.Run("tempo", func(t *testing.T) {
		t.Parallel()
		// Tempo is pinned by commit on every side:
		//   - go.mod require is a pseudo-version vX-0.<ts>-<commit12>.
		//   - the compat reference image is grafana/tempo:main-<commit7>.
		//   - compatibility/tempo/upstream/VERSION records upstream_commit.
		// Compare on the shortest available commit prefix.
		lib := requireVersion(t, goMod, "github.com/grafana/tempo")
		libCommit := pseudoVersionCommit(t, lib)

		img := composeImageTag(
			t,
			"../../compatibility/tempo/docker-compose.yml",
			regexp.MustCompile(`image:\s*grafana/tempo:main-([0-9a-f]+)`),
		)
		imgCommit := img[0]

		if !commitPrefixMatch(libCommit, imgCommit) {
			t.Fatalf("tempo version skew: go.mod fork base is at commit %s (require %s), "+
				"but compatibility/tempo reference image is grafana/tempo:main-%s. The "+
				"compat lane would diff cerberus's parser at %s against a reference "+
				"engine at %s — parity claims are unsound. Align the two "+
				"(see docs/upstream-forks.md § version-skew gate).",
				libCommit, lib, imgCommit, libCommit, imgCommit)
		}

		// Cross-check the committed VERSION file too: it records the same
		// upstream commit and must not drift from go.mod either.
		verBuf, err := os.ReadFile("../../compatibility/tempo/upstream/VERSION")
		if err != nil {
			t.Fatalf("read compatibility/tempo/upstream/VERSION: %v", err)
		}
		verCommit := versionFileField(t, string(verBuf), "upstream_commit")
		if !commitPrefixMatch(libCommit, verCommit) {
			t.Fatalf("tempo version skew: go.mod fork base is at commit %s, but "+
				"compatibility/tempo/upstream/VERSION records upstream_commit %s. "+
				"Align the two (see docs/upstream-forks.md § version-skew gate).",
				libCommit, verCommit)
		}
	})
}

// requireVersion extracts the version pinned for modPath in a go.mod
// `require` block. It matches the module path as a whole word so
// github.com/grafana/loki/v3 does not also match a hypothetical
// github.com/grafana/loki/v3/foo.
func requireVersion(t *testing.T, goMod []byte, modPath string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(modPath) + `\s+(\S+)\s*$`)
	m := re.FindSubmatch(goMod)
	if m == nil {
		t.Fatalf("go.mod: no require line for %s", modPath)
	}
	return string(m[1])
}

// prometheusLibToRelease lowers a Prometheus Go-library version
// (v0.(300+MINOR).PATCH) into release coordinates (MAJOR=3, MINOR, PATCH),
// returning MAJOR and MINOR as strings. Since the v3.0.0 release the library
// has reported v0.300.0+; this mapping is stable and documented upstream.
func prometheusLibToRelease(t *testing.T, lib string) (major, minor string) {
	t.Helper()
	re := regexp.MustCompile(`^v0\.(\d+)\.\d+$`)
	m := re.FindStringSubmatch(lib)
	if m == nil {
		t.Fatalf("prometheus require %q is not a v0.<N>.<P> library version; "+
			"the v0.(300+MINOR).PATCH <-> v3.MINOR.PATCH mapping no longer holds — "+
			"update prometheusLibToRelease and the gate.", lib)
	}
	var n int
	if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
		t.Fatalf("parse prometheus library minor from %q: %v", lib, err)
	}
	if n < 300 {
		t.Fatalf("prometheus require %q has library-minor %d < 300; the v0.300+ "+
			"<-> v3.x mapping does not apply. Update the gate.", lib, n)
	}
	return "3", fmt.Sprintf("%d", n-300)
}

// semverMajorMinor splits vMAJOR.MINOR.PATCH (or MAJOR.MINOR.PATCH) into its
// MAJOR and MINOR components.
func semverMajorMinor(t *testing.T, ver string) (major, minor string) {
	t.Helper()
	re := regexp.MustCompile(`^v?(\d+)\.(\d+)\.`)
	m := re.FindStringSubmatch(ver)
	if m == nil {
		t.Fatalf("version %q is not vMAJOR.MINOR.PATCH", ver)
	}
	return m[1], m[2]
}

// pseudoVersionCommit extracts the 12-char commit suffix from a Go
// pseudo-version like v1.5.1-0.20260508211128-2f74ea818de1.
func pseudoVersionCommit(t *testing.T, ver string) string {
	t.Helper()
	re := regexp.MustCompile(`-([0-9a-f]{12})$`)
	m := re.FindStringSubmatch(ver)
	if m == nil {
		t.Fatalf("tempo require %q is not a pseudo-version ending in a 12-char "+
			"commit hash; if the fork moved to a tagged release, update the gate "+
			"to compare the tag instead.", ver)
	}
	return m[1]
}

// composeImageTag reads a docker-compose file and returns the capture groups
// of the first line matching re. Fails if there is no match.
func composeImageTag(t *testing.T, path string, re *regexp.Regexp) []string {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := re.FindStringSubmatch(string(buf))
	if m == nil {
		t.Fatalf("%s: no reference image line matching %s — the image pin moved or "+
			"was renamed; update the gate's regex.", path, re)
	}
	return m[1:]
}

// versionFileField pulls `field: value` from a VERSION-style file, returning
// the trimmed value.
func versionFileField(t *testing.T, body, field string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		prefix := field + ":"
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("VERSION file has no %q field", field)
	return ""
}

// commitPrefixMatch reports whether two commit hashes agree on the shorter
// one's length (so a 12-char go.mod hash matches a 7-char image tag).
func commitPrefixMatch(a, b string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return false
	}
	return a[:n] == b[:n]
}
