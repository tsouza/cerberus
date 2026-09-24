//go:build integration

package regexjit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// Container resource bounds. The benchmark reports server CPU time, so the
// server must own a fixed CPU share rather than whatever the host has free.
const (
	serverNanoCPUs    = 2_000_000_000 // two CPUs
	serverMemoryBytes = 6 << 30       // 6 GiB; ClickHouse sizes its own memory cap from the cgroup limit
)

// Credentials of the administrative user every container is booted with.
const (
	adminUser     = "cerberus"
	adminPassword = "cerberus"
	serverDB      = "default"
)

// serverBootBudget covers a cold image pull plus server startup.
const serverBootBudget = 5 * time.Minute

// The regular-expression compiler's settings (ClickHouse #108004).
// minCountCompileNow makes the first evaluation of a pattern compile it.
const (
	settingCompileRegexp  = "compile_regular_expressions"
	settingMinCountRegexp = "min_count_to_compile_regular_expression"
	minCountCompileNow    = 0
	settingConditionCache = "use_query_condition_cache"
)

// otherJITSettings are the server's other native-code compilers. They share
// the compiled-expression cache the regular-expression compiler reports its
// entries through, so a server whose compiled entries must all be regular
// expressions has them off in the default profile — for every query it runs,
// including any a handler issues outside the request under test.
var otherJITSettings = []string{
	"compile_expressions",
	"compile_aggregate_expressions",
	"compile_sort_description",
}

// otherJITOffProfile switches otherJITSettings off in the server's default
// profile.
func otherJITOffProfile() testcontainers.ContainerCustomizer {
	var b strings.Builder
	b.WriteString("<clickhouse><profiles><default>")
	for _, name := range otherJITSettings {
		fmt.Fprintf(&b, "<%s>0</%s>", name, name)
	}
	b.WriteString("</default></profiles></clickhouse>")
	return testcontainers.WithFiles(testcontainers.ContainerFile{
		Reader:            strings.NewReader(b.String()),
		ContainerFilePath: "/etc/clickhouse-server/users.d/regexjit-other-jit-off.xml",
		FileMode:          configFileMode,
	})
}

// configFileMode is the permission bits of a configuration file copied into
// the container: readable by the server's own user.
const configFileMode = 0o644

// requireOtherJITOff fails unless every setting in otherJITSettings reads 0.
func (s *server) requireOtherJITOff(ctx context.Context, t testing.TB) {
	t.Helper()
	for _, name := range otherJITSettings {
		var v string
		if err := s.admin.Conn().QueryRow(ctx, "SELECT value FROM system.settings WHERE name = ?", name).Scan(&v); err != nil {
			t.Fatalf("%s: read %s: %v", s.image, name, err)
		}
		if v != "0" {
			t.Fatalf("%s: %s = %s in the default profile; want 0", s.image, name, v)
		}
	}
}

// compiledCacheMetric counts the entries in the server's cache of compiled
// machine code; a compiled regular expression adds one.
const compiledCacheMetric = "CompiledExpressionCacheCount"

// server is one booted ClickHouse build.
type server struct {
	image string
	addr  string
	// admin is the observer connection. Requests under test never go
	// through it, so its settings stay independent of the mode measured.
	admin *chclient.Client
	// version is the build's full version string.
	version string
	// regexpJIT reports whether the build has compile_regular_expressions.
	regexpJIT bool
	// conditionCache reports whether the build has the query condition
	// cache, which [server.modeCtx] switches off.
	conditionCache bool
}

// hasSetting reports whether the build knows the setting name.
func (s *server) hasSetting(ctx context.Context, t testing.TB, name string) bool {
	t.Helper()
	var n uint64
	if err := s.admin.Conn().QueryRow(ctx, "SELECT count() FROM system.settings WHERE name = ?", name).Scan(&n); err != nil {
		t.Fatalf("%s: probe setting %s: %v", s.image, name, err)
	}
	return n > 0
}

// modeCtx is withMode with the query condition cache off. A request's
// filter must evaluate its regular expression on every granule: a cache
// entry left by an earlier request in another mode would let the server
// skip granules it proved empty, so neither the answer nor the compiled
// code would come from the mode under test.
func (s *server) modeCtx(ctx context.Context, mode jitMode) context.Context {
	ctx = withMode(ctx, mode)
	if s.conditionCache {
		ctx = chclient.WithQuerySetting(ctx, settingConditionCache, 0)
	}
	return ctx
}

// startServer boots image under the package's CPU and memory limits and
// applies cerberus's metrics and logs DDL to it.
// extra customizes the container further.
func startServer(ctx context.Context, t testing.TB, image string, extra ...testcontainers.ContainerCustomizer) *server {
	t.Helper()
	bootCtx, cancel := context.WithTimeout(ctx, serverBootBudget)
	defer cancel()

	opts := append([]testcontainers.ContainerCustomizer{
		tcclickhouse.WithUsername(adminUser),
		tcclickhouse.WithPassword(adminPassword),
		tcclickhouse.WithDatabase(serverDB),
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
	s.admin = s.client(t)
	if err := s.admin.Conn().QueryRow(ctx, "SELECT version()").Scan(&s.version); err != nil {
		t.Fatalf("%s: read version: %v", image, err)
	}
	s.regexpJIT = s.hasSetting(ctx, t, settingCompileRegexp)
	s.conditionCache = s.hasSetting(ctx, t, settingConditionCache)
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics, ddl.Logs}); err != nil {
		t.Fatalf("%s: apply DDL: %v", image, err)
	}
	t.Logf("%s: server version %s, %s=%v", image, s.version, settingCompileRegexp, s.regexpJIT)
	return s
}

