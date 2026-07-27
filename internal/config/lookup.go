package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Lookup resolves a CERBERUS_* setting that the server's typed registry does
// not own — the `cerberus migrate` verify/inventory settings, which configure
// a one-shot CLI run rather than the running gateway — from the same two
// sources, in the same order, as every other cerberus setting: the
// environment variable first, then an optional cerberus.yaml in the working
// directory or /etc/cerberus.
//
// Those settings stay out of Config (and therefore out of the generated
// docs/configuration.md) because nothing in the server reads them, but a
// migration is configured from the same file as the gateway it migrates to,
// so one cerberus.yaml carries both surfaces.
type Lookup struct {
	v *viper.Viper
}

// NewLookup probes for cerberus.yaml and returns a Lookup over it. A missing
// — or malformed — file is not an error: every setting a Lookup serves also
// has an environment variable and a CLI flag, so the file is pure convenience
// and its absence leaves the other two sources intact.
func NewLookup() *Lookup {
	v := viper.New()
	v.SetConfigName(configFileBaseName)
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/cerberus")
	_ = v.ReadInConfig()
	return &Lookup{v: v}
}

// String returns key's value — the environment variable of that exact name
// when it is set and non-empty, else the cerberus.yaml entry, else "".
func (l *Lookup) String(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return l.v.GetString(key)
}

// Lines returns key's value as an ordered list of entries, for the settings
// whose CLI flag is repeatable. A YAML sequence maps to the list directly; a
// scalar (and the environment variable, which has no way to express a
// sequence) is split on newlines — never on commas, because these values are
// LogQL stream selectors and a comma is ordinary syntax inside one.
//
// Entries are returned exactly as written; trimming and empty-entry pruning
// belong to the caller, which applies the same normalisation to flag values.
// A non-string sequence entry is rendered rather than dropped, so a
// mistyped selector fails loudly downstream instead of vanishing.
func (l *Lookup) Lines(key string) []string {
	if v := os.Getenv(key); v != "" {
		return strings.Split(v, "\n")
	}
	if seq, ok := l.v.Get(key).([]any); ok {
		out := make([]string, 0, len(seq))
		for _, item := range seq {
			out = append(out, fmt.Sprint(item))
		}
		return out
	}
	s := l.v.GetString(key)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
