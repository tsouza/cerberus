package chclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/tsouza/cerberus/internal/chopt"
)

// SettingUseQueryConditionCache is ClickHouse's query-condition-cache switch.
// The engine stamps it to 1 on predicate-stable reads when the condition_cache
// feature resolves in; the client stamps it to 0 on EVERY data-plane query
// while the override is chopt.DefectGated.
const SettingUseQueryConditionCache = "use_query_condition_cache"

// ErrUnsafeMixedFleet is the sentinel a refused dispatch wraps
// (UnsafeMixedFleetError): the fleet mixes a ClickHouse build that returns
// wrong rows for the query unless a setting switches the defective mechanism
// off, with a build too old to accept that setting (UNKNOWN_SETTING). Neither
// sending the setting nor omitting it is safe on such a fleet, so the query is
// refused until the upgrade finishes.
var ErrUnsafeMixedFleet = errors.New("chclient: the ClickHouse fleet mixes a build that needs a defect-gate setting with a build too old to accept it")

// UnsafeMixedFleetError is a refused dispatch: Reason names the defect, the
// setting and the remedy. It unwraps to ErrUnsafeMixedFleet.
type UnsafeMixedFleetError struct {
	Reason string
}

func (e *UnsafeMixedFleetError) Error() string {
	return ErrUnsafeMixedFleet.Error() + ": " + e.Reason
}

// Unwrap returns ErrUnsafeMixedFleet.
func (e *UnsafeMixedFleetError) Unwrap() error { return ErrUnsafeMixedFleet }

// SetQueryConditionCacheOverride installs the client-wide condition-cache
// override. Under chopt.DefectGated every query dispatched through
// queryContext carries use_query_condition_cache=0, applied after the
// per-query settings so no plan-shape rule can re-enable the cache. Under
// chopt.DefectRefused every data-plane dispatch fails with
// ErrUnsafeMixedFleet before it reaches ClickHouse. chopt.DefectAbsent lifts
// it.
//
// Leaving the setting unstamped is not enough to keep a query off the cache:
// ClickHouse enables it by default (and a server profile may too), so a query
// that merely omits it still reads — and writes — cached granule verdicts.
// cmd/cerberus gates it whenever a node its fleet probe reaches runs a build
// inside chopt.FeatureConditionCache's UnsafeBuilds, where a cached verdict
// written by another query can make a later read silently drop rows, and
// re-evaluates it on every capability re-probe. The setting does not exist
// below chopt.QueryConditionCacheSettingSince, and a server that does not know
// it rejects the whole query, so a fleet that also reaches such a node is
// refused instead (chopt.FleetDefectGate).
//
// The switch is shared by every ForHead view of the client.
func (c *Client) SetQueryConditionCacheOverride(g chopt.DefectGate) {
	c.conditionCacheOverride.Store(uint32(g))
}

// QueryConditionCacheOverride reports the override in force. A Client built
// without New (a zero value in a unit test) has no switch and reports
// chopt.DefectAbsent.
func (c *Client) QueryConditionCacheOverride() chopt.DefectGate {
	if c.conditionCacheOverride == nil {
		return chopt.DefectAbsent
	}
	switch c.conditionCacheOverride.Load() {
	case uint32(chopt.DefectGated):
		return chopt.DefectGated
	case uint32(chopt.DefectRefused):
		return chopt.DefectRefused
	default:
		return chopt.DefectAbsent
	}
}

// dispatchRefusalKey carries the reason the engine refuses one query.
type dispatchRefusalKey struct{}

// WithDispatchRefusal marks ctx's query as refused: its dispatch fails with
// ErrUnsafeMixedFleet and reason before reaching ClickHouse. The engine sets
// it on a query of a shape a shape defect refuses on the fleet in force.
func WithDispatchRefusal(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, dispatchRefusalKey{}, reason)
}

// DispatchRefusal returns the reason WithDispatchRefusal put on ctx, if any.
func DispatchRefusal(ctx context.Context) (string, bool) {
	reason, ok := ctx.Value(dispatchRefusalKey{}).(string)
	return reason, ok
}

// admitDispatch is the refusal check every data-plane dispatch passes before
// the fan-out admission gate: a query the engine refused, or any query while
// the condition-cache override is chopt.DefectRefused.
func (c *Client) admitDispatch(ctx context.Context) error {
	if reason, ok := DispatchRefusal(ctx); ok {
		return &UnsafeMixedFleetError{Reason: reason}
	}
	if c.QueryConditionCacheOverride() == chopt.DefectRefused {
		return &UnsafeMixedFleetError{Reason: fmt.Sprintf(
			"a reachable node runs a known-unsafe condition-cache build that needs %s=0 on every query, and another rejects that setting; upgrade every node to %s or later",
			SettingUseQueryConditionCache, chopt.QueryConditionCacheSettingSince,
		)}
	}
	return nil
}
