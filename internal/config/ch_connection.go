package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/spf13/viper"

	"github.com/tsouza/cerberus/internal/chclient"
)

// chExtra is the parsed result of the "full surface" ClickHouse connection
// knobs (protocol, multi-host, TLS, compression, buffers, HTTP-only). It is an
// internal carrier between chExtraFromEnv and FromEnv; the fields land flat on
// chclient.Config, which is the one place the driver options are assembled.
type chExtra struct {
	Addrs                []string
	Protocol             clickhouse.Protocol
	ConnOpenStrategy     clickhouse.ConnOpenStrategy
	ReadTimeout          time.Duration
	TLS                  *tls.Config
	Compression          *clickhouse.Compression
	BlockBufferSize      uint8
	MaxCompressionBuffer int
	FreeBufOnConnRelease bool
	ColumnarMatrixDecode bool
	Debug                bool
	HTTPHeaders          map[string]string
	HTTPURLPath          string
	HTTPMaxConnsPerHost  int
	HTTPProxyURL         *url.URL

	// protocolHTTP records whether Protocol resolved to HTTP, so the
	// cross-setting checks in FromEnv can reject HTTP-only knobs under native
	// without re-deriving the enum.
	protocolHTTP bool

	// raw* fields preserve the operator's literal input for the cross-setting
	// dependency checks that run in FromEnv (where every parsed value, including
	// the query timeout, is visible). Validation that needs only a single knob
	// happens here; validation that spans knobs from different groups defers to
	// chValidateDependencies.
	rawTLSEnabled      bool
	rawTLSCAFile       string
	rawTLSCertFile     string
	rawTLSKeyFile      string
	rawTLSServerName   string
	rawTLSSkipVerify   bool
	rawCompression     string
	rawCompressionLvl  int
	rawCompressionLvlS string
	rawHTTPHeaders     string
	rawHTTPURLPath     string
	rawHTTPMaxConns    int
	rawHTTPProxyURL    string
	rawConnStrategy    string
	singleAddr         bool
}

// keepAliveInputs groups the four already-parsed TCP-keepalive knobs so they
// thread through assembleCHConfig as one field, not four.
type keepAliveInputs struct {
	enabled  bool
	idle     time.Duration
	interval time.Duration
	probes   int
}

// chConfigInputs carries every already-parsed, already-validated scalar
// FromEnv resolved for the ClickHouse client, so assembling the
// chclient.Config is a single helper call rather than a long inline literal
// that pushes FromEnv past the statement-count linter.
type chConfigInputs struct {
	database        string
	username        string
	password        string
	dial            time.Duration
	maxOpen         int
	maxIdle         int
	connMaxLifetime time.Duration
	keepAlive       keepAliveInputs
	maxSamples      int64
	maxMemory       int64
	queryTimeout    time.Duration
	breaker         breakerConfig
	extra           chExtra
}

// assembleCHConfig builds the chclient.Config from the parsed scalars and
// overlays the full-surface connection knobs (applyCHExtra). It is the single
// place the driver config is assembled; FromEnv hands it the validated inputs.
func assembleCHConfig(in chConfigInputs) chclient.Config {
	cc := chclient.Config{
		Database:            in.database,
		Username:            in.username,
		Password:            in.password,
		DialTimeout:         in.dial,
		MaxOpenConns:        in.maxOpen,
		MaxIdleConns:        in.maxIdle,
		ConnMaxLifetime:     in.connMaxLifetime,
		KeepAliveEnabled:    in.keepAlive.enabled,
		KeepAliveIdle:       in.keepAlive.idle,
		KeepAliveInterval:   in.keepAlive.interval,
		KeepAliveProbes:     in.keepAlive.probes,
		MaxQuerySamples:     in.maxSamples,
		MaxQueryMemoryBytes: in.maxMemory,
		QueryTimeout:        in.queryTimeout,
		BreakerThreshold:    in.breaker.Threshold,
		BreakerWindow:       in.breaker.Window,
		BreakerOpenInterval: in.breaker.OpenInterval,
		BreakerDisabled:     in.breaker.Disabled,
	}
	applyCHExtra(&cc, in.extra)
	return cc
}

// surfaceConfig bundles the parsed full-surface connection knobs, the HTTP
// server timeouts, and the Loki tail write timeout. It is the single carrier
// surfaceFromEnv returns so FromEnv threads one value, not four, keeping its
// branch count (cyclomatic complexity) under the linter ceiling.
type surfaceConfig struct {
	ch                   chExtra
	httpServer           HTTPServerConfig
	lokiTailWriteTimeout time.Duration
}

