//go:build integration

package chserver

import (
	"context"
	"math/rand/v2"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/testcontainers/testcontainers-go"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// httpCanaryImage maintains system.all_query_log, so the query-log union
// canary can be granted on one server and refused on the other.
const httpCanaryImage = "clickhouse/clickhouse-server:26.8.10.6-alpine"

// unionLocalConfig makes a server maintain system.all_query_log over its own
// query_log tables.
const unionLocalConfig = "../querylog/testdata/union-local.xml"

// httpBalancerSeed seeds the balancer's routing, so every run routes the
// same requests to the same servers.
const httpBalancerSeed = 4148

// httpBalancedPasses is how many fleet probe passes the test makes through
// the balancer.
const httpBalancedPasses = 16

// httpCanaries are every canary whose verdict can name no server over HTTP:
// the settings canaries, whose refusal is an exception, and the query-log
// union canary, whose answer is the reader's record selection.
var httpCanaries = []chclient.FleetCanary{chclient.TSGridCanary(), chclient.ResultCacheCanary(), chclient.QueryLogUnionCanary()}

// startHTTPCanaryServer boots a server with the canary user, granted the
// query-log union table. A refusing server pins the user's time-series and
// query-cache settings (canaryProfile) and does not maintain the union table,
// so it refuses every canary in httpCanaries; a permitting server pins
// nothing and maintains the table, so it accepts them all.
func startHTTPCanaryServer(ctx context.Context, t *testing.T, refusing bool) *server {
	t.Helper()
	var extra []testcontainers.ContainerCustomizer
	if !refusing {
		extra = append(extra, testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      unionLocalConfig,
			ContainerFilePath: "/etc/clickhouse-server/config.d/union-local.xml",
			FileMode:          0o644,
		}))
	}
	s := startServer(ctx, t, httpCanaryImage, extra...)
	if refusing {
		s.exec(ctx, t, "CREATE SETTINGS PROFILE "+canaryProfile+" SETTINGS "+chclient.SettingExperimentalTSGridAggregate+" = 0 CONST, "+chclient.SettingUseQueryCache+" = 0 CONST")
		s.exec(ctx, t, "CREATE USER "+canaryUser+" IDENTIFIED BY '"+canaryPassword+"' SETTINGS PROFILE "+canaryProfile)
	} else {
		s.exec(ctx, t, "CREATE USER "+canaryUser+" IDENTIFIED BY '"+canaryPassword+"'")
	}
	s.exec(ctx, t, "GRANT SELECT ON system.all_query_log TO "+canaryUser)
	return s
}

// startPerRequestBalancer serves an HTTP load balancer on a loopback port
// that sends each request it receives on its own to one of backends, drawn
// from a sequence seeded with httpBalancerSeed — the routing of chproxy, an
// nginx http upstream or a cloud L7 balancer, under which two statements of
// one client can reach different servers.
func startPerRequestBalancer(t *testing.T, backends ...string) string {
	t.Helper()
	var mu sync.Mutex
	pick := rand.New(rand.NewPCG(httpBalancerSeed, httpBalancerSeed))
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		mu.Lock()
		backend := backends[pick.IntN(len(backends))]
		mu.Unlock()
		r.SetURL(&url.URL{Scheme: "http", Host: backend})
	}}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// TestFleetProbe_PerRequestHTTPBalancerHoldsTheRefusingReplica puts a
// refusing and a permitting server behind one HTTP address whose balancer
// routes every request on its own, and probes the fleet through it pass after
// pass with the settings canaries and the query-log union canary. A refusal
// and a union answer name no server, and the identity read before them may
// have reached the other one. On every canary, the fleet's verdict must be
// the refusing server's from the first pass that read it, and stay there on
// every later pass while the refusing server keeps serving.
func TestFleetProbe_PerRequestHTTPBalancerHoldsTheRefusingReplica(t *testing.T) {
	ctx := context.Background()
	refusing := startHTTPCanaryServer(ctx, t, true)
	permitting := startHTTPCanaryServer(ctx, t, false)
	cfg := canaryConfig(startPerRequestBalancer(t, refusing.httpAddr, permitting.httpAddr))
	cfg.Protocol = clickhouse.HTTP

	memory := chclient.NewFleetMemory()
	refusedFrom := map[string]int{}
	for pass := range httpBalancedPasses {
		fv := memory.Observe(chclient.ProbeFleet(ctx, cfg, "", memory.CanaryOrder, httpCanaries...))
		for _, canary := range httpCanaries {
			got := fv.Capability(canary.Name)
			first, refused := refusedFrom[canary.Name]
			switch {
			case !refused && got == chopt.CapabilityForbidden:
				refusedFrom[canary.Name] = pass
			case refused && got != chopt.CapabilityForbidden:
				t.Errorf("pass %d: %s %s; want %s, refused by the refusing server since pass %d", pass+1, canary.Name, got, chopt.CapabilityForbidden, first+1)
			}
		}
	}
	for _, canary := range httpCanaries {
		if _, refused := refusedFrom[canary.Name]; !refused {
			t.Errorf("%s: no pass in %d read the refusing server's refusal", canary.Name, httpBalancedPasses)
		}
	}
}
