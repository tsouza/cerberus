// Command dead-end-receiver is the Tier-2 migration substrate's null
// notification sink (Layer 14). Grafana's "dead-end" contact point posts
// every alert notification here; it logs each one to a file and answers 200
// OK, but never re-dispatches anywhere — "computes, never pages". /notifications
// reports how many it has captured, so the substrate self-check can assert
// delivery without parsing the log; /notifications/list returns the captured
// bodies themselves (each with its receipt time), for MIG-18's fire/resolve
// diffing to parse.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	listenAddr = ":8090"
	logPath    = "/var/log/dead-end-receiver/notifications.log"
	// logDirPerm/logFilePerm are deliberately narrower than the 0755/0644
	// defaults gosec (G301/G302) flags: this substrate's own container is
	// the only reader, and the log holds real (if synthetic) alert payloads.
	logDirPerm  = 0o750
	logFilePerm = 0o600
	// readHeaderTimeout bounds one request's header read (gosec G114: a bare
	// http.ListenAndServe has no timeout at all, so a slow-header client can
	// hold a connection open indefinitely).
	readHeaderTimeout = 5 * time.Second
	// maxCaptured bounds the in-memory /notifications/list buffer. A
	// substrate run drives at most a handful of scenarios against one
	// receiver instance, so this is generous headroom, not a tuned limit;
	// it exists so a runaway rule group can never turn this test double into
	// an unbounded memory sink.
	maxCaptured = 10_000
)

var (
	mu       sync.Mutex
	count    int
	captured []capturedNotification
)

// capturedNotification is one /webhook POST, as /notifications/list reports
// it: the raw body (Grafana's webhook JSON — left un-decoded so this receiver
// stays notifier-schema-agnostic; the consumer parses it) plus the instant
// this receiver observed it, which fire/resolve diffing needs since the
// payload's own timestamps only cover the alert's OWN evaluation, not
// delivery.
type capturedNotification struct {
	ReceivedAt time.Time       `json:"received_at"`
	Body       json.RawMessage `json:"body"`
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0/1 (distroless has no shell/wget for Docker's exec-form HEALTHCHECK)")
	flag.Parse()
	if *healthcheck {
		os.Exit(probeSelf())
	}

	if err := os.MkdirAll(filepath.Dir(logPath), logDirPerm); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(logPath), err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, logFilePerm)
	if err != nil {
		log.Fatalf("open %s: %v", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		count++
		// A ring-buffer-by-truncation, not append-forever: drop the oldest
		// entry once at capacity, so a long-running stack never grows this
		// unbounded even though a single scenario run never gets close.
		if len(captured) >= maxCaptured {
			captured = captured[1:]
		}
		captured = append(captured, capturedNotification{ReceivedAt: time.Now().UTC(), Body: json.RawMessage(body)})
		mu.Unlock()
		if _, err := logFile.Write(append(body, '\n')); err != nil {
			log.Printf("write notification to log: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/notifications", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"count": count})
	})
	mux.HandleFunc("/notifications/list", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		out := make([]capturedNotification, len(captured))
		copy(out, captured)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]capturedNotification{"notifications": out})
	})
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	log.Fatal(server.ListenAndServe())
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
