package routerrules

import (
	"errors"
	"fmt"
	"strings"
)

// Validate enforces the no-numbers invariant and every structural rule of the
// catalog. It is run at load time (LoadCatalog) and standalone (the CLI's
// --validate-only gate). It returns a single error joining all problems found,
// so a reviewer or CI run sees every issue at once rather than one-at-a-time.
//
// The strongest invariant guard is upstream of this function: the condition AST
// (condition.go) has no number-literal node, so a number in a comparison
// operand is unrepresentable. Validate adds the load-time checks the AST alone
// cannot make: dangling param refs, unknown resolver kinds, unknown columns,
// duplicate rule ids, cyclic param dependencies, and a numeric scalar smuggled
// into an enum or scope slot.
func Validate(cat *Catalog) error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if cat.APIVersion != SchemaAPIVersion {
		add("apiVersion %q is not the supported %q", cat.APIVersion, SchemaAPIVersion)
	}
	if cat.CatalogVersion <= 0 {
		add("catalogVersion must be a positive integer, got %d", cat.CatalogVersion)
	}

	paramNames := validateParams(cat, add)

	// The param dependency graph must be acyclic (topo sort fails closed).
	specs := make(map[string]ParamSpec, len(cat.Params))
	for _, p := range cat.Params {
		specs[p.Name] = p
	}
	if _, err := topoSortParams(specs); err != nil {
		add("%v", err)
	}

	validateRules(cat, paramNames, specs, add)

	return errors.Join(errs...)
}

// validateParams checks each param spec and returns the set of declared param
// names for cross-referencing by rules.
func validateParams(cat *Catalog, add func(string, ...any)) map[string]struct{} {
	names := map[string]struct{}{}
	for i := range cat.Params {
		p := &cat.Params[i]
		if p.Name == "" {
			add("params[%d] has no name", i)
			continue
		}
		if _, dup := names[p.Name]; dup {
			add("duplicate param name %q", p.Name)
		}
		names[p.Name] = struct{}{}
		validateParamKind(p, add)
	}
	// A second pass resolves cross-param refs now that all names exist.
	for i := range cat.Params {
		p := &cat.Params[i]
		switch p.Kind {
		case ParamCorpusPercentile:
			if p.Percentile != nil && p.Percentile.Ref != "" {
				if _, ok := names[p.Percentile.Ref]; !ok {
					add("param %q references undeclared fraction param %q", p.Name, p.Percentile.Ref)
				}
			}
		case ParamConfigScaled:
			if p.Ref != "" {
				if _, ok := names[p.Ref]; !ok {
					add("param %q references undeclared ref param %q", p.Name, p.Ref)
				}
			}
			if p.ScaleBy != "" {
				if _, ok := names[p.ScaleBy]; !ok {
					add("param %q references undeclared scale_by param %q", p.Name, p.ScaleBy)
				}
			}
		}
	}
	return names
}

