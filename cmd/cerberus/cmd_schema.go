package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/deltaprefix"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// errNoSchemaSubcommand is returned when `cerberus schema` is invoked bare.
// schema is a group of verbs — with none selected there is nothing to do,
// and falling through to a silent success would hide an operator mistake,
// mirroring errNoMigrateSubcommand.
var errNoSchemaSubcommand = errors.New("nothing to do: pass a schema subcommand (see `cerberus schema --help`)")

// newSchemaCmd builds the `cerberus schema` command group: the durable home
// for one-time schema-related operational tasks against a LIVE ClickHouse
// connection — as opposed to `migrate schema`, which only PREVIEWS the DDL
// cerberus expects, offline, with no connection at all. Every verb under
// this group opens a real ClickHouse connection from the same CERBERUS_* /
// cerberus.yaml configuration the server reads, via schemaConnectClickHouse.
//
// The first two verbs, delta-prefix-backfill and delta-prefix-verify,
// populate and check the DELTA-temporality prefix-reconstruction aggregate
// table (schema.Metrics.DeltaPrefixTable, cerberus issue #2389; see
// internal/deltaprefix and docs/operations.md's "DELTA-prefix aggregate
// backfill" runbook). Later one-time schema/data operational tasks are
// added here as siblings, each its own newSchemaXxxCmd() function, rather
// than a one-off script living outside this tree.
func newSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "One-time schema-related operational tasks against a live ClickHouse",
		Long: "schema is the durable home for one-time schema-related operational\n" +
			"tasks cerberus runs against a LIVE ClickHouse connection — as opposed to\n" +
			"`migrate schema`, which only previews the DDL cerberus expects, offline,\n" +
			"with no connection at all. delta-prefix-backfill and delta-prefix-verify,\n" +
			"the first two verbs, populate and check the DELTA-temporality prefix-\n" +
			"reconstruction aggregate table (cerberus issue #2389); later one-time\n" +
			"schema/data operational tasks are added here as siblings.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			printUsageToStderr(cmd)
			return errNoSchemaSubcommand
		},
	}
	cmd.AddCommand(
		newSchemaDeltaPrefixBackfillCmd(),
		newSchemaDeltaPrefixVerifyCmd(),
	)
	return cmd
}

// runSchema dispatches through the full schema command group. It is the
// seam unit tests drive (and mirrors the production `cerberus schema <verb>`
// path), matching runMigrate's shape in cmd_migrate.go.
func runSchema(args []string, stdout, stderr io.Writer) error {
	return execCmd(newSchemaCmd(), args, stdout, stderr)
}

// schemaConnectClickHouse builds a ClickHouse connection from the
// deployment's own CERBERUS_* / cerberus.yaml configuration — the same
// config.FromEnv() every other CLI tool and the server itself read —
// returning both the resolved config (a schema verb needs cfg.Schema for
// table/column names) and the opened *chclient.Client (chclient.New is
// lazy; it never dials until the first query, matching every other
// CLI-driven ClickHouse connection in this codebase — see rrConnectClickHouse
// in cmd_routerules.go and setupSchema in main.go).
func schemaConnectClickHouse() (config.Config, *chclient.Client, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("load config from environment: %w", err)
	}
	client, err := chclient.New(cfg.ClickHouse)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("connect ClickHouse: %w", err)
	}
	return cfg, client, nil
}

// deltaPrefixColumns builds the deltaprefix.Columns a schema verb needs from
// cfg, failing fast when the DELTA-prefix table has no name at all.
// schema.Metrics.DeltaPrefixTable is empty unless the operator opted in via
// CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED (see DefaultOTelMetricsFrom in
// internal/schema/env.go) — the same signal that gates DDL provisioning —
// so an empty value here almost always means the operator hasn't set that
// flag yet, not a broken deployment.
func deltaPrefixColumns(cfg config.Config) (deltaprefix.Columns, error) {
	if cfg.Schema.DeltaPrefixTable == "" {
		return deltaprefix.Columns{}, errors.New(
			"the resolved schema has no DeltaPrefixTable name — set CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED=true " +
				"(the deployment must already have provisioned the table via CERBERUS_AUTO_CREATE_SCHEMA=true " +
				"and that same flag, or provisioned it manually)",
		)
	}
	return deltaprefix.FromSchema(cfg.ClickHouse.Database, cfg.Schema), nil
}

// deltaPrefixBackfillInputs carries newSchemaDeltaPrefixBackfillCmd's
// resolved flags to runDeltaPrefixBackfill.
type deltaPrefixBackfillInputs struct {
	before string
	dryRun bool
}

