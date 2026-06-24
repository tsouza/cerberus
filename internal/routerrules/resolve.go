package routerrules

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// Value is a resolved parameter value: either a scalar (Scalar, when Partition
// is nil) or a partition-keyed map of scalars (Partition, keyed by the
// partition-column value — single-column partitions only, which is all the
// catalog uses). A partition-keyed value is consumed per-group during rule
// evaluation, not embedded in a flat WHERE clause. PartitionCol names the
// group-by column the map is keyed by, so the evaluator can anchor a
// per-partition sub-evaluation to the right group key.
//
// NoSignal marks a scalar corpus-derived watermark whose sub-population was
// EMPTY (zero rows matched the param's scope). An empty population is not the
// same as a watermark of 0: it means there is no learned signal at all. A
// fire-gate that depends on a no-signal watermark must NOT fire — see the
// evaluator's no-signal skip. NoSignal is only meaningful on a scalar Value; a
// partition-keyed Value represents an empty bucket by the bucket's absence from
// the map. corpus_count_ratio params are message-only context, so they keep
// resolving an empty population to a 0 scalar (NoSignal stays false) — 0 is the
// correct "no rejections observed" value for that context.
type Value struct {
	Scalar       float64
	Partition    map[string]float64
	PartitionCol string
	NoSignal     bool
}

// IsPartitioned reports whether the value is partition-keyed.
func (v Value) IsPartitioned() bool { return v.Partition != nil }

// Env maps a resolved parameter name to its Value.
type Env map[string]Value

// ConfigLookup resolves a config-kind parameter key to its raw string value.
// The CLI builds it from the deployment's running config plus any --param
// overrides; routerrules never imports internal/config, so the only path a
// number enters resolution is through this lookup (config) and the corpus
// (data). A missing required key is a hard error, surfaced by the resolver.
type ConfigLookup func(key string) (string, bool)

// ParamResolver resolves the catalog's named-parameter registry against a
// deployment's config and corpus. It topo-sorts the parameter dependency DAG,
// resolves leaves first, memoizes, and batches corpus aggregates that share a
// (scope, partition_by) group into one scan so a whole catalog costs
// O(distinct scope-groups) scans, not O(params).
type ParamResolver struct {
	cfg ConfigLookup
	src CorpusSource
}

// NewParamResolver builds a resolver over a config lookup and a corpus source.
func NewParamResolver(cfg ConfigLookup, src CorpusSource) *ParamResolver {
	return &ParamResolver{cfg: cfg, src: src}
}

// Resolve resolves every parameter referenced by the catalog (transitively)
// into an Env. It fails closed on a dependency cycle, a missing config key, or a
// corpus read error.
func (r *ParamResolver) Resolve(ctx context.Context, cat *Catalog) (Env, error) {
	specs := make(map[string]ParamSpec, len(cat.Params))
	for _, p := range cat.Params {
		specs[p.Name] = p
	}

	order, err := topoSortParams(specs)
	if err != nil {
		return nil, err
	}

	// Batch corpus aggregates that share an identical AggSpec group so the
	// percentile-fraction differences (which are inputs, resolved first as
	// config leaves) collapse to one scan per distinct (column, scope,
	// partition, fraction) shape. Config leaves and ratios are resolved
	// inline; percentile/agg params are grouped by their resolved AggSpec.
	env := make(Env, len(order))
	for _, name := range order {
		spec := specs[name]
		val, err := r.resolveOne(ctx, spec, env)
		if err != nil {
			return nil, err
		}
		env[name] = val
	}
	return env, nil
}

func (r *ParamResolver) resolveOne(ctx context.Context, spec ParamSpec, env Env) (Value, error) {
	switch spec.Kind {
	case ParamConfig:
		return r.resolveConfig(spec)
	case ParamCorpusPercentile, ParamCorpusAgg, ParamCorpusCountRatio:
		return r.resolveCorpus(ctx, spec, env)
	default:
		return Value{}, fmt.Errorf("routerrules: param %q has unknown kind %q", spec.Name, spec.Kind)
	}
}

