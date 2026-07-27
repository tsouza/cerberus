package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFileName is the file cerberus discovers in the working directory (or
// in /etc/cerberus). The gateway and `cerberus migrate` read the same one, so
// an operator configures a migration and the server it migrates to from a
// single document — the path docs/migration.md puts in front of them, and
// therefore the path these scenarios drive the CLI through.
const ConfigFileName = "cerberus.yaml"

// configFileMode keeps the written file readable only by its owner, matching
// the guidance for a real operator's copy: it carries backend bearer tokens.
const configFileMode os.FileMode = 0o600

// Setting is one CERBERUS_* entry in that file. List renders a YAML sequence,
// for the settings whose CLI flag is repeatable; otherwise Value renders a
// scalar.
type Setting struct {
	Key   string
	Value string
	List  []string
}

// WriteConfigFile renders settings into dir/cerberus.yaml. Each entry is
// marshalled on its own — a single-key map has no order to lose — so the file
// reads top to bottom in the order the caller declared it, the way a
// hand-written one does.
func WriteConfigFile(dir string, settings []Setting) error {
	if len(settings) == 0 {
		return fmt.Errorf(
			"migration harness: refusing to write an empty %s: a command configured from nothing would prove nothing about the config-file path",
			ConfigFileName,
		)
	}
	var doc strings.Builder
	for _, s := range settings {
		var value any = s.Value
		if s.List != nil {
			value = s.List
		}
		rendered, err := yaml.Marshal(map[string]any{s.Key: value})
		if err != nil {
			return fmt.Errorf("migration harness: render setting %s: %w", s.Key, err)
		}
		doc.Write(rendered)
	}
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(doc.String()), configFileMode); err != nil {
		return fmt.Errorf("migration harness: write %s: %w", path, err)
	}
	return nil
}

// settingFlagRE matches one line of a cobra flag table that names the setting
// a flag's default comes from, e.g.
//
//	--ref string   reference Prometheus base URL (setting: CERBERUS_VERIFY_REF)
var settingFlagRE = regexp.MustCompile(`--([a-zA-Z0-9-]+).*\(setting: (CERBERUS_[A-Z_]+)\)`)

// settingFlags maps flag name to setting key for every flag a command's help
// text declares one for. Reading the mapping back off the binary under test,
// rather than restating it here, is what keeps RequireSettingsFromFile honest
// as flags are added.
func settingFlags(help string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(help, "\n") {
		if m := settingFlagRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// subcommandOf returns the leading subcommand path of args — everything before
// the first flag.
func subcommandOf(args []string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i]
		}
	}
	return args
}

// RequireSettingsFromFile fails when args carry a flag the command itself
// documents a setting for. Those values belong in cerberus.yaml: an operator
// following docs/migration.md writes one file and runs a bare command, so that
// is the path these scenarios must exercise, and a later edit quietly moving a
// value back onto the command line would leave it uncovered while every
// assertion still passed. Flags with no setting — --json, --out, --top — are
// per-run output choices rather than configuration, and stay where they are.
func RequireSettingsFromFile(bin string, args []string) error {
	sub := subcommandOf(args)
	help := append(append([]string{}, sub...), "--help")
	res, err := Run(RunSpec{Bin: bin, Args: help, Env: OfflineEnv()})
	if err != nil {
		return err
	}
	name := strings.Join(sub, " ")
	if res.ExitCode != 0 {
		return fmt.Errorf("migration harness: `%s --help` exited %d: %s",
			name, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	declared := settingFlags(string(res.Stdout))
	if len(declared) == 0 {
		return fmt.Errorf(
			"migration harness: `%s --help` declares no setting at all, so this check would pass by proving nothing; the flag-to-setting contract it reads has changed",
			name,
		)
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		flag, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if key, ok := declared[flag]; ok {
			return fmt.Errorf(
				"migration harness: `%s` was given --%s on the command line, but that value belongs in %s as %s: these scenarios configure a migration from the config file, because that is what an operator following docs/migration.md does",
				name, flag, ConfigFileName, key,
			)
		}
	}
	return nil
}