// poolKnobs carries the pool sizing FromEnv parsed plus whether the operator
// EXPLICITLY set MAX_IDLE_CONNS (viper.IsSet), so the idle<=open dependency
// rule fires only on operator input and not on a defaulted idle.
type poolKnobs struct {
	maxOpen      int
	maxIdle      int
	idleExplicit bool
}

// surfaceFromEnv parses + cross-validates the full-surface ClickHouse
// connection knobs, the HTTP server timeouts, and the Loki tail write timeout
// in one place. The query / pool knobs (owned by FromEnv) are threaded in so
// the cross-setting dependency checks that span groups can run here rather than
// inflating FromEnv.
func surfaceFromEnv(v *viper.Viper, queryTimeout time.Duration, pools poolKnobs) (surfaceConfig, error) {
	ch, err := chExtraFromEnv(v)
	if err != nil {
		return surfaceConfig{}, err
	}
	if err := chValidateDependencies(ch, queryTimeout, pools); err != nil {
		return surfaceConfig{}, err
	}
	httpServer, err := httpServerFromEnv(v)
	if err != nil {
		return surfaceConfig{}, err
	}
	lokiTailWriteTimeout, err := getDuration(v, envLokiTailWriteTO)
	if err != nil {
		return surfaceConfig{}, err
	}
	if lokiTailWriteTimeout <= 0 {
		return surfaceConfig{}, fmt.Errorf("%s: must be > 0, got %s", envLokiTailWriteTO, lokiTailWriteTimeout)
	}
	return surfaceConfig{ch: ch, httpServer: httpServer, lokiTailWriteTimeout: lokiTailWriteTimeout}, nil
}

