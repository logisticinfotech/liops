// SPDX-License-Identifier: Apache-2.0

// Command pgtijori backs up a PostgreSQL server: point-in-time backups with pgBackRest
// and a nightly pg_dump of every database, with a status UI and email alerts.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/logisticinfotech/pgtijori/app/version"
	"github.com/logisticinfotech/pgtijori/config"
)

const usage = `pgtijori — PostgreSQL backups: pgBackRest point-in-time backups + per-database dumps

Usage:
  pgtijori version
  pgtijori config init     [--config PATH] [--force]   write a config file with defaults
  pgtijori config validate [--config PATH]             check a config file

The default config path is ` + config.DefaultPath + `.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "pgtijori:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return nil
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(out, version.String())
		return nil
	case "help", "--help", "-h":
		fmt.Fprint(out, usage)
		return nil
	case "config":
		if len(args) < 2 {
			return errors.New("config: expected a subcommand (init or validate)")
		}
		return runConfig(args[1], args[2:], out)
	default:
		return fmt.Errorf("unknown command %q (run 'pgtijori help')", args[0])
	}
}

func runConfig(sub string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("config "+sub, flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "config file path")
	force := flags.Bool("force", false, "overwrite an existing file (init only)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	switch sub {
	case "init":
		if _, err := os.Stat(*path); err == nil && !*force {
			return fmt.Errorf("%s already exists (use --force to overwrite)", *path)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := config.Default().Save(*path); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote %s (mode 0600).\nFill in pgbackrest.stanza, storage and email, then run: pgtijori config validate --config %s\n", *path, *path)
		return nil
	case "validate":
		if _, err := config.Load(*path); err != nil {
			return fmt.Errorf("config is not valid:\n%w", err)
		}
		fmt.Fprintf(out, "%s is valid.\n", *path)
		return nil
	default:
		return fmt.Errorf("config: unknown subcommand %q (expected init or validate)", sub)
	}
}