func validateParamKind(p *ParamSpec, add func(string, ...any)) {
	switch p.Kind {
	case ParamConfig:
		if p.Key == "" {
			add("config param %q must set a key", p.Name)
		}
		if p.Ref != "" || p.ScaleBy != "" {
			add("config param %q must not set ref/scale_by (those are config_scaled fields)", p.Name)
		}
		forbidCorpusFields(p, add)
	case ParamCorpusPercentile:
		validateColumn(p.Name, p.Column, add)
		if p.Percentile == nil || p.Percentile.Ref == "" {
			add("corpus_percentile param %q must set percentile.ref", p.Name)
		}
		validateScope(p.Name, p.Scope, add)
		validatePartition(p.Name, p.PartitionBy, add)
	case ParamCorpusAgg:
		validateColumn(p.Name, p.Column, add)
		switch AggFunc(p.Agg) {
		case AggMax, AggAvg, AggMin, AggStdDev:
		default:
			add("corpus_agg param %q has unknown agg %q (want max|avg|min|stddevPop)", p.Name, p.Agg)
		}
		validateScope(p.Name, p.Scope, add)
		validatePartition(p.Name, p.PartitionBy, add)
	case ParamCorpusCountRatio:
		if len(p.NumeratorScope) == 0 || len(p.DenominatorScope) == 0 {
			add("corpus_count_ratio param %q must set numerator_scope and denominator_scope", p.Name)
		}
		validateScope(p.Name, p.NumeratorScope, add)
		validateScope(p.Name, p.DenominatorScope, add)
	case ParamConfigScaled:
		if p.Ref == "" {
			add("config_scaled param %q must set ref (the fraction param)", p.Name)
		}
		if p.ScaleBy == "" {
			add("config_scaled param %q must set scale_by (the magnitude param)", p.Name)
		}
		forbidCorpusFields(p, add)
	default:
		add("param %q has unknown kind %q (want config|config_scaled|corpus_percentile|corpus_agg|corpus_count_ratio)", p.Name, p.Kind)
	}
}

// forbidCorpusFields catches a config param that also sets corpus-only fields, a
// likely smuggling attempt or a copy-paste error.
func forbidCorpusFields(p *ParamSpec, add func(string, ...any)) {
	if p.Column != "" || p.Percentile != nil || p.Agg != "" || len(p.PartitionBy) > 0 || len(p.Scope) > 0 {
		add("config param %q must not set corpus fields (column/percentile/agg/partition_by/scope)", p.Name)
	}
}

func validateColumn(owner, col string, add func(string, ...any)) {
	if col == "" {
		add("%q must set a column", owner)
		return
	}
	if !knownColumn(col) {
		add("%q references unknown column %q", owner, col)
		return
	}
	if columnKinds[col] != ColumnNumeric {
		add("%q aggregates non-numeric column %q", owner, col)
	}
}

func validatePartition(owner string, partitionBy []string, add func(string, ...any)) {
	for _, col := range partitionBy {
		if !knownColumn(col) {
			add("%q partitions by unknown column %q", owner, col)
		}
	}
	if len(partitionBy) > 1 {
		add("%q partitions by more than one column (%v); only single-column partitions are supported", owner, partitionBy)
	}
}

// validateScope checks a scope filter references only enum columns and valid
// category tokens. A numeric value in a scope slot is caught here: an enum
// column never accepts a number, and a non-enum column is rejected outright, so
// no number can hide in a scope.
func validateScope(owner string, scope Scope, add func(string, ...any)) {
	for col, val := range scope {
		if !knownColumn(col) {
			add("%q scope references unknown column %q", owner, col)
			continue
		}
		if !isEnumColumn(col) {
			add("%q scope filters non-enum column %q (only route/exit_status/language may be scoped)", owner, col)
			continue
		}
		if !validEnumValue(col, val) {
			add("%q scope value %q is not a valid category of enum column %q", owner, val, col)
		}
	}
}

func validateRules(cat *Catalog, paramNames map[string]struct{}, specs map[string]ParamSpec, add func(string, ...any)) {
	seen := map[string]struct{}{}
	for i := range cat.Rules {
		r := &cat.Rules[i]
		if r.ID == "" {
			add("rules[%d] has no id", i)
			continue
		}
		if _, dup := seen[r.ID]; dup {
			add("duplicate rule id %q", r.ID)
		}
		seen[r.ID] = struct{}{}

		if _, ok := parseSeverity(r.Severity); !ok {
			add("rule %q has unknown severity %q", r.ID, r.Severity)
		}
		validateStatus(r, add)
		validateAppliesTo(r, add)
		validateGroupBy(r, add)
		validateMinSupport(r, paramNames, add)
		validateEvidence(r, add)
		validateCondition(r, paramNames, add)
		validatePartitionInGroupBy(r, specs, add)
	}
}

