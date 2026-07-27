package lib

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// readConfigFile writes settings and reads the document back as YAML, so the
// assertions below judge what the CLI's own loader would see rather than the
// bytes' incidental shape.
func readConfigFile(t *testing.T, settings []Setting) (string, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	if err := WriteConfigFile(dir, settings); err != nil {
		t.Fatalf("WriteConfigFile: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatalf("read %s: %v", ConfigFileName, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the written %s is not valid YAML: %v\n%s", ConfigFileName, err, raw)
	}
	return string(raw), doc
}

// TestWriteConfigFileRoundTrips asserts a scalar setting survives as itself —
// including a value carrying the quotes and braces a LogQL selector is made of,
// which a naive `key: value` renderer would emit unquoted and corrupt.
func TestWriteConfigFileRoundTrips(t *testing.T) {
	const selector = `{app="checkout", env="prod"}`
	_, doc := readConfigFile(t, []Setting{
		{Key: "CERBERUS_VERIFY_REF", Value: "http://prometheus.internal:9090"},
		{Key: "CERBERUS_VERIFY_START", Value: "-6h"},
		{Key: "CERBERUS_INVENTORY_LOKI_SELECTORS", Value: selector},
	})
	for key, want := range map[string]string{
		"CERBERUS_VERIFY_REF":               "http://prometheus.internal:9090",
		"CERBERUS_VERIFY_START":             "-6h",
		"CERBERUS_INVENTORY_LOKI_SELECTORS": selector,
	} {
		if got := doc[key]; got != want {
			t.Errorf("%s = %#v; want %q", key, got, want)
		}
	}
}

// TestWriteConfigFileRendersASequence asserts a repeatable setting becomes a
// YAML sequence with each entry whole, which is how the CLI's Lookup.Lines
// reads it — a comma inside one entry must never split it.
func TestWriteConfigFileRendersASequence(t *testing.T) {
	want := []string{`{app="checkout", env="prod"}`, `{app="cart"}`}
	_, doc := readConfigFile(t, []Setting{{Key: "CERBERUS_INVENTORY_LOKI_SELECTORS", List: want}})

	seq, ok := doc["CERBERUS_INVENTORY_LOKI_SELECTORS"].([]any)
	if !ok {
		t.Fatalf("CERBERUS_INVENTORY_LOKI_SELECTORS = %#v; want a YAML sequence", doc["CERBERUS_INVENTORY_LOKI_SELECTORS"])
	}
	got := make([]string, 0, len(seq))
	for _, item := range seq {
		got = append(got, item.(string))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sequence = %#v; want %#v", got, want)
	}
}

// TestWriteConfigFileKeepsDeclarationOrder asserts the document reads in the
// order the caller declared, so a failing scenario's artifact is a file an
// operator can read top to bottom rather than a reshuffled map.
func TestWriteConfigFileKeepsDeclarationOrder(t *testing.T) {
	keys := []string{
		"CERBERUS_VERIFY_CORPUS", "CERBERUS_VERIFY_REF", "CERBERUS_VERIFY_CERBERUS",
		"CERBERUS_VERIFY_START", "CERBERUS_VERIFY_END", "CERBERUS_VERIFY_STEP",
	}
	settings := make([]Setting, 0, len(keys))
	for _, k := range keys {
		settings = append(settings, Setting{Key: k, Value: "x"})
	}
	raw, _ := readConfigFile(t, settings)

	at := -1
	for _, k := range keys {
		i := strings.Index(raw, k)
		if i < 0 {
			t.Fatalf("%s is missing from the written file:\n%s", k, raw)
		}
		if i < at {
			t.Fatalf("%s appears out of declaration order:\n%s", k, raw)
		}
		at = i
	}
}

