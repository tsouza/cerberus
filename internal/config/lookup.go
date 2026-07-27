package config

import (
	"os"
	"strings"
)

// Lookup resolves a CERBERUS_* setting that the server's typed registry does
// not own — the `cerberus migrate` verify/inventory settings, which configure a
// one-shot CLI run rather than the running gateway, and the read-side schema
// shape that `internal/schema` resolves for itself — from the same two sources,
// in the same order, as every other cerberus setting: the environment variable
// first, then an optional cerberus.yaml in the working directory or
// /etc/cerberus.
//
// The migrate settings stay out of Config (and therefore out of the generated
// docs/configuration.md) because nothing in the server reads them, but a
// migration is configured from the same file as the gateway it migrates to, so
// one cerberus.yaml carries both surfaces.
type Lookup struct {
	settings map[string]string
	err      error
}

// NewLookup probes for cerberus.yaml and returns a Lookup over it. A missing
// file is not an error: every setting a Lookup serves also has an environment
// variable and a CLI flag, so the file is pure convenience and its absence
// leaves the other two sources intact. A file that exists but cannot be
// understood is reported by Err — callers surface it before doing any work,
// because a migration configured from a file nobody could read is a migration
// configured by accident.
func NewLookup() *Lookup {
	settings, _, err := loadConfigFile()
	return &Lookup{settings: settings, err: err}
}

// Err reports why the config file could not be used, or nil when there was
// none or it loaded cleanly.
func (l *Lookup) Err() error { return l.err }

// String returns key's value — the environment variable of that exact name
// when it is set and non-empty, else the cerberus.yaml entry, else "".
func (l *Lookup) String(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return l.settings[key]
}

// Lines returns key's value as an ordered list of entries, for the settings
// whose CLI flag is repeatable. The file's YAML sequence has already been
// joined on newlines by the translation step, and the environment variable —
// which has no way to express a sequence — carries the same shape, so both
// split identically here. The split is never on commas: these values are LogQL
// stream selectors, and a comma is ordinary syntax inside one.
//
// Entries are returned exactly as written; trimming and empty-entry pruning
// belong to the caller, which applies the same normalisation to flag values.
func (l *Lookup) Lines(key string) []string {
	v := l.String(key)
	if v == "" {
		return nil
	}
	return strings.Split(v, "\n")
}
