package routerrules

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Evaluator runs a validated catalog against a corpus source: it resolves the
// named parameters, evaluates each active rule into grouped findings, drops
// thin classes below min_support, and substitutes resolved values into each
// finding message.
type Evaluator struct {
	cat *Catalog
	src CorpusSource
	res *ParamResolver
}

// EvalOptions tunes which rules run.
type EvalOptions struct {
	// IncludeExperimental evaluates rules with status: experimental in addition
	// to active rules. Deprecated rules are never evaluated.
	IncludeExperimental bool
}

// NewEvaluator builds an evaluator. The catalog must already have passed
// Validate.
func NewEvaluator(cat *Catalog, cfg ConfigLookup, src CorpusSource) *Evaluator {
	return &Evaluator{cat: cat, src: src, res: NewParamResolver(cfg, src)}
}

// Evaluate resolves params and runs every selected rule, returning the ordered
// report. It returns an error only on an operational failure (param resolution,
// corpus read); an empty report is a valid, non-error outcome.
func (e *Evaluator) Evaluate(ctx context.Context, opts EvalOptions) (*Report, error) {
	env, err := e.res.Resolve(ctx, e.cat)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	var skipped []SkippedRule
	for i := range e.cat.Rules {
		rule := &e.cat.Rules[i]
		if !ruleSelected(rule, opts) {
			continue
		}
		if sk, ok := noSignalSkip(rule, env); ok {
			skipped = append(skipped, sk)
			continue
		}
		got, err := e.evalRule(ctx, rule, env)
		if err != nil {
			return nil, fmt.Errorf("routerrules: evaluate rule %q: %w", rule.ID, err)
		}
		findings = append(findings, got...)
	}
	sortFindings(findings)
	sortSkipped(skipped)
	return &Report{Findings: findings, Skipped: skipped}, nil
}

// noSignalSkip reports whether a rule must be skipped because a fire-gate
// parameter it references resolved to NoSignal — i.e. the param's scoped
// sub-population was empty, so there is no learned watermark to compare against.
// An empty fire-gate population is the inverse of safe: normalizing it to 0
// would make a `>=` gate fire on every row and a `<` gate fire on none, so the
// only honest outcome is to not run the rule and say so. A param referenced only
// in the finding message (never in a ParamCmp condition) is not a fire-gate and
// never triggers a skip; the message simply renders that param's empty/0 form.
func noSignalSkip(rule *Rule, env Env) (SkippedRule, bool) {
	cond, err := lowerPredicate(rule.Condition)
	if err != nil {
		// A malformed condition is surfaced by evalRule's own lowering, which
		// runs next and returns the structured error; don't mask it here.
		return SkippedRule{}, false
	}
	refs := map[string]struct{}{}
	cond.paramRefs(refs)

	var noSignal []string
	for name := range refs {
		if v, ok := env[name]; ok && !v.IsPartitioned() && v.NoSignal {
			noSignal = append(noSignal, name)
		}
	}
	if len(noSignal) == 0 {
		return SkippedRule{}, false
	}
	sort.Strings(noSignal)
	return SkippedRule{
		RuleID: rule.ID,
		Params: noSignal,
		Reason: fmt.Sprintf("fire-gate param(s) %s have no signal (empty sub-population); rule not evaluated", strings.Join(noSignal, ", ")),
	}, true
}

// sortSkipped orders skipped-rule records by rule id for a deterministic report.
func sortSkipped(skipped []SkippedRule) {
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].RuleID < skipped[j].RuleID })
}

func ruleSelected(rule *Rule, opts EvalOptions) bool {
	switch rule.Status {
	case StatusActive:
		return true
	case StatusExperimental:
		return opts.IncludeExperimental
	default: // deprecated or unset-but-validated
		return false
	}
}

