// Command bench fires a battery of histogram_quantile queries against cerberus,
// Prometheus, and Mimir, records per-backend latency percentiles + peak
// container CPU/mem, verifies the three backends return numerically-equivalent
// results (a fast query over wrong answers is worthless), and writes RESULTS.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// equivRelTol is the relative tolerance for declaring two backends' float
// results equivalent. Prometheus and Mimir share an engine (exact); cerberus
// computes histogram_quantile in ClickHouse, so a tiny interpolation delta is
// tolerated — anything larger is a real correctness bug, flagged loudly.
const equivRelTol = 1e-4

type backend struct {
	Name      string
	BaseURL   string // includes any API prefix, e.g. mimir's /prometheus
	Container string
	Headers   map[string]string
}

type querySpec struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"` // instant | range
	Query    string `yaml:"query"`
	Range    string `yaml:"range"`    // range only: window back from end (e.g. 1h)
	Step     int    `yaml:"step"`     // range only: seconds
	Lookback string `yaml:"lookback"` // informational
	Probes   string `yaml:"probes"`   // informational: what the shape stresses
}

type manifest struct {
	Metric       string    `json:"metric"`
	MetricBucket string    `json:"metric_bucket"`
	StartUnix    int64     `json:"start_unix"`
	EndUnix      int64     `json:"end_unix"`
	StepInterval int       `json:"step_interval_seconds"`
	Steps        int       `json:"steps"`
	Routes       int       `json:"routes"`
	Instances    int       `json:"instances"`
	Bounds       []float64 `json:"bounds"`
	OtelSeries   int       `json:"otel_series"`
}

type flags struct {
	iters, warmup int
	queriesPath   string
	manifestPath  string
	resultsPath   string
	profile       string
	cerberusURL   string
	promURL       string
	mimirURL      string
	mimirOrgID    string
	cerberusCtr   string
	promCtr       string
	mimirCtr      string
}

func main() {
	log.SetFlags(log.Ltime)
	f := parseFlags()

	man, err := loadManifest(f.manifestPath)
	if err != nil {
		log.Fatalf("manifest: %v", err)
	}
	queries, err := loadQueries(f.queriesPath)
	if err != nil {
		log.Fatalf("queries: %v", err)
	}

	backends := []backend{
		{Name: "cerberus", BaseURL: f.cerberusURL, Container: f.cerberusCtr},
		{Name: "prometheus", BaseURL: f.promURL, Container: f.promCtr},
	}
	if f.mimirURL != "" {
		h := map[string]string{}
		if f.mimirOrgID != "" {
			h["X-Scope-OrgID"] = f.mimirOrgID
		}
		backends = append(backends, backend{Name: "mimir", BaseURL: f.mimirURL, Container: f.mimirCtr, Headers: h})
	}

	// Probe reachability + non-empty data; drop unavailable backends (Mimir is
	// the stretch target — the run still produces cerberus-vs-Prometheus numbers).
	live := []backend{}
	for _, b := range backends {
		if err := probe(b, man); err != nil {
			log.Printf("WARN: backend %s unavailable, skipping: %v", b.Name, err)
			continue
		}
		live = append(live, b)
	}
	if len(live) == 0 {
		log.Fatalf("no reachable backends")
	}
	log.Printf("live backends: %s", backendNames(live))

	// Peak-resource sampler runs for the whole battery.
	ctrs := map[string]string{}
	for _, b := range live {
		if b.Container != "" {
			ctrs[b.Name] = b.Container
		}
	}
	stopStats := make(chan struct{})
	statsDone := make(chan map[string]resStat)
	go sampleStats(ctrs, stopStats, statsDone)

	results := []queryResult{}
	for _, q := range queries {
		log.Printf("query: %s", q.Name)
		qr := queryResult{Spec: q}
		perBackend := map[string]promResult{}
		for _, b := range live {
			lat, sample, err := runQuery(b, q, man, f.iters, f.warmup)
			if err != nil {
				log.Printf("  %s: ERROR %v", b.Name, err)
				qr.Backends = append(qr.Backends, backendResult{Backend: b.Name, Err: err.Error()})
				continue
			}
			qr.Backends = append(qr.Backends, backendResult{Backend: b.Name, Lat: lat})
			perBackend[b.Name] = sample
			log.Printf("  %s: p50=%.1fms p95=%.1fms p99=%.1fms (n=%d)", b.Name,
				ms(lat.p50), ms(lat.p95), ms(lat.p99), lat.n)
		}
		qr.Equiv = checkEquivalence(perBackend)
		if !qr.Equiv.OK {
			log.Printf("  EQUIVALENCE MISMATCH: %s", qr.Equiv.Detail)
		}
		results = append(results, qr)
	}

	close(stopStats)
	stats := <-statsDone

	out := renderResults(f, man, live, results, stats)
	if err := os.WriteFile(f.resultsPath, []byte(out), 0o644); err != nil {
		log.Fatalf("write results: %v", err)
	}
	log.Printf("wrote %s", f.resultsPath)
	fmt.Println(out)
}

