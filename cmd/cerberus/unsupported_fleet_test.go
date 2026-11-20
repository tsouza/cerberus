package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Builds of a fleet that mixes a supported node with two nodes on a line
// below chopt.MinSupportedVersion.
var (
	unsupportedFleetBuild = chopt.Version{Major: 25, Minor: 8, Patch: 16, Build: 32}
	supportedFleetBuild   = chopt.Version{Major: 26, Minor: 8, Patch: 36, Build: 3}
	floorFleetBuild       = chopt.Version{Major: chopt.MinSupportedMajor, Minor: chopt.MinSupportedMinor, Patch: 17, Build: 56}
)

// unsupportedNodesMetric is the gauge reportUnsupportedFleet records.
const unsupportedNodesMetric = "cerberus_clickhouse_unsupported_nodes"

// syncBuffer is a bytes.Buffer safe for the re-probe goroutine to log into
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// unsupportedWarnings decodes every JSON log line in logs that warns of a
// server below the minimum supported version.
func unsupportedWarnings(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(logs) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if msg, _ := rec["msg"].(string); rec["level"] == "WARN" && strings.Contains(msg, "below the minimum supported version") {
			out = append(out, rec)
		}
	}
	return out
}

// unsupportedNodesGauge collects reader and returns the gauge's value, with
// ok=false when no value was recorded.
func unsupportedNodesGauge(t *testing.T, reader *sdkmetric.ManualReader) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != unsupportedNodesMetric {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || len(g.DataPoints) != 1 {
				t.Fatalf("%s: want one int64 gauge point, got %#v", unsupportedNodesMetric, m.Data)
			}
			return g.DataPoints[0].Value, true
		}
	}
	return 0, false
}

// installManualReader installs a MeterProvider whose metrics the test
// collects, and restores the no-op provider when it ends.
func installManualReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	telemetry.Install(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { telemetry.Install(nil) })
	return reader
}

// bootAgainst runs the boot resolution against fleet under the auto selection
// with a JSON logger writing to logs.
func bootAgainst(t *testing.T, fleet chclient.FleetVersions, logs *syncBuffer) (chOptResolution, config.Config) {
	t.Helper()
	cfg := config.Config{CHOptimizations: chopt.SelectionAuto, CHOptimizationsMode: chopt.Permissive, Schema: schema.DefaultOTelMetrics()}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	client, err := chclient.New(chclient.Config{Addr: cfg.ClickHouse.Addr, Database: "otel"})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	bootCfg := cfg
	res, err := resolveCHOptimizations(context.Background(), logger, client, &bootCfg, func(context.Context) chclient.FleetVersions { return fleet })
	if err != nil {
		t.Fatalf("boot against a fleet with an unsupported node must still start; resolveCHOptimizations: %v", err)
	}
	return res, cfg
}

// TestBoot_UnsupportedNodeWarnsAndStarts pins what a fleet holding nodes
// below the minimum supported version gets at boot: the boot succeeds and
// resolves against the oldest node exactly as it would for that build, a WARN
// names the build, the minimum, the node count and the release that refuses
// it, the gauge counts the nodes, and /info reports the count and the minimum.
func TestBoot_UnsupportedNodeWarnsAndStarts(t *testing.T) {
	reader := installManualReader(t)
	fleet := chclient.FleetVersions{
		Versions: []chopt.Version{supportedFleetBuild, unsupportedFleetBuild, unsupportedFleetBuild},
		Complete: true,
	}
	var logs syncBuffer
	res, _ := bootAgainst(t, fleet, &logs)

	if res.VersionFallback || res.ResolvedVersion != unsupportedFleetBuild {
		t.Errorf("boot resolved against %s (fallback %v); want the oldest node, %s, as for any fleet", res.ResolvedVersion, res.VersionFallback, unsupportedFleetBuild)
	}
	want, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Mode: chopt.Permissive}, unsupportedFleetBuild)
	if err != nil {
		t.Fatalf("resolve auto against %s: %v", unsupportedFleetBuild, err)
	}
	if got, wantIDs := res.Set.IDs(), want.IDs(); strings.Join(got, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("boot enabled %v; want the set auto resolves to on %s, %v", got, unsupportedFleetBuild, wantIDs)
	}

	warns := unsupportedWarnings(t, logs.String())
	if len(warns) != 1 {
		t.Fatalf("want one unsupported-version WARN for the one unsupported build, got %d:\n%s", len(warns), logs.String())
	}
	w := warns[0]
	if w["server_version"] != unsupportedFleetBuild.String() || w["minimum_supported_version"] != "26.3" || w["nodes"] != float64(2) {
		t.Errorf("warning attributes = %v; want server_version %s, minimum_supported_version 26.3, nodes 2", w, unsupportedFleetBuild)
	}
	if msg, _ := w["msg"].(string); !strings.Contains(msg, "unsupported") || !strings.Contains(msg, "v1.26.0") {
		t.Errorf("warning %q must say the server is unsupported and that v1.26.0 refuses it", msg)
	}

	if got, ok := unsupportedNodesGauge(t, reader); !ok || got != 2 {
		t.Errorf("%s = %d (recorded %v); want 2", unsupportedNodesMetric, got, ok)
	}
	state := newCHOptLive(res).infoState()
	if state.UnsupportedNodes != 2 || state.MinimumSupportedVersion != "26.3" {
		t.Errorf("/info state reports %d unsupported nodes below %q; want 2 below 26.3", state.UnsupportedNodes, state.MinimumSupportedVersion)
	}
}

