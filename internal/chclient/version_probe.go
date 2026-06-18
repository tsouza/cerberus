package chclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chopt"
)

// ProbeVersion issues SELECT version() once and parses the result into a
// comparable major.minor chopt.Version. It is the single runtime version probe
// the optimization auto-picker resolves against: cmd/cerberus calls it after
// building the client, then hands the result straight to chopt.Resolve.
//
// The major.minor parse is canonical in internal/chopt (chopt.ParseVersion);
// chclient does not re-implement it. Patch and build suffixes are dropped:
// ClickHouse feature availability lands at minor-version granularity, so the
// auto-picker compares over (major, minor) only.
//
// The probe runs ONCE. A rolling ClickHouse upgrade that crosses a feature
// floor needs a cerberus restart/reconnect to re-probe and re-resolve; that is
// the documented v1 behaviour (see docs/clickhouse-optimizations.md).
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