// chExtraFromEnv parses every "full surface" ClickHouse connection knob from
// the viper loader, applying per-field fail-fast validation (unknown enum,
// out-of-range buffer, malformed URL / header list, missing TLS file). The
// CROSS-setting dependency checks (TLS sub-knobs require enable, HTTP knobs
// require http, compression level requires a method, …) run in FromEnv via
// chValidateDependencies, where the query timeout and pool knobs are also in
// scope. Every unset knob resolves to a zero value that leaves the driver's
// pre-knob default in place, so an operator who sets none of these gets the
// exact connection cerberus has always opened.
func chExtraFromEnv(v *viper.Viper) (chExtra, error) {
	var out chExtra

	// Multi-host address list. CERBERUS_CH_ADDR is comma-separated; trim each
	// entry, drop empties, require at least one. A single host (the common
	// case) yields a one-element slice and singleAddr=true (used to note the
	// pointless round_robin-with-one-host combo).
	addrs, err := parseAddrs(getString(v, envCHAddr))
	if err != nil {
		return chExtra{}, fmt.Errorf("%s: %w", envCHAddr, err)
	}
	out.Addrs = addrs
	out.singleAddr = len(addrs) == 1

	// Protocol enum.
	switch proto := strings.ToLower(getString(v, envCHProtocol)); proto {
	case chProtocolNative:
		out.Protocol = clickhouse.Native
	case chProtocolHTTP:
		out.Protocol = clickhouse.HTTP
		out.protocolHTTP = true
	default:
		return chExtra{}, fmt.Errorf("%s: invalid value %q (want %q or %q)", envCHProtocol, proto, chProtocolNative, chProtocolHTTP)
	}

	// Connection-open strategy enum.
	strategy := strings.ToLower(getString(v, envCHConnOpenStrategy))
	out.rawConnStrategy = strategy
	switch strategy {
	case chConnOpenInOrder:
		out.ConnOpenStrategy = clickhouse.ConnOpenInOrder
	case chConnOpenRoundRobin:
		out.ConnOpenStrategy = clickhouse.ConnOpenRoundRobin
	default:
		return chExtra{}, fmt.Errorf("%s: invalid value %q (want %q or %q)", envCHConnOpenStrategy, strategy, chConnOpenInOrder, chConnOpenRoundRobin)
	}

	// First-class read timeout (empty → derived from QueryTimeout downstream).
	if raw := getString(v, envCHReadTimeout); raw != "" {
		rt, err := getDuration(v, envCHReadTimeout)
		if err != nil {
			return chExtra{}, err
		}
		if rt < 0 {
			return chExtra{}, fmt.Errorf("%s: must be >= 0, got %s", envCHReadTimeout, rt)
		}
		out.ReadTimeout = rt
	}

	// Compression method + level. The cross-method validation (level requires a
	// method; level in the method's range) is split: the method enum is checked
	// here, the level↔method coupling in chValidateDependencies.
	out.rawCompression = strings.ToLower(getString(v, envCHCompression))
	out.rawCompressionLvlS = getString(v, envCHCompressionLevel)
	switch out.rawCompression {
	case chCompressionNone, chCompressionLZ4, chCompressionZSTD:
	default:
		return chExtra{}, fmt.Errorf("%s: invalid value %q (want %q, %q, or %q)", envCHCompression, out.rawCompression, chCompressionNone, chCompressionLZ4, chCompressionZSTD)
	}
	lvl, err := getInt(v, envCHCompressionLevel)
	if err != nil {
		return chExtra{}, err
	}
	out.rawCompressionLvl = lvl
	out.Compression = buildCompression(out.rawCompression, lvl)

	// Block buffer size (uint8 1..255; 0 = unset/driver default).
	bbs, err := getInt(v, envCHBlockBufferSize)
	if err != nil {
		return chExtra{}, err
	}
	if bbs < 0 || bbs > chBlockBufferMax {
		return chExtra{}, fmt.Errorf("%s: must be in 0..%d, got %d", envCHBlockBufferSize, chBlockBufferMax, bbs)
	}
	out.BlockBufferSize = uint8(bbs)

	// Max compression buffer (bytes; 0 = unset/driver default; positive only).
	mcb, err := getInt(v, envCHMaxComprBuffer)
	if err != nil {
		return chExtra{}, err
	}
	if mcb < 0 {
		return chExtra{}, fmt.Errorf("%s: must be >= 0, got %d", envCHMaxComprBuffer, mcb)
	}
	out.MaxCompressionBuffer = mcb

	freeBuf, err := getBool(v, envCHFreeBufOnRelease)
	if err != nil {
		return chExtra{}, err
	}
	out.FreeBufOnConnRelease = freeBuf

	columnarMatrix, err := getBool(v, envColumnarMatrixDecode)
	if err != nil {
		return chExtra{}, err
	}
	out.ColumnarMatrixDecode = columnarMatrix

	debug, err := getBool(v, envCHDebug)
	if err != nil {
		return chExtra{}, err
	}
	out.Debug = debug

	// TLS group: parse the raw inputs; the coherence checks (cert/key pairing,
	// sub-knob requires enable, skip-verify contradicts CA/serverName) run in
	// chValidateDependencies. The *tls.Config is built here only when enabled.
	if err := out.parseTLS(v); err != nil {
		return chExtra{}, err
	}

	// HTTP-protocol-only knobs. Parse + per-field validate here; the
	// "requires Protocol=http" coupling is in chValidateDependencies.
	if err := out.parseHTTP(v); err != nil {
		return chExtra{}, err
	}

	return out, nil
}

// parseAddrs splits a comma-separated CERBERUS_CH_ADDR into a trimmed,
// empty-dropped slice and requires at least one host.
func parseAddrs(raw string) ([]string, error) {
	out := make([]string, 0, 1)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one host is required")
	}
	return out, nil
}

// buildCompression maps the method enum + level onto the driver's
// *clickhouse.Compression, or nil for the "none" method (so the connection
// stays byte-identical to the no-compression default). A level of 0 means
// "unset" and is left at the driver's per-method default.
func buildCompression(method string, level int) *clickhouse.Compression {
	switch method {
	case chCompressionLZ4:
		return &clickhouse.Compression{Method: clickhouse.CompressionLZ4, Level: level}
	case chCompressionZSTD:
		return &clickhouse.Compression{Method: clickhouse.CompressionZSTD, Level: level}
	default:
		return nil
	}
}

