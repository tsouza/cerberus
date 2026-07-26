// Command query-write-fanout is the Tier-2 migration substrate's one
// Grafana-facing Prometheus endpoint (Layer 14, MIG-09). It exists because
// Grafana's recording-rule write-back always posts to the SAME datasource
// URL a rule's own `data[].datasourceUid` names for its QUERY leg —
// confirmed empirically against Grafana 12.2.9: neither the file-provisioning
// YAML's `record.target_datasource_uid` field (silently accepted and
// dropped — the alerting-as-code YAML schema does not carry it yet, even
// though the same field works over the REST provisioning API) nor the
// `[recording_rules] default_datasource_uid` ini fallback
// (docker-compose.ruler.yml) actually redirects a file-provisioned rule's
// write leg away from its query datasource. Cerberus is a query-only
// gateway and implements no Remote Write receiver, so a rule that queries
// cerberus directly 404s on every write attempt forever.
//
// This binary makes the ONE datasource Grafana is given do both jobs by
// routing on path: a GET (the query leg — /api/v1/query, /api/v1/query_range,
// /api/v1/label/*, /api/v1/series, …) reverse-proxies to cerberus, so
// "Grafana evaluates the rule against cerberus" stays literally true; a POST
// to writePath (the write leg — Remote Write's own fixed path) reverse-proxies
// to write-relay, the plain Prometheus this substrate's write-relay/prometheus.yml
// stands up as a remote-write-compatible landing zone. Neither backend needs
// to know the other exists, and cerberus never accepts a write — the split
// happens entirely in front of both.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

const (
	// listenAddr is this binary's own listen address inside the compose
	// network.
	listenAddr = ":8095"
	// writePath is the fixed path Prometheus's Remote Write protocol posts
	// to, and therefore the one path this fanout routes to the write
	// target instead of the query target.
	writePath = "/api/v1/write"
	// readHeaderTimeout bounds one request's header read (gosec G114: a bare
	// http.ListenAndServe has no timeout at all, so a slow-header client
	// could hold a connection open indefinitely).
	readHeaderTimeout = 5 * time.Second
	// proxyDialTimeout bounds establishing the upstream connection for one
	// proxied request.
	proxyDialTimeout = 10 * time.Second

	queryTargetEnv = "FANOUT_QUERY_TARGET"
	writeTargetEnv = "FANOUT_WRITE_TARGET"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false,
		"probe /healthz and exit 0/1 (distroless: no shell/wget for Docker's exec-form HEALTHCHECK)")
	flag.Parse()
	if *healthcheck {
		os.Exit(probeSelf())
	}

	queryTarget := mustParseTargetEnv(queryTargetEnv)
	writeTarget := mustParseTargetEnv(writeTargetEnv)
	queryProxy := newProxy(queryTarget)
	writeProxy := newProxy(writeTarget)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == writePath {
			writeProxy.ServeHTTP(w, r)
			return
		}
		queryProxy.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	log.Printf("query-write-fanout listening on %s (query -> %s, write -> %s)", listenAddr, queryTarget, writeTarget)
	log.Fatal(server.ListenAndServe())
}

// mustParseTargetEnv reads and parses a required upstream base URL from the
// environment, failing fast (a misconfigured fanout that silently proxied
// nowhere would be far harder to diagnose than a refusal to start).
func mustParseTargetEnv(name string) *url.URL {
	raw := os.Getenv(name)
	if raw == "" {
		log.Fatalf("query-write-fanout: %s is not set", name)
	}
	target, err := url.Parse(raw)
	if err != nil {
		// %q already quotes/escapes raw, so control characters from the env
		// var can't forge a fake log line — gosec's taint tracking doesn't
		// credit the verb, only the source.
		log.Fatalf("query-write-fanout: %s=%q: %v", name, raw, err) //nolint:gosec // %q escapes control chars
	}
	return target
}

// newProxy builds a reverse proxy to target with a bounded dial timeout, so
// an upstream that never accepts a connection fails the request rather than
// hanging it indefinitely.
func newProxy(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: proxyDialTimeout}).DialContext
	proxy.Transport = transport
	return proxy
}

// probeSelf is the Docker exec-form HEALTHCHECK body: it hits the server's
// own /healthz over loopback and translates the result into an exit code.
func probeSelf() int {
	resp, err := http.Get("http://127.0.0.1" + listenAddr + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	_ = resp.Body.Close()
	return 0
}
