package routerrules

import (
	"bytes"
	"fmt"

	yaml "gopkg.in/yaml.v3"
)

// SchemaAPIVersion is the schema-shape contract the loader accepts. It is
// bumped only on a breaking structural change to the YAML grammar; additive
// rule growth bumps the catalog's own catalogVersion instead.
const SchemaAPIVersion = "routerrules.cerberus/v1"

// Catalog is the decoded top-level shape of a router-rules YAML file: the
// schema-version contract, a content revision counter, the named-parameter
// registry, and the generic detector rules.
type Catalog struct {
	APIVersion     string      `yaml:"apiVersion"`
	CatalogVersion int         `yaml:"catalogVersion"`
	Params         []ParamSpec `yaml:"params"`
	Rules          []Rule      `yaml:"rules"`
}

// ParamKind is the closed set of resolver strategies. A param of an unknown
// kind is a hard load error.
type ParamKind string

const (
	// ParamConfig reads a deployment config value by Key. This is the only
	// place an operator number enters the system.
	ParamConfig ParamKind = "config"
	// ParamCorpusPercentile resolves to a quantile of a corpus column over an
	// optional scope, optionally partitioned.
	ParamCorpusPercentile ParamKind = "corpus_percentile"
	// ParamCorpusAgg resolves to a simple aggregate (max/avg/min/stddevPop) of
	// a corpus column.
	ParamCorpusAgg ParamKind = "corpus_agg"
	// ParamCorpusCountRatio resolves to countIf(num scope) / countIf(den
	// scope) over the corpus population.
	ParamCorpusCountRatio ParamKind = "corpus_count_ratio"
	// ParamConfigScaled resolves to the product of two already-declared config
	// params: a fraction (Ref) times a magnitude (ScaleBy). It lets a rule gate
	// on a tunable fraction of an operator-configured absolute (e.g. a near-cap
	// threshold = 0.8 × the deployment's query memory cap) while keeping the
	// catalog number-free — both the fraction default and the absolute live in
	// deployment config/env, never in catalog YAML.
	ParamConfigScaled ParamKind = "config_scaled"
)

// ParamRef is a reference to another declared parameter by name. It is the only
// way a numeric quantity (e.g. a percentile fraction) can appear inside a param
// spec — the spec grammar has no production for a bare number.
type ParamRef struct {
	Ref string `yaml:"ref"`
}

// Scope is an equality filter over enum columns, restricting the population a
// corpus param aggregates over (e.g. the healthy route-A rows). Keys must be
// enum columns; values must be valid category tokens. This is the one place a
// scalar appears in a param spec, and it is validated to be an enum category,
// never a number.
type Scope map[string]string

// ParamSpec is one entry in the named-parameter registry: a name plus how to
// resolve it. It carries the NAME and the resolver KIND, never the value.
type ParamSpec struct {
	Name string    `yaml:"name"`
	Kind ParamKind `yaml:"kind"`

	// Config kind.
	Key string `yaml:"key,omitempty"`

	// Corpus kinds.
	Column      string    `yaml:"column,omitempty"`
	Percentile  *ParamRef `yaml:"percentile,omitempty"` // corpus_percentile: fraction param ref
	Agg         string    `yaml:"agg,omitempty"`        // corpus_agg: max|avg|min|stddevPop
	PartitionBy []string  `yaml:"partition_by,omitempty"`
	Scope       Scope     `yaml:"scope,omitempty"`

	// corpus_count_ratio kind: numerator / denominator scopes.
	NumeratorScope   Scope `yaml:"numerator_scope,omitempty"`
	DenominatorScope Scope `yaml:"denominator_scope,omitempty"`

	// config_scaled kind: Ref is the fraction param, ScaleBy is the magnitude
	// param; the resolved value is Ref.Scalar * ScaleBy.Scalar. Both must name
	// already-declared params (in practice config-kind ones), so the only numbers
	// in play enter through deployment config, never the catalog.
	Ref     *ParamRef `yaml:"ref,omitempty"`
	ScaleBy *ParamRef `yaml:"scale_by,omitempty"`
}

// RuleStatus gates whether a rule is evaluated. Experimental rules are loaded
// and validated but only evaluated when explicitly opted in; deprecated rules
// are loaded and validated but never evaluated.
type RuleStatus string

const (
	StatusActive       RuleStatus = "active"
	StatusExperimental RuleStatus = "experimental"
	StatusDeprecated   RuleStatus = "deprecated"
)

// Rule is one generic detector. Its condition references corpus columns and
// ${param} references only — never a number.
type Rule struct {
	ID         string     `yaml:"id"`
	Severity   string     `yaml:"severity"`
	Since      int        `yaml:"since"`
	Status     RuleStatus `yaml:"status"`
	AppliesTo  []string   `yaml:"applies_to,omitempty"`
	GroupBy    []string   `yaml:"group_by"`
	MinSupport *ParamRef  `yaml:"min_support,omitempty"`
	Condition  Predicate  `yaml:"condition"`
	Evidence   *Evidence  `yaml:"evidence,omitempty"`
	Finding    string     `yaml:"finding"`
	Action     string     `yaml:"action,omitempty"`
}

// Evidence lists extra aggregate expressions (over a closed vocabulary) to
// surface alongside each finding's support count.
type Evidence struct {
	Report []string `yaml:"report,omitempty"`
}

// Predicate is the decoded condition tree node. Exactly one of the boolean
// combinators (All/Any/Not) or a leaf comparison may be set; validation
// enforces the exclusivity. The decoded form is lowered into the strongly-typed
// condition AST (condition.go), which deliberately has no number-literal node.
type Predicate struct {
	All []Predicate `yaml:"all,omitempty"`
	Any []Predicate `yaml:"any,omitempty"`
	Not *Predicate  `yaml:"not,omitempty"`

	// Leaf comparison fields (set only on a leaf node).
	Col   string `yaml:"col,omitempty"`
	Op    string `yaml:"op,omitempty"`
	Enum  any    `yaml:"enum,omitempty"`  // string | []string for enum columns
	Param string `yaml:"param,omitempty"` // a ${param} ref — the ONLY numeric operand path
}

// DecodeCatalog strict-decodes YAML bytes into a Catalog, rejecting unknown
// fields so a smuggled key (e.g. a numeric `value:`) is a hard error rather
// than silently ignored.
func DecodeCatalog(data []byte) (*Catalog, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cat Catalog
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("routerrules: decode catalog: %w", err)
	}
	return &cat, nil
}