// client opens a cerberus data-plane client against s.
func (s *server) client(t testing.TB) *chclient.Client {
	t.Helper()
	c, err := chclient.New(chclient.Config{
		Addr:            s.addr,
		Database:        serverDB,
		Username:        adminUser,
		Password:        adminPassword,
		BreakerDisabled: true,
	})
	if err != nil {
		t.Fatalf("%s: connect: %v", s.image, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// exec runs a statement on the administrative connection.
func (s *server) exec(ctx context.Context, t testing.TB, stmt string, args ...any) {
	t.Helper()
	if err := s.admin.Conn().Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %s: %v", s.image, stmt, err)
	}
}

// dropCompiled waits until the server runs no other query, then empties the
// compiled-code cache and the regular-expression compiler's per-pattern use
// counts, so the next request starts cold and nothing an earlier request left
// running can compile into the cache after the drop.
func (s *server) dropCompiled(ctx context.Context, t testing.TB) {
	t.Helper()
	s.awaitIdle(ctx, t)
	s.exec(ctx, t, "SYSTEM DROP COMPILED EXPRESSION CACHE")
}

// idleBudget bounds how long awaitIdle waits for other queries to finish,
// and idlePoll how often it looks.
const (
	idleBudget = time.Minute
	idlePoll   = 50 * time.Millisecond
)

// awaitIdle waits until system.processes lists no query but its own.
func (s *server) awaitIdle(ctx context.Context, t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(idleBudget)
	for {
		var n uint64
		if err := s.admin.Conn().QueryRow(ctx, "SELECT count() FROM system.processes WHERE query_id != queryID()").Scan(&n); err != nil {
			t.Fatalf("%s: read system.processes: %v", s.image, err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d queries still running after %s", s.image, n, idleBudget)
		}
		time.Sleep(idlePoll)
	}
}

// compiledEntries reads how many compiled functions the server holds once
// every query in flight has finished.
func (s *server) compiledEntries(ctx context.Context, t testing.TB) int64 {
	t.Helper()
	s.awaitIdle(ctx, t)
	var n int64
	if err := s.admin.Conn().QueryRow(ctx, "SELECT toInt64(value) FROM system.metrics WHERE metric = ?", compiledCacheMetric).Scan(&n); err != nil {
		t.Fatalf("%s: read %s: %v", s.image, compiledCacheMetric, err)
	}
	return n
}

// jitMode is how a request configures the regular-expression compiler.
type jitMode string

const (
	// jitOff forces every pattern through RE2.
	jitOff jitMode = "off"
	// jitNow compiles every eligible pattern on its first evaluation.
	jitNow jitMode = "on"
	// jitDefault leaves both settings at the server's defaults, so a
	// pattern compiles once it has been evaluated the default number of
	// times — the path a repeated dashboard query takes in production.
	jitDefault jitMode = "default"
)

// withMode attaches mode's settings to a request context.
func withMode(ctx context.Context, mode jitMode) context.Context {
	switch mode {
	case jitOff:
		return chclient.WithQuerySetting(ctx, settingCompileRegexp, 0)
	case jitNow:
		ctx = chclient.WithQuerySetting(ctx, settingCompileRegexp, 1)
		return chclient.WithQuerySetting(ctx, settingMinCountRegexp, minCountCompileNow)
	}
	return ctx
}

// handlers is the production Prometheus and Loki API surface over one client.
type handlers struct {
	mux *http.ServeMux
}

func newHandlers(client *chclient.Client) handlers {
	mux := http.NewServeMux()
	prom.New(client, schema.DefaultOTelMetrics(), nil).Mount(mux)
	loki.New(client, schema.DefaultOTelLogs(), nil).Mount(mux)
	return handlers{mux: mux}
}

// get issues GET path under ctx and returns the 200 body. Any other status
// fails the test.
func (h handlers) get(ctx context.Context, t testing.TB, path string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d: %s", path, rec.Code, body)
	}
	return body
}

// promInstant evaluates a PromQL instant query at ts.
func (h handlers) promInstant(ctx context.Context, t testing.TB, query string, ts time.Time) []byte {
	t.Helper()
	params := url.Values{}
	params.Set("query", query)
	params.Set("time", strconv.FormatInt(ts.Unix(), 10))
	return h.get(ctx, t, "/api/v1/query?"+params.Encode())
}

// promRange evaluates a PromQL range query over [start, end] at step.
func (h handlers) promRange(ctx context.Context, t testing.TB, query string, start, end time.Time, step time.Duration) []byte {
	t.Helper()
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))
	params.Set("step", strconv.FormatInt(int64(step/time.Second), 10))
	return h.get(ctx, t, "/api/v1/query_range?"+params.Encode())
}

// lokiRange evaluates a LogQL range query over [start, end); a metric query
// uses step, a log query returns up to limit lines oldest first.
func (h handlers) lokiRange(ctx context.Context, t testing.TB, query string, start, end time.Time, step time.Duration, limit int) []byte {
	t.Helper()
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	params.Set("step", strconv.FormatInt(int64(step/time.Second), 10))
	params.Set("limit", strconv.Itoa(limit))
	params.Set("direction", "forward")
	return h.get(ctx, t, "/loki/api/v1/query_range?"+params.Encode())
}
