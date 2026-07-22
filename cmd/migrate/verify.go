package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
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
		startStr  = fs.String("start", envOr("CERBERUS_VERIFY_START", "-1h"), "range start (RFC3339, Unix seconds, or relative like -1h/now)")
		endStr    = fs.String("end", envOr("CERBERUS_VERIFY_END", "now"), "range end (RFC3339, Unix seconds, or relative like -1h/now)")
		stepStr   = fs.String("step", envOr("CERBERUS_VERIFY_STEP", "60s"), "range step (e.g. 60s)")
		tolerance = fs.Float64("tolerance", tolDefault, "absolute value tolerance for a match")
		asJSON    = fs.Bool("json", false, "emit the machine-readable JSON report instead of the text report")
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

	refBackend := migrateverify.NewHTTPBackend(*ref)
	cerBackend := migrateverify.NewHTTPBackend(*cerberus)
	rep := migrateverify.Verify(context.Background(), c, refBackend, cerBackend, params)

	if err := writeReport(stdout, rep, *asJSON); err != nil {
		return err
	}
	if rep.Failed() {
		return verifyFailedError{summary: rep.Summary}
	}
	return nil
}

// writeReport renders the report in the requested form.
func writeReport(w io.Writer, rep migrateverify.Report, asJSON bool) error {
	if asJSON {
		return rep.WriteJSON(w)
	}
	return rep.WriteText(w)
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