// parseTLS reads the CERBERUS_CH_TLS_* raw inputs and, when TLS is enabled,
// builds the *tls.Config (CA pool from CA_FILE, client cert from CERT/KEY for
// mTLS, ServerName, InsecureSkipVerify). A path that is set but unreadable /
// unparseable fails fast. The cross-knob coherence (sub-knobs require enable,
// cert/key both-or-neither, skip-verify vs CA/serverName) is enforced in
// chValidateDependencies, which runs before this is consulted by the caller.
func (c *chExtra) parseTLS(v *viper.Viper) error {
	c.rawTLSCAFile = getString(v, envCHTLSCAFile)
	c.rawTLSCertFile = getString(v, envCHTLSCertFile)
	c.rawTLSKeyFile = getString(v, envCHTLSKeyFile)
	c.rawTLSServerName = getString(v, envCHTLSServerName)

	enabled, err := getBool(v, envCHTLSEnabled)
	if err != nil {
		return err
	}
	c.rawTLSEnabled = enabled

	skip, err := getBool(v, envCHTLSSkipVerify)
	if err != nil {
		return err
	}
	c.rawTLSSkipVerify = skip

	if !enabled {
		// Building the *tls.Config is deferred to chValidateDependencies having
		// already proven the sub-knobs are coherent (which, when disabled, means
		// they are all empty). Leave c.TLS nil → plaintext dial.
		return nil
	}

	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         c.rawTLSServerName,
		InsecureSkipVerify: skip, //nolint:gosec // operator-opt-in; rejected in combo with CA/ServerName
	}
	if c.rawTLSCAFile != "" {
		pool, err := loadCAPool(c.rawTLSCAFile)
		if err != nil {
			return fmt.Errorf("%s: %w", envCHTLSCAFile, err)
		}
		cfg.RootCAs = pool
	}
	if c.rawTLSCertFile != "" && c.rawTLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.rawTLSCertFile, c.rawTLSKeyFile)
		if err != nil {
			return fmt.Errorf("%s / %s: load client key pair: %w", envCHTLSCertFile, envCHTLSKeyFile, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	c.TLS = cfg
	return nil
}

// loadCAPool reads a PEM CA bundle from path and returns an x509 pool. A
// missing file or a bundle with no parseable certificate is an error.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // operator-supplied CA path
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM certificates found in %s", path)
	}
	return pool, nil
}