func (e *Evaluator) evalRule(ctx context.Context, rule *Rule, env Env) ([]Finding, error) {
	cond, err := lowerPredicate(rule.Condition)
	if err != nil {
		return nil, err
	}

	// applies_to scopes the rule to a language subset by AND-ing a language IN
	// (...) leaf onto the condition. Omitted = all languages.
	if len(rule.AppliesTo) > 0 {
		cond = &AndCond{Children: []Condition{
			cond,
			&EnumCmp{Column: "language", Op: OpIn, Values: rule.AppliesTo},
		}}
	}

	evidence, err := parseEvidence(rule.Evidence)
	if err != nil {
		return nil, err
	}

	severity, ok := parseSeverity(rule.Severity)
	if !ok {
		return nil, fmt.Errorf("rule %q has unknown severity %q", rule.ID, rule.Severity)
	}

	minSupport, err := resolveMinSupport(rule, env)
	if err != nil {
		return nil, err
	}

	// A condition that references a partition-keyed param (e.g. a per-language
	// memory watermark) cannot lower to a single flat WHERE clause: the
	// threshold differs per partition. The evaluator expands such a rule into
	// one sub-evaluation per partition value, each carrying a scalar-bound Env,
	// so the backend (CH or JSONL) only ever sees scalar-bound conditions. The
	// catalog always partitions by a group_by column, so the partition value is
	// recoverable as a group-key value.
	subEvals, err := expandPartitioned(cond, rule.GroupBy, env)
	if err != nil {
		return nil, err
	}

	var groups []GroupResult
	for _, sub := range subEvals {
		got, err := e.src.EvalRule(ctx, RuleQuery{
			Condition: sub.cond,
			GroupBy:   rule.GroupBy,
			Evidence:  evidence,
			Env:       sub.env,
		})
		if err != nil {
			return nil, err
		}
		groups = append(groups, sub.restrict(got)...)
	}

	out := make([]Finding, 0, len(groups))
	for _, g := range groups {
		if g.Support < minSupport {
			continue
		}
		groupMap := make(map[string]string, len(rule.GroupBy))
		for i, col := range rule.GroupBy {
			if i < len(g.GroupKey) {
				groupMap[col] = g.GroupKey[i]
			}
		}
		evMap := make(map[string]float64, len(evidence))
		for i, ev := range evidence {
			if i < len(g.Evidence) {
				evMap[ev.raw] = g.Evidence[i]
			}
		}
		out = append(out, Finding{
			RuleID:          rule.ID,
			Severity:        severity.String(),
			GroupKey:        groupMap,
			Support:         g.Support,
			Evidence:        evMap,
			Action:          rule.Action,
			Message:         substituteMessage(rule.Finding, groupMap, env),
			severity:        severity,
			groupKeyOrdered: g.GroupKey,
		})
	}
	return out, nil
}

// resolveMinSupport resolves the rule's min_support param to an int64 floor. An
// absent min_support means no floor (every positive-support class is reported).
func resolveMinSupport(rule *Rule, env Env) (int64, error) {
	if rule.MinSupport == nil || rule.MinSupport.Ref == "" {
		return 0, nil
	}
	v, ok := env[rule.MinSupport.Ref]
	if !ok {
		return 0, fmt.Errorf("rule %q references unresolved min_support param %q", rule.ID, rule.MinSupport.Ref)
	}
	if v.IsPartitioned() {
		return 0, fmt.Errorf("rule %q min_support param %q must be a scalar", rule.ID, rule.MinSupport.Ref)
	}
	return int64(v.Scalar), nil
}

func parseEvidence(ev *Evidence) ([]evidenceExpr, error) {
	if ev == nil {
		return nil, nil
	}
	out := make([]evidenceExpr, 0, len(ev.Report))
	for _, tok := range ev.Report {
		if tok == "count" {
			// count is always reported as Support; skip it as an explicit
			// evidence column.
			continue
		}
		ex, err := parseEvidenceExpr(tok)
		if err != nil {
			return nil, err
		}
		out = append(out, ex)
	}
	return out, nil
}

// substituteMessage replaces {column} placeholders with this class's group-key
// values and {param} placeholders with the runtime-resolved scalar (or, for a
// partition-keyed param, the value for this class's partition), so the operator
// sees concrete numbers. An unresolved placeholder is left as-is.
func substituteMessage(msg string, groupKey map[string]string, env Env) string {
	var sb strings.Builder
	for {
		open := strings.IndexByte(msg, '{')
		if open < 0 {
			sb.WriteString(msg)
			break
		}
		close := strings.IndexByte(msg[open:], '}')
		if close < 0 {
			sb.WriteString(msg)
			break
		}
		close += open
		sb.WriteString(msg[:open])
		name := msg[open+1 : close]
		sb.WriteString(resolvePlaceholder(name, groupKey, env))
		msg = msg[close+1:]
	}
	return sb.String()
}

func resolvePlaceholder(name string, groupKey map[string]string, env Env) string {
	if v, ok := groupKey[name]; ok {
		return v
	}
	if v, ok := env[name]; ok {
		if v.IsPartitioned() {
			// A partition-keyed param's value depends on this class's
			// partition. The catalog's partitioned params partition by a
			// group_by column (language or shape_id), so the partition key is
			// one of this class's group-key values; try each until one hits.
			for _, gv := range groupKey {
				if pv, ok := v.Partition[gv]; ok {
					return formatNumeric(pv)
				}
			}
			return "{" + name + "}"
		}
		return formatNumeric(v.Scalar)
	}
	return "{" + name + "}"
}
