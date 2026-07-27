package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/config"
)

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

	f := newMigrateVerifyCmd().Flags()
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

	f := newMigrateInventoryCmd().Flags()
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

// TestEnvBeatsCerberusYAML pins the precedence at the command level: an
// operator overriding one setting from the environment still wins over the
// file, matching every other cerberus setting.
func TestEnvBeatsCerberusYAML(t *testing.T) {
	inCerberusYAML(t, "CERBERUS_VERIFY_REF: http://fromfile:9090\n")
	t.Setenv("CERBERUS_VERIFY_REF", "http://fromenv:9090")

	if got := newMigrateVerifyCmd().Flags().Lookup("ref").DefValue; got != "http://fromenv:9090" {
		t.Errorf("--ref default = %q; want the env value, which outranks the file", got)
	}
}
