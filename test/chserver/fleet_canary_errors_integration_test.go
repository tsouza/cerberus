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

// preSettingImage predates the time-series setting the ts_grid canary turns
// on and the query-log union table.
const preSettingImage = "clickhouse/clickhouse-server:24.8.14.39-alpine"

// undefinedWorkload names no WORKLOAD the refusal test defines.
const undefinedWorkload = "cerberus_undefined_workload"

// TestFleetProbe_EveryRefusalIsForbidden provokes, on real servers, every
// condition under which a server refuses a capability canary, and requires
// the canary's verdict to be forbidden over both protocols: a readonly
// profile, a CONST and a MAX settings constraint on the setting the canary
// turns on, a CONST constraint on a setting it pins alongside it, an
// overflow mode the query cache refuses, a workload the server cannot
// resolve, a build that predates the setting, and a query-log union table
// the user may not read or the server does not maintain.
func TestFleetProbe_EveryRefusalIsForbidden(t *testing.T) {
	ctx := context.Background()
	strict := startServer(ctx, t, httpCanaryImage, testcontainers.WithFiles(testcontainers.ContainerFile{
		HostFilePath:      strictWorkloadUnionConfig,
		ContainerFilePath: "/etc/clickhouse-server/config.d/strict-workload-union.xml",
		FileMode:          0o644,
	}))
	old := startServer(ctx, t, preSettingImage)
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
		readonly bool
	}
	settingsCanaries := []canaryProbe{probeTSGrid, probeResultCache}
	refusals := []refusal{
		{"readonly profile", strict, "readonly = 1", true, []canaryProbe{probeTSGrid, probeResultCache, probeWorkload("all"), probeQueryLogUnion}, true},
		{"CONST constraint", strict, chclient.SettingExperimentalTSGridAggregate + " = 0 CONST, " + chclient.SettingUseQueryCache + " = 0 CONST", true, settingsCanaries, false},
		{"MAX constraint", strict, chclient.SettingExperimentalTSGridAggregate + " MAX 0, " + chclient.SettingUseQueryCache + " MAX 0", true, settingsCanaries, false},
		{"constraint on a pinned setting", strict, chclient.SettingParallelReplicas + " = 1 CONST, " + chclient.SettingQueryCacheTTL + " = 60 CONST", true, settingsCanaries, false},
		{"overflow mode other than throw", strict, "result_overflow_mode = 'break'", true, []canaryProbe{probeResultCache}, false},
		{"build without the setting", old, "", false, []canaryProbe{probeTSGrid}, false},
		{"workload pinned by a constraint", strict, chclient.SettingWorkload + " = 'default' CONST", true, []canaryProbe{probeWorkload(undefinedWorkload)}, false},
		{"undefined workload", strict, "", true, []canaryProbe{probeWorkload(undefinedWorkload)}, false},
		{"no grant on the union table", strict, "", false, []canaryProbe{probeQueryLogUnion}, false},
		{"no union table", old, "", false, []canaryProbe{probeQueryLogUnion}, false},
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
					t.Errorf("%s over %s: %s %s; want %s", r.name, protocol, canary.fleet.Name, got, chopt.CapabilityForbidden)
				}
				if protocol == clickhouse.HTTP && !r.readonly {
					requireRefusalHeld(ctx, t, cfg, r.name, canary.fleet)
				}
			}
			_ = c.Close()
		}
	}
}

// canaryProbe is one capability canary: run on its own on a client (probe),
// and run by the fleet probe (fleet).
type canaryProbe struct {
	fleet chclient.FleetCanary
	probe func(ctx context.Context, c *chclient.Client) chopt.Capability
}

var (
	probeTSGrid = canaryProbe{chclient.TSGridCanary(), func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeTSGridCapability(ctx)
	}}
	probeResultCache = canaryProbe{chclient.ResultCacheCanary(), func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeResultCacheCapability(ctx)
	}}
	probeQueryLogUnion = canaryProbe{chclient.QueryLogUnionCanary(), func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeQueryLogUnionCapability(ctx)
	}}
)

// probeWorkload is the workload canary for workload.
func probeWorkload(workload string) canaryProbe {
	return canaryProbe{chclient.QueryWorkloadCanary(workload), func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeQueryWorkloadCapability(ctx, workload)
	}}
}