func parseFlags() flags {
	var f flags
	flag.IntVar(&f.iters, "iters", envIntOr("BENCH_ITERS", 30), "timed iterations per query")
	flag.IntVar(&f.warmup, "warmup", envIntOr("BENCH_WARMUP", 3), "warmup iterations per query")
	flag.StringVar(&f.queriesPath, "queries", envOr("BENCH_QUERIES", "queries.yaml"), "query battery YAML")
	flag.StringVar(&f.manifestPath, "manifest", envOr("BENCH_MANIFEST", "data-window.json"), "data-window manifest")
	flag.StringVar(&f.resultsPath, "results", envOr("BENCH_RESULTS", "RESULTS.md"), "results markdown output")
	flag.StringVar(&f.profile, "profile", envOr("BENCH_PROFILE", "smoke"), "profile label for the report")
	flag.StringVar(&f.cerberusURL, "cerberus-url", envOr("BENCH_CERBERUS_URL", "http://localhost:49091"), "cerberus base URL")
	flag.StringVar(&f.promURL, "prom-url", envOr("BENCH_PROM_URL", "http://localhost:49090"), "prometheus base URL")
	flag.StringVar(&f.mimirURL, "mimir-url", envOr("BENCH_MIMIR_URL", "http://localhost:49009/prometheus"), "mimir query base URL (empty to skip)")
	flag.StringVar(&f.mimirOrgID, "mimir-org-id", envOr("BENCH_MIMIR_ORGID", ""), "mimir X-Scope-OrgID header")
	flag.StringVar(&f.cerberusCtr, "cerberus-ctr", envOr("BENCH_CERBERUS_CTR", "histbench-cerberus"), "cerberus container name")
	flag.StringVar(&f.promCtr, "prom-ctr", envOr("BENCH_PROM_CTR", "histbench-prometheus"), "prometheus container name")
	flag.StringVar(&f.mimirCtr, "mimir-ctr", envOr("BENCH_MIMIR_CTR", "histbench-mimir"), "mimir container name")
	flag.Parse()
	return f
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envIntOr(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

// --- query execution -------------------------------------------------------

type latency struct {
	all            []time.Duration
	p50, p95, p99  time.Duration
	min, mean, max time.Duration
	n              int
}

func runQuery(b backend, q querySpec, man manifest, iters, warmup int) (latency, promResult, error) {
	u, err := buildURL(b, q, man)
	if err != nil {
		return latency{}, promResult{}, err
	}
	for i := 0; i < warmup; i++ {
		if _, err := doGet(b, u); err != nil {
			return latency{}, promResult{}, err
		}
	}
	var lat latency
	var lastBody []byte
	for i := 0; i < iters; i++ {
		t0 := time.Now()
		body, err := doGet(b, u)
		d := time.Since(t0)
		if err != nil {
			return latency{}, promResult{}, err
		}
		lat.all = append(lat.all, d)
		lastBody = body
	}
	lat.compute()
	pr, err := parsePromResult(lastBody)
	if err != nil {
		return lat, promResult{}, fmt.Errorf("parse result: %w", err)
	}
	return lat, pr, nil
}

func (l *latency) compute() {
	if len(l.all) == 0 {
		return
	}
	s := append([]time.Duration(nil), l.all...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	l.n = len(s)
	l.min = s[0]
	l.max = s[len(s)-1]
	l.p50 = pct(s, 50)
	l.p95 = pct(s, 95)
	l.p99 = pct(s, 99)
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	l.mean = sum / time.Duration(len(s))
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func buildURL(b backend, q querySpec, man manifest) (string, error) {
	v := url.Values{}
	v.Set("query", q.Query)
	var path string
	switch q.Type {
	case "instant":
		path = "/api/v1/query"
		v.Set("time", strconv.FormatInt(man.EndUnix, 10))
	case "range":
		path = "/api/v1/query_range"
		rng, err := time.ParseDuration(q.Range)
		if err != nil {
			return "", fmt.Errorf("bad range %q: %w", q.Range, err)
		}
		step := q.Step
		if step <= 0 {
			step = 60
		}
		// Clamp the window to the available data so a "24h" shape still runs on
		// the 1h smoke dataset (it just covers the whole window).
		start := man.EndUnix - int64(rng.Seconds())
		if start < man.StartUnix {
			start = man.StartUnix
		}
		end := man.EndUnix
		// Snap start/end onto the step grid, exactly as Grafana's Prometheus
		// datasource does before issuing a range query. Without this the two
		// backends land on phase-shifted grids for a non-step-aligned window:
		// Prometheus anchors its points to the literal `start`, cerberus anchors
		// them to `end`, so their timestamps never coincide and the value-by-
		// timestamp equivalence check sees "no overlapping timestamps" even
		// though the values at each grid point are identical. Snapping start up
		// and end down keeps every sample inside the data window.
		step64 := int64(step)
		start = ((start + step64 - 1) / step64) * step64 // ceil to next step multiple
		end = (end / step64) * step64                    // floor to prev step multiple
		v.Set("start", strconv.FormatInt(start, 10))
		v.Set("end", strconv.FormatInt(end, 10))
		v.Set("step", strconv.Itoa(step))
	default:
		return "", fmt.Errorf("unknown query type %q", q.Type)
	}
	return strings.TrimRight(b.BaseURL, "/") + path + "?" + v.Encode(), nil
}

func doGet(b backend, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range b.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(buf.String(), 300))
	}
	return buf.Bytes(), nil
}

func probe(b backend, man manifest) error {
	u := strings.TrimRight(b.BaseURL, "/") + "/api/v1/query?" +
		url.Values{"query": {"1"}, "time": {strconv.FormatInt(man.EndUnix, 10)}}.Encode()
	_, err := doGet(b, u)
	return err
}

// --- result parsing + equivalence -----------------------------------------

type promResult struct {
	ResultType string
	// series keyed by canonical label string → sorted (ts,value) points
	series map[string][]point
}

type point struct {
	ts int64
	v  float64
}

type rawResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

func parsePromResult(body []byte) (promResult, error) {
	var r rawResp
	if err := json.Unmarshal(body, &r); err != nil {
		return promResult{}, err
	}
	if r.Status != "success" {
		return promResult{}, fmt.Errorf("status=%s", r.Status)
	}
	pr := promResult{ResultType: r.Data.ResultType, series: map[string][]point{}}
	for _, raw := range r.Data.Result {
		var m struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return promResult{}, err
		}
		key := canonLabels(m.Metric)
		var pts []point
		if len(m.Value) == 2 {
			pts = append(pts, mustPoint(m.Value))
		}
		for _, vv := range m.Values {
			pts = append(pts, mustPoint(vv))
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].ts < pts[j].ts })
		pr.series[key] = pts
	}
	return pr, nil
}

