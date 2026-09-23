//go:build integration

package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// Container resource bounds. Every probe here is deliberately CPU-bound or
// scans a seeded table, so a server that ignored its own deadline must burn a
// bounded share of the host, never the whole of it.
const (
	serverNanoCPUs    = 2_000_000_000 // two CPUs
	serverMemoryBytes = 6 << 30       // 6 GiB; ClickHouse sizes its own memory cap from the cgroup limit
)

// Credentials of the administrative user every container is booted with. It
// carries access management so a test can create the restricted users and
// row policies a probe needs.
const (
	adminUser     = "cerberus"
	adminPassword = "cerberus"
	serverDB      = "default"
)

// serverBootBudget covers a cold image pull plus server startup.
const serverBootBudget = 5 * time.Minute

// server is one booted ClickHouse build.
type server struct {
	image   string
	addr    string
	version chopt.Version
	// admin is the observer/administrative connection. Probes never dispatch
	// the query under test through it, so its pool and settings stay
	// independent of the client being measured.
	admin *chclient.Client
}

// startServer boots image under the package's CPU and memory limits and
// returns it with an administrative client and its probed build version.
// extra customizes the container further (a config file, for example).
func startServer(ctx context.Context, t *testing.T, image string, extra ...testcontainers.ContainerCustomizer) *server {
	t.Helper()
	bootCtx, cancel := context.WithTimeout(ctx, serverBootBudget)
	defer cancel()

	opts := append([]testcontainers.ContainerCustomizer{
		tcclickhouse.WithUsername(adminUser),
		tcclickhouse.WithPassword(adminPassword),
		tcclickhouse.WithDatabase(serverDB),
		testcontainers.WithEnv(map[string]string{"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1"}),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.NanoCPUs = serverNanoCPUs
			hc.Memory = serverMemoryBytes
		}),
	}, extra...)
	ctr, err := tcclickhouse.Run(bootCtx, image, opts...)
	if err != nil {
		t.Fatalf("start %s: %v", image, err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(bootCtx)
	if err != nil {
		t.Fatalf("%s host: %v", image, err)
	}
	port, err := ctr.MappedPort(bootCtx, "9000/tcp")
	if err != nil {
		t.Fatalf("%s port: %v", image, err)
	}
	s := &server{image: image, addr: host + ":" + port.Port()}
	s.admin = s.client(t, adminUser, adminPassword, chclient.Config{})
	s.version, err = s.admin.ProbeVersion(ctx)
	if err != nil {
		t.Fatalf("%s: probe version: %v", image, err)
	}
	t.Logf("%s: server version %s", image, s.version)
	return s
}

// client opens a cerberus data-plane client for user against s. cfg supplies
// any extra knobs; its address and credentials are overwritten, and an empty
// database defaults to serverDB.
func (s *server) client(t *testing.T, user, password string, cfg chclient.Config) *chclient.Client {
	t.Helper()
	cfg.Addr = s.addr
	if cfg.Database == "" {
		cfg.Database = serverDB
	}
	cfg.Username = user
	cfg.Password = password
	cfg.BreakerDisabled = true
	c, err := chclient.New(cfg)
	if err != nil {
		t.Fatalf("%s: connect as %s: %v", s.image, user, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// exec runs a statement on the administrative connection.
func (s *server) exec(ctx context.Context, t *testing.T, stmt string, args ...any) {
	t.Helper()
	if err := s.admin.Conn().Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %s: %v", s.image, stmt, err)
	}
}

// flushLogs makes every finished query visible in system.query_log.
func (s *server) flushLogs(ctx context.Context, t *testing.T) {
	t.Helper()
	s.exec(ctx, t, "SYSTEM FLUSH LOGS")
}

// serveJSON issues GET path against mux under ctx and decodes the 200 body
// into out.
func serveJSON(ctx context.Context, t *testing.T, mux http.Handler, path string, out any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d: %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: decode: %v (%s)", path, err, rec.Body.String())
	}
}
