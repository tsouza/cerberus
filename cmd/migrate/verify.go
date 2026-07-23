package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/migrateverify"
)

// defaultVerifyTolerance is the epsilon used when neither --tolerance nor
// CERBERUS_VERIFY_TOLERANCE is supplied. It reuses the package default so the
// CLI and the comparator agree on what "the same number" means.
const defaultVerifyTolerance = migrateverify.DefaultTolerance

// verifyExitFail is the process exit code when the parity gate fails (any query
// diverges or errors). It is distinct from the code Go uses for an internal
// error so a divergence reads as "the gate did its job", not "the tool broke".
const verifyExitFail = 2

// reportFileMode is the permission for the --report JSON diagnostic: operator-
// readable, not world-writable.
const reportFileMode = 0o600

// unknownToolVersion is the tool_version stamped into the diagnostic when the
// binary carries no VCS/module build stamp (e.g. a `go test` binary or a
// -buildvcs=false build).
const unknownToolVersion = "unknown"

// runVerify replays a harvested PromQL corpus against a reference Prometheus and
// cerberus over one query_range window and diffs the results. It is the cutover
// parity gate: on any divergence or error it returns a verifyFailedError so
// main can exit non-zero. Flags fall back to CERBERUS_VERIFY_* environment
// variables, so the same run can be driven by flags or env.
func runVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tolDefault, err := envFloat("CERBERUS_VERIFY_TOLERANCE", defaultVerifyTolerance)
	if err != nil {
		return err
	}
	var (
		corpus    = fs.String("corpus", envOr("CERBERUS_VERIFY_CORPUS", ""), "corpus.json produced by `migrate harvest`")
		ref       = fs.String("ref", envOr("CERBERUS_VERIFY_REF", ""), "reference Prometheus base URL")
		cerberus  = fs.String("cerberus", envOr("CERBERUS_VERIFY_CERBERUS", ""), "cerberus base URL")
		refToken  = fs.String("ref-token", envOr("CERBERUS_VERIFY_REF_TOKEN", ""), "bearer token for the reference (sent as an Authorization header; keeps credentials out of the URL and every artifact)")
		cerToken  = fs.String("cerberus-token", envOr("CERBERUS_VERIFY_CERBERUS_TOKEN", ""), "bearer token for cerberus (sent as an Authorization header; keeps credentials out of the URL and every artifact)")
		startStr  = fs.String("start", envOr("CERBERUS_VERIFY_START", "-1h"), "range start (RFC3339, Unix seconds, or relative like -1h/now)")
		endStr    = fs.String("end", envOr("CERBERUS_VERIFY_END", "now"), "range end (RFC3339, Unix seconds, or relative like -1h/now)")
		stepStr   = fs.String("step", envOr("CERBERUS_VERIFY_STEP", "60s"), "range step (e.g. 60s)")
		tolerance = fs.Float64("tolerance", tolDefault, "absolute value tolerance for a match")
		asJSON    = fs.Bool("json", false, "emit the machine-readable JSON report instead of the text report")
		report    = fs.String("report", envOr("CERBERUS_VERIFY_REPORT", ""), "write the full JSON diagnostics to this file (additive; the text report still prints)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case *corpus == "":
		return errors.New("missing --corpus (or CERBERUS_VERIFY_CORPUS): the harvested corpus.json to replay")
	case *ref == "":
		return errors.New("missing --ref (or CERBERUS_VERIFY_REF): the reference Prometheus base URL")
	case *cerberus == "":
		return errors.New("missing --cerberus (or CERBERUS_VERIFY_CERBERUS): the cerberus base URL")
	}

	c, err := migrateverify.LoadCorpus(*corpus)
	if err != nil {
		return err
	}
	params, err := migrateverify.BuildParams(*startStr, *endStr, *stepStr, *tolerance, time.Now().UTC())
	if err != nil {
		return err
	}

	refBackend := migrateverify.NewHTTPBackend(*ref, migrateverify.WithBearerToken(*refToken))
	cerBackend := migrateverify.NewHTTPBackend(*cerberus, migrateverify.WithBearerToken(*cerToken))
	rep := migrateverify.Verify(context.Background(), c, refBackend, cerBackend, params)

	// The resolved run params drive both the JSON diagnostic and the copy-pasteable
	// repro command, so the two always describe the exact same window. The backend
	// URLs are REDACTED here (the live requests above already used the real URLs):
	// any user:pass@ basic-auth credential must never reach the repro line, the
	// report JSON, or the text output — the operator re-supplies auth via
	// --ref-token / --cerberus-token (or their own URL) on replay.
	reportParams := migrateverify.VerifyReportParams{
		RefURL:      migrateverify.RedactURL(*ref),
		CerberusURL: migrateverify.RedactURL(*cerberus),
		Start:       params.Start.UTC().Format(time.RFC3339),
		End:         params.End.UTC().Format(time.RFC3339),
		Step:        params.Step.String(),
		Tolerance:   params.Tolerance,
		Corpus:      *corpus,
	}
	repro := reproCommand(reportParams)

	if *report != "" {
		diag := migrateverify.NewVerifyReport(rep, reportParams, toolVersion(), time.Now().UTC())
		if err := writeReportFile(*report, diag); err != nil {
			return err
		}
	}

	if err := writeReport(stdout, rep, *asJSON, repro); err != nil {
		return err
	}
	if rep.Failed() {
		return verifyFailedError{summary: rep.Summary}
	}
	return nil
}

