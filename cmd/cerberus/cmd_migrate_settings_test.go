package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/tsouza/cerberus/internal/config"
)

// pkgSourceDir is this package's directory. `go test` starts each package in
// its own source directory and variable initialisation runs before any test, so
// this captures it before inCerberusYAML chdirs away — runtime.Caller cannot,
// because -trimpath rewrites the recorded path to a module-relative one.
var pkgSourceDir, _ = os.Getwd()

// inCerberusYAML writes a cerberus.yaml into a fresh temp directory and makes
// it the working directory, so the migrate commands' config.Lookup finds it —
// the same file, same discovery path, the gateway itself reads.
func inCerberusYAML(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cerberus.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write cerberus.yaml: %v", err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// TestVerifyFlagsFromCerberusYAML asserts `migrate verify` takes its defaults
// from cerberus.yaml, not only from the environment. Configuring a migration
// by exporting two dozen variables was the gap: one file now carries the
// engine settings and the migration settings together.
func TestVerifyFlagsFromCerberusYAML(t *testing.T) {
	inCerberusYAML(t, ""+
		"CERBERUS_VERIFY_CORPUS: corpus.json\n"+
		"CERBERUS_VERIFY_REF: http://prometheus.internal:9090\n"+
		"CERBERUS_VERIFY_CERBERUS: http://cerberus.internal:8080\n"+
		"CERBERUS_VERIFY_START: -6h\n"+
		"CERBERUS_VERIFY_STEP: 30s\n"+
		"CERBERUS_VERIFY_TOLERANCE: 0.5\n")

	f := newMigrateVerifyCmd(config.NewLookup()).Flags()
	for _, tc := range []struct{ flag, want string }{
		{"corpus", "corpus.json"},
		{"ref", "http://prometheus.internal:9090"},
		{"cerberus", "http://cerberus.internal:8080"},
		{"start", "-6h"},
		{"step", "30s"},
		{"end", "now"}, // absent from the file: the built-in default stands.
	} {
		if got := f.Lookup(tc.flag).DefValue; got != tc.want {
			t.Errorf("--%s default = %q; want %q from cerberus.yaml", tc.flag, got, tc.want)
		}
	}
}

// TestVerifyToleranceFromCerberusYAML asserts the one setting resolved at run
// time rather than flag-registration time also reads the file.
func TestVerifyToleranceFromCerberusYAML(t *testing.T) {
	inCerberusYAML(t, "CERBERUS_VERIFY_TOLERANCE: 0.5\n")

	got, err := settingFloat(config.NewLookup(), "CERBERUS_VERIFY_TOLERANCE", defaultVerifyTolerance)
	if err != nil {
		t.Fatalf("settingFloat: %v", err)
	}
	if got != 0.5 {
		t.Errorf("tolerance = %v; want 0.5 from cerberus.yaml", got)
	}
}

// TestInventoryFlagsFromCerberusYAML asserts `migrate inventory` reads the same
// file, including the repeatable --loki-selector flag whose YAML form is a
// sequence — each entry one whole selector, commas and all.
func TestInventoryFlagsFromCerberusYAML(t *testing.T) {
	inCerberusYAML(t, ""+
		"CERBERUS_INVENTORY_SOURCE: http://prometheus.internal:9090\n"+
		"CERBERUS_INVENTORY_WINDOW: 24h\n"+
		"CERBERUS_INVENTORY_LOKI_SOURCE: http://loki.internal:3100\n"+
		"CERBERUS_INVENTORY_LOKI_SELECTORS:\n"+
		"  - '{app=\"checkout\", env=\"prod\"}'\n"+
		"  - '{app=\"cart\"}'\n")

	f := newMigrateInventoryCmd(config.NewLookup()).Flags()
	if got := f.Lookup("source").DefValue; got != "http://prometheus.internal:9090" {
		t.Errorf("--source default = %q; want the cerberus.yaml value", got)
	}
	if got := f.Lookup("window").DefValue; got != "24h" {
		t.Errorf("--window default = %q; want 24h", got)
	}
	if got := f.Lookup("loki-source").DefValue; got != "http://loki.internal:3100" {
		t.Errorf("--loki-source default = %q; want the cerberus.yaml value", got)
	}

	got, err := f.GetStringArray("loki-selector")
	if err != nil {
		t.Fatalf("GetStringArray: %v", err)
	}
	want := []string{`{app="checkout", env="prod"}`, `{app="cart"}`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--loki-selector default = %#v; want %#v (sequence entries kept whole)", got, want)
	}
}

// settingKeyRE pulls the CERBERUS_* key out of a flag's help text, which is the
// only place the flag↔setting mapping is written down.
var settingKeyRE = regexp.MustCompile(`setting: (CERBERUS_[A-Z_]+)`)

// migrateSettingFlags walks both commands that take settings and returns the
// flag→key mapping their help text declares. Deriving the list at run time
// rather than hard-coding it is what makes TestEverySettingReadsCerberusYAML
// exhaustive: a flag added later with a documented setting is covered the day
// it lands, and one wired to os.Getenv instead of the Lookup fails immediately.
func migrateSettingFlags(t *testing.T) map[string]string {
	t.Helper()
	byFlag := map[string]string{}
	for _, cmd := range []*cobra.Command{
		newMigrateVerifyCmd(config.NewLookup()),
		newMigrateInventoryCmd(config.NewLookup()),
	} {
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if m := settingKeyRE.FindStringSubmatch(f.Usage); m != nil {
				byFlag[f.Name] = m[1]
			}
		})
	}
	// Guard against a vacuous pass: if the help-text contract ever changes and
	// discovery silently finds two keys instead of every one, the exhaustive
	// test would still go green. Count the keys the source declares and demand
	// the same number — a floor derived from the code, not a magic number.
	src, err := os.ReadFile(filepath.Join(pkgSourceDir, "cmd_migrate.go"))
	if err != nil {
		t.Fatalf("read cmd_migrate.go: %v", err)
	}
	declared := map[string]bool{}
	for _, m := range settingKeyRE.FindAllSubmatch(src, -1) {
		declared[string(m[1])] = true
	}
	if len(byFlag) != len(declared) {
		t.Fatalf("discovered %d settings from flag help, but cmd_migrate.go declares %d; "+
			"a setting is documented without a flag, or discovery is broken", len(byFlag), len(declared))
	}
	return byFlag
}

