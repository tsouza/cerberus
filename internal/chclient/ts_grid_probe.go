package chclient

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// tsGridCapabilityProbeSQL is the canary body. It is deliberately a trivial
// `SELECT 1` and NOT a call to a native timeSeries*ToGrid aggregate: the canary
// only needs to learn whether the server will ACCEPT the experimental setting
// cerberus stamps on native queries, and that verdict is decided when ClickHouse
// applies the per-query settings map (constraint / readonly enforcement), BEFORE
// the query body is analysed. Stamping the setting over `SELECT 1` therefore
// isolates exactly the FORBIDDEN signal with zero dependence on getting an
// aggregate's argument types right -- a type slip in a real-aggregate canary
// would misclassify a permitting server as not-capable and silently lose the
// optimization. The setting itself is added via WithTSGridSetting, the SAME
// production carrier the engine uses, so the probe rides the identical
// settings-map path a real native query does.
const tsGridCapabilityProbeSQL = "SELECT 1"

// ProbeTSGridCapability runs the boot-time capability canary once and classifies
// whether the connected ClickHouse server will run the experimental
// timeSeries*ToGrid aggregate family. It mirrors ProbeVersion: cmd/cerberus
// calls it over the bootstrap/default-DB connection after building the client,
// then hands the verdict straight to chopt.Resolve as Config.Capability.
//
// The probe stamps allow_experimental_time_series_aggregate_functions=1 (via
// WithTSGridSetting) on a trivial query. The server's response is mapped to a
// tri-state:
//
//   - success                         -> CapabilityAvailable
//   - typed server rejection          -> CapabilityForbidden (constrained /
//     readonly profile refused the setting; SETTING_CONSTRAINT_VIOLATION 452 or
//     READONLY 164 are the canonical codes, but any typed answer to `SELECT 1`
//     means the server refused the stamped setting)
//   - transport / connectivity failure -> CapabilityUnreachable (conservative:
//     do NOT enable native, matching the version probe's connectivity fallback)
//
// It never returns an error: a probe failure is a verdict (Forbidden /
// Unreachable), not a fatal. The resolver decides what an inconclusive verdict
// means per selection mode (auto silently falls back, an explicit list errors
// under enforcing).
func (c *Client) ProbeTSGridCapability(ctx context.Context) chopt.Capability {
	ctx = WithTSGridSetting(ctx)
	_, err := c.QueryStrings(ctx, tsGridCapabilityProbeSQL)
	return classifyTSGridCapability(err)
}

// classifyTSGridCapability maps the canary's error into the tri-state verdict.
// A nil error is Available. A typed *clickhouse.Exception means the server
// ANSWERED with a rejection -- since the probe body is a trivial `SELECT 1`
// that cannot fail on its own, a server-side exception means it refused the
// stamped experimental setting, so it is Forbidden (the constraint 452 /
// readonly 164 codes are the expected shapes, and any other typed answer is
// treated the same: the server will not run the setting). Anything else is a
// transport failure with no server verdict -- Unreachable, the conservative
// state that leaves native off until a restart re-probes.
func classifyTSGridCapability(err error) chopt.Capability {
	if err == nil {
		return chopt.CapabilityAvailable
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		return chopt.CapabilityForbidden
	}
	return chopt.CapabilityUnreachable
}
