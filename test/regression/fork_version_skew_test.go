package regression

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// tempoForkReplace matches the go.mod directive that routes grafana/tempo
// through the accessor fork. Whitespace is flexible; the module path and
// the tag family are not.
var tempoForkReplace = regexp.MustCompile(
	`(?m)^replace\s+github\.com/grafana/tempo\s+=>\s+github\.com/tsouza/tempo\s+v\d+\.\d+\.\d+-cerberus-accessors\s*$`,
)

// prometheusForkReplace matches the go.mod directive that routes
// prometheus/prometheus through the parser fork and captures the fork
// tag's version core, which names the release the tag is cut on.
var prometheusForkReplace = regexp.MustCompile(
	`(?m)^replace\s+github\.com/prometheus/prometheus\s+=>\s+github\.com/tsouza/prometheus\s+(v\d+\.\d+\.\d+)-cerberus-parser\s*$`,
)

// TestForkVersionSkew (#832) pins the correspondence between the upstream version
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
// Every restatement of the reference Prometheus image outside the
// compatibility lane — the migration tiers' stacks and the histogram bench —
// must name the compatibility image byte-for-byte, and test/oracle's own
// go.mod must replace the parser with the same fork tag the root module
// does, so the one pin covers every place a PromQL reference runs.
//
// Granularity: loki is checked at MAJOR.MINOR — the grain at which the
// query-language grammar and evaluation semantics that parity rests on
// change; the reference image may lag the Go library by a bug-fix patch
// (go.mod v3.7.1 against grafana/loki:3.7.0 — same 3.7 grammar).
// Prometheus is checked at the exact release: its `replace` is a fork tag
// cut on one release commit, so the require, the fork tag and the reference
// image must all name that release. Tempo is pinned by commit on every
// side, so it is checked at the exact commit prefix.
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
		// v3.MINOR.PATCH (e.g. v0.315.0 <-> v3.15.0). Lower the library
		// version into release coordinates and require the reference
		// container to run exactly that release.
		lib := requireVersion(t, goMod, "github.com/prometheus/prometheus")
		relMajor, relMinor, relPatch := prometheusLibToRelease(t, lib)

		img := composeImageTag(
			t,
			"../../compatibility/prometheus/docker-compose.yml",
			regexp.MustCompile(`image:\s*prom/prometheus:v(\d+)\.(\d+)\.(\d+)`),
		)
		imgMajor, imgMinor, imgPatch := img[0], img[1], img[2]

		if relMajor != imgMajor || relMinor != imgMinor || relPatch != imgPatch {
			t.Fatalf("prometheus version skew: go.mod require resolves to release v%s.%s.%s "+
				"(library %s), but compatibility/prometheus reference image is "+
				"prom/prometheus:v%s.%s.%s. The compat lane would diff cerberus's "+
				"parser against a different reference engine — parity claims "+
				"are unsound. Align the two (see docs/upstream-forks.md § version-skew gate).",
				relMajor, relMinor, relPatch, lib, imgMajor, imgMinor, imgPatch)
		}

		// The `require` line is superseded by the `replace` onto the
		// tsouza/prometheus fork, so on its own it does not say what
		// compiles. The fork tag is cut on the release commit and named
		// after the library version of that release
		// (v0.315.0-cerberus-parser sits on v3.15.0), so the replace
		// must name exactly the version the require records. A tag cut
		// anywhere else — upstream main, say, whose VERSION file still
		// reads the last release — carries a different name and fails
		// here: the parser and the spec lane's in-process engine would
		// otherwise run a grammar the reference server does not.
		m := prometheusForkReplace.FindSubmatch(goMod)
		if m == nil {
			t.Fatalf("prometheus fork replace missing or repointed: go.mod must carry a "+
				"`replace github.com/prometheus/prometheus => github.com/tsouza/prometheus "+
				"v0.<300+MINOR>.<PATCH>-cerberus-parser` directive matching %s, a fork tag "+
				"cut on the release the reference image runs "+
				"(see docs/upstream-forks.md § version-skew gate).",
				prometheusForkReplace)
		}
		if forkBase := string(m[1]); forkBase != lib {
			t.Fatalf("prometheus fork skew: go.mod replaces github.com/prometheus/prometheus "+
				"with the fork tag %s-cerberus-parser, but the require (and the reference image "+
				"prom/prometheus:v%s.%s.%s) is %s. The fork tag must be cut on that release "+
				"(see docs/upstream-forks.md § version-skew gate).",
				forkBase, imgMajor, imgMinor, imgPatch, lib)
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

		// Tempo's `require` line is superseded by a `replace` onto the
		// tsouza/tempo fork, so on its own it no longer describes what
		// compiles — it describes the upstream base the fork is rebased
		// onto, which is exactly what the skew comparison above needs it to
		// mean. Pin the replace target so that stays true: repointing tempo
		// at some other module or tag family would leave the comparison
		// above measuring a version nothing builds against, and it would do
		// so silently.
		if !tempoForkReplace.Match(goMod) {
			t.Fatalf("tempo fork replace missing or repointed: go.mod must carry a "+
				"`replace github.com/grafana/tempo => github.com/tsouza/tempo "+
				"vX.Y.Z-cerberus-accessors` directive matching %s. Without it the "+
				"require line above is not the version that compiles, and the "+
				"skew assertions in this subtest certify nothing "+
				"(see docs/upstream-forks.md § version-skew gate).",
				tempoForkReplace)
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

	t.Run("prometheus-restatements", func(t *testing.T) {
		t.Parallel()
		compat := composeImageTag(t, "../../compatibility/prometheus/docker-compose.yml", prometheusImageLine)[0]
		for _, path := range prometheusImageRestatements {
			buf, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			lines := prometheusImageLine.FindAllStringSubmatch(string(buf), -1)
			if len(lines) == 0 {
				t.Fatalf("%s runs no prom/prometheus image; drop it from prometheusImageRestatements "+
					"or fix the gate's regex.", path)
			}
			for _, m := range lines {
				if m[1] != compat {
					t.Fatalf("%s runs %s, but the compatibility lane's reference is %s. The "+
						"compatibility compose file is the sole pin site for the reference "+
						"Prometheus; restate its tag byte-for-byte.", path, m[1], compat)
				}
			}
		}
	})

	t.Run("oracle-module", func(t *testing.T) {
		t.Parallel()
		// test/oracle is a module of its own, so it carries its own require
		// and replace for the parser; both must be the root module's.
		oracleMod, err := os.ReadFile("../../test/oracle/go.mod")
		if err != nil {
			t.Fatalf("read test/oracle/go.mod: %v", err)
		}
		const mod = "github.com/prometheus/prometheus"
		if root, oracle := requireVersion(t, goMod, mod), requireVersion(t, oracleMod, mod); root != oracle {
			t.Fatalf("test/oracle/go.mod requires %s %s, the root go.mod %s. Align the two.", mod, oracle, root)
		}
		root, oracle := prometheusForkReplace.FindSubmatch(goMod), prometheusForkReplace.FindSubmatch(oracleMod)
		if root == nil || oracle == nil {
			t.Fatalf("a prometheus fork replace matching %s is missing (root: %v, test/oracle: %v)",
				prometheusForkReplace, root != nil, oracle != nil)
		}
		if string(root[1]) != string(oracle[1]) {
			t.Fatalf("test/oracle/go.mod replaces %s with the fork tag %s-cerberus-parser, the root "+
				"go.mod with %s-cerberus-parser. Align the two.", mod, oracle[1], root[1])
		}
	})

	t.Run("migration-tier1", func(t *testing.T) {
		t.Parallel()
		// The migration lane's Tier-1 stack runs the same three reference
		// backends the compat lanes do, and it must run them at the SAME
		// versions: the compat compose files are the sole pin site, so a
		// tier-1 restatement that drifts would silently diff cerberus against
		// a reference engine the compat gate never exercised. Binding the
		// tags here makes the go.mod <-> compat assertions above cover tier-1
		// transitively, with one enforcement mechanism instead of two.
		const tier1Compose = "../../test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml"
		for _, tc := range []struct {
			name        string
			compat      string
			re          *regexp.Regexp
			description string
		}{
			{
				name:        "prometheus",
				compat:      "../../compatibility/prometheus/docker-compose.yml",
				re:          regexp.MustCompile(`image:\s*(prom/prometheus:\S+)`),
				description: "reference Prometheus",
			},
			{
				name:        "loki",
				compat:      "../../compatibility/loki/docker-compose.yml",
				re:          regexp.MustCompile(`image:\s*(grafana/loki:\S+)`),
				description: "reference Loki",
			},
			{
				name:        "tempo",
				compat:      "../../compatibility/tempo/docker-compose.yml",
				re:          regexp.MustCompile(`image:\s*(grafana/tempo:\S+)`),
				description: "reference Tempo",
			},
		} {
			compatTag := composeImageTag(t, tc.compat, tc.re)[0]
			tier1Tag := composeImageTag(t, tier1Compose, tc.re)[0]
			if compatTag != tier1Tag {
				t.Fatalf("migration tier-1 %s image %q != compatibility image %q. The "+
					"compatibility compose file is the sole pin site for reference "+
					"backend versions; tier-1 restates the tag and must restate it "+
					"byte-for-byte, so the go.mod <-> compat version gate above covers "+
					"tier-1 too. Align %s.",
					tc.description, tier1Tag, compatTag, tier1Compose)
			}
		}
	})
}

// prometheusImageRestatements are the compose files outside the
// compatibility lane, besides the migration tier-1 stack its own subtest
// covers, that run the reference Prometheus image.
var prometheusImageRestatements = []string{
	"../../test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml",
	"../../bench/histogram/docker-compose.yml",
}

// prometheusImageLine matches every reference Prometheus image line of a
// compose file.
var prometheusImageLine = regexp.MustCompile(`(?m)^\s*image:\s*(prom/prometheus:\S+)`)

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
// returned as strings. Since the v3.0.0 release the library has reported
// v0.300.0+; this mapping is stable and documented upstream.
func prometheusLibToRelease(t *testing.T, lib string) (major, minor, patch string) {
	t.Helper()
	re := regexp.MustCompile(`^v0\.(\d+)\.(\d+)$`)
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
	if n < prometheusV3LibraryMinorBase {
		t.Fatalf("prometheus require %q has library-minor %d < %d; the v0.300+ "+
			"<-> v3.x mapping does not apply. Update the gate.", lib, n, prometheusV3LibraryMinorBase)
	}
	return "3", fmt.Sprintf("%d", n-prometheusV3LibraryMinorBase), m[2]
}

// prometheusV3LibraryMinorBase is the offset the Prometheus Go library adds
// to a v3 release's MINOR: release v3.MINOR.PATCH is library v0.(300+MINOR).PATCH.
const prometheusV3LibraryMinorBase = 300

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
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		prefix := field + ":"
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("VERSION file has no %q field", field)
	return ""
}

// commitPrefixMatch reports whether two commit hashes agree on the shorter
// one's length (so a 12-char go.mod hash matches a 7-char image tag).
func commitPrefixMatch(a, b string) bool {
	n := min(len(b), len(a))
	if n == 0 {
		return false
	}
	return a[:n] == b[:n]
}
