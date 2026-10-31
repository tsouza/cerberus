//go:build integration

package chserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// codeUnknownSetting is ClickHouse's UNKNOWN_SETTING error code.
const codeUnknownSetting = 115

// clickHouseRejected is the client-facing text of a query ClickHouse
// rejected; the server's own message stays in cerberus's log.
const clickHouseRejected = "ClickHouse rejected the query"

// gateSettings are the settings the defect gates stamp, each with the first
// release chopt records as accepting it.
var gateSettings = []struct {
	name  string
	since chopt.Version
}{
	{chclient.SettingUseQueryConditionCache, chopt.QueryConditionCacheSettingSince},
	{settingOptimizeUseProjectionFiltering, chopt.ProjectionFilteringSettingSince},
}

// settingFloorBuilds straddle both floors: the supported floor (24.8)
// predates both settings, 25.3 accepts use_query_condition_cache only, and
// 25.10 accepts both.
var settingFloorBuilds = []string{
	"clickhouse/clickhouse-server:24.8.14.39-alpine",
	"clickhouse/clickhouse-server:25.3.14.14-alpine",
	"clickhouse/clickhouse-server:25.10.7.6-alpine",
}

// floorFleetAffectedBuild is the build a mixed fleet pairs with the supported
// floor: a 26.2 build, which carries ClickHouse#119168 and sits inside
// condition_cache's known-unsafe ranges.
var floorFleetAffectedBuild = chopt.Version{Major: 26, Minor: 2, Patch: 19, Build: 43}

// TestShapeDefectGates_SettingFloors pins the setting floors the fleet gate
// decisions rest on, the way production sends a setting: through
// chclient.WithQuerySetting over the native protocol. A build below a floor
// rejects the whole query with UNKNOWN_SETTING, rather than ignoring the
// setting, and a build at or above it accepts it.
func TestShapeDefectGates_SettingFloors(t *testing.T) {
	for _, image := range settingFloorBuilds {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s := startServer(ctx, t, image)
			c := s.client(t, adminUser, adminPassword, chclient.Config{})
			for _, setting := range gateSettings {
				_, err := c.QueryStrings(chclient.WithQuerySetting(ctx, setting.name, 0), "SELECT toString(1)")
				wantRejected := s.version.Less(setting.since)
				if got := unknownSetting(err); got != wantRejected {
					t.Errorf("%s=0 on %s: err = %v; want rejected with UNKNOWN_SETTING = %v (floor %s)",
						setting.name, s.version, err, wantRejected, setting.since)
				}
				if !wantRejected && err != nil {
					t.Errorf("%s=0 on %s: %v", setting.name, s.version, err)
				}
			}
		})
	}
}