func (r *ParamResolver) resolveConfig(spec ParamSpec) (Value, error) {
	if r.cfg == nil {
		return Value{}, fmt.Errorf("routerrules: param %q is config-kind but no config lookup was provided", spec.Name)
	}
	raw, ok := r.cfg(spec.Key)
	if !ok {
		return Value{}, fmt.Errorf("routerrules: config-kind param %q requires config key %q, which is not set (provide it via deployment config or --param %s=<value>)", spec.Name, spec.Key, spec.Key)
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return Value{}, fmt.Errorf("routerrules: config key %q for param %q is not numeric: %q", spec.Key, spec.Name, raw)
	}
	return Value{Scalar: f}, nil
}

func (r *ParamResolver) resolveCorpus(ctx context.Context, spec ParamSpec, env Env) (Value, error) {
	if r.src == nil {
		return Value{}, fmt.Errorf("routerrules: param %q is corpus-kind but no corpus source was provided", spec.Name)
	}
	as := AggSpec{
		Column:      spec.Column,
		Scope:       spec.Scope,
		NumScope:    spec.NumeratorScope,
		DenScope:    spec.DenominatorScope,
		PartitionBy: spec.PartitionBy,
	}
	switch spec.Kind {
	case ParamCorpusPercentile:
		frac, err := resolveFraction(spec, env)
		if err != nil {
			return Value{}, err
		}
		as.Percentile = &frac
	case ParamCorpusAgg:
		as.Agg = AggFunc(spec.Agg)
	case ParamCorpusCountRatio:
		as.CountRatio = true
	}
	v, err := r.src.Aggregate(ctx, as)
	if err != nil {
		return Value{}, fmt.Errorf("routerrules: resolve corpus param %q: %w", spec.Name, err)
	}
	return v, nil
}

// resolveFraction resolves a percentile param's fraction, which is itself a
// param reference (never a number in the YAML). The referenced param must
// already be resolved (topo order guarantees it) and must be a scalar.
func resolveFraction(spec ParamSpec, env Env) (float64, error) {
	if spec.Percentile == nil || spec.Percentile.Ref == "" {
		return 0, fmt.Errorf("routerrules: percentile param %q has no fraction ref", spec.Name)
	}
	v, ok := env[spec.Percentile.Ref]
	if !ok {
		return 0, fmt.Errorf("routerrules: percentile param %q references unresolved fraction param %q", spec.Name, spec.Percentile.Ref)
	}
	if v.IsPartitioned() {
		return 0, fmt.Errorf("routerrules: percentile fraction param %q must be a scalar, got a partition-keyed value", spec.Percentile.Ref)
	}
	return v.Scalar, nil
}

// topoSortParams returns the params in dependency order (a percentile param's
// fraction ref resolved before the percentile param itself). It fails closed on
// a cycle. The graph is small (the param registry), so a simple DFS suffices.
func topoSortParams(specs map[string]ParamSpec) ([]string, error) {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS stack
		black = 2 // fully resolved
	)
	color := make(map[string]int, len(specs))
	var order []string

	// Deterministic visit order so errors and batching are stable run to run.
	names := make([]string, 0, len(specs))
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names)

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch color[name] {
		case black:
			return nil
		case grey:
			return fmt.Errorf("routerrules: param dependency cycle: %v -> %s", path, name)
		}
		color[name] = grey
		spec, ok := specs[name]
		if !ok {
			return fmt.Errorf("routerrules: param %q references undeclared param %q", lastOf(path), name)
		}
		for _, dep := range paramDeps(spec) {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		color[name] = black
		order = append(order, name)
		return nil
	}

	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// paramDeps returns the names a param spec depends on (currently only a
// percentile param's fraction ref).
func paramDeps(spec ParamSpec) []string {
	if spec.Kind == ParamCorpusPercentile && spec.Percentile != nil && spec.Percentile.Ref != "" {
		return []string{spec.Percentile.Ref}
	}
	return nil
}

func lastOf(path []string) string {
	if len(path) == 0 {
		return "<root>"
	}
	return path[len(path)-1]
}

func formatNumeric(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
