// Package promrules parses Prometheus recording/alerting rule files into the
// minimal shape cerberus's migration preview needs: the PromQL expression of
// each rule plus its record/alert name.
//
// It deliberately does NOT import prometheus/rulefmt. That package drags in
// ~112 transitive packages (k8s apimachinery among them) for validation the
// preview does not need — the preview only harvests the `expr` of every rule so
// it can be dry-run through the engine. A leaf yaml.v3 decode over the
// documented rule-group schema is the whole dependency surface.
package promrules

import (
	"fmt"

	yaml "gopkg.in/yaml.v3"
)

// RuleGroups is the top-level shape of a Prometheus rule file: a list of named
// groups, each holding recording and/or alerting rules.
type RuleGroups struct {
	Groups []RuleGroup `yaml:"groups"`
}

// RuleGroup is one named group of rules.
type RuleGroup struct {
	Name  string `yaml:"name"`
	Rules []Rule `yaml:"rules"`
}

// Rule is a single recording or alerting rule. A recording rule sets Record
// (the series name it produces); an alerting rule sets Alert (the alert name).
// Both carry Expr — the PromQL the preview dry-runs.
type Rule struct {
	Record string `yaml:"record"`
	Alert  string `yaml:"alert"`
	Expr   string `yaml:"expr"`
}

// Parse decodes a Prometheus rule file's YAML into RuleGroups. It wraps decode
// errors so the caller (the migration harvester) can attribute a parse failure
// to the file it read.
func Parse(data []byte) (RuleGroups, error) {
	var rg RuleGroups
	if err := yaml.Unmarshal(data, &rg); err != nil {
		return RuleGroups{}, fmt.Errorf("promrules: parse rule file: %w", err)
	}
	return rg, nil
}
