package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/config"
)

// TestBuildDualStackServer_CarriesConfiguredTimeouts asserts every
// CERBERUS_HTTP_* timeout / size knob threads onto the http.Server the dual
// stack builder returns — the wiring that makes the timeouts more than dead
// config.
func TestBuildDualStackServer_CarriesConfiguredTimeouts(t *testing.T) {
	t.Parallel()
	hs := config.HTTPServerConfig{
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	mux := http.NewServeMux()
	srv := buildDualStackServer(":0", hs, mux, nil)

	if srv.ReadTimeout != hs.ReadTimeout {
		t.Errorf("ReadTimeout = %v; want %v", srv.ReadTimeout, hs.ReadTimeout)
	}
	if srv.ReadHeaderTimeout != hs.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v; want %v", srv.ReadHeaderTimeout, hs.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != hs.WriteTimeout {
		t.Errorf("WriteTimeout = %v; want %v", srv.WriteTimeout, hs.WriteTimeout)
	}
	if srv.IdleTimeout != hs.IdleTimeout {
		t.Errorf("IdleTimeout = %v; want %v", srv.IdleTimeout, hs.IdleTimeout)
	}
	if srv.MaxHeaderBytes != hs.MaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d; want %d", srv.MaxHeaderBytes, hs.MaxHeaderBytes)
	}
}

// TestBuildDualStackServer_StreamingSafeDefaults pins that the streaming-safe
// zero defaults (read/write unlimited) survive the builder unchanged.
func TestBuildDualStackServer_StreamingSafeDefaults(t *testing.T) {
	t.Parallel()
	hs := config.HTTPServerConfig{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second}
	srv := buildDualStackServer(":0", hs, http.NewServeMux(), nil)
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("read/write timeouts = (%v,%v); want both 0 (streaming-safe)", srv.ReadTimeout, srv.WriteTimeout)
	}
}