// validatePartitionInGroupBy enforces at LOAD time the contract the evaluator
// otherwise only catches at report time (sharedPartition): if a rule's condition
// references a partitioned corpus param, that param's partition column must be
// one of the rule's group_by columns, so the per-partition sub-evaluation can be
// anchored to a group key. Failing at catalog load is better than at report
// time, and it makes the partitioned-param contract a structural guarantee.
func validatePartitionInGroupBy(r *Rule, specs map[string]ParamSpec, add func(string, ...any)) {
	cond, err := lowerPredicate(r.Condition)
	if err != nil {
		return // a malformed condition is already reported by validateCondition.
	}
	gb := make(map[string]struct{}, len(r.GroupBy))
	for _, c := range r.GroupBy {
		gb[c] = struct{}{}
	}
	refs := map[string]struct{}{}
	cond.paramRefs(refs)
	for name := range refs {
		spec, ok := specs[name]
		if !ok || len(spec.PartitionBy) == 0 {
			continue
		}
		for _, partCol := range spec.PartitionBy {
			if _, ok := gb[partCol]; !ok {
				add("rule %q references partitioned param %q whose partition column %q is not in the rule's group_by %v", r.ID, name, partCol, r.GroupBy)
			}
		}
	}
}

func validateStatus(r *Rule, add func(string, ...any)) {
	switch r.Status {
	case StatusActive, StatusExperimental, StatusDeprecated:
	case "":
		add("rule %q has no status (want active|experimental|deprecated)", r.ID)
	default:
		add("rule %q has unknown status %q", r.ID, r.Status)
	}
}

func validateAppliesTo(r *Rule, add func(string, ...any)) {
	for _, lang := range r.AppliesTo {
		if !validEnumValue("language", lang) {
			add("rule %q applies_to has unknown language %q", r.ID, lang)
		}
	}
}

func validateGroupBy(r *Rule, add func(string, ...any)) {
	if len(r.GroupBy) == 0 {
		add("rule %q has no group_by", r.ID)
	}
	for _, col := range r.GroupBy {
		if !knownColumn(col) {
			add("rule %q group_by references unknown column %q", r.ID, col)
		}
	}
}

func validateMinSupport(r *Rule, paramNames map[string]struct{}, add func(string, ...any)) {
	if r.MinSupport == nil || r.MinSupport.Ref == "" {
		return
	}
	if _, ok := paramNames[r.MinSupport.Ref]; !ok {
		add("rule %q min_support references undeclared param %q", r.ID, r.MinSupport.Ref)
	}
}

func validateEvidence(r *Rule, add func(string, ...any)) {
	if r.Evidence == nil {
		return
	}
	for _, tok := range r.Evidence.Report {
		if tok == "count" {
			continue
		}
		if _, err := parseEvidenceExpr(tok); err != nil {
			add("rule %q evidence: %v", r.ID, err)
		}
	}
}

// validateCondition lowers the rule's condition (which structurally cannot carry
// a number), then walks it to verify every column is known, every enum
// comparison targets an enum column with valid tokens, every param comparison
// targets a numeric column and references a declared param, and an enum column
// is never compared against a resolved param (a category is not a threshold).
func validateCondition(r *Rule, paramNames map[string]struct{}, add func(string, ...any)) {
	cond, err := lowerPredicate(r.Condition)
	if err != nil {
		add("rule %q condition: %v", r.ID, err)
		return
	}
	walkCondition(cond, func(c Condition) {
		switch n := c.(type) {
		case *EnumCmp:
			if !knownColumn(n.Column) {
				add("rule %q condition references unknown column %q", r.ID, n.Column)
				return
			}
			if !isEnumColumn(n.Column) {
				add("rule %q compares non-enum column %q against a category (use a param instead)", r.ID, n.Column)
				return
			}
			for _, v := range n.Values {
				if !validEnumValue(n.Column, v) {
					add("rule %q condition value %q is not a category of enum column %q", r.ID, v, n.Column)
				}
			}
		case *ParamCmp:
			if !knownColumn(n.Column) {
				add("rule %q condition references unknown column %q", r.ID, n.Column)
				return
			}
			if columnKinds[n.Column] != ColumnNumeric {
				add("rule %q compares non-numeric column %q against a param", r.ID, n.Column)
			}
			if _, ok := paramNames[n.Param]; !ok {
				add("rule %q condition references undeclared param %q", r.ID, n.Param)
			}
		}
	})
}

