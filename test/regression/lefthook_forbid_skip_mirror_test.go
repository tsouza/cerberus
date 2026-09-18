package regression

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The forbid-skip discipline scans have ONE source of truth: the `CHECKS`
// registry in .github/scripts/forbid-skip.mjs. lefthook's pre-push hook is a
// local mirror of the CI gate, and a mirror that re-spells the regexes inline
// is not a mirror — it is a second copy that nothing pins. The lefthook copies
// drifted exactly that way: they still read the git INDEX only (the shape the
// script left behind with #1938), and mutating a registry regex to match
// nothing left every lefthook command green.
//
// The rule: every pre-push `forbid-skip-*` command runs the script with
// `env: CHECK: <arm>`, every registry arm has exactly one such command, every
// `CHECK:` names a live arm, and no pre-push command carries a grep/perl scan
// of its own.

const forbidSkipScriptPath = repoRoot + "/.github/scripts/forbid-skip.mjs"

// forbidSkipRegistryArmRE matches one `'<arm>': () => {` key of the CHECKS
// registry, the same shape doc-counts.mjs's countForbidSkipChecks reads.
var forbidSkipRegistryArmRE = regexp.MustCompile(`(?m)^  '([a-z][a-z0-9-]*)': \(\) => \{`)

// forbidSkipRegistryArms returns the CHECK arms forbid-skip.mjs registers.
func forbidSkipRegistryArms(t *testing.T) []string {
	t.Helper()
	src := readFileString(t, forbidSkipScriptPath)
	var arms []string
	for _, m := range forbidSkipRegistryArmRE.FindAllStringSubmatch(src, -1) {
		arms = append(arms, m[1])
	}
	if len(arms) == 0 {
		t.Fatalf("parsed no CHECKS registry arms out of %s", forbidSkipScriptPath)
	}
	sort.Strings(arms)
	return arms
}

type lefthookCommand struct {
	Run string            `yaml:"run"`
	Env map[string]string `yaml:"env"`
}

type lefthookStage struct {
	Commands map[string]lefthookCommand `yaml:"commands"`
}

// lefthookPrePushCommands returns lefthook.yml's pre-push commands by name.
func lefthookPrePushCommands(t *testing.T) map[string]lefthookCommand {
	t.Helper()
	var hook struct {
		PrePush lefthookStage `yaml:"pre-push"`
	}
	if err := yaml.Unmarshal([]byte(readFileString(t, lefthookPath)), &hook); err != nil {
		t.Fatalf("parse %s: %v", lefthookPath, err)
	}
	if len(hook.PrePush.Commands) == 0 {
		t.Fatalf("%s declares no pre-push commands", lefthookPath)
	}
	return hook.PrePush.Commands
}

// lefthookForbidSkipPrefix is the pre-push command-name prefix under which
// the forbid-skip arms are mirrored.
const lefthookForbidSkipPrefix = "forbid-skip-"

// forbidSkipScriptInvocation is the exact run body a mirror command carries.
const forbidSkipScriptInvocation = "node .github/scripts/forbid-skip.mjs"

// inlineScanRE spots a discipline scan re-implemented inside a hook body
// rather than delegated to its script.
var inlineScanRE = regexp.MustCompile(`\b(grep|perl|xargs)\b`)

func TestLefthookMirrorsEveryForbidSkipArmThroughTheScript(t *testing.T) {
	t.Parallel()

	arms := forbidSkipRegistryArms(t)
	commands := lefthookPrePushCommands(t)

	mirroredBy := map[string][]string{}
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		command := commands[name]
		if inlineScanRE.MatchString(command.Run) {
			t.Errorf("%s pre-push command %q runs an inline scan (%s). A discipline scan lives in "+
				".github/scripts and the hook delegates to it, so the regex has one copy; an inline "+
				"re-spelling is a second copy nothing pins:\n%s",
				lefthookPath, name, inlineScanRE.FindString(command.Run), command.Run)
		}
		if !strings.HasPrefix(name, lefthookForbidSkipPrefix) {
			continue
		}
		if strings.TrimSpace(command.Run) != forbidSkipScriptInvocation {
			t.Errorf("%s pre-push command %q must run exactly %q (got %q)",
				lefthookPath, name, forbidSkipScriptInvocation, strings.TrimSpace(command.Run))
		}
		arm := command.Env["CHECK"]
		if arm == "" {
			t.Errorf("%s pre-push command %q passes no `env: CHECK:`; forbid-skip.mjs exits 1 on an "+
				"empty CHECK, so the hook would refuse every push", lefthookPath, name)
			continue
		}
		mirroredBy[arm] = append(mirroredBy[arm], name)
	}

	for _, arm := range arms {
		switch len(mirroredBy[arm]) {
		case 0:
			t.Errorf("forbid-skip.mjs arm %q has no %s* mirror in %s pre-push; a push that CI would "+
				"reject on that arm passes the hook", arm, lefthookForbidSkipPrefix, lefthookPath)
		case 1:
		default:
			t.Errorf("forbid-skip.mjs arm %q is mirrored %d times in %s pre-push: %v",
				arm, len(mirroredBy[arm]), lefthookPath, mirroredBy[arm])
		}
	}
	for arm, names := range mirroredBy {
		if sort.SearchStrings(arms, arm) < len(arms) && arms[sort.SearchStrings(arms, arm)] == arm {
			continue
		}
		t.Errorf("%s pre-push %v passes CHECK=%q, which forbid-skip.mjs does not register (live arms: %v); "+
			"that invocation exits 1 and refuses every push", lefthookPath, names, arm, arms)
	}
}
