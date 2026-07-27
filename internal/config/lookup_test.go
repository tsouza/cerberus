package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeLookupYAML drops a cerberus.yaml into a fresh temp directory and makes
// it the working directory, so NewLookup's AddConfigPath(".") finds it.
func writeLookupYAML(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cerberus.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write cerberus.yaml: %v", err)
	}
	chdir(t, dir)
}

// TestLookupString_EnvBeatsFile pins the precedence the migrate CLI inherits
// from the server loader: cerberus.yaml supplies the value when the
// environment is silent, and the environment always wins when both speak.
func TestLookupString_EnvBeatsFile(t *testing.T) {
	writeLookupYAML(t, "CERBERUS_VERIFY_REF: http://fromfile:9090\n")

	l := NewLookup()
	if got := l.String("CERBERUS_VERIFY_REF"); got != "http://fromfile:9090" {
		t.Errorf("String = %q; want the cerberus.yaml value when env is unset", got)
	}

	t.Setenv("CERBERUS_VERIFY_REF", "http://fromenv:9090")
	if got := l.String("CERBERUS_VERIFY_REF"); got != "http://fromenv:9090" {
		t.Errorf("String = %q; want the env value, which outranks the file", got)
	}
}

// TestLookupString_EmptyEnvFallsThrough asserts an env var set to the empty
// string does not shadow the file: the CLI treats unset and empty alike, so
// an exported-but-blank variable must not blank out a configured setting.
func TestLookupString_EmptyEnvFallsThrough(t *testing.T) {
	writeLookupYAML(t, "CERBERUS_VERIFY_REF: http://fromfile:9090\n")
	t.Setenv("CERBERUS_VERIFY_REF", "")

	if got := NewLookup().String("CERBERUS_VERIFY_REF"); got != "http://fromfile:9090" {
		t.Errorf("String = %q; want the file value (empty env is not a value)", got)
	}
}

// TestLookupString_NoFile asserts a missing cerberus.yaml is not an error and
// leaves the environment as the sole source — the CLI must run from any
// directory, with or without a config file.
func TestLookupString_NoFile(t *testing.T) {
	chdir(t, t.TempDir())

	l := NewLookup()
	if got := l.String("CERBERUS_VERIFY_REF"); got != "" {
		t.Errorf("String = %q; want empty with no file and no env", got)
	}

	t.Setenv("CERBERUS_VERIFY_REF", "http://fromenv:9090")
	if got := l.String("CERBERUS_VERIFY_REF"); got != "http://fromenv:9090" {
		t.Errorf("String = %q; want the env value with no file present", got)
	}
}

// TestLookupString_MalformedFile asserts an unparseable cerberus.yaml degrades
// to "no file" rather than killing the CLI: every setting still has an
// environment variable and a flag, so a broken file must not be fatal.
func TestLookupString_MalformedFile(t *testing.T) {
	writeLookupYAML(t, "CERBERUS_VERIFY_REF: [unterminated\n")
	t.Setenv("CERBERUS_VERIFY_REF", "http://fromenv:9090")

	if got := NewLookup().String("CERBERUS_VERIFY_REF"); got != "http://fromenv:9090" {
		t.Errorf("String = %q; want the env value despite the malformed file", got)
	}
}

// TestLookupLines_YAMLSequence asserts the repeatable-flag settings read a YAML
// sequence element-for-element — including selectors containing commas, which
// any comma-splitting fallback would shred into malformed fragments.
func TestLookupLines_YAMLSequence(t *testing.T) {
	writeLookupYAML(t, ""+
		"CERBERUS_INVENTORY_LOKI_SELECTORS:\n"+
		"  - '{app=\"checkout\", env=\"prod\"}'\n"+
		"  - '{app=\"cart\"}'\n")

	want := []string{`{app="checkout", env="prod"}`, `{app="cart"}`}
	if got := NewLookup().Lines("CERBERUS_INVENTORY_LOKI_SELECTORS"); !reflect.DeepEqual(got, want) {
		t.Errorf("Lines = %#v; want %#v (one element per sequence entry, commas intact)", got, want)
	}
}

// TestLookupLines_Scalar asserts a scalar file value and the environment
// variable share the one-per-line encoding, the only form an env var can
// express.
func TestLookupLines_Scalar(t *testing.T) {
	writeLookupYAML(t, "CERBERUS_INVENTORY_LOKI_SELECTORS: \"{app=\\\"a\\\"}\\n{app=\\\"b\\\"}\"\n")

	want := []string{`{app="a"}`, `{app="b"}`}
	if got := NewLookup().Lines("CERBERUS_INVENTORY_LOKI_SELECTORS"); !reflect.DeepEqual(got, want) {
		t.Errorf("Lines (file scalar) = %#v; want %#v", got, want)
	}

	t.Setenv("CERBERUS_INVENTORY_LOKI_SELECTORS", "{app=\"x\"}\n{app=\"y\"}")
	want = []string{`{app="x"}`, `{app="y"}`}
	if got := NewLookup().Lines("CERBERUS_INVENTORY_LOKI_SELECTORS"); !reflect.DeepEqual(got, want) {
		t.Errorf("Lines (env) = %#v; want %#v (env outranks the file)", got, want)
	}
}

// TestLookupLines_Unset asserts nothing configured yields nil, not an empty
// non-nil slice: cobra's flag default must stay indistinguishable from
// "never set."
func TestLookupLines_Unset(t *testing.T) {
	chdir(t, t.TempDir())

	if got := NewLookup().Lines("CERBERUS_INVENTORY_LOKI_SELECTORS"); got != nil {
		t.Errorf("Lines = %#v; want nil when nothing is configured", got)
	}
}
