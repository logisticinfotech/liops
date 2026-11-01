// SPDX-License-Identifier: Apache-2.0

// Command liops backs up a database server, with a status UI and email alerts.
// PostgreSQL: point-in-time backups with pgBackRest and a nightly pg_dump of every
// database. MySQL: a nightly mysqldump of every database (point-in-time to come).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/version"
	"github.com/logisticinfotech/liops/config"
)

const usage = `liops — database backups: PostgreSQL (pgBackRest point-in-time + per-database dumps) and MySQL (per-database dumps)

Usage:
  liops version
  liops config init     [--config PATH] [--force]   write a config file with defaults
  liops config validate [--config PATH]             check a config file
  liops setup           [flags]                     set up backups for this server (safe to re-run)
                        [--mysql-root-password-stdin]  MySQL: root's password on stdin, if root needs one
                        [--enable-gtid]             MySQL: make the GTID changes setup listed (online)
  liops run JOB         [flags]                     run a job now: full, diff, dumps or report (MySQL: also binlogs)
  liops status          [flags]                     show recent job runs
  liops serve           [flags]                     run the scheduler and the UI (the systemd service runs this)
  liops logs            [-n LINES] [-f]             show the log
  liops dumps list [DB] | url KEY                   list stored dumps; print a 10-minute download link
  liops restore-files   --dir DIR [--to TIME]       MySQL: prepare a point-in-time restore in DIR (docs/restore-mysql.md)
  liops monitor         [--socket PATH] [--data DIR] the monitoring service (liops-monitor.service runs this)
                        [--pg-socket-dir DIR] [--mysql-socket PATH]  where the databases' sockets are
                        [--nginx-log PATH] [--caddyfile PATH]       where the access logs are
                        [--redis ADDR]                              Redis (host:port or socket)
  liops monitor-setup   [--apply]                   list (or make) the changes monitoring needs: database logins, web logs, PHP-FPM, Redis
  liops verdict                                     print the monitoring verdict (Monitoring → Overview)
  liops install-system                              install the systemd service (install.sh runs this)
  liops uninstall       [--purge] [--yes]           remove liops; backups in storage are kept

Flags for setup, run, status, serve and dumps:
  --config PATH   default ` + config.DefaultPath + `
  --state PATH    default ` + state.DefaultPath + `
  --log PATH      default ` + defaultLogPath + `
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "liops:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	jobs.MonitorSocket = defaultPaths().monitorSocket
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
	case "setup":
		return runSetup(args[1:], out)
	case "run":
		if len(args) < 2 {
			return errors.New("run: expected a job (full, diff, dumps or report)")
		}
		return runJob(args[1], args[2:], out)
	case "status":
		return runStatus(args[1:], out)
	case "serve":
		return runServe(args[1:], out)
	case "logs":
		return runLogs(args[1:], out)
	case "dumps":
		if len(args) < 2 {
			return errors.New("dumps: expected list or url")
		}
		return runDumps(args[1], args[2:], out)
	case "monitor":
		return runMonitor(args[1:], out)
	case "monitor-setup":
		return runMonitorSetup(args[1:], out)
	case "verdict":
		return runVerdict(args[1:], out)
	case "restore-files":
		return runRestoreFiles(args[1:], out)
	case "install-system":
		return runInstallSystem(args[1:], out)
	case "uninstall":
		return runUninstall(args[1:], os.Stdin, out)
	default:
		return fmt.Errorf("unknown command %q (run 'liops help')", args[0])
	}
}

func runConfig(sub string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("config "+sub, flag.ContinueOnError)
	path := flags.String("config", defaultPaths().config, "config file path")
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
		fmt.Fprintf(out, "Wrote %s (mode 0600).\nFill in pgbackrest.stanza (for MySQL: engine: mysql and mysql.backup_name), storage and email, then run: liops config validate --config %s\n", *path, *path)
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