func walkCondition(c Condition, fn func(Condition)) {
	fn(c)
	switch n := c.(type) {
	case *AndCond:
		for _, ch := range n.Children {
			walkCondition(ch, fn)
		}
	case *OrCond:
		for _, ch := range n.Children {
			walkCondition(ch, fn)
		}
	case *NotCond:
		walkCondition(n.Child, fn)
	}
}

// LoadCatalog decodes and validates a catalog from raw YAML bytes. It is the
// single entry point callers use to obtain a trusted Catalog.
func LoadCatalog(data []byte) (*Catalog, error) {
	cat, err := DecodeCatalog(data)
	if err != nil {
		return nil, err
	}
	if err := Validate(cat); err != nil {
		return nil, fmt.Errorf("routerrules: catalog invalid:\n%w", indentErr(err))
	}
	return cat, nil
}

// LoadEmbeddedCatalog merges the shipped split catalog (base + one file per
// rule) into a single Catalog and validates it. The base file carries
// apiVersion / catalogVersion / params; each rules/<rule_id>.yaml carries
// exactly one rule. Files are merged in a deterministic order (base first, then
// rule files sorted by filename) so the resulting rule order is reproducible and
// independent of FS iteration order.
func LoadEmbeddedCatalog() (*Catalog, error) {
	files, err := EmbeddedCatalogFiles()
	if err != nil {
		return nil, err
	}
	cat, err := mergeCatalogFiles(files)
	if err != nil {
		return nil, err
	}
	if err := Validate(cat); err != nil {
		return nil, fmt.Errorf("routerrules: catalog invalid:\n%w", indentErr(err))
	}
	return cat, nil
}

// mergeCatalogFiles strict-decodes the base file (which must carry the schema
// contract + params) and folds every subsequent rule file's rules into it,
// rejecting a rule id declared by two files. Each file is decoded with the same
// strict KnownFields(true) contract as the single-file path, so a malformed
// file or a smuggled unknown key is a hard error pointing at the offending file.
func mergeCatalogFiles(files []CatalogFile) (*Catalog, error) {
	if len(files) == 0 {
		return nil, errors.New("routerrules: no catalog files to merge")
	}
	base, err := DecodeCatalog(files[0].Bytes)
	if err != nil {
		return nil, fmt.Errorf("routerrules: base catalog %q: %w", files[0].Name, err)
	}
	// idSource maps each merged rule id to the file that declared it, so a
	// cross-file collision names both offenders.
	idSource := make(map[string]string, len(files))
	for _, r := range base.Rules {
		idSource[r.ID] = files[0].Name
	}
	for _, f := range files[1:] {
		sub, err := DecodeCatalog(f.Bytes)
		if err != nil {
			return nil, fmt.Errorf("routerrules: rule file %q: %w", f.Name, err)
		}
		for _, r := range sub.Rules {
			if prev, dup := idSource[r.ID]; dup {
				return nil, fmt.Errorf("routerrules: duplicate rule id %q declared in both %q and %q", r.ID, prev, f.Name)
			}
			idSource[r.ID] = f.Name
			base.Rules = append(base.Rules, r)
		}
	}
	return base, nil
}

func indentErr(err error) error {
	lines := strings.Split(err.Error(), "\n")
	for i, l := range lines {
		lines[i] = "  - " + l
	}
	return errors.New(strings.Join(lines, "\n"))
}
