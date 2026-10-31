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

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// codeUnknownSetting is ClickHouse's UNKNOWN_SETTING error code.
const codeUnknownSetting = 115

// settingFloorBuilds straddle both gate settings' first accepting releases:
// the supported floor (24.8) predates both, 25.3 accepts
// use_query_condition_cache only, and 25.10 accepts both.
var settingFloorBuilds = []string{
	"clickhouse/clickhouse-server:24.8.14.39-alpine",
	"clickhouse/clickhouse-server:25.3.14.14-alpine",
	"clickhouse/clickhouse-server:25.10.7.6-alpine",
}

// mixedFleetAffectedImage is the node a mixed fleet pairs with the supported
// floor: a 26.3 LTS build carrying ClickHouse#119168 and outside
// condition_cache's known-unsafe ranges, so only the conversion shape is
// gated.
const mixedFleetAffectedImage = "clickhouse/clickhouse-server:26.3.17.56-alpine"

// TestShapeDefectGates_SettingFloors pins chopt's gate-setting floors, the way
// production sends a setting: through chclient.WithQuerySetting over the
// native protocol. A build below a floor does not ignore the setting: it
// rejects the whole query with UNKNOWN_SETTING, which chclient reports as a
// *GateSettingUnsupportedError for that setting. A build at or above the floor
// accepts it.
func TestShapeDefectGates_SettingFloors(t *testing.T) {
	for _, image := range settingFloorBuilds {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s := startServer(ctx, t, image)
			c := s.client(t, adminUser, adminPassword, chclient.Config{})
			for _, g := range chopt.GateSettings() {
				_, err := c.QueryStrings(chclient.WithQuerySetting(ctx, g.Name, 0), "SELECT toString(1)")
				if !s.version.Less(g.Since) {
					if err != nil {
						t.Errorf("%s=0 on %s (at or above %s): %v", g.Name, s.version, g.Since, err)
					}
					continue
				}
				if code := exceptionCode(err); code != codeUnknownSetting {
					t.Errorf("%s=0 on %s (below %s): err = %v, code %d; want UNKNOWN_SETTING", g.Name, s.version, g.Since, err, code)
				}
				var gate *chclient.GateSettingUnsupportedError
				if !errors.As(err, &gate) || gate.Gate.Name != g.Name {
					t.Errorf("%s=0 on %s: err = %v; want a *chclient.GateSettingUnsupportedError for %s", g.Name, s.version, err, g.Name)
				}
			}
		})
	}
}