// requireRefusalHeld fails the test unless canary's refusal, read over HTTP
// by a fleet probe against cfg, is one the fleet memory holds as a refusal: a
// following pass that reaches the same server without reading it still
// reads the canary forbidden. A refusal answered with a code the classifier
// does not know as one would count for its own pass only.
func requireRefusalHeld(ctx context.Context, t *testing.T, cfg chclient.Config, name string, canary chclient.FleetCanary) {
	t.Helper()
	memory := chclient.NewFleetMemory()
	pass := chclient.ProbeFleet(ctx, cfg, "", nil, canary)
	if got := memory.Observe(pass).Capability(canary.Name); got != chopt.CapabilityForbidden {
		t.Errorf("%s over http, fleet probe: %s %s; want %s", name, canary.Name, got, chopt.CapabilityForbidden)
		return
	}
	pass.Unattributed = nil
	if got := memory.Observe(pass).Capability(canary.Name); got != chopt.CapabilityForbidden {
		t.Errorf("%s over http, the pass after the refusal: %s %s; want the refusal held, %s", name, canary.Name, got, chopt.CapabilityForbidden)
	}
}

// Server exceptions the injecting proxy provokes.
const (
	chCodeMemoryLimitExceeded = 241
	chCodeTooBigAST           = 168
)

// memoryLimitSettings are settings the injecting proxy adds to a canary
// request: a one-byte memory limit, tracked from the first byte, which the
// server enforces on any statement with MEMORY_LIMIT_EXCEEDED — a condition
// of the moment that says nothing about whether the server accepts the
// capability.
var memoryLimitSettings = url.Values{"max_memory_usage": {"1"}, "max_untracked_memory": {"0"}}

// astLimitSettings are settings the injecting proxy adds to a canary request:
// a one-element syntax tree limit, which the server enforces on any statement
// with TOO_BIG_AST — a server error the canary classifier knows neither as a
// refusal nor as a condition of the moment.
var astLimitSettings = url.Values{"max_ast_elements": {"1"}}

// startInjectingProxy serves an HTTP proxy to backend on a loopback port
// that adds the settings inject points to, when it points to any, to every
// canary request — the ts_grid canary and the query-log union read — and
// passes every other request, the identity read among them, through
// unchanged.
func startInjectingProxy(t *testing.T, backend string, inject *atomic.Pointer[url.Values]) string {
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
		settings := inject.Load()
		if settings == nil || !canary {
			return
		}
		for name, values := range *settings {
			query[name] = values
		}
		r.Out.URL.RawQuery = query.Encode()
	}}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// requireInjected fails the test unless the proxy at cfg, injecting, makes
// the server answer the ts_grid canary's statement with code want and the
// union canary's with wantUnion (0: success), so the timeline test exercises
// real server exceptions rather than a proxy failure.
func requireInjected(ctx context.Context, t *testing.T, cfg chclient.Config, want, wantUnion int32) {
	t.Helper()
	c, err := chclient.New(cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", cfg.Addr, err)
	}
	defer func() { _ = c.Close() }()
	_, tsGridErr := c.QueryStrings(chclient.WithTSGridSetting(ctx), "SELECT toString(serverUUID()), version()")
	_, unionErr := c.QueryLogActuals(ctx, chclient.QueryLogActualsRequest{Union: true, After: chclient.QueryLogCursor{EventTime: time.Now()}})
	for name, check := range map[string]struct {
		err  error
		want int32
	}{chclient.CanaryTSGrid: {tsGridErr, want}, chclient.CanaryQueryLogUnion: {unionErr, wantUnion}} {
		var ex *clickhouse.Exception
		switch {
		case check.want == 0 && check.err != nil:
			t.Fatalf("%s statement through the injecting proxy: %v; want success", name, check.err)
		case check.want != 0 && (!errors.As(check.err, &ex) || ex.Code != check.want):
			t.Fatalf("%s statement through the injecting proxy: %v; want server exception %d", name, check.err, check.want)
		}
	}
}

