package main

import (
	"context"
	"log/slog"
	"slices"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// unsupportedRefusalRelease is the cerberus release that refuses to start
// against a ClickHouse server below chopt.MinSupportedVersion.
const unsupportedRefusalRelease = "v1.26.0"

// reportUnsupportedFleet reads one fleet probe pass for servers below the
// minimum supported ClickHouse version. It records how many nodes fleet holds
// below it on cerberus_clickhouse_unsupported_nodes, logs one WARN per
// unsupported build naming that build, the minimum and the node count, and
// returns the count with ok=true. A pass that holds no node records and logs
// nothing and returns ok=false, so the count in force stays.
//
// It decides nothing else: the optimization set still resolves against the
// fleet's oldest build, so cerberus emits for an unsupported server the SQL it
// would emit for that build anyway.
func reportUnsupportedFleet(ctx context.Context, logger *slog.Logger, fleet chclient.FleetVersions) (int, bool) {
	if len(fleet.Versions) == 0 {
		return 0, false
	}
	perBuild := make(map[chopt.Version]int)
	for _, v := range fleet.Versions {
		if v.Unsupported() {
			perBuild[v]++
		}
	}
	builds := make([]chopt.Version, 0, len(perBuild))
	total := 0
	for v, n := range perBuild {
		builds = append(builds, v)
		total += n
	}
	slices.SortFunc(builds, chopt.Version.Compare)
	telemetry.RecordClickHouseUnsupportedNodes(ctx, total)
	for _, v := range builds {
		logger.Warn(
			"clickhouse server is below the minimum supported version; servers below it are unsupported, "+
				"and cerberus "+unsupportedRefusalRelease+" refuses to start against them",
			"server_version", v.String(),
			"minimum_supported_version", chopt.MinSupportedVersion().String(),
			"nodes", perBuild[v],
		)
	}
	return total, true
}