// TestBoot_SupportedFleetDoesNotWarn pins the other side: a fleet whose oldest
// node is on the minimum supported line logs no unsupported-version WARN and
// records a zero gauge.
func TestBoot_SupportedFleetDoesNotWarn(t *testing.T) {
	reader := installManualReader(t)
	fleet := chclient.FleetVersions{Versions: []chopt.Version{supportedFleetBuild, floorFleetBuild}, Complete: true}
	var logs syncBuffer
	res, _ := bootAgainst(t, fleet, &logs)

	if warns := unsupportedWarnings(t, logs.String()); len(warns) != 0 {
		t.Errorf("a fleet at or above 26.3 must not warn, got %v", warns)
	}
	if got, ok := unsupportedNodesGauge(t, reader); !ok || got != 0 {
		t.Errorf("%s = %d (recorded %v); want a recorded 0", unsupportedNodesMetric, got, ok)
	}
	if res.UnsupportedNodes != 0 {
		t.Errorf("resolution reports %d unsupported nodes; want 0", res.UnsupportedNodes)
	}
}

// TestReprobe_UnsupportedNodeJoinsWarns pins the re-probe: a node below the
// minimum supported version joining a supported fleet after boot is warned of,
// counted on the gauge and in the live resolution /info reads.
func TestReprobe_UnsupportedNodeJoinsWarns(t *testing.T) {
	reader := installManualReader(t)
	var bootLogs syncBuffer
	boot, cfg := bootAgainst(t, chclient.FleetVersions{Versions: []chopt.Version{supportedFleetBuild}, Complete: true}, &bootLogs)
	if boot.UnsupportedNodes != 0 {
		t.Fatalf("premise: the supported boot fleet reports %d unsupported nodes", boot.UnsupportedNodes)
	}

	live := newCHOptLive(boot)
	joined := chclient.FleetVersions{Versions: []chopt.Version{supportedFleetBuild, unsupportedFleetBuild}, Complete: true}
	probe := func(context.Context) chclient.FleetVersions { return joined }
	var logs syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, slog.New(slog.NewJSONHandler(&logs, nil)), cfg, live, chOptConsumers{}, testReprobeTiming, nil, "", probe)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for live.get().UnsupportedNodes != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the live resolution never counted the unsupported node that joined")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := live.get().ResolvedVersion; got != unsupportedFleetBuild {
		t.Errorf("re-probe resolved against %s; want the oldest node, %s, as for any fleet", got, unsupportedFleetBuild)
	}
	warns := unsupportedWarnings(t, logs.String())
	if len(warns) == 0 {
		t.Fatalf("the re-probe logged no unsupported-version WARN:\n%s", logs.String())
	}
	if w := warns[0]; w["server_version"] != unsupportedFleetBuild.String() || w["nodes"] != float64(1) {
		t.Errorf("warning attributes = %v; want server_version %s, nodes 1", w, unsupportedFleetBuild)
	}
	if got, ok := unsupportedNodesGauge(t, reader); !ok || got != 1 {
		t.Errorf("%s = %d (recorded %v); want 1", unsupportedNodesMetric, got, ok)
	}
}