// newSchemaDeltaPrefixBackfillCmd builds `cerberus schema
// delta-prefix-backfill` — the one-time historical backfill into the
// DELTA-prefix aggregate table for data that predates its materialized
// view's own creation (see internal/deltaprefix's package doc and
// docs/operations.md for the full runbook, including why --before MUST be
// the MV's own creation timestamp).
func newSchemaDeltaPrefixBackfillCmd() *cobra.Command {
	var in deltaPrefixBackfillInputs
	cmd := &cobra.Command{
		Use:   "delta-prefix-backfill",
		Short: "One-time historical backfill into the DELTA-prefix aggregate table",
		Long: "Backfill the DELTA-temporality prefix-reconstruction aggregate table\n" +
			"(CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED, cerberus issue #2389) for history\n" +
			"that predates its materialized view: the MV only captures INSERTs into\n" +
			"the base sum table from the moment it was created onward, so existing\n" +
			"history needs this one-time INSERT ... SELECT pass. --before MUST be the\n" +
			"MV's own creation timestamp (system.tables.metadata_modification_time,\n" +
			"or an operator-recorded timestamp from the moment CREATE MATERIALIZED\n" +
			"VIEW ran) — rows at or after it are already covered by the live MV, and\n" +
			"backfilling past it double-counts every bucket that straddles the\n" +
			"cutover. Both this bound and the aggregate table's own bucket boundary\n" +
			"round to the calendar day, so the cutover day itself is left for the\n" +
			"live MV to capture as ordinary new inserts arrive — see\n" +
			"docs/operations.md's DELTA-prefix backfill runbook.",
		Example:       "  cerberus schema delta-prefix-backfill --before 2026-08-20T00:00:00Z",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeltaPrefixBackfill(cmd, in)
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.before, "before", "",
		"REQUIRED: the delta-prefix MV's creation timestamp (RFC3339, Unix seconds, or relative like -24h/now)")
	f.BoolVar(&in.dryRun, "dry-run", false, "print the INSERT ... SELECT statement without executing it")
	return cmd
}

func runDeltaPrefixBackfill(cmd *cobra.Command, in deltaPrefixBackfillInputs) error {
	if in.before == "" {
		return errors.New("missing --before: the delta-prefix MV's creation timestamp " +
			"(see `cerberus schema delta-prefix-backfill --help`)")
	}
	before, err := migrateverify.ParseTime(in.before, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("parse --before: %w", err)
	}

	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config from environment: %w", err)
	}
	cols, err := deltaPrefixColumns(cfg)
	if err != nil {
		return err
	}

	if in.dryRun {
		sql, args := deltaprefix.BackfillSQL(cols, before)
		fmt.Fprintln(cmd.OutOrStdout(), sql)
		fmt.Fprintf(cmd.OutOrStdout(), "-- args: %v\n", args)
		return nil
	}

	client, err := chclient.New(cfg.ClickHouse)
	if err != nil {
		return fmt.Errorf("connect ClickHouse: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := deltaprefix.Backfill(context.Background(), client.Conn(), cols, before); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "backfilled %s from %s (rows before %s, %s day-bucket boundary)\n",
		cols.DeltaPrefixTable, cols.SumTable, before.Format(time.RFC3339), "toStartOfDay")
	return nil
}

// deltaPrefixVerifyInputs carries newSchemaDeltaPrefixVerifyCmd's resolved
// flags to runDeltaPrefixVerify.
type deltaPrefixVerifyInputs struct {
	before    string
	tolerance float64
	asJSON    bool
}

// defaultDeltaPrefixVerifyTolerance is the absolute epsilon two per-metric
// totals may differ by and still count as matching. Looser than
// migrateverify.DefaultTolerance (1e-9, tuned for comparing two independent
// EVALUATIONS of the same sample values): this compares two SUMS, each over
// a potentially enormous and differently-ordered set of float64 additions
// (ClickHouse's own parallel `sum()` aggregation order is not guaranteed
// stable run to run), so a real completeness match can differ from zero by
// ordinary floating-point rounding alone.
const defaultDeltaPrefixVerifyTolerance = 1e-6

