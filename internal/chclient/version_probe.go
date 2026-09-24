package chclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chopt"
)

// ProbeVersion issues SELECT version() once and parses the result into a
// comparable chopt.Version (major.minor.patch.build). It is the runtime version
// probe the optimization auto-picker resolves against: cmd/cerberus calls it at
// boot and on every capability re-probe, then hands the result to
// chopt.Resolve. The parse is canonical in internal/chopt (chopt.ParseVersion);
// chclient does not re-implement it.
//
// It reuses the breaker-guarded QueryStrings read surface, so a probe against
// an unreachable server fails like any other read rather than hanging.
func (c *Client) ProbeVersion(ctx context.Context) (chopt.Version, error) {
	rows, err := c.QueryStrings(ctx, "SELECT version()")
	if err != nil {
		return chopt.Version{}, fmt.Errorf("probe clickhouse version: %w", err)
	}
	if len(rows) == 0 {
		return chopt.Version{}, fmt.Errorf("probe clickhouse version: empty result")
	}
	v, ok := chopt.ParseVersion(rows[0])
	if !ok {
		return chopt.Version{}, fmt.Errorf("probe clickhouse version: unparseable %q", strings.TrimSpace(rows[0]))
	}
	return v, nil
}

// clusterVersionsSQL lists the distinct build of every replica of one cluster.
// clusterAllReplicas fails the statement when a replica cannot be reached, so
// a success reports every replica.
const clusterVersionsSQL = `SELECT DISTINCT version() FROM clusterAllReplicas(?, system.one)`

// ProbeClusterVersions returns the distinct builds every replica of cluster
// runs, parsed like ProbeVersion. It fails — rather than reporting a partial
// list — when any replica cannot be reached or reports an unparseable version.
func (c *Client) ProbeClusterVersions(ctx context.Context, cluster string) ([]chopt.Version, error) {
	rows, err := c.QueryStrings(ctx, clusterVersionsSQL, cluster)
	if err != nil {
		return nil, fmt.Errorf("probe clickhouse versions across cluster %q: %w", cluster, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("probe clickhouse versions across cluster %q: empty result", cluster)
	}
	out := make([]chopt.Version, 0, len(rows))
	for _, raw := range rows {
		v, ok := chopt.ParseVersion(raw)
		if !ok {
			return nil, fmt.Errorf("probe clickhouse versions across cluster %q: unparseable %q", cluster, strings.TrimSpace(raw))
		}
		out = append(out, v)
	}
	return out, nil
}