// parseHTTP reads the CERBERUS_CH_HTTP_* raw inputs and per-field validates
// them (header list shape, proxy URL parse, non-negative per-host cap). The
// "requires Protocol=http" coupling runs in chValidateDependencies.
func (c *chExtra) parseHTTP(v *viper.Viper) error {
	c.rawHTTPHeaders = getString(v, envCHHTTPHeaders)
	c.rawHTTPURLPath = getString(v, envCHHTTPURLPath)
	c.rawHTTPProxyURL = getString(v, envCHHTTPProxyURL)

	headers, err := parseHeaders(c.rawHTTPHeaders)
	if err != nil {
		return fmt.Errorf("%s: %w", envCHHTTPHeaders, err)
	}
	c.HTTPHeaders = headers
	c.HTTPURLPath = c.rawHTTPURLPath

	maxConns, err := getInt(v, envCHHTTPMaxConns)
	if err != nil {
		return err
	}
	if maxConns < 0 {
		return fmt.Errorf("%s: must be >= 0, got %d", envCHHTTPMaxConns, maxConns)
	}
	c.rawHTTPMaxConns = maxConns
	c.HTTPMaxConnsPerHost = maxConns

	if c.rawHTTPProxyURL != "" {
		u, err := url.Parse(c.rawHTTPProxyURL)
		if err != nil {
			return fmt.Errorf("%s: invalid URL %q: %w", envCHHTTPProxyURL, c.rawHTTPProxyURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("%s: invalid URL %q: missing scheme or host", envCHHTTPProxyURL, c.rawHTTPProxyURL)
		}
		c.HTTPProxyURL = u
	}
	return nil
}

// chValidateDependencies enforces the cross-setting dependency matrix: rules
// that span knobs (and the query / pool knobs FromEnv already parsed) so an
// incoherent COMBINATION of individually-valid values is rejected at startup
// with an error naming both knobs. queryTimeout / pools are passed in because
// they are owned by FromEnv, not chExtra.
func chValidateDependencies(c chExtra, queryTimeout time.Duration, pools poolKnobs) error {
	tlsSubKnobSet := c.rawTLSCAFile != "" || c.rawTLSCertFile != "" || c.rawTLSKeyFile != "" ||
		c.rawTLSServerName != "" || c.rawTLSSkipVerify

	// TLS sub-knobs require enable — a silently-ignored TLS config is a
	// security footgun.
	if !c.rawTLSEnabled && tlsSubKnobSet {
		return fmt.Errorf("%s is false but a TLS sub-knob (%s / %s / %s / %s / %s) is set; enable TLS or unset the sub-knob",
			envCHTLSEnabled, envCHTLSCAFile, envCHTLSCertFile, envCHTLSKeyFile, envCHTLSServerName, envCHTLSSkipVerify)
	}

	// TLS cert/key must be BOTH set or BOTH empty (a lone one cannot form a
	// client key pair for mTLS).
	if (c.rawTLSCertFile == "") != (c.rawTLSKeyFile == "") {
		return fmt.Errorf("%s and %s must be set together (got one without the other)", envCHTLSCertFile, envCHTLSKeyFile)
	}

	// skip-verify contradicts CA / serverName: skip-verify makes the driver
	// ignore both, so the combination is incoherent and dangerous.
	if c.rawTLSSkipVerify && c.rawTLSCAFile != "" {
		return fmt.Errorf("%s=true ignores %s; the combination is incoherent — set one, not both", envCHTLSSkipVerify, envCHTLSCAFile)
	}
	if c.rawTLSSkipVerify && c.rawTLSServerName != "" {
		return fmt.Errorf("%s=true ignores %s; the combination is incoherent — set one, not both", envCHTLSSkipVerify, envCHTLSServerName)
	}

	// HTTP-protocol knobs require Protocol=http — under native they would be
	// silently ignored.
	if !c.protocolHTTP {
		httpSet := []struct {
			name string
			set  bool
		}{
			{envCHHTTPHeaders, c.rawHTTPHeaders != ""},
			{envCHHTTPURLPath, c.rawHTTPURLPath != ""},
			{envCHHTTPMaxConns, c.rawHTTPMaxConns > 0},
			{envCHHTTPProxyURL, c.rawHTTPProxyURL != ""},
		}
		for _, k := range httpSet {
			if k.set {
				return fmt.Errorf("%s is set but %s is %q; HTTP-protocol knobs require %s=%s",
					k.name, envCHProtocol, chProtocolNative, envCHProtocol, chProtocolHTTP)
			}
		}
	}

	// Compression level requires a method, and must sit in the method's range.
	if err := validateCompressionLevel(c); err != nil {
		return err
	}

	// Read timeout >= query timeout (when both > 0): a socket read shorter than
	// the query budget would kill legitimate long queries.
	if c.ReadTimeout > 0 && queryTimeout > 0 && c.ReadTimeout < queryTimeout {
		return fmt.Errorf("%s (%s) must be >= %s (%s); a shorter socket read would kill legitimate long queries",
			envCHReadTimeout, c.ReadTimeout, envQueryTimeout, queryTimeout)
	}

	// Idle conns <= open conns. Fires ONLY when the operator EXPLICITLY set
	// MAX_IDLE_CONNS (idleExplicit) — lowering only MAX_OPEN_CONNS below the
	// default idle of 5 is a coherent idiom (clickhouse-go clamps idle to open
	// internally), so a defaulted idle above a lowered open is not rejected.
	if pools.idleExplicit && pools.maxIdle > 0 && pools.maxOpen > 0 && pools.maxIdle > pools.maxOpen {
		return fmt.Errorf("%s (%d) must be <= %s (%d)", envCHMaxIdleConns, pools.maxIdle, envCHMaxOpenConns, pools.maxOpen)
	}

	return nil
}

// validateCompressionLevel enforces the level↔method coupling: a level set
// while the method is "none" is rejected (it would do nothing), and a level
// for lz4 / zstd must sit in that method's documented range.
func validateCompressionLevel(c chExtra) error {
	// "level set" means the operator supplied a non-empty, non-zero value.
	// 0 is "unset / driver default", which is benign for any method.
	levelSet := c.rawCompressionLvlS != "" && c.rawCompressionLvl != 0
	if c.rawCompression == chCompressionNone {
		if levelSet {
			return fmt.Errorf("%s=%d is set but %s=%s; a level needs a compression method (%s or %s)",
				envCHCompressionLevel, c.rawCompressionLvl, envCHCompression, chCompressionNone, chCompressionLZ4, chCompressionZSTD)
		}
		return nil
	}
	if !levelSet {
		return nil
	}
	var lo, hi int
	switch c.rawCompression {
	case chCompressionLZ4:
		lo, hi = chCompressionLZ4MinLevel, chCompressionLZ4MaxLevel
	case chCompressionZSTD:
		lo, hi = chCompressionZSTDMinLevel, chCompressionZSTDMaxLevel
	}
	if c.rawCompressionLvl < lo || c.rawCompressionLvl > hi {
		return fmt.Errorf("%s=%d out of range for %s=%s (valid %d..%d)",
			envCHCompressionLevel, c.rawCompressionLvl, envCHCompression, c.rawCompression, lo, hi)
	}
	return nil
}

// httpServerFromEnv parses the CERBERUS_HTTP_* server-timeout knobs into an
// HTTPServerConfig. Read / write default to 0 (unlimited, streaming-safe);
// the header timeout is the promoted 5s; idle defaults to 120s. The
// "header-timeout <= read-timeout when read-timeout > 0" coupling is enforced
// here since both knobs belong to this group.
func httpServerFromEnv(v *viper.Viper) (HTTPServerConfig, error) {
	readTO, err := getDuration(v, envHTTPReadTimeout)
	if err != nil {
		return HTTPServerConfig{}, err
	}
	if readTO < 0 {
		return HTTPServerConfig{}, fmt.Errorf("%s: must be >= 0, got %s", envHTTPReadTimeout, readTO)
	}
	readHdrTO, err := getDuration(v, envHTTPReadHdrTimeout)
	if err != nil {
		return HTTPServerConfig{}, err
	}
	if readHdrTO < 0 {
		return HTTPServerConfig{}, fmt.Errorf("%s: must be >= 0, got %s", envHTTPReadHdrTimeout, readHdrTO)
	}
	writeTO, err := getDuration(v, envHTTPWriteTimeout)
	if err != nil {
		return HTTPServerConfig{}, err
	}
	if writeTO < 0 {
		return HTTPServerConfig{}, fmt.Errorf("%s: must be >= 0, got %s", envHTTPWriteTimeout, writeTO)
	}
	idleTO, err := getDuration(v, envHTTPIdleTimeout)
	if err != nil {
		return HTTPServerConfig{}, err
	}
	if idleTO < 0 {
		return HTTPServerConfig{}, fmt.Errorf("%s: must be >= 0, got %s", envHTTPIdleTimeout, idleTO)
	}
	maxHdr, err := getInt(v, envHTTPMaxHeaderBytes)
	if err != nil {
		return HTTPServerConfig{}, err
	}
	if maxHdr < 0 {
		return HTTPServerConfig{}, fmt.Errorf("%s: must be >= 0, got %d", envHTTPMaxHeaderBytes, maxHdr)
	}

	// Cross-setting: a header read deadline longer than the whole-request read
	// deadline can never fire — reject the incoherent pair.
	if readTO > 0 && readHdrTO > readTO {
		return HTTPServerConfig{}, fmt.Errorf("%s (%s) must be <= %s (%s)", envHTTPReadHdrTimeout, readHdrTO, envHTTPReadTimeout, readTO)
	}

	return HTTPServerConfig{
		ReadTimeout:       readTO,
		ReadHeaderTimeout: readHdrTO,
		WriteTimeout:      writeTO,
		IdleTimeout:       idleTO,
		MaxHeaderBytes:    maxHdr,
	}, nil
}

// applyCHExtra writes the parsed full-surface connection knobs onto a
// chclient.Config (the single assembly point). Addr stays the canonical scalar
// (Addrs[0]); the multi-host slice is set only when more than one host is
// configured so the bare single-host path is byte-unchanged.
func applyCHExtra(cc *chclient.Config, c chExtra) {
	cc.Addr = c.Addrs[0]
	if len(c.Addrs) > 1 {
		cc.Addrs = c.Addrs
	}
	cc.Protocol = c.Protocol
	cc.ConnOpenStrategy = c.ConnOpenStrategy
	cc.ReadTimeout = c.ReadTimeout
	cc.TLS = c.TLS
	cc.Compression = c.Compression
	cc.BlockBufferSize = c.BlockBufferSize
	cc.MaxCompressionBuffer = c.MaxCompressionBuffer
	cc.FreeBufOnConnRelease = c.FreeBufOnConnRelease
	cc.ColumnarMatrixDecode = c.ColumnarMatrixDecode
	cc.Debug = c.Debug
	cc.HTTPHeaders = c.HTTPHeaders
	cc.HTTPURLPath = c.HTTPURLPath
	cc.HTTPMaxConnsPerHost = c.HTTPMaxConnsPerHost
	cc.HTTPProxyURL = c.HTTPProxyURL
}
