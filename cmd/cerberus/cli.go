package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// printVersion writes the bare build version (and nothing else) to w, then a
// newline. It is the single renderer shared by the pre-cobra `--version`
// fast-path in main() and the `version` subcommand, so both emit byte-identical
// output. The output is DELIBERATELY just the version string — the distroless
// container healthcheck (see isVersionFlag + PR #297) greps for it, and cobra's
// built-in `.Version` mechanism would prefix it with "cerberus version ", which
// is why we never use that mechanism.
func printVersion(w io.Writer) {
	fmt.Fprintln(w, Version)
}

// newRootCmd builds the cobra command tree. The bare `cerberus` invocation (no
// subcommand) starts the server via runServer — helm, compose, and the Docker
// image ENTRYPOINT all depend on this, so the root RunE MUST be the server and
// bare invocation MUST NOT degrade to a help dump. runServer is injected so
// tests can assert the no-arg path reaches it without booting a real server.
//
// SilenceUsage/SilenceErrors keep cobra from printing a usage wall or a second
// error line on failure: main() owns the single slog.Error + exit-code mapping.
func newRootCmd(runServer func() error) *cobra.Command {
	root := &cobra.Command{
		Use:   "cerberus",
		Short: "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse",
		Long: "cerberus is an env-driven, stateless HTTP gateway that speaks the\n" +
			"Prometheus, Loki, and Tempo wire formats and translates queries into\n" +
			"ClickHouse SQL. Invoked with no subcommand it starts the server, reading\n" +
			"all configuration from CERBERUS_* environment variables (see\n" +
			"`cerberus config-docs`). Subcommands cover the offline migration preview\n" +
			"(`migrate`) and the repo's documentation/analysis generators.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServer()
		},
	}
	root.AddCommand(newServeCmd(runServer))
	root.AddCommand(newVersionCmd())
	root.AddCommand(newMigrateCmd())
	root.AddCommand(newConfigDocsCmd())
	root.AddCommand(newOptDocsCmd())
	root.AddCommand(newRouteRulesCmd())
	return root
}

// newServeCmd is the explicit spelling of the bare-invocation server start.
// `cerberus` and `cerberus serve` are identical: both call runServer. serve
// exists so the server has a discoverable, self-documenting verb alongside the
// tool subcommands.
func newServeCmd(runServer func() error) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the cerberus server (identical to bare `cerberus`)",
		Long: "Start the three-headed query gateway server. This is exactly what a\n" +
			"bare `cerberus` invocation does; the verb exists for discoverability.\n" +
			"All configuration comes from CERBERUS_* environment variables.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServer()
		},
	}
}

// newVersionCmd mirrors the `--version` fast-path as a subcommand via the shared
// printVersion helper. In practice isVersionFlag intercepts `cerberus version`
// before cobra is even constructed (so the distroless healthcheck stays cheap);
// this command exists so `--help` lists it and the behaviour has a canonical
// home.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cerberus version and exit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			printVersion(cmd.OutOrStdout())
			return nil
		},
	}
}

// exitCodeForError maps a command error to the process exit code. cobra collapses
// every RunE error into a single non-zero return from Execute, so the typed
// migration errors are re-discriminated here: a parity-gate failure and a
// cutover-gate no-go each get their own code so callers can tell an expected
// "the gate said no" apart from a tool malfunction.
func exitCodeForError(err error) int {
	var vgate verifyFailedError
	if errors.As(err, &vgate) {
		return verifyExitFail
	}
	var cgate gateFailedError
	if errors.As(err, &cgate) {
		return gateExitFail
	}
	return 1
}