// writeReport renders the report in the requested form. The text report is guided
// by the repro command so a failing run ends with a copy-pasteable reproduction.
func writeReport(w io.Writer, rep migrateverify.Report, asJSON bool, repro string) error {
	if asJSON {
		return rep.WriteJSON(w)
	}
	return rep.WriteTextGuided(w, migrateverify.TextGuidance{ReproCommand: repro})
}

// writeReportFile marshals the full JSON diagnostic to path, buffering first so a
// marshal failure never leaves a half-written file behind.
func writeReportFile(path string, diag migrateverify.VerifyReport) error {
	var buf strings.Builder
	if err := diag.WriteJSON(&buf); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(buf.String()), reportFileMode); err != nil {
		return fmt.Errorf("write report file %q: %w", path, err)
	}
	return nil
}

// reproCommand reconstructs the exact, copy-pasteable `migrate verify …`
// invocation that regenerates this diagnostic, using the RESOLVED window (so a
// relative -1h/now input reproduces the same instants) and suggesting --report so
// the operator captures the JSON to attach to a bug report.
func reproCommand(p migrateverify.VerifyReportParams) string {
	return strings.Join([]string{
		"migrate verify",
		"--corpus", shellQuote(p.Corpus),
		"--ref", shellQuote(p.RefURL),
		"--cerberus", shellQuote(p.CerberusURL),
		"--start", shellQuote(p.Start),
		"--end", shellQuote(p.End),
		"--step", shellQuote(p.Step),
		"--tolerance", strconv.FormatFloat(p.Tolerance, 'g', -1, 64),
		"--report", "verify-report.json",
	}, " ")
}

// shellQuote renders s as a single copy-pasteable shell word: bare when it holds
// only safe characters, single-quoted otherwise (with embedded single quotes
// escaped), so a URL or path with special characters survives a paste.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		if !isShellSafe(s[i]) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShellSafe reports whether c can appear unquoted in a shell word.
func isShellSafe(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-' || c == '.' || c == '/' || c == ':' || c == '=' || c == '@' || c == '%' || c == '+':
		return true
	default:
		return false
	}
}

// toolVersion returns the migrate binary's version for the diagnostic, read from
// the embedded module build info. It is "unknown" when the build carries no such
// stamp (e.g. a `go test` binary).
func toolVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownToolVersion
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return unknownToolVersion
}

// verifyFailedError signals a failed parity gate: the run completed and the
// report was written, but cerberus diverged from the reference (or a backend
// errored). main maps it to a dedicated non-zero exit code.
type verifyFailedError struct {
	summary migrateverify.Summary
}

func (e verifyFailedError) Error() string {
	return fmt.Sprintf("parity gate failed: %d diverge, %d error (of %d queries)",
		e.summary.Diverge, e.summary.Error, e.summary.Total)
}

// envOr returns the environment value for key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envFloat returns the float parsed from the environment value for key, or def
// when the variable is unset. A set-but-unparseable value is an error, not a
// silent fallback to def: because the tolerance default is deliberately tiny, a
// fat-fingered value would otherwise silently tighten the gate into spurious
// divergences instead of surfacing the misconfiguration.
func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid float: %w", key, v, err)
	}
	return f, nil
}
