// Command migrate is cerberus's pre-cutover migration preview tool. It renders
// the ClickHouse schema cerberus expects — offline, without a database
// connection — so an operator can review exactly what cerberus will create
// before provisioning anything.
//
// Usage:
//
//	migrate --schema      # print the CREATE statements cerberus expects, to stdout
//
// Configuration is read from the SAME CERBERUS_* environment the server uses
// (config.FromEnv), so the previewed schema is byte-identical to what the
// server would apply on startup. The rendered DDL is directly pipeable into
// clickhouse-client.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/schemaboot"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

// options holds the parsed flags. It grows as the tool gains subcommands
// (explain, verify); for now --schema is the only capability.
type options struct {
	schema bool
}

// run is the testable entrypoint: it parses args, then dispatches. Splitting it
// from main() (mirroring cmd/route-rules) keeps flag parsing and dispatch unit
// -testable without spawning a process.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts options
	fs.BoolVar(&opts.schema, "schema", false,
		"print the ClickHouse schema (CREATE statements) cerberus expects, then exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !opts.schema {
		fs.Usage()
		return fmt.Errorf("nothing to do: pass --schema to print the expected ClickHouse schema")
	}

	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("load config from environment: %w", err)
	}
	return writeSchema(stdout, cfg)
}

// writeSchema renders the schema cerberus expects for cfg and writes it to w.
// It takes an already-loaded config so it is unit-testable without touching the
// process environment.
func writeSchema(w io.Writer, cfg config.Config) error {
	ddlCfg, err := schemaboot.DDLConfig(cfg)
	if err != nil {
		return fmt.Errorf("build schema config: %w", err)
	}
	stmts, err := ddl.RenderAll(ddlCfg, ddl.All)
	if err != nil {
		return fmt.Errorf("render schema: %w", err)
	}
	if len(stmts) == 0 {
		return fmt.Errorf("no schema to render (no signals configured)")
	}
	// Terminate every statement with ';' so the output pipes straight into
	// clickhouse-client; blank lines between statements keep it readable.
	if _, err := fmt.Fprintln(w, strings.Join(stmts, ";\n\n")+";"); err != nil {
		return fmt.Errorf("write schema: %w", err)
	}
	return nil
}