// TestEverySettingReadsCerberusYAML is the exhaustive form of the sampled tests
// above: EVERY setting the migrate commands document must resolve from
// cerberus.yaml, not just the handful spot-checked. It writes one file carrying
// a distinct sentinel per key and asserts each flag picks its own up.
func TestEverySettingReadsCerberusYAML(t *testing.T) {
	// Discovery runs somewhere without a cerberus.yaml, so the help text is read
	// from pristine defaults.
	inCerberusYAML(t, "")
	byFlag := migrateSettingFlags(t)

	// --tolerance is a Float64Var resolved in RunE rather than at flag
	// registration, and --loki-selector is a repeatable StringArrayVar whose
	// DefValue renders as a bracketed list; both are asserted separately, so
	// they carry their own sentinel shapes here.
	const tolerance, selectors = "CERBERUS_VERIFY_TOLERANCE", "CERBERUS_INVENTORY_LOKI_SELECTORS"

	var yaml strings.Builder
	want := map[string]string{}
	for flag, key := range byFlag {
		switch key {
		case tolerance:
			yaml.WriteString(key + ": 0.5\n")
		case selectors:
			yaml.WriteString(key + ":\n  - '{sentinel=\"" + flag + "\"}'\n")
		default:
			sentinel := "sentinel-" + flag
			yaml.WriteString(key + ": " + sentinel + "\n")
			want[flag] = sentinel
		}
	}
	inCerberusYAML(t, yaml.String())

	verify, inventory := newMigrateVerifyCmd(config.NewLookup()), newMigrateInventoryCmd(config.NewLookup())
	for flag, wantValue := range want {
		f := verify.Flags().Lookup(flag)
		if f == nil {
			f = inventory.Flags().Lookup(flag)
		}
		if f == nil {
			t.Errorf("--%s vanished between discovery and assertion", flag)
			continue
		}
		if f.DefValue != wantValue {
			t.Errorf("--%s default = %q; want %q — %s is not read from cerberus.yaml",
				flag, f.DefValue, wantValue, byFlag[flag])
		}
	}

	got, err := inventory.Flags().GetStringArray("loki-selector")
	if err != nil {
		t.Fatalf("GetStringArray: %v", err)
	}
	if wantSel := []string{`{sentinel="loki-selector"}`}; !reflect.DeepEqual(got, wantSel) {
		t.Errorf("--loki-selector default = %#v; want %#v", got, wantSel)
	}
	if tol, err := settingFloat(config.NewLookup(), tolerance, defaultVerifyTolerance); err != nil || tol != 0.5 {
		t.Errorf("tolerance = %v, %v; want 0.5, nil", tol, err)
	}
}

// TestNoDirectGetenvForMigrateSettings pins the fix at the source. Every
// CERBERUS_VERIFY_* / CERBERUS_INVENTORY_* setting once resolved through a bare
// os.Getenv, which bypassed the config file entirely; a new flag reaching for
// os.Getenv would silently reintroduce exactly that gap, and no behavioural
// test catches a setting nobody thought to add a case for.
func TestNoDirectGetenvForMigrateSettings(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(pkgSourceDir, "cmd_migrate.go"))
	if err != nil {
		t.Fatalf("read cmd_migrate.go: %v", err)
	}
	if bytes.Contains(src, []byte("os.Getenv")) {
		t.Error("cmd_migrate.go calls os.Getenv directly: settings must resolve " +
			"through config.Lookup so cerberus.yaml is honoured alongside the environment")
	}
}

// TestEnvBeatsCerberusYAML pins the precedence at the command level: an
// operator overriding one setting from the environment still wins over the
// file, matching every other cerberus setting.
func TestEnvBeatsCerberusYAML(t *testing.T) {
	inCerberusYAML(t, "CERBERUS_VERIFY_REF: http://fromfile:9090\n")
	t.Setenv("CERBERUS_VERIFY_REF", "http://fromenv:9090")

	if got := newMigrateVerifyCmd(config.NewLookup()).Flags().Lookup("ref").DefValue; got != "http://fromenv:9090" {
		t.Errorf("--ref default = %q; want the env value, which outranks the file", got)
	}
}
