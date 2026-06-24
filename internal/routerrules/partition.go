package routerrules

import (
	"fmt"
	"sort"
)

// subEval is one scalar-bound evaluation of a rule: a condition whose params
// are all scalar in env, plus an optional restriction tying results to a single
// partition value (so per-partition sub-evaluations don't leak across
// partitions). The partition column is always a group_by column, so the
// restriction is a group-key match.
type subEval struct {
	cond Condition
	env  Env
	// restrictCol/restrictVal, when set, keep only GroupResults whose value for
	// restrictCol equals restrictVal. Empty when the rule references no
	// partitioned param.
	restrictCol string
	restrictVal string
	groupBy     []string
}

// restrict filters a backend's GroupResults to this sub-eval's partition.
func (s subEval) restrict(in []GroupResult) []GroupResult {
	if s.restrictCol == "" {
		return in
	}
	idx := indexOf(s.groupBy, s.restrictCol)
	if idx < 0 {
		return in
	}
	out := in[:0:0]
	for _, g := range in {
		if idx < len(g.GroupKey) && g.GroupKey[idx] == s.restrictVal {
			out = append(out, g)
		}
	}
	return out
}

// expandPartitioned splits a rule into per-partition sub-evaluations. If the
// condition references no partition-keyed param, it returns a single sub-eval
// with the original (already scalar-resolvable) env. Otherwise it requires that
// all partitioned params referenced share one partition column (the catalog's
// rules satisfy this), enumerates that column's values, and emits one
// scalar-bound sub-eval per value.
func expandPartitioned(cond Condition, groupBy []string, env Env) ([]subEval, error) {
	refs := map[string]struct{}{}
	cond.paramRefs(refs)

	partitionParams := make([]string, 0, len(refs))
	for name := range refs {
		v, ok := env[name]
		if ok && v.IsPartitioned() {
			partitionParams = append(partitionParams, name)
		}
	}
	if len(partitionParams) == 0 {
		return []subEval{{cond: cond, env: env, groupBy: groupBy}}, nil
	}
	sort.Strings(partitionParams)

	// Determine the partition column shared by every partitioned param. The
	// resolver keys partition maps by the single partition-column value, so the
	// column is the group_by column that is NOT an identity-only key. We rely on
	// the partition values themselves: every partitioned param must expose the
	// same key set, and that key set is the partition column's value domain.
	partitionCol, values, err := sharedPartition(partitionParams, env, groupBy)
	if err != nil {
		return nil, err
	}

	subs := make([]subEval, 0, len(values))
	for _, val := range values {
		scalarEnv := make(Env, len(env))
		for k, v := range env {
			if v.IsPartitioned() {
				if pv, ok := v.Partition[val]; ok {
					scalarEnv[k] = Value{Scalar: pv}
				}
				// A param without this partition value is omitted; a condition
				// that references it will surface a clear unresolved-param error
				// rather than silently matching.
				continue
			}
			scalarEnv[k] = v
		}
		subs = append(subs, subEval{
			cond:        cond,
			env:         scalarEnv,
			restrictCol: partitionCol,
			restrictVal: val,
			groupBy:     groupBy,
		})
	}
	return subs, nil
}

// sharedPartition verifies the partitioned params agree on one partition column
// and returns that column plus the sorted value domain. The partition column is
// the group_by column whose value set matches the params' partition-map key
// set.
func sharedPartition(params []string, env Env, groupBy []string) (string, []string, error) {
	// Collect the common key set across all partitioned params.
	var keySet map[string]struct{}
	for _, p := range params {
		ks := map[string]struct{}{}
		for k := range env[p].Partition {
			ks[k] = struct{}{}
		}
		if keySet == nil {
			keySet = ks
			continue
		}
		// Intersect: a value present in every partitioned param is safe to
		// scalar-bind for all of them in one sub-eval.
		for k := range keySet {
			if _, ok := ks[k]; !ok {
				delete(keySet, k)
			}
		}
	}

	// The partition column is carried on each resolved Value (PartitionCol). All
	// partitioned params in one rule must agree on it, and it must be a group_by
	// column so the per-partition results can be anchored to a group key.
	partitionCol := ""
	for _, p := range params {
		pc := env[p].PartitionCol
		if pc == "" {
			return "", nil, fmt.Errorf("routerrules: partitioned param %q has no partition column", p)
		}
		if partitionCol == "" {
			partitionCol = pc
		} else if partitionCol != pc {
			return "", nil, fmt.Errorf("routerrules: rule references partitioned params on differing columns (%q vs %q); split into separate rules", partitionCol, pc)
		}
	}
	if indexOf(groupBy, partitionCol) < 0 {
		return "", nil, fmt.Errorf("routerrules: partition column %q is not in the rule's group_by %v", partitionCol, groupBy)
	}

	values := make([]string, 0, len(keySet))
	for k := range keySet {
		values = append(values, k)
	}
	sort.Strings(values)
	return partitionCol, values, nil
}

func indexOf(xs []string, target string) int {
	for i, x := range xs {
		if x == target {
			return i
		}
	}
	return -1
}
