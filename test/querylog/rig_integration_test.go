//go:build integration

package querylog

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/network"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/engine"
)

// The pinned builds: the supported floor (versions.yaml min_clickhouse) and
// the first line whose server maintains system.all_query_log. Exact tags,
// because each test asserts what that specific build does.
const (
	floorImage = "clickhouse/clickhouse-server:24.8.14.39-alpine"
	unionImage = "clickhouse/clickhouse-server:26.8.10.6-alpine"
)

// Container resource bounds: every server here holds a few hundred thousand
// rows, so a runaway server must burn a bounded share of the host.
const (
	serverNanoCPUs    = 1_000_000_000 // one CPU
	serverMemoryBytes = 2 << 30       // 2 GiB
)

// Credentials of the administrative user every container boots with; it
// carries access management so a test can create a restricted user. The
// cluster definition (testdata/cluster.xml) reaches the other shard with it.
const (
	adminUser     = "cerberus"
	adminPassword = "cerberus"
	serverDB      = "default"
)

// serverBootBudget covers a cold image pull plus server startup.
const serverBootBudget = 5 * time.Minute

// shardRows is how many rows each shard's local table holds, so a
// Distributed read's initiator totals are exactly twice a child's.
const shardRows = 100_000

// Network aliases of the two shards, as testdata/cluster.xml names them.
const (
	aliasA = "ch-a"
	aliasB = "ch-b"
)

// Server-side paths the config fixtures are mounted at.
const (
	configDir     = "/etc/clickhouse-server/config.d/"
	serverDataDir = "/var/lib/clickhouse"
)

// node is one booted ClickHouse server.
type node struct {
	image string
	addr  string
	ctr   *tcclickhouse.ClickHouseContainer
	// admin is the administrative connection; it never dispatches a query
	// under measurement.
	admin *chclient.Client
	// terminateOpts are applied when the server is terminated.
	terminateOpts []testcontainers.TerminateOption
}

// nodeOptions customises one server.
type nodeOptions struct {
	// configs are testdata file names mounted into config.d.
	configs []string
	// network and alias attach the server to a shared network under a name.
	network *testcontainers.DockerNetwork
	alias   string
	// volume, when set, holds the server's data directory, so a later server
	// booted on the same volume sees this one's tables and query log.
	volume string
	// removeVolume deletes volume when this server is terminated; set on the
	// last server to use it.
	removeVolume bool
}

// startNode boots image under the package's CPU and memory limits and
// returns it with an administrative client. The container is terminated when
// the test ends; its data volume is kept for a later server to reuse.
func startNode(ctx context.Context, t *testing.T, image string, opts nodeOptions) *node {
	t.Helper()
	bootCtx, cancel := context.WithTimeout(ctx, serverBootBudget)
	defer cancel()

	custom := []testcontainers.ContainerCustomizer{
		tcclickhouse.WithUsername(adminUser),
		tcclickhouse.WithPassword(adminPassword),
		tcclickhouse.WithDatabase(serverDB),
		testcontainers.WithEnv(map[string]string{"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1"}),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.NanoCPUs = serverNanoCPUs
			hc.Memory = serverMemoryBytes
		}),
	}
	for _, name := range opts.configs {
		custom = append(custom, testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      fixturePath(t, name),
			ContainerFilePath: configDir + name,
			FileMode:          0o644,
		}))
	}
	if opts.network != nil {
		custom = append(custom, network.WithNetwork([]string{opts.alias}, opts.network))
	}
	if opts.volume != "" {
		custom = append(custom, testcontainers.WithMounts(testcontainers.VolumeMount(opts.volume, serverDataDir)))
	}
	ctr, err := tcclickhouse.Run(bootCtx, image, custom...)
	if err != nil {
		t.Fatalf("start %s: %v", image, err)
	}
	n := &node{image: image, ctr: ctr}
	if opts.removeVolume {
		n.terminateOpts = append(n.terminateOpts, testcontainers.RemoveVolumes(opts.volume))
	}
	t.Cleanup(func() { n.stop(t) })

	host, err := ctr.Host(bootCtx)
	if err != nil {
		t.Fatalf("%s host: %v", image, err)
	}
	port, err := ctr.MappedPort(bootCtx, "9000/tcp")
	if err != nil {
		t.Fatalf("%s port: %v", image, err)
	}
	n.addr = net.JoinHostPort(host, port.Port())
	n.admin = newClient(t, chclient.Config{Addr: n.addr}, adminUser, adminPassword)
	return n
}

// stop terminates the server; later calls are no-ops.
func (n *node) stop(t *testing.T) {
	t.Helper()
	if n.ctr == nil {
		return
	}
	if err := n.ctr.Terminate(context.Background(), n.terminateOpts...); err != nil {
		t.Errorf("%s: terminate: %v", n.image, err)
	}
	n.ctr = nil
}