func mustPoint(v []any) point {
	var p point
	if len(v) != 2 {
		return p
	}
	switch t := v[0].(type) {
	case float64:
		p.ts = int64(t)
	}
	if s, ok := v[1].(string); ok {
		f, _ := strconv.ParseFloat(s, 64)
		p.v = f
	}
	return p
}

func canonLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte(',')
	}
	return b.String()
}

type equivResult struct {
	OK      bool
	Detail  string
	NSeries int
}

// checkEquivalence compares every available backend against the first
// (cerberus, when present) series-by-series, value-by-value within tolerance.
func checkEquivalence(byBackend map[string]promResult) equivResult {
	if len(byBackend) < 2 {
		return equivResult{OK: true, Detail: "single backend (no cross-check)"}
	}
	// Reference: prefer cerberus, else any deterministic pick.
	refName := "cerberus"
	if _, ok := byBackend[refName]; !ok {
		names := make([]string, 0, len(byBackend))
		for n := range byBackend {
			names = append(names, n)
		}
		sort.Strings(names)
		refName = names[0]
	}
	ref := byBackend[refName]
	nSeries := len(ref.series)
	for name, other := range byBackend {
		if name == refName {
			continue
		}
		if len(other.series) != len(ref.series) {
			return equivResult{
				OK: false, NSeries: nSeries,
				Detail: fmt.Sprintf("%s has %d series, %s has %d", refName, len(ref.series), name, len(other.series)),
			}
		}
		for key, rpts := range ref.series {
			opts, ok := other.series[key]
			if !ok {
				return equivResult{
					OK: false, NSeries: nSeries,
					Detail: fmt.Sprintf("series {%s} present in %s, missing in %s", key, refName, name),
				}
			}
			if d, ok := comparePoints(rpts, opts); !ok {
				return equivResult{
					OK: false, NSeries: nSeries,
					Detail: fmt.Sprintf("%s vs %s series {%s}: %s", refName, name, key, d),
				}
			}
		}
	}
	return equivResult{OK: true, NSeries: nSeries, Detail: fmt.Sprintf("%d series matched within rel tol %.0e", nSeries, equivRelTol)}
}

