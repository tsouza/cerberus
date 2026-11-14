package chclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chopt"
)

// ProbeVersion issues SELECT version() once and parses the result into a
// comparable chopt.Version (major.minor.patch.build), for a caller that needs
// the build of the one server a client reaches (the test helpers that resolve
// a set against a live server). The fleet probe reads the build together with
// the server's identity instead (ProbeNode). The parse is canonical in
// internal/chopt (chopt.ParseVersion); chclient does not re-implement it.
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

// nodeSQL reads the identity and build of the server a connection reached.
// serverUUID() is the UUID the server generated on first start and keeps in its
// data directory, so it names one server across restarts and builds and tells
// apart servers that share a host name.
const nodeSQL = `SELECT toString(serverUUID()), version()`

// clusterNodesSQL reads the identity and build of every replica of one
// cluster. clusterAllReplicas fails the statement when a replica cannot be
// reached, so a success reports every replica.
const clusterNodesSQL = `SELECT DISTINCT toString(serverUUID()), version() FROM clusterAllReplicas(?, system.one)`

// ProbeNode reads the identity and build of the server the connection
// reached, the build parsed like ProbeVersion. The fleet probe runs it on
// every configured address.
func (c *Client) ProbeNode(ctx context.Context) (FleetNode, error) {
	nodes, err := c.probeNodes(ctx, nodeSQL)
	if err != nil {
		return FleetNode{}, fmt.Errorf("probe clickhouse node: %w", err)
	}
	return nodes[0], nil
}

// ProbeClusterNodes returns the identity and build of every replica of
// cluster, the builds parsed like ProbeVersion. It fails — rather than
// reporting a partial list — when any replica cannot be reached or reports an
// unparseable version.
func (c *Client) ProbeClusterNodes(ctx context.Context, cluster string) ([]FleetNode, error) {
	nodes, err := c.probeNodes(ctx, clusterNodesSQL, cluster)
	if err != nil {
		return nil, fmt.Errorf("probe clickhouse nodes across cluster %q: %w", cluster, err)
	}
	return nodes, nil
}

// probeNodes runs a (serverUUID, version) statement and parses every row,
// failing on an empty result or an unparseable version.
func (c *Client) probeNodes(ctx context.Context, sql string, args ...any) ([]FleetNode, error) {
	type row struct{ id, version string }
	rows, err := queryScanRows(ctx, c, sql, args, func(rows driver.Rows) (row, error) {
		var r row
		err := rows.Scan(&r.id, &r.version)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("empty result")
	}
	out := make([]FleetNode, 0, len(rows))
	for _, r := range rows {
		v, ok := chopt.ParseVersion(r.version)
		if !ok {
			return nil, fmt.Errorf("unparseable version %q", strings.TrimSpace(r.version))
		}
		out = append(out, FleetNode{ID: r.id, Version: v})
	}
	return out, nil
}
