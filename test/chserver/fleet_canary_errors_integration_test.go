//go:build integration

package chserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/testcontainers/testcontainers-go"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// strictWorkloadUnionConfig makes a server maintain system.all_query_log and
// refuse a workload name that resolves to no defined WORKLOAD.
const strictWorkloadUnionConfig = "testdata/strict-workload-union.xml"

// undefinedWorkload names no WORKLOAD the refusal test defines.
const undefinedWorkload = "cerberus_undefined_workload"

// TestFleetProbe_EveryRefusalIsForbidden provokes, on one real server, every
// condition under which a server refuses a capability canary, and requires
// the canary's verdict to be forbidden over both protocols: a readonly
// profile, a CONST and a MAX settings constraint on the setting the canary
// turns on, a CONST constraint on a setting it pins alongside it, a workload
// the server cannot resolve, and a query-log union table the user may not
// read or the server does not maintain. A refusal the classifier did not
// know would read as unreachable and let a server that rejects the
// capability keep a verdict that turns it on.
func TestFleetProbe_EveryRefusalIsForbidden(t *testing.T) {
	ctx := context.Background()
	strict := startServer(ctx, t, httpCanaryImage, testcontainers.WithFiles(testcontainers.ContainerFile{
		HostFilePath:      strictWorkloadUnionConfig,
		ContainerFilePath: "/etc/clickhouse-server/config.d/strict-workload-union.xml",
		FileMode:          0o644,
	}))
	bare := startServer(ctx, t, httpCanaryImage)
	strict.exec(ctx, t, "CREATE RESOURCE cpu (MASTER THREAD, WORKER THREAD)")
	strict.exec(ctx, t, "CREATE WORKLOAD all")
	// A statement that names no workload runs under `default`, which must
	// resolve too, or the server refuses every such statement and each case
	// below would read that refusal rather than its own.
	strict.exec(ctx, t, "CREATE WORKLOAD default IN all")

	type refusal struct {
		name     string
		on       *server
		settings string
		granted  bool
		canaries []canaryProbe
	}
	settingsCanaries := []canaryProbe{probeTSGrid, probeResultCache}
	refusals := []refusal{
		{"readonly profile", strict, "readonly = 1", true, []canaryProbe{probeTSGrid, probeResultCache, probeWorkload("all"), probeQueryLogUnion}},
		{"CONST constraint", strict, chclient.SettingExperimentalTSGridAggregate + " = 0 CONST, " + chclient.SettingUseQueryCache + " = 0 CONST", true, settingsCanaries},
		{"MAX constraint", strict, chclient.SettingExperimentalTSGridAggregate + " MAX 0, " + chclient.SettingUseQueryCache + " MAX 0", true, settingsCanaries},
		{"constraint on a pinned setting", strict, chclient.SettingParallelReplicas + " = 1 CONST, " + chclient.SettingQueryCacheTTL + " = 60 CONST", true, settingsCanaries},
		{"workload pinned by a constraint", strict, chclient.SettingWorkload + " = 'all' CONST", true, []canaryProbe{probeWorkload(undefinedWorkload)}},
		{"undefined workload", strict, "", true, []canaryProbe{probeWorkload(undefinedWorkload)}},
		{"no grant on the union table", strict, "", false, []canaryProbe{probeQueryLogUnion}},
		{"no union table", bare, "", false, []canaryProbe{probeQueryLogUnion}},
	}
	for i, r := range refusals {
		user := "refusal_" + string(rune('a'+i))
		ddl := "CREATE USER " + user + " IDENTIFIED BY '" + canaryPassword + "'"
		if r.settings != "" {
			ddl += " SETTINGS " + r.settings
		}
		r.on.exec(ctx, t, ddl)
		if r.granted {
			r.on.exec(ctx, t, "GRANT SELECT ON system.all_query_log TO "+user)
		}
		for _, protocol := range []clickhouse.Protocol{clickhouse.Native, clickhouse.HTTP} {
			addr := r.on.addr
			if protocol == clickhouse.HTTP {
				addr = r.on.httpAddr
			}
			cfg := canaryConfig(addr)
			cfg.Username, cfg.Protocol = user, protocol
			c, err := chclient.New(cfg)
			if err != nil {
				t.Fatalf("connect %s: %v", addr, err)
			}
			// Each canary runs on its own: a readonly profile refuses the
			// settings the fleet probe's identity read carries too, so a
			// fleet probe would read no server to run it on.
			for _, canary := range r.canaries {
				if got := canary.probe(ctx, c); got != chopt.CapabilityForbidden {
					t.Errorf("%s over %s: %s %s; want %s", r.name, protocol, canary.name, got, chopt.CapabilityForbidden)
				}
			}
			_ = c.Close()
		}
	}
}

// canaryProbe is one capability canary run on its own on a client.
type canaryProbe struct {
	name  string
	probe func(ctx context.Context, c *chclient.Client) chopt.Capability
}

var (
	probeTSGrid = canaryProbe{chclient.CanaryTSGrid, func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeTSGridCapability(ctx)
	}}
	probeResultCache = canaryProbe{chclient.CanaryResultCache, func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeResultCacheCapability(ctx)
	}}
	probeQueryLogUnion = canaryProbe{chclient.CanaryQueryLogUnion, func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeQueryLogUnionCapability(ctx)
	}}
)

// probeWorkload is the workload canary for workload.
func probeWorkload(workload string) canaryProbe {
	return canaryProbe{chclient.CanaryQueryWorkload, func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeQueryWorkloadCapability(ctx, workload)
	}}
}