// newSchemaDeltaPrefixVerifyCmd builds `cerberus schema delta-prefix-verify`
// — runs the backfill-completeness verification query from the DELTA-prefix
// design doc, comparing the aggregate table's per-metric totals against the
// base table's own DELTA-temporality totals over the same backfilled
// history, and reports a clear pass/fail.
func newSchemaDeltaPrefixVerifyCmd() *cobra.Command {
	var in deltaPrefixVerifyInputs
	cmd := &cobra.Command{
		Use:   "delta-prefix-verify",
		Short: "Verify DELTA-prefix backfill completeness before enabling the fast path",
		Long: "Compare the DELTA-prefix aggregate table's per-metric-name totals against\n" +
			"the base sum table's own DELTA-temporality totals, over the same\n" +
			"backfilled history (--before, the delta-prefix MV's creation timestamp —\n" +
			"see `cerberus schema delta-prefix-backfill --help`). A clean pass is the\n" +
			"required confirmation before an operator sets\n" +
			"CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED=true. This is a per-metric\n" +
			"COMPLETENESS check (did every DELTA row get backfilled), not a per-series\n" +
			"identity-alignment check — see internal/deltaprefix's package doc for the\n" +
			"scope boundary against cerberus issue #2389's still-open read-side task.",
		Example:       "  cerberus schema delta-prefix-verify --before 2026-08-20T00:00:00Z",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeltaPrefixVerify(cmd, in)
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.before, "before", "",
		"REQUIRED: the delta-prefix MV's creation timestamp (RFC3339, Unix seconds, or relative like -24h/now)")
	f.Float64Var(&in.tolerance, "tolerance", defaultDeltaPrefixVerifyTolerance,
		"absolute per-metric total tolerance for a match")
	f.BoolVar(&in.asJSON, "json", false, "emit the machine-readable JSON report instead of text")
	return cmd
}

// deltaPrefixVerifyFailedError is returned when delta-prefix-verify finds at
// least one mismatch, so exitCodeForError can map it to a distinct non-zero
// exit — mirrors verifyFailedError / gateFailedError's "the gate did its
// job, not a tool malfunction" contract in cmd_migrate.go, reusing the SAME
// exit code (verifyExitFail) since both report a parity/completeness gate
// failure to a scripted caller.
type deltaPrefixVerifyFailedError struct{ mismatches int }

func (e deltaPrefixVerifyFailedError) Error() string {
	return fmt.Sprintf("delta-prefix verify: %d metric(s) mismatched", e.mismatches)
}

func runDeltaPrefixVerify(cmd *cobra.Command, in deltaPrefixVerifyInputs) error {
	if in.before == "" {
		return errors.New("missing --before: the delta-prefix MV's creation timestamp " +
			"(see `cerberus schema delta-prefix-verify --help`)")
	}
	before, err := migrateverify.ParseTime(in.before, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("parse --before: %w", err)
	}

	cfg, client, err := schemaConnectClickHouse()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	cols, err := deltaPrefixColumns(cfg)
	if err != nil {
		return err
	}

	rep, err := deltaprefix.Verify(context.Background(), client.Conn(), cols, before, in.tolerance)
	if err != nil {
		return err
	}
	if err := writeDeltaPrefixVerifyReport(cmd.OutOrStdout(), rep, in.asJSON); err != nil {
		return err
	}
	if !rep.Pass() {
		return deltaPrefixVerifyFailedError{mismatches: len(rep.Mismatches)}
	}
	return nil
}

func writeDeltaPrefixVerifyReport(w io.Writer, rep deltaprefix.Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if rep.Pass() {
		fmt.Fprintf(w, "PASS: %d metric(s) matched within tolerance %g (before %s)\n",
			len(rep.AggregateTotals), rep.Tolerance, rep.Before.Format(time.RFC3339))
		return nil
	}
	fmt.Fprintf(w, "FAIL: %d of %d metric(s) mismatched (tolerance %g, before %s)\n\n",
		len(rep.Mismatches), len(unionMetricNames(rep)), rep.Tolerance, rep.Before.Format(time.RFC3339))
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tAGGREGATE\tBASE\tDIFF")
	for _, m := range rep.Mismatches {
		fmt.Fprintf(tw, "%s\t%g\t%g\t%g\n", m.MetricName, m.Aggregate, m.Base, m.Diff())
	}
	return tw.Flush()
}

// unionMetricNames is the total distinct metric-name population Verify
// compared, for the FAIL summary line's "X of Y" count.
func unionMetricNames(rep deltaprefix.Report) map[string]struct{} {
	out := make(map[string]struct{}, len(rep.AggregateTotals)+len(rep.BaseTotals))
	for name := range rep.AggregateTotals {
		out[name] = struct{}{}
	}
	for name := range rep.BaseTotals {
		out[name] = struct{}{}
	}
	return out
}