func comparePoints(a, b []point) (string, bool) {
	// Align on timestamp intersection; both are time-sorted.
	am := map[int64]float64{}
	for _, p := range a {
		am[p.ts] = p.v
	}
	compared := 0
	for _, p := range b {
		av, ok := am[p.ts]
		if !ok {
			continue
		}
		compared++
		if !floatClose(av, p.v) {
			return fmt.Sprintf("at ts=%d ref=%.6g other=%.6g", p.ts, av, p.v), false
		}
	}
	if compared == 0 {
		return "no overlapping timestamps", false
	}
	return "", true
}

func floatClose(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if a == b {
		return true
	}
	diff := math.Abs(a - b)
	scale := math.Max(math.Abs(a), math.Abs(b))
	if scale == 0 {
		return diff < 1e-9
	}
	return diff/scale < equivRelTol
}

// --- docker stats sampler --------------------------------------------------

type resStat struct {
	maxCPU  float64 // percent
	maxMem  float64 // MiB
	samples int
}

func sampleStats(ctrs map[string]string, stop <-chan struct{}, done chan<- map[string]resStat) {
	acc := map[string]resStat{}
	if len(ctrs) == 0 {
		done <- acc
		return
	}
	names := make([]string, 0, len(ctrs))
	byCtr := map[string]string{}
	for backendName, ctr := range ctrs {
		names = append(names, ctr)
		byCtr[ctr] = backendName
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			done <- acc
			return
		case <-ticker.C:
			snap := dockerStats(names)
			for ctr, s := range snap {
				bn := byCtr[ctr]
				cur := acc[bn]
				if s.maxCPU > cur.maxCPU {
					cur.maxCPU = s.maxCPU
				}
				if s.maxMem > cur.maxMem {
					cur.maxMem = s.maxMem
				}
				cur.samples++
				acc[bn] = cur
			}
		}
	}
}

func dockerStats(containers []string) map[string]resStat {
	out := map[string]resStat{}
	args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, containers...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	buf, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(buf)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		cpu := parsePercent(fields[1])
		mem := parseMemMiB(fields[2])
		out[fields[0]] = resStat{maxCPU: cpu, maxMem: mem}
	}
	return out
}

func parsePercent(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return f
}

func parseMemMiB(s string) float64 {
	// e.g. "45.6MiB / 7.5GiB" — take the used side.
	used := strings.TrimSpace(strings.SplitN(s, "/", 2)[0])
	return parseSizeMiB(used)
}

func parseSizeMiB(s string) float64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suf   string
		toMiB float64
	}{
		{"GiB", 1024},
		{"MiB", 1},
		{"KiB", 1.0 / 1024},
		{"GB", 1000 * 1000 * 1000 / 1048576.0},
		{"MB", 1000 * 1000 / 1048576.0},
		{"kB", 1000 / 1048576.0},
		{"B", 1.0 / 1048576.0},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suf) {
			f, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suf)), 64)
			return f * u.toMiB
		}
	}
	return 0
}

// --- rendering -------------------------------------------------------------

type backendResult struct {
	Backend string
	Lat     latency
	Err     string
}

type queryResult struct {
	Spec     querySpec
	Backends []backendResult
	Equiv    equivResult
}