// TestWriteConfigFileRejectsNothing asserts an empty setting list is an error
// rather than an empty file: a command configured from nothing would fall back
// to its built-in defaults and prove nothing about the config-file path.
func TestWriteConfigFileRejectsNothing(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfigFile(dir, nil); err == nil {
		t.Fatal("an empty setting list was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFileName)); !os.IsNotExist(err) {
		t.Errorf("a file was written anyway: %v", err)
	}
}

// helpFixture is the shape cobra prints a flag table in: name, type, usage,
// and — for the flags whose default comes from a setting — the setting key.
const helpFixture = `Usage:
  cerberus migrate verify [flags]

Flags:
      --cerberus string   cerberus base URL (setting: CERBERUS_VERIFY_CERBERUS)
      --corpus string     corpus.json produced by ` + "`cerberus migrate harvest`" + ` (setting: CERBERUS_VERIFY_CORPUS)
      --json              emit machine-readable JSON report instead of a text report
      --out string        write the report here (default: stdout)
      --ref string        reference Prometheus base URL (setting: CERBERUS_VERIFY_REF)
`

// TestSettingFlagsReadsTheHelpTable asserts the parser picks up exactly the
// flags that document a setting, and leaves the per-run output flags alone.
func TestSettingFlagsReadsTheHelpTable(t *testing.T) {
	got := settingFlags(helpFixture)
	want := map[string]string{
		"cerberus": "CERBERUS_VERIFY_CERBERUS",
		"corpus":   "CERBERUS_VERIFY_CORPUS",
		"ref":      "CERBERUS_VERIFY_REF",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("settingFlags = %#v; want %#v", got, want)
	}
}

// fakeHelpBinary writes an executable that prints help on stdout and exits
// zero, standing in for the cerberus binary so RequireSettingsFromFile's whole
// path — exec the command's own --help, parse it, judge the command line —
// runs here rather than only inside the Tier-1 stack. printf is a shell
// builtin, so the child needs nothing resolved off PATH but the shell itself.
func fakeHelpBinary(t *testing.T, help string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cerberus")
	script := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(help, "'", `'\''`) + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a test fixture that must be executable
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// TestRequireSettingsFromFileAcceptsPerRunFlags asserts the flags with no
// setting behind them — the output choices every invocation makes for itself —
// are left alone.
func TestRequireSettingsFromFileAcceptsPerRunFlags(t *testing.T) {
	bin := fakeHelpBinary(t, helpFixture)
	if err := RequireSettingsFromFile(bin, []string{"migrate", "verify", "--json", "--out", "/tmp/report.json"}); err != nil {
		t.Fatalf("a command line carrying only per-run flags was rejected: %v", err)
	}
}

// TestRequireSettingsFromFileRejectsASettingFlag is the guard itself: a value
// that belongs in cerberus.yaml must not be smuggled back onto the command
// line, because the scenario would then pass while covering the path an
// operator does not take.
func TestRequireSettingsFromFileRejectsASettingFlag(t *testing.T) {
	bin := fakeHelpBinary(t, helpFixture)
	for _, arg := range []string{"--ref", "--ref=http://prometheus:9090", "--corpus"} {
		t.Run(arg, func(t *testing.T) {
			err := RequireSettingsFromFile(bin, []string{"migrate", "verify", arg, "x", "--json"})
			if err == nil {
				t.Fatalf("%s was accepted on the command line", arg)
			}
			flag, _, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			if !strings.Contains(err.Error(), flag) || !strings.Contains(err.Error(), ConfigFileName) {
				t.Errorf("error %q names neither %s nor %s", err, flag, ConfigFileName)
			}
		})
	}
}

// TestRequireSettingsFromFileRejectsAHelpTableWithNoSettings pins the vacuity
// guard: if the flag-to-setting contract the parser reads ever changes shape,
// this check must fail loudly rather than pass by finding nothing to object to.
func TestRequireSettingsFromFileRejectsAHelpTableWithNoSettings(t *testing.T) {
	bin := fakeHelpBinary(t, "Flags:\n      --json   emit JSON\n      --out string   write here\n")
	err := RequireSettingsFromFile(bin, []string{"migrate", "verify", "--json"})
	if err == nil {
		t.Fatal("a help table declaring no setting was accepted, so the check proved nothing")
	}
	if !strings.Contains(err.Error(), "no setting") {
		t.Errorf("error %q does not name the empty contract as the reason", err)
	}
}

// TestSubcommandOfStopsAtTheFirstFlag asserts the help probe is aimed at the
// command being run, not at the whole command line.
func TestSubcommandOfStopsAtTheFirstFlag(t *testing.T) {
	for _, tc := range []struct {
		args, want []string
	}{
		{args: []string{"migrate", "verify", "--json", "--out", "/tmp/r.json"}, want: []string{"migrate", "verify"}},
		{args: []string{"migrate", "inventory"}, want: []string{"migrate", "inventory"}},
		{args: []string{"--version"}, want: []string{}},
	} {
		if got := subcommandOf(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("subcommandOf(%v) = %v; want %v", tc.args, got, tc.want)
		}
	}
}
