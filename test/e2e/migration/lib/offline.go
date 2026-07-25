package lib

import "os"

// blackholeProxy points a child process's proxy variables at a closed loopback
// port. Every Tier-0 command is documented as offline; routing its HTTP client
// through a port nothing listens on makes that a property of the run rather
// than a claim about the tool, so a scenario cannot pass because the machine
// running it happened to have network access.
const blackholeProxy = "http://127.0.0.1:1"

// unreachableClickHouseAddr is the ClickHouse address an offline child is given.
// A renderer that opens a connection fails against a closed port instead of
// quietly succeeding against a database the developer has running locally.
const unreachableClickHouseAddr = "127.0.0.1:1"

// pathEnv is forwarded from the parent so a child that shells out to a toolchain
// binary still resolves it. It is the only ambient variable that survives.
const pathEnv = "PATH"

// OfflineEnv builds the complete environment for an offline scenario command:
// blackholed proxies, an unreachable ClickHouse address, and nothing else the
// developer's shell might be exporting. extra is appended last, so a case's own
// CERBERUS_* settings win over the defaults.
func OfflineEnv(extra ...string) []string {
	env := []string{
		pathEnv + "=" + os.Getenv(pathEnv),
		"HTTP_PROXY=" + blackholeProxy,
		"HTTPS_PROXY=" + blackholeProxy,
		"http_proxy=" + blackholeProxy,
		"https_proxy=" + blackholeProxy,
		// An empty NO_PROXY closes the bypass the proxy variables would
		// otherwise offer for loopback and in-cluster hosts.
		"NO_PROXY=",
		"no_proxy=",
		"CERBERUS_CH_ADDR=" + unreachableClickHouseAddr,
	}
	return append(env, extra...)
}