// chCodeMemoryLimitExceeded is MEMORY_LIMIT_EXCEEDED, the server exception
// the injecting proxy provokes.
const chCodeMemoryLimitExceeded = 241

// transientMemoryLimit are the settings the injecting proxy adds to a canary
// request: a one-byte memory limit, tracked from the first byte, which the
// server enforces on any statement with MEMORY_LIMIT_EXCEEDED — a real server
// exception that says nothing about whether the server accepts the
// capability.
var transientMemoryLimit = url.Values{"max_memory_usage": {"1"}, "max_untracked_memory": {"0"}}

// startInjectingProxy serves an HTTP proxy to backend on a loopback port
// that, while inject is set, adds transientMemoryLimit to every canary
// request — the ts_grid canary and the query-log union read — and passes
// every other request, the identity read among them, through unchanged.
func startInjectingProxy(t *testing.T, backend string, inject *atomic.Bool) string {
	t.Helper()
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(&url.URL{Scheme: "http", Host: backend})
		body, err := io.ReadAll(r.In.Body)
		if err != nil {
			t.Errorf("read proxied request: %v", err)
		}
		r.Out.Body = io.NopCloser(bytes.NewReader(body))
		r.Out.ContentLength = int64(len(body))
		query := r.Out.URL.Query()
		canary := query.Has(chclient.SettingExperimentalTSGridAggregate) ||
			strings.Contains(string(body)+query.Get("query"), "all_query_log")
		if !inject.Load() || !canary {
			return
		}
		for name, values := range transientMemoryLimit {
			query[name] = values
		}
		r.Out.URL.RawQuery = query.Encode()
	}}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// requireTransientInjected fails the test unless the proxy at cfg, injecting,
// makes the server answer both canaries' statements with
// MEMORY_LIMIT_EXCEEDED, so the timeline test exercises a real server
// exception rather than a proxy failure.
func requireTransientInjected(ctx context.Context, t *testing.T, cfg chclient.Config) {
	t.Helper()
	c, err := chclient.New(cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", cfg.Addr, err)
	}
	defer func() { _ = c.Close() }()
	_, tsGridErr := c.QueryStrings(chclient.WithTSGridSetting(ctx), "SELECT toString(serverUUID()), version()")
	_, unionErr := c.QueryLogActuals(ctx, chclient.QueryLogActualsRequest{Union: true, After: chclient.QueryLogCursor{EventTime: time.Now()}})
	for name, err := range map[string]error{chclient.CanaryTSGrid: tsGridErr, chclient.CanaryQueryLogUnion: unionErr} {
		var ex *clickhouse.Exception
		if !errors.As(err, &ex) || ex.Code != chCodeMemoryLimitExceeded {
			t.Fatalf("%s statement through the injecting proxy: %v; want server exception %d", name, err, chCodeMemoryLimitExceeded)
		}
	}
}

// TestFleetProbe_TransientCanaryErrorDoesNotHoldACapabilityOff probes one
// permitting server over HTTP through a proxy that, on chosen passes, makes
// the server fail the ts_grid canary and the query-log union canary with
// MEMORY_LIMIT_EXCEEDED. Such a pass must not turn a capability the server
// accepts off: after a pass that read the capability available it stays
// available, and on a memory with no verdict yet it reads unreachable for
// that pass only, so the capability is off until the next probe at most —
// one steady re-probe interval, or sooner when the pass leaves the fleet held
// — and the next pass reads it again. The log lines are the timeline.
func TestFleetProbe_TransientCanaryErrorDoesNotHoldACapabilityOff(t *testing.T) {
	ctx := context.Background()
	srv := startHTTPCanaryServer(ctx, t, false)
	var inject atomic.Bool
	cfg := canaryConfig(startInjectingProxy(t, srv.httpAddr, &inject))
	cfg.Protocol = clickhouse.HTTP
	inject.Store(true)
	requireTransientInjected(ctx, t, cfg)
	inject.Store(false)

	canaries := []chclient.FleetCanary{chclient.TSGridCanary(), chclient.QueryLogUnionCanary()}
	for _, scenario := range []struct {
		name      string
		transient []bool
		want      []chopt.Capability
	}{
		{"after a verdict", []bool{false, false, true, false, false}, []chopt.Capability{chopt.CapabilityAvailable, chopt.CapabilityAvailable, chopt.CapabilityAvailable, chopt.CapabilityAvailable, chopt.CapabilityAvailable}},
		{"before any verdict", []bool{true, false, false}, []chopt.Capability{chopt.CapabilityUnreachable, chopt.CapabilityAvailable, chopt.CapabilityAvailable}},
	} {
		memory := chclient.NewFleetMemory()
		for pass, transient := range scenario.transient {
			inject.Store(transient)
			fv := memory.Observe(chclient.ProbeFleet(ctx, cfg, "", memory.CanaryOrder, canaries...))
			cadence := "steady"
			if fv.Held {
				cadence = "held"
			}
			line := []string{}
			for _, canary := range canaries {
				got := fv.Capability(canary.Name)
				line = append(line, canary.Name+"="+got.String())
				if got != scenario.want[pass] {
					t.Errorf("%s, pass %d (transient=%v): %s %s; want %s", scenario.name, pass+1, transient, canary.Name, got, scenario.want[pass])
				}
			}
			t.Logf("%s, pass %d: transient=%v %s; next probe on the %s cadence", scenario.name, pass+1, transient, strings.Join(line, " "), cadence)
		}
	}
	inject.Store(false)
}