// TestShapeDefectGates_MixedFleetRefusesTheGatedShape is the production
// wiring on the supported floor (24.8), as one node of a fleet whose other
// node runs an affected 26.2 build. Stamping either gate's setting, as a gated
// decision would, fails the query on this node with UNKNOWN_SETTING; so the
// fleet decision refuses instead. The refused shape — a LogQL numeric label
// filter, ClickHouse#119168's — fails with chclient.ErrUnsafeMixedFleet before
// reaching ClickHouse, a query of any other shape answers, and the refused
// client-wide condition-cache override refuses every query.
func TestShapeDefectGates_MixedFleetRefusesTheGatedShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := startServer(ctx, t, settingFloorBuilds[0])
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	seedConversionProbe(ctx, t, s)

	fleet := []chopt.Version{s.version, floorFleetAffectedBuild}
	set := chopttest.ResolveEnabledSet(ctx, t, s.admin, chopt.SelectionAuto)
	set = set.WithShapeDefects(fleetShapeDefects(fleet))
	if got := set.ShapeDefectGate(chopt.ShapeDefectConversionConditionCache); got != chopt.DefectRefused {
		t.Fatalf("premise: %s on the fleet %v = %d; want refused", chopt.ShapeDefectConversionConditionCache, fleet, got)
	}
	override := chopt.FleetDefectGate(fleet, true, func(v chopt.Version) bool {
		return chopt.KnownUnsafe(chopt.FeatureConditionCache, v)
	}, chopt.QueryConditionCacheSettingSince, chopt.DefectAbsent)
	if override != chopt.DefectRefused {
		t.Fatalf("premise: the condition-cache override on the fleet %v = %d; want refused", fleet, override)
	}
	rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	conversionQuery := conversionProbeQueries[0]

	// The gated decision's stamps are what the old node cannot take.
	gatedSet := set.WithShapeDefects(map[string]chopt.DefectGate{chopt.ShapeDefectConversionConditionCache: chopt.DefectGated})
	gatedRules := choptwire.SettingsRules(gatedSet, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	gated := newProbeMux(t, s.client(t, adminUser, adminPassword, chclient.Config{}), gatedRules)
	if code, body := gated.logQuery(ctx, t, conversionQuery); code == http.StatusOK || !strings.Contains(body, clickHouseRejected) {
		t.Errorf("gated stamp on %s: HTTP %d %s; want ClickHouse to reject the query", s.version, code, body)
	}
	gatedOverrideClient := s.client(t, adminUser, adminPassword, chclient.Config{})
	gatedOverrideClient.SetQueryConditionCacheOverride(chopt.DefectGated)
	if code, body := newProbeMux(t, gatedOverrideClient, rules).logQuery(ctx, t, conversionProbeLineFilterQuery); code == http.StatusOK || !strings.Contains(body, clickHouseRejected) {
		t.Errorf("gated override on %s: HTTP %d %s; want ClickHouse to reject the query", s.version, code, body)
	}

	// The refused decision: the conversion shape is refused, the rest answers.
	prodClient := s.client(t, adminUser, adminPassword, chclient.Config{})
	prod := newProbeMux(t, prodClient, rules)
	code, body := prod.logQuery(ctx, t, conversionQuery)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, chclient.ErrUnsafeMixedFleet.Error()) || !strings.Contains(body, chopt.ShapeDefectConversionConditionCache) {
		t.Errorf("refused shape: HTTP %d %s; want 503 naming %s and %q", code, body, chopt.ShapeDefectConversionConditionCache, chclient.ErrUnsafeMixedFleet)
	}
	if got := prod.logCount(ctx, t, conversionProbeLineFilterQuery, -1); got != conversionProbeLogs {
		t.Errorf("other shape on a refused fleet: count = %v; want %d", got, conversionProbeLogs)
	}

	// The refused override: every query is refused before dispatch.
	prodClient.SetQueryConditionCacheOverride(override)
	if code, body := prod.logQuery(ctx, t, conversionProbeLineFilterQuery); code != http.StatusServiceUnavailable || !strings.Contains(body, chclient.SettingUseQueryConditionCache) {
		t.Errorf("refused override: HTTP %d %s; want every query refused 503 naming %s", code, body, chclient.SettingUseQueryConditionCache)
	}
	if _, err := prodClient.QueryStrings(ctx, "SELECT toString(1)"); !errors.Is(err, chclient.ErrUnsafeMixedFleet) {
		t.Errorf("refused override, direct read: err = %v; want ErrUnsafeMixedFleet", err)
	}
}

// fleetShapeDefects is the shape-defect decision cmd/cerberus takes on a
// complete probe of fleet (shapeDefectsOnFleet).
func fleetShapeDefects(fleet []chopt.Version) map[string]chopt.DefectGate {
	out := map[string]chopt.DefectGate{}
	for _, d := range chopt.ShapeDefects() {
		affected := func(v chopt.Version) bool { return chopt.KnownShapeDefect(d.ID, v) }
		out[d.ID] = chopt.FleetDefectGate(fleet, true, affected, d.SettingSince, chopt.DefectAbsent)
	}
	return out
}

// logQuery runs a LogQL instant query at the probe window's end and returns
// the HTTP status and body, for a query expected to fail.
func (p probeMux) logQuery(ctx context.Context, t *testing.T, q string) (int, string) {
	t.Helper()
	params := url.Values{}
	params.Set("query", q)
	params.Set("time", strconv.FormatInt(probeWindowEnd.Unix(), 10))
	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/query?"+params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	p.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// unknownSetting reports whether err is ClickHouse's UNKNOWN_SETTING.
func unknownSetting(err error) bool {
	var ex *clickhouse.Exception
	return errors.As(err, &ex) && ex.Code == codeUnknownSetting
}