// TestShapeDefectGates_MixedFleetNeverAnswersWrong is the production wiring on
// a fleet mixing the supported floor (24.8) with a node carrying
// ClickHouse#119168. Both nodes get the same settings rules — the fleet
// decision puts the defect in force, as one affected node does — and so the
// same use_query_condition_cache=0 on the conversion shape, a LogQL numeric
// label filter:
//
//   - the affected node answers it correctly through a cache another session
//     poisoned, where the same query without the gate is poisoned;
//   - the 24.8 node, which predates the setting and the defect, answers it
//     with HTTP 502 naming the setting, the defects and the release to
//     upgrade to, never with rows;
//   - a query of an ungated shape answers correctly on both nodes;
//   - the client-wide condition-cache override, which stamps the setting on
//     every query, gets the same named error from the 24.8 node.
func TestShapeDefectGates_MixedFleetNeverAnswersWrong(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	old := startServer(ctx, t, settingFloorBuilds[0])
	affected := startServer(ctx, t, mixedFleetAffectedImage)
	for _, s := range []*server{old, affected} {
		if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
			t.Fatalf("%s: apply DDL: %v", s.image, err)
		}
		seedConversionProbe(ctx, t, s)
	}

	// The fleet decision cmd/cerberus takes: ids resolved against the oldest
	// node, shape defects carried by any node.
	set := chopttest.ResolveEnabledSet(ctx, t, old.admin, chopt.SelectionAuto)
	var defects []string
	for _, d := range chopt.ShapeDefects() {
		if chopt.KnownShapeDefect(d.ID, old.version) || chopt.KnownShapeDefect(d.ID, affected.version) {
			defects = append(defects, d.ID)
		}
	}
	set = set.WithShapeDefects(defects)
	if !set.HasShapeDefect(chopt.ShapeDefectConversionConditionCache) {
		t.Fatalf("premise: the fleet %s + %s must carry %s", old.version, affected.version, chopt.ShapeDefectConversionConditionCache)
	}
	if chopt.KnownUnsafe(chopt.FeatureConditionCache, old.version) || chopt.KnownUnsafe(chopt.FeatureConditionCache, affected.version) {
		t.Fatalf("premise: neither %s nor %s may engage the client-wide override", old.version, affected.version)
	}
	rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
	if rules.ResultCache {
		t.Fatal("the resolved set enabled result_cache; every probe answer must be computed, not served from cache")
	}
	gate, ok := chopt.GateSettingByName(chopt.SettingUseQueryConditionCache)
	if !ok {
		t.Fatal("premise: use_query_condition_cache is a gate setting")
	}

	// --- the affected node: the gated shape answers correctly ---
	raw := newProbeMux(t, affected.client(t, adminUser, adminPassword, chclient.Config{}), engine.SettingsRules{})
	truth := raw.conversionTruths(ctx, t, affected)
	if !raw.conversionPoisoned(ctx, t, affected, truth, 1) {
		t.Fatalf("premise: %s must poison the conversion shape with the cache on", affected.version)
	}
	onAffected := newProbeMux(t, affected.client(t, adminUser, adminPassword, chclient.Config{}), rules)
	if onAffected.conversionPoisoned(ctx, t, affected, truth, -1) {
		t.Errorf("affected node %s: the gated conversion shape answered from a poisoned cache", affected.version)
	}
	if got := onAffected.logCount(ctx, t, conversionProbeLineFilterQuery, -1); got != conversionProbeLogs {
		t.Errorf("affected node %s: ungated line-filter count = %v; want %d", affected.version, got, conversionProbeLogs)
	}

	// --- the 24.8 node: the gated shape is a named error, never rows ---
	onOld := newProbeMux(t, old.client(t, adminUser, adminPassword, chclient.Config{}), rules)
	for _, q := range conversionProbeQueries {
		code, body := onOld.logQuery(ctx, t, q)
		assertGateSettingError(t, "24.8 node, gated shape "+q, code, body, gate)
	}
	if got := onOld.logCount(ctx, t, conversionProbeLineFilterQuery, -1); got != conversionProbeLogs {
		t.Errorf("24.8 node %s: ungated line-filter count = %v; want %d", old.version, got, conversionProbeLogs)
	}

	// --- the client-wide override stamps every query the same way ---
	overridden := old.client(t, adminUser, adminPassword, chclient.Config{})
	overridden.SetQueryConditionCacheDisabled(true)
	code, body := newProbeMux(t, overridden, rules).logQuery(ctx, t, conversionProbeLineFilterQuery)
	assertGateSettingError(t, "24.8 node, condition-cache override", code, body, gate)
}

// assertGateSettingError checks that a response is the named gate-setting
// error: HTTP 502, as any ClickHouse rejection, with the setting, every
// defect it avoids and the release to upgrade to in the body.
func assertGateSettingError(t *testing.T, what string, code int, body string, gate chopt.GateSetting) {
	t.Helper()
	if code != http.StatusBadGateway {
		t.Errorf("%s: HTTP %d %s; want 502", what, code, body)
	}
	for _, want := range append([]string{gate.Name, gate.Since.String()}, gate.Defects...) {
		if !strings.Contains(body, want) {
			t.Errorf("%s: body %s does not name %q", what, body, want)
		}
	}
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
