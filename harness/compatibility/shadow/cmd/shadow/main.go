// Command shadow is the RC3 R3.9 shadow-mode differential testing CLI.
//
// It reads a corpus of PromQL queries, evaluates each one against cerberus
// (native path, over HTTP) and an in-process PromQL oracle, and diffs the
// two result vectors. The oracle is stubbed until R3.10 lands
// `internal/promshim/local/`.
//
// See ../../README.md for the strategy matrix, exit codes, and corpus format.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/harness/compatibility/shadow"
)

// Strategy selects which evaluator(s) to consult and whether disagreement is fatal.
type Strategy string

const (
	StrategyPreferNative Strategy = "prefer-native"
	StrategyForceNative  Strategy = "force-native"
	StrategyOracleOnly   Strategy = "oracle-only"
)

// Exit codes (kept in sync with README).
const (
	exitOK              = 0
	exitDiffForceNative = 1
	exitSetupFailure    = 2
	exitOracleMissing   = 3
)

// OracleProvider is the seam R3.10 fills. Until then, noopOracle returns
// ErrOracleSkipped for every query.
type OracleProvider interface {
	Evaluate(ctx context.Context, expr string) (shadow.VectorResult, error)
}

// ErrOracleSkipped signals the noop oracle has been invoked. Callers handle
// per strategy.
var ErrOracleSkipped = errors.New("oracle: skipped (R3.10 not yet wired)")

type noopOracle struct{}

func (noopOracle) Evaluate(_ context.Context, _ string) (shadow.VectorResult, error) {
	return shadow.VectorResult{}, ErrOracleSkipped
}

// QueryResult is the per-query record emitted in the report.
type QueryResult struct {
	Name             string      `json:"name"`
	Expr             string      `json:"expr"`
	Strategy         string      `json:"strategy"`
	NativeError      string      `json:"native_error,omitempty"`
	OracleError      string      `json:"oracle_error,omitempty"`
	OracleSkipped    bool        `json:"oracle_skipped,omitempty"`
	Diff             *shadow.Diff `json:"diff,omitempty"`
	NativeSeriesLen  int         `json:"native_series_len"`
	OracleSeriesLen  int         `json:"oracle_series_len"`
	DurationNativeMs int64       `json:"duration_native_ms"`
	DurationOracleMs int64       `json:"duration_oracle_ms"`
}

