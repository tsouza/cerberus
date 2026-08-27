package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/tsouza/cerberus/internal/chaudit"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
)

// Defaults for `cerberus audit`. They describe the panel an operator is
// actually defending — the wide dashboard window that trips a resource-bound
// guard first — rather than a typical one, because a bound that holds for the
// widest panel holds for every narrower one.
const (
	// auditDefaultWindowSeconds is 6h, the dashboard window that produced the
	// #2677 production incident and the widest one Grafana's stock range
	// picker offers before the day scale.
	auditDefaultWindowSeconds = 6 * 60 * 60
	// auditDefaultAnchors is the grid point count a 6h window yields at a 60s
	// step — the `anchors` factor of the density guard's own cost model.
	auditDefaultAnchors = 361
	// auditDefaultTop bounds the report to the worst offenders. An audit is
	// read top-down and acted on a few metrics at a time; a full dump of a
	// large deployment's metric namespace is not actionable.
	auditDefaultTop = 20
)

// auditInputs carries newAuditCmd's resolved flags to runAudit, mirroring the
// deltaPrefixBackfillInputs shape in cmd_schema.go.
type auditInputs struct {
	table         string
	windowSeconds int64
	anchors       int64
	budget        int64
	top           int
}

// newAuditCmd builds `cerberus audit`: the live-deployment counterpart to
// `cerberus migrate inventory`. inventory probes a SOURCE Prometheus before a
// cutover; audit probes the ALREADY-CONNECTED ClickHouse afterwards, so a
// panel walking up to a resource-bound guard's cap as real traffic and
// cardinality grow is visible before it goes red rather than after (#2679).
//
// It is strictly read-only: chaudit.Querier is a one-method interface over
// QueryContext precisely so that is checkable by reading the type.
func newAuditCmd() *cobra.Command {
	in := auditInputs{
		windowSeconds: auditDefaultWindowSeconds,
		anchors:       auditDefaultAnchors,
		top:           auditDefaultTop,
	}
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit the connected ClickHouse for metrics near a resource-bound guard",
		Long: "Probe the connected ClickHouse for classic-histogram metrics whose real\n" +
			"cardinality and bucket width put them close to (or past) the density\n" +
			"guard's budget, and name the label amplifying each one.\n\n" +
			"Reports worst headroom first. Exits non-zero when any metric is already\n" +
			"over budget, so it can gate a deploy. All connection settings come from\n" +
			"the same CERBERUS_* environment variables the server reads.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAudit(cmd, in)
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.table, "table", "",
		"classic-histogram table to probe (default: the resolved schema's histogram table)")
	f.Int64Var(&in.windowSeconds, "window-seconds", in.windowSeconds,
		"lookback window to evaluate, in seconds")
	f.Int64Var(&in.anchors, "anchors", in.anchors,
		"grid point count that window implies at the panel's step")
	f.Int64Var(&in.budget, "budget", 0,
		"density-unit ceiling to compare against (default: the deployment's resolved bound)")
	f.IntVar(&in.top, "top", in.top, "how many metrics to report, worst headroom first")
	return cmd
}

// runAudit resolves the deployment's own configuration, probes it, and writes
// the report. Every default that is not supplied on the command line is read
// from the SAME resolved config the server runs with — auditing a deployment
// against a bound it does not actually enforce would report confident numbers
// about a ceiling that is not there.
func runAudit(cmd *cobra.Command, in auditInputs) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config from environment: %w", err)
	}
	if in.table == "" {
		in.table = cfg.Schema.HistogramTable
	}
	if in.budget == 0 {
		in.budget = cfg.RangeBucketGridNativeMaxDensityUnits
	}
	opts := chaudit.Options{
		Table:             in.table,
		WindowSeconds:     in.windowSeconds,
		Anchors:           in.anchors,
		DensityUnitBudget: in.budget,
		Top:               in.top,
	}
	if err := opts.Validate(); err != nil {
		return err
	}

	db, err := chclient.OpenSQLDB(cfg.ClickHouse)
	if err != nil {
		return fmt.Errorf("connect ClickHouse: %w", err)
	}
	defer func() { _ = db.Close() }()

	report, err := chaudit.Probe(cmd.Context(), db, opts)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if err := writeAuditReport(cmd.OutOrStdout(), report); err != nil {
		return err
	}
	// A non-zero exit is the point of running this in CI: an over-budget
	// metric is not advice, it is a panel that fails today. The report is
	// written FIRST so the operator sees which metric and by how much, rather
	// than only an exit code.
	if over := report.OverBudget(); len(over) > 0 {
		return &auditOverBudgetError{count: len(over)}
	}
	return nil
}

// writeAuditReport emits the report as indented JSON. JSON rather than a table
// because the useful consumers are a CI gate and a diff between two runs, both
// of which need the individual factors (series / rawRows / bucketWidth) that a
// human-readable summary would collapse.
func writeAuditReport(w io.Writer, report chaudit.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("write audit report: %w", err)
	}
	return nil
}

// auditOverBudgetError reports that the audit found metrics already past the
// guard's budget. It is a distinct type so exitCodeForError can map it without
// string-matching a message.
type auditOverBudgetError struct{ count int }

func (e *auditOverBudgetError) Error() string {
	return fmt.Sprintf("%d metric(s) are already over the density-unit budget", e.count)
}