// fixturePath is the absolute path of a testdata file.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return p
}

// newClient opens a cerberus data-plane client. cfg supplies the address(es)
// and any extra knobs; credentials and the database are filled in.
func newClient(t *testing.T, cfg chclient.Config, user, password string) *chclient.Client {
	t.Helper()
	cfg.Database = serverDB
	cfg.Username = user
	cfg.Password = password
	cfg.BreakerDisabled = true
	c, err := chclient.New(cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// failoverClient opens a client whose first address refuses connections, so
// every connection it opens fails over, in order, to n — the reconciler's
// connection landing on a different server than the one that ran a query.
func failoverClient(t *testing.T, n *node) *chclient.Client {
	t.Helper()
	return newClient(t, chclient.Config{
		Addr:             n.addr,
		Addrs:            []string{refusedAddr(t), n.addr},
		ConnOpenStrategy: clickhouse.ConnOpenInOrder,
	}, adminUser, adminPassword)
}

// refusedAddr returns a loopback address nothing listens on.
func refusedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release %s: %v", addr, err)
	}
	return addr
}

// exec runs a statement on the administrative connection.
func (n *node) exec(ctx context.Context, t *testing.T, stmt string, args ...any) {
	t.Helper()
	if err := n.admin.Conn().Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %s: %v", n.image, stmt, err)
	}
}

// uint64Of runs a single-value query on the administrative connection.
func (n *node) uint64Of(ctx context.Context, t *testing.T, query string, args ...any) uint64 {
	t.Helper()
	var v uint64
	if err := n.admin.Conn().QueryRow(ctx, query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %s: %v", n.image, query, err)
	}
	return v
}

// flushLogs makes every finished query visible in the server's query log.
func (n *node) flushLogs(ctx context.Context, t *testing.T) {
	t.Helper()
	n.exec(ctx, t, "SYSTEM FLUSH LOGS")
}

// seedShard creates the shard-local table and the Distributed table over the
// querylog cluster.
func (n *node) seedShard(ctx context.Context, t *testing.T) {
	t.Helper()
	n.exec(ctx, t, "CREATE TABLE samples (x UInt64) ENGINE = MergeTree ORDER BY x")
	n.exec(ctx, t, "INSERT INTO samples SELECT number FROM numbers("+strconv.Itoa(shardRows)+")")
	n.exec(ctx, t, "CREATE TABLE samples_all AS samples ENGINE = Distributed(querylog, default, samples)")
}

// dispatch runs query through c exactly as the engine dispatches a stamped
// statement: log_comment carries shape, and when tracker is non-nil the
// packet path's actuals capture is armed on it (progress + ProfileEvents
// recorded into tracker, the query id claimed at dispatch). A nil tracker is
// a stamped query no packet observation in this process covers — one another
// cerberus process dispatched.
func dispatch(ctx context.Context, t *testing.T, c *chclient.Client, tracker *actuals.Tracker, shape, query string) {
	t.Helper()
	ctx = chclient.WithProgressFor(ctx, "promql")
	ctx = chclient.WithQuerySetting(ctx, "log_comment", shape)
	if tracker != nil {
		ctx = chclient.WithActualsCapture(ctx, tracker, shape)
	}
	if _, err := c.QueryStrings(ctx, query); err != nil {
		t.Fatalf("dispatch %s: %v", shape, err)
	}
}

// actualsConfig is the reconciler configuration every test uses: logs are
// flushed explicitly before each read, so nothing needs to settle, and the
// lookback comfortably covers a server upgrade's restart.
func actualsConfig() actuals.Config {
	cfg := actuals.DefaultConfig()
	cfg.Enabled = true
	cfg.QueryLogSettleDelay = 0
	cfg.QueryLogLookback = 10 * time.Minute
	return cfg
}

// reconcile runs polls reconciliation passes of a reconciler reading through
// c into tracker, from the union table when union is set.
func reconcile(ctx context.Context, t *testing.T, c *chclient.Client, tracker *actuals.Tracker, union bool, polls int) {
	t.Helper()
	r := engine.NewQueryLogActualsReconciler(c, tracker, actualsConfig(), func() bool { return union }, nil)
	for range polls {
		r.Poll(ctx)
	}
}

// observations is shape's observation count and actual-rows EMA in tracker,
// zero when the tracker has none.
func observations(tracker *actuals.Tracker, shape string) (int, float64) {
	report, ok := tracker.Snapshot(shape)
	if !ok {
		return 0, 0
	}
	return report.Observations, report.ActualEMARows
}