func renderResults(f flags, man manifest, live []backend, results []queryResult, stats map[string]resStat) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Histogram-quantile benchmark — cerberus vs Prometheus vs Mimir\n\n")
	fmt.Fprintf(&b, "_Generated %s • profile `%s` • %d iterations/query (%d warmup)_\n\n",
		time.Now().UTC().Format(time.RFC3339), f.profile, f.iters, f.warmup)

	fmt.Fprintf(&b, "## Dataset\n\n")
	fmt.Fprintf(&b, "| Property | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Metric | `%s` (queried as `%s`) |\n", man.Metric, man.MetricBucket)
	fmt.Fprintf(&b, "| OTel series (ClickHouse rows/step) | %d (%d routes × %d instances) |\n", man.OtelSeries, man.Routes, man.Instances)
	fmt.Fprintf(&b, "| Buckets (le) | %d finite + `+Inf` |\n", len(man.Bounds))
	fmt.Fprintf(&b, "| Samples/series | %d @ %ds |\n", man.Steps, man.StepInterval)
	fmt.Fprintf(&b, "| Time window | %s → %s |\n",
		time.Unix(man.StartUnix, 0).UTC().Format(time.RFC3339), time.Unix(man.EndUnix, 0).UTC().Format(time.RFC3339))
	promSeries := man.OtelSeries * (len(man.Bounds) + 1 + 2)
	fmt.Fprintf(&b, "| Prometheus/Mimir series (exploded) | %d |\n\n", promSeries)

	fmt.Fprintf(&b, "## Latency (lower is better)\n\n")
	fmt.Fprintf(&b, "| Query shape | Type | Backend | p50 ms | p95 ms | p99 ms | mean ms | Equivalence |\n")
	fmt.Fprintf(&b, "|---|---|---|--:|--:|--:|--:|---|\n")
	for _, qr := range results {
		eq := "✅ match"
		if !qr.Equiv.OK {
			eq = "❌ " + qr.Equiv.Detail
		}
		first := true
		for _, br := range qr.Backends {
			shape := ""
			typ := ""
			eqCell := ""
			if first {
				shape = qr.Spec.Name
				typ = qr.Spec.Type
				eqCell = eq
				first = false
			}
			if br.Err != "" {
				fmt.Fprintf(&b, "| %s | %s | %s | ERR | ERR | ERR | ERR | %s |\n",
					shape, typ, br.Backend, eqCell)
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %.1f | %.1f | %.1f | %.1f | %s |\n",
				shape, typ, br.Backend, ms(br.Lat.p50), ms(br.Lat.p95), ms(br.Lat.p99), ms(br.Lat.mean), eqCell)
		}
	}

	fmt.Fprintf(&b, "\n## Peak container resources (sampled ~1 Hz during the run)\n\n")
	fmt.Fprintf(&b, "| Backend | Peak CPU %% | Peak mem (MiB) |\n|---|--:|--:|\n")
	for _, be := range live {
		s := stats[be.Name]
		fmt.Fprintf(&b, "| %s | %.0f | %.0f |\n", be.Name, s.maxCPU, s.maxMem)
	}

	fmt.Fprintf(&b, "\n## Query shapes\n\n")
	for _, qr := range results {
		fmt.Fprintf(&b, "- **%s** (`%s`): %s\n  ```promql\n  %s\n  ```\n",
			qr.Spec.Name, qr.Spec.Type, qr.Spec.Probes, qr.Spec.Query)
	}

	anyMismatch := false
	for _, qr := range results {
		if !qr.Equiv.OK {
			anyMismatch = true
		}
	}
	fmt.Fprintf(&b, "\n## Equivalence verdict\n\n")
	if anyMismatch {
		fmt.Fprintf(&b, "⚠️ **One or more query shapes returned non-equivalent results across backends.** "+
			"The latency numbers for those rows compare different answers and must not be published until the mismatch is understood.\n")
	} else {
		fmt.Fprintf(&b, "✅ All query shapes returned equivalent results across the live backends (relative tolerance %.0e). The latency comparison is apples-to-apples.\n", equivRelTol)
	}
	return b.String()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// --- loaders + tiny flag shim ----------------------------------------------

func loadManifest(p string) (manifest, error) {
	var m manifest
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func loadQueries(p string) ([]querySpec, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Queries []querySpec `yaml:"queries"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc.Queries, nil
}

func backendNames(bs []backend) string {
	n := make([]string, len(bs))
	for i, b := range bs {
		n[i] = b.Name
	}
	return strings.Join(n, ", ")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