// Report is the JSON written to --report.
type Report struct {
	Strategy        string        `json:"strategy"`
	CerberusURL     string        `json:"cerberus_url"`
	Corpus          string        `json:"corpus"`
	TotalQueries    int           `json:"total_queries"`
	Diffs           int           `json:"diffs"`
	NativeErrors    int           `json:"native_errors"`
	OracleSkipped   int           `json:"oracle_skipped"`
	Queries         []QueryResult `json:"queries"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shadow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	corpusPath := fs.String("corpus", "", "path to a corpus TXTAR file (required)")
	strategyFlag := fs.String("strategy", string(StrategyPreferNative),
		"one of: prefer-native, force-native, oracle-only")
	cerberusURL := fs.String("cerberus-url", os.Getenv("CERBERUS_URL"),
		"base URL of running cerberus (e.g. http://localhost:9090). Falls back to $CERBERUS_URL.")
	reportPath := fs.String("report", "", "if set, write a JSON report to this path")
	timeoutSec := fs.Int("timeout", 30, "per-query timeout in seconds")
	queryTimestamp := fs.Int64("at", 0, "evaluate instant queries at this UNIX timestamp (0 = now)")

	if err := fs.Parse(args); err != nil {
		return exitSetupFailure
	}

	strategy := Strategy(*strategyFlag)
	switch strategy {
	case StrategyPreferNative, StrategyForceNative, StrategyOracleOnly:
	default:
		fmt.Fprintf(stderr, "unknown strategy %q (want prefer-native | force-native | oracle-only)\n", *strategyFlag)
		return exitSetupFailure
	}

	if *corpusPath == "" {
		fmt.Fprintln(stderr, "--corpus is required")
		return exitSetupFailure
	}

	queries, err := shadow.LoadCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintf(stderr, "load corpus: %v\n", err)
		return exitSetupFailure
	}

	if strategy != StrategyOracleOnly && *cerberusURL == "" {
		fmt.Fprintln(stderr, "native path required but neither --cerberus-url nor $CERBERUS_URL set")
		return exitSetupFailure
	}

	oracle := OracleProvider(noopOracle{})

	// Under force-native / oracle-only, an unavailable oracle is fatal up front.
	if strategy == StrategyForceNative || strategy == StrategyOracleOnly {
		if _, isNoop := oracle.(noopOracle); isNoop {
			fmt.Fprintf(stderr, "strategy %q requires an oracle but R3.10 has not landed; aborting\n", strategy)
			return exitOracleMissing
		}
	}

	at := time.Now()
	if *queryTimestamp != 0 {
		at = time.Unix(*queryTimestamp, 0)
	}

	report := Report{
		Strategy:    string(strategy),
		CerberusURL: *cerberusURL,
		Corpus:      *corpusPath,
	}

	httpClient := &http.Client{Timeout: time.Duration(*timeoutSec) * time.Second}

	for _, q := range queries {
		qStrategy := strategy
		if q.ExpectedStrategy != "" {
			qStrategy = Strategy(q.ExpectedStrategy)
		}

		result := QueryResult{
			Name:     q.Name,
			Expr:     q.Expr,
			Strategy: string(qStrategy),
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)

		var nativeRes, oracleRes shadow.VectorResult

		if qStrategy != StrategyOracleOnly {
			t0 := time.Now()
			r, err := evaluateNative(ctx, httpClient, *cerberusURL, q.Expr, at)
			result.DurationNativeMs = time.Since(t0).Milliseconds()
			if err != nil {
				result.NativeError = err.Error()
				report.NativeErrors++
			} else {
				nativeRes = r
				result.NativeSeriesLen = len(r.Series)
			}
		}

		// We always run the oracle when available, since prefer-native /
		// force-native both need its output for the diff. The noop oracle
		// short-circuits below.
		t0 := time.Now()
		or, err := oracle.Evaluate(ctx, q.Expr)
		result.DurationOracleMs = time.Since(t0).Milliseconds()
		switch {
		case errors.Is(err, ErrOracleSkipped):
			result.OracleSkipped = true
			report.OracleSkipped++
		case err != nil:
			result.OracleError = err.Error()
		default:
			oracleRes = or
			result.OracleSeriesLen = len(or.Series)
		}

		// Compute diff only when both sides produced something.
		if !result.OracleSkipped && result.OracleError == "" && result.NativeError == "" && qStrategy != StrategyOracleOnly {
			d := shadow.Compare(nativeRes, oracleRes, shadow.DefaultDiffOptions())
			if !d.Equal {
				result.Diff = &d
				report.Diffs++
			}
		}

		report.Queries = append(report.Queries, result)
		printQueryLine(stdout, result)

		cancel()
	}

	report.TotalQueries = len(report.Queries)

	if *reportPath != "" {
		if err := writeReport(*reportPath, report); err != nil {
			fmt.Fprintf(stderr, "write report: %v\n", err)
			return exitSetupFailure
		}
	}

	printSummary(stdout, report)

	if strategy == StrategyForceNative && report.Diffs > 0 {
		return exitDiffForceNative
	}
	return exitOK
}

func printQueryLine(w io.Writer, r QueryResult) {
	status := "OK"
	switch {
	case r.NativeError != "":
		status = "NATIVE_ERR"
	case r.OracleSkipped:
		status = "ORACLE_SKIP"
	case r.OracleError != "":
		status = "ORACLE_ERR"
	case r.Diff != nil:
		status = "DIFF"
	}
	fmt.Fprintf(w, "[%-11s] %-32s  native=%dms oracle=%dms  expr=%s\n",
		status, r.Name, r.DurationNativeMs, r.DurationOracleMs, oneline(r.Expr))
}

func printSummary(w io.Writer, r Report) {
	fmt.Fprintln(w, "----")
	fmt.Fprintf(w, "total=%d  diffs=%d  native_errors=%d  oracle_skipped=%d  strategy=%s\n",
		r.TotalQueries, r.Diffs, r.NativeErrors, r.OracleSkipped, r.Strategy)
}

func oneline(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == '\t' {
			out = append(out, ' ')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

func writeReport(path string, r Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// evaluateNative calls cerberus's Prometheus-compatible /api/v1/query endpoint
// and decodes the result into the shadow VectorResult shape. It deliberately
// avoids importing prometheus/common/model to keep the harness's dependency
// surface minimal.
func evaluateNative(ctx context.Context, c *http.Client, base, expr string, at time.Time) (shadow.VectorResult, error) {
	u, err := url.Parse(base)
	if err != nil {
		return shadow.VectorResult{}, fmt.Errorf("parse cerberus URL: %w", err)
	}
	u.Path = "/api/v1/query"
	q := u.Query()
	q.Set("query", expr)
	q.Set("time", strconv.FormatInt(at.Unix(), 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return shadow.VectorResult{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return shadow.VectorResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return shadow.VectorResult{}, fmt.Errorf("native HTTP %d: %s", resp.StatusCode, body)
	}

	var raw struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return shadow.VectorResult{}, fmt.Errorf("decode native response: %w", err)
	}
	if raw.Status != "success" {
		return shadow.VectorResult{}, fmt.Errorf("native query failed: %s: %s", raw.ErrorType, raw.Error)
	}
	return parsePromVector(raw.Data.ResultType, raw.Data.Result)
}

// parsePromVector handles the two response shapes shadow-mode cares about:
// "vector" (instant) and "matrix" (range). Each result entry has a "metric"
// label map plus either "value" [ts, "v"] or "values" [[ts, "v"], ...].
func parsePromVector(resultType string, results []json.RawMessage) (shadow.VectorResult, error) {
	out := shadow.VectorResult{Series: make([]shadow.Series, 0, len(results))}
	for i, raw := range results {
		var entry struct {
			Metric map[string]string `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
			Values [][2]json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return shadow.VectorResult{}, fmt.Errorf("decode series %d: %w", i, err)
		}
		series := shadow.Series{Labels: entry.Metric}
		switch resultType {
		case "vector":
			s, err := decodeSample(entry.Value)
			if err != nil {
				return shadow.VectorResult{}, fmt.Errorf("series %d sample: %w", i, err)
			}
			series.Samples = []shadow.Sample{s}
		case "matrix":
			for j, v := range entry.Values {
				s, err := decodeSample(v)
				if err != nil {
					return shadow.VectorResult{}, fmt.Errorf("series %d sample %d: %w", i, j, err)
				}
				series.Samples = append(series.Samples, s)
			}
		default:
			return shadow.VectorResult{}, fmt.Errorf("unsupported resultType %q", resultType)
		}
		out.Series = append(out.Series, series)
	}
	return out, nil
}

func decodeSample(raw [2]json.RawMessage) (shadow.Sample, error) {
	var tsFloat float64
	if err := json.Unmarshal(raw[0], &tsFloat); err != nil {
		return shadow.Sample{}, fmt.Errorf("timestamp: %w", err)
	}
	var valStr string
	if err := json.Unmarshal(raw[1], &valStr); err != nil {
		return shadow.Sample{}, fmt.Errorf("value: %w", err)
	}
	v, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return shadow.Sample{}, fmt.Errorf("parse value %q: %w", valStr, err)
	}
	return shadow.Sample{TimestampMs: int64(tsFloat * 1000), Value: v}, nil
}
