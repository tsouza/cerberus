package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/config"
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

// Setting is one setting to put in that file, named by its CERBERUS_* key —
// the name the binary's own flag table declares, which WriteConfigFile resolves
// to the nested path it writes. List renders a YAML sequence, for the settings
// whose CLI flag is repeatable; otherwise Value renders a scalar.
type Setting struct {
	Key   string
	Value string
	List  []string
}

// WriteConfigFile renders settings into dir/cerberus.yaml in the nested shape —
// `migrate.verify.ref`, not `CERBERUS_VERIFY_REF` — because that is the shape
// docs/migration.md hands an operator and the shape the Helm chart's values.yaml
// uses. Callers name settings by their CERBERUS_* key, which is what the
// binary's own flag table declares; the nested path comes from the binary's own
// binding table via config.ConfigFilePath, so the harness never restates a
// mapping that could drift from it.
//
// A setting with no nested name is written by its flat key, which the loader
// accepts in the same document. That is the long tail's escape hatch, and a
// scenario reaching for one still gets a working file.
//
// Sibling keys appear in the order the caller declared them, so a failing
// scenario's artifact reads top to bottom like a hand-written file rather than
// like a reshuffled map.
func WriteConfigFile(dir string, settings []Setting) error {
	if len(settings) == 0 {
		return fmt.Errorf(
			"migration harness: refusing to write an empty %s: a command configured from nothing would prove nothing about the config-file path",
			ConfigFileName,
		)
	}
	root := &yaml.Node{Kind: yaml.MappingNode}
	for _, s := range settings {
		path, ok := config.ConfigFilePath(s.Key)
		if !ok {
			path = s.Key
		}
		leaf, err := valueNode(s)
		if err != nil {
			return err
		}
		if err := placeNode(root, strings.Split(path, "."), leaf); err != nil {
			return fmt.Errorf("migration harness: setting %s at %s: %w", s.Key, path, err)
		}
	}
	rendered, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("migration harness: render %s: %w", ConfigFileName, err)
	}
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, rendered, configFileMode); err != nil {
		return fmt.Errorf("migration harness: write %s: %w", path, err)
	}
	return nil
}

// valueNode encodes a setting's value, letting yaml decide how to quote it: a
// LogQL selector is made of the quotes and braces a naive `key: value` renderer
// would emit unescaped and corrupt.
func valueNode(s Setting) (*yaml.Node, error) {
	var v any = s.Value
	if s.List != nil {
		v = s.List
	}
	node := &yaml.Node{}
	if err := node.Encode(v); err != nil {
		return nil, fmt.Errorf("migration harness: render setting %s: %w", s.Key, err)
	}
	return node, nil
}

// placeNode grafts leaf onto the document at path, creating the blocks above it
// on the way down and reusing any that a previous setting already created. Two
// settings whose paths collide — one claiming a block the other claims as a
// value — is a bug in the caller's setting list rather than something to
// silently resolve one way.
func placeNode(root *yaml.Node, path []string, leaf *yaml.Node) error {
	node := root
	for i, seg := range path {
		last := i == len(path)-1
		child := childOf(node, seg)
		if child == nil {
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: seg})
			if last {
				node.Content = append(node.Content, leaf)
				return nil
			}
			child = &yaml.Node{Kind: yaml.MappingNode}
			node.Content = append(node.Content, child)
		} else if last || child.Kind != yaml.MappingNode {
			return fmt.Errorf("%q is claimed twice", strings.Join(path[:i+1], "."))
		}
		node = child
	}
	return nil
}

// childOf returns the value node stored under key in a mapping node, or nil.
func childOf(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
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
			where := key
			if path, ok := config.ConfigFilePath(key); ok {
				where = path
			}
			return fmt.Errorf(
				"migration harness: `%s` was given --%s on the command line, but that value belongs in %s under %s: these scenarios configure a migration from the config file, because that is what an operator following docs/migration.md does",
				name, flag, ConfigFileName, where,
			)
		}
	}
	return nil
}
