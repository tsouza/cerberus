package spec

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

const (
	probeQueryMemoryCap       = int64(1 << 30)
	probeResultCacheIngestLag = 5 * time.Minute
	probeResultCacheTTL       = 5 * time.Minute
)

// FormatQuerySettings snapshots eligibility with every optional plan-dependent
// rule enabled. This is a deterministic capability profile, not a claim that a
// deployed server supports every setting. These settings are recorded only;
// RunRoundTripSQL retains its own floor-compatible execution configuration.
func FormatQuerySettings(plan chplan.Node) string {
	probe := engine.ProbeQuerySettings(plan, engine.SettingsRules{
		OptimizeAggregationInOrder: true,
		ConditionCache:             true,
		TraceIDBitmapFilter:        true,
		LogCommentShape:            true,
		ResultCache:                true,
		LazyMaterialization:        true,
		JoinSpill:                  true,
		ExpHistogramTwoLevel:       true,
		ResultCacheIngestLag:       probeResultCacheIngestLag,
		ResultCacheTTL:             probeResultCacheTTL,
		Now:                        func() time.Time { return time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC) },
		Metrics:                    schema.DefaultOTelMetrics(),
		Logs:                       schema.DefaultOTelLogs(),
		Traces:                     schema.DefaultOTelTraces(),
	}, probeQueryMemoryCap)
	sort.Strings(probe.EnabledOpts)
	lines := []string{"enabled_opts=" + strings.Join(probe.EnabledOpts, ",")}
	for key, value := range probe.Settings {
		lines = append(lines, fmt.Sprintf("%s=%v", key, value))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}