// TestFleetProbe_TransientCanaryErrorDoesNotHoldACapabilityOff probes one
// permitting server over HTTP through a proxy that, on chosen passes, makes
// the server fail canaries with a real server exception, and checks that no
// such pass keeps a capability the server accepts off for longer than until
// the next probe — one steady re-probe interval, or sooner when the pass
// leaves the fleet held:
//
//   - MEMORY_LIMIT_EXCEEDED, a condition of the moment, on both canaries:
//     after a pass that read the capability available it stays available,
//     and on a memory with no verdict yet it reads unreachable for that pass.
//   - TOO_BIG_AST, a server error that is neither a known refusal nor a known
//     condition, on both canaries: it reads forbidden for that pass, and the
//     next pass reads the capability available again.
//
// The log lines are the timeline.
func TestFleetProbe_TransientCanaryErrorDoesNotHoldACapabilityOff(t *testing.T) {
	ctx := context.Background()
	srv := startHTTPCanaryServer(ctx, t, false)
	var inject atomic.Pointer[url.Values]
	cfg := canaryConfig(startInjectingProxy(t, srv.httpAddr, &inject))
	cfg.Protocol = clickhouse.HTTP
	inject.Store(&memoryLimitSettings)
	requireInjected(ctx, t, cfg, chCodeMemoryLimitExceeded, chCodeMemoryLimitExceeded)
	inject.Store(&astLimitSettings)
	requireInjected(ctx, t, cfg, chCodeTooBigAST, chCodeTooBigAST)
	inject.Store(nil)

	available, forbidden, unreachable := chopt.CapabilityAvailable, chopt.CapabilityForbidden, chopt.CapabilityUnreachable
	type pass struct {
		inject          *url.Values
		tsGrid, qlUnion chopt.Capability
	}
	canaries := []chclient.FleetCanary{chclient.TSGridCanary(), chclient.QueryLogUnionCanary()}
	for _, scenario := range []struct {
		name   string
		passes []pass
	}{
		{"memory limit after a verdict", []pass{{nil, available, available}, {nil, available, available}, {&memoryLimitSettings, available, available}, {nil, available, available}}},
		{"memory limit before any verdict", []pass{{&memoryLimitSettings, unreachable, unreachable}, {nil, available, available}, {nil, available, available}}},
		{"unrecognised server error after a verdict", []pass{{nil, available, available}, {&astLimitSettings, forbidden, forbidden}, {nil, available, available}, {nil, available, available}}},
	} {
		memory := chclient.NewFleetMemory()
		for i, p := range scenario.passes {
			inject.Store(p.inject)
			fv := memory.Observe(chclient.ProbeFleet(ctx, cfg, "", memory.CanaryOrder, canaries...))
			cadence := "steady"
			if fv.Held {
				cadence = "held"
			}
			line := []string{}
			for j, want := range []chopt.Capability{p.tsGrid, p.qlUnion} {
				got := fv.Capability(canaries[j].Name)
				line = append(line, canaries[j].Name+"="+got.String())
				if got != want {
					t.Errorf("%s, pass %d (injected: %v): %s %s; want %s", scenario.name, i+1, p.inject != nil, canaries[j].Name, got, want)
				}
			}
			t.Logf("%s, pass %d: injected=%v %s; next probe on the %s cadence", scenario.name, i+1, p.inject != nil, strings.Join(line, " "), cadence)
		}
	}
}

// TestFleetProbe_ProfileChangeTurnsACapabilityOff reads result_cache available
// on a server, then gives the canary user a profile under which the query
// cache refuses every statement (result_overflow_mode='break'), and requires
// the next pass to read it forbidden, over both protocols: a refusal that
// appears on a server the memory already holds as permitting turns the
// capability off rather than leaving the remembered permit in force.
func TestFleetProbe_ProfileChangeTurnsACapabilityOff(t *testing.T) {
	ctx := context.Background()
	srv := startHTTPCanaryServer(ctx, t, false)
	for _, protocol := range []clickhouse.Protocol{clickhouse.Native, clickhouse.HTTP} {
		srv.exec(ctx, t, "ALTER USER "+canaryUser+" SETTINGS NONE")
		addr := srv.addr
		if protocol == clickhouse.HTTP {
			addr = srv.httpAddr
		}
		cfg := canaryConfig(addr)
		cfg.Protocol = protocol
		memory := chclient.NewFleetMemory()
		probe := func() chopt.Capability {
			fv := memory.Observe(chclient.ProbeFleet(ctx, cfg, "", memory.CanaryOrder, chclient.ResultCacheCanary()))
			return fv.Capability(chclient.CanaryResultCache)
		}
		if got := probe(); got != chopt.CapabilityAvailable {
			t.Fatalf("over %s, before the profile change: %s %s; want %s", protocol, chclient.CanaryResultCache, got, chopt.CapabilityAvailable)
		}
		srv.exec(ctx, t, "ALTER USER "+canaryUser+" SETTINGS result_overflow_mode = 'break'")
		if got := probe(); got != chopt.CapabilityForbidden {
			t.Errorf("over %s, after the profile change: %s %s; want %s", protocol, chclient.CanaryResultCache, got, chopt.CapabilityForbidden)
		}
	}
}
