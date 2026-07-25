package lib

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

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

// blackholedProxyVars are the variables that must point at the closed loopback
// port for a child's HTTP clients to have no route out, and bypassVars are the
// ones that must be empty so loopback and in-cluster hosts get no exemption.
var (
	blackholedProxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"}
	bypassVars          = []string{"NO_PROXY", "no_proxy"}
)

// RequireOffline reports whether env leaves a child process with no HTTP route
// out: every proxy variable points at the closed loopback port and neither
// bypass list exempts anything.
//
// This is what "offline" is asserted BY, rather than a claim taken from the
// tool: a scenario that passes under this environment cannot have passed
// because the machine running it happened to have connectivity through a proxy.
// It bounds HTTP clients, which is the only route any `cerberus migrate` block
// in the Tier-0 set has; it is not a syscall-level network jail, and a command
// that opened a raw socket would need a different bound.
func RequireOffline(env []string) error {
	seen := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			seen[k] = v
		}
	}
	var problems []string
	for _, name := range blackholedProxyVars {
		v, ok := seen[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s is unset, leaving the child's default route open", name))
		case v != blackholeProxy:
			problems = append(problems, fmt.Sprintf("%s=%q does not point at the blackhole %q", name, v, blackholeProxy))
		}
	}
	// A bypass list that is absent exempts nothing, which is what "offline"
	// needs; one that names anything at all reopens a route around the
	// blackhole for exactly the loopback and in-cluster hosts a scenario would
	// otherwise reach.
	for _, name := range bypassVars {
		if v := seen[name]; v != "" {
			problems = append(problems, fmt.Sprintf("%s=%q exempts hosts from the blackhole", name, v))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("migration harness: the command did not run offline: %s", strings.Join(problems, "; "))
}
