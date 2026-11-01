// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/http/controllers"
	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/laravel"
	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/php"
	"github.com/logisticinfotech/liops/app/service"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

const defaultLogPath = "/var/log/liops/liops.log"

// paths are the file locations shared by setup, run, status and serve.
type paths struct {
	config, state, log         string
	monitorSocket, monitorData string // liops-monitor's; no flags except on `liops monitor`
}

func parseFlags(name string, args []string) (*flag.FlagSet, *paths, error) {
	flags, p := newFlags(name)
	return flags, p, flags.Parse(args)
}

// newFlags is parseFlags for commands that add flags of their own before parsing.
func newFlags(name string) (*flag.FlagSet, *paths) {
	d := defaultPaths()
	p := &paths{monitorSocket: d.monitorSocket, monitorData: d.monitorData}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.StringVar(&p.config, "config", d.config, "config file path")
	flags.StringVar(&p.state, "state", d.state, "state file path")
	flags.StringVar(&p.log, "log", d.log, "log file path")
	return flags, p
}

func (p *paths) lock() string { return filepath.Join(filepath.Dir(p.state), "job.lock") }

// openLog opens the log file for appending. Extra writers (e.g. the terminal) get a copy.
func openLog(path string, extra ...io.Writer) (io.Writer, func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, nil, err
	}
	return io.MultiWriter(append([]io.Writer{f}, extra...)...), func() { f.Close() }, nil
}

// requireRoot is a variable so tests can run commands as a normal user.
var requireRoot = func(what string) error {
	if os.Geteuid() != 0 && !devMode() {
		return fmt.Errorf("%s needs root (run it with sudo)", what)
	}
	return nil
}

// newRunner loads the config and builds a job runner.
func newRunner(p *paths, logExtra ...io.Writer) (*jobs.Runner, func(), error) {
	cfg, err := config.Load(p.config)
	if err != nil {
		return nil, nil, err
	}
	st, err := state.Open(p.state)
	if err != nil {
		return nil, nil, err
	}
	log, closeLog, err := openLog(p.log, logExtra...)
	if err != nil {
		return nil, nil, err
	}
	r, err := jobs.New(cfg, st, p.lock(), log)
	if err != nil {
		closeLog()
		return nil, nil, err
	}
	return r, closeLog, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runJob(job string, args []string, out io.Writer) error {
	_, p, err := parseFlags("run "+job, args)
	if err != nil {
		return err
	}
	if err := requireRoot("liops run"); err != nil {
		return err
	}
	r, closeLog, err := newRunner(p, out)
	if err != nil {
		return err
	}
	defer closeLog()
	ctx, stop := signalContext()
	defer stop()
	return r.Run(ctx, job, jobs.Manual)
}

// runServe runs the scheduler and the UI. Before setup is complete only the UI
// runs, for the setup wizard.
func runServe(args []string, out io.Writer) error {
	_, p, err := parseFlags("serve", args)
	if err != nil {
		return err
	}
	if err := requireRoot("liops serve"); err != nil {
		return err
	}
	st, err := state.Open(p.state)
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(p.log, out)
	if err != nil {
		return err
	}
	defer closeLog()
	ctx, stop := signalContext()
	defer stop()

	svc, err := service.New(ctx, p.config, p.lock(), st, log, nil)
	if err != nil {
		return err
	}
	defer svc.Close()
	ui, err := controllers.New(svc)
	if err != nil {
		return err
	}
	ui.Monitor = monitor.NewClient(p.monitorSocket)
	ui.WebBackupDir = filepath.Join(filepath.Dir(p.state), "backups")
	go svc.PushDiskUsage(ui.Monitor.Push)
	go svc.CollectPHP(php.DefaultEtc, ui.Monitor.PushFor)
	go svc.WatchDeploys(func() []laravel.App { return laravel.FindApps(ui.NginxDir, ui.Caddyfile) })
	listen := svc.Config().UI.Listen
	if err := config.CheckUIListen(listen); err != nil {
		return fmt.Errorf("ui.listen: %w", err)
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(log, "%s [serve] UI on http://%s (open it through an SSH tunnel: ssh -L %s %s@<server>)\n",
		time.Now().Format("2006-01-02 15:04:05"), listen, tunnelSpec(listen), "<user>")

	select {
	case err := <-errc:
		return fmt.Errorf("UI: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// tunnelSpec is the -L argument for an SSH tunnel to the UI, e.g. 8899:127.0.0.1:8899.
func tunnelSpec(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	return port + ":" + listen
}

func runStatus(args []string, out io.Writer) error {
	_, p, err := parseFlags("status", args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	loc, _ := cfg.Location()
	st, err := state.Open(p.state)
	if err != nil {
		return err
	}
	now := time.Now().In(loc)

	fmt.Fprintln(out, "Last run of each job:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, job := range jobs.Names {
		r, err := st.LastRun(job, "")
		if err != nil {
			return err
		}
		if r == nil {
			fmt.Fprintf(tw, "  %s\tnever\t\n", job)
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", job, r.Status, notify.When(r.Start, now), runDetail(r))
	}
	tw.Flush()

	runs, err := st.Runs("", 15)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\nRecent runs:")
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range runs {
		dur := ""
		if !r.End.IsZero() {
			dur = notify.Duration(r.End.Sub(r.Start))
		}
		fmt.Fprintf(tw, "  #%d\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Job, r.Status, notify.When(r.Start, now), dur, runDetail(&r))
	}
	tw.Flush()

	deleted, err := st.Deleted()
	if err == nil && len(deleted) > 0 {
		fmt.Fprintln(out, "\nDatabases no longer found (newest dump kept for now):")
		for db, since := range deleted {
			fmt.Fprintf(out, "  %s since %s\n", db, since.In(loc).Format("2 Jan"))
		}
	}
	if cfg.IsMySQL() {
		fmt.Fprintf(out, "\nFull command output is in %s.\n", p.log)
		return nil
	}
	fmt.Fprintf(out, "\nFull command output is in %s. For backup details: sudo -u %s pgbackrest --stanza=%s info\n",
		p.log, cfg.Postgres.OSUser, cfg.PgBackRest.Stanza)
	return nil
}

func runDetail(r *state.Run) string {
	switch {
	case r.Error != "":
		return r.Error
	case r.Job == jobs.Dumps && len(r.Dumps) > 0:
		ok := 0
		for _, d := range r.Dumps {
			if d.Error == "" {
				ok++
			}
		}
		return fmt.Sprintf("%d of %d databases, %s", ok, len(r.Dumps), notify.Bytes(r.Size))
	case r.Size > 0:
		return notify.Bytes(r.Size)
	}
	return ""
}

// runSetup writes the pgBackRest and PostgreSQL settings, then creates and checks the
// stanza. Turning on archive_mode needs a PostgreSQL restart, which setup never does
// by itself: it says so and stops, and the next run continues from there.
func runSetup(args []string, out io.Writer) error {
	flags, p := newFlags("setup")
	skipPgBackRest := flags.Bool("skip-pgbackrest", false, "development only: mark setup complete without pgBackRest")
	rootPasswordStdin := flags.Bool("mysql-root-password-stdin", false, "MySQL: read root's MySQL password from stdin, if root needs one (never stored)")
	enableGTID := flags.Bool("enable-gtid", false, "MySQL: make the setting changes point-in-time backups need (GTIDs), online; setup lists them first")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *skipPgBackRest && !devMode() {
		return fmt.Errorf("--skip-pgbackrest is only for development (set %s=1); on a server, run setup without it", devEnv)
	}
	if err := requireRoot("liops setup"); err != nil {
		return err
	}
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	if cfg.IsMySQL() {
		return runMySQLSetup(cfg, p, setupFlags{rootPasswordStdin: *rootPasswordStdin, enableGTID: *enableGTID}, os.Stdin, out)
	}
	if *skipPgBackRest {
		return skipSetup(p, out)
	}
	ctx, stop := signalContext()
	defer stop()

	store, err := storage.Open(cfg)
	if err != nil {
		return err
	}
	engine := pgbackrest.New(cfg, command.Exec{})
	archiving, err := setup.WriteSettings(ctx, engine, store)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Settings are in %s and %s.\n", pgbackrest.ConfPath(cfg), engine.Cluster.ConfDPath())
	if archiving == setup.NeedsRestart {
		fmt.Fprintf(out, "\nPostgreSQL must be restarted once to turn on WAL archiving (a few seconds of downtime).\n"+
			"When it suits you, run:\n\n  %s\n  %s setup\n", setup.RestartCommand(cfg), self())
		return nil
	}

	fmt.Fprintln(out, "Creating and checking the pgBackRest stanza...")
	if err := setup.CreateStanza(ctx, engine); err != nil {
		var se *setup.StepError
		if errors.As(err, &se) {
			fmt.Fprintln(out, command.LastLines(se.Output, 20))
		}
		return err
	}
	if err := markSetupComplete(p); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nSetup is complete. WAL archiving works.")
	if engine.Cluster.Homebrew() {
		fmt.Fprintf(out, "Next: take the first full backup and dumps, then start liops:\n\n  %s run full\n  make dev\n", self())
		return nil
	}
	printNextSteps(out)
	return nil
}

// printNextSteps says what's left after setup. install.sh has usually started the
// service already, and it picks the finished setup up by itself.
func printNextSteps(out io.Writer) {
	if exec.Command("systemctl", "is-active", "--quiet", "liops").Run() == nil {
		fmt.Fprintln(out, "The liops service is running and starts the backup schedule within a few seconds; nothing to restart.")
		fmt.Fprintln(out, "If this server has no full backup yet, take one now (the dumps follow it):\n\n  sudo liops run full")
		return
	}
	fmt.Fprintln(out, "Next: take the first full backup and dumps, then start the service:")
	fmt.Fprintln(out, "\n  sudo liops run full\n  sudo systemctl enable --now liops")
}

// setupFlags are the MySQL choices on the setup command line.
type setupFlags struct{ rootPasswordStdin, enableGTID bool }

// runMySQLSetup gives liops its own MySQL account, checks the server and claims the
// backup name. MySQL needs no restart and no settings changes for its dumps.
func runMySQLSetup(cfg *config.Config, p *paths, f setupFlags, in io.Reader, out io.Writer) error {
	var rootPassword string
	if f.rootPasswordStdin {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if rootPassword = strings.TrimRight(line, "\r\n"); rootPassword == "" {
			return errors.New("--mysql-root-password-stdin: no password on stdin")
		}
	}
	store, err := storage.Open(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	res, err := setup.SetupMySQL(ctx, cfg, command.Exec{}, store, setup.MySQLOptions{RootPassword: rootPassword, ApplySettings: f.enableGTID})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "MySQL %s: liops logs in with its own account (%s, login in %s).\n", res.Version, setup.MySQLUser, cfg.MySQL.DefaultsFile)
	if len(res.Pending) > 0 {
		printPendingMySQL(out, cfg, res, f.rootPasswordStdin)
		return nil
	}
	if err := markSetupComplete(p); err != nil {
		return err
	}
	fmt.Fprintf(out, "GTIDs and binary logging are ready for point-in-time backups (settings kept in %s).\n", cfg.MySQL.ConfFile)
	fmt.Fprintln(out, "\nSetup is complete.")
	printNextSteps(out)
	return nil
}

// printPendingMySQL explains the MySQL setting changes setup is waiting to make, and
// how to approve them.
func printPendingMySQL(out io.Writer, cfg *config.Config, res setup.MySQLResult, rootPasswordStdin bool) {
	fmt.Fprintln(out, "\nPoint-in-time backups need these MySQL settings changed:")
	for _, c := range res.Pending {
		fmt.Fprintln(out, "  "+c.String())
	}
	fmt.Fprintf(out, "\nsetup --enable-gtid changes them in the running server (no restart, no downtime) and keeps\n"+
		"them in %s. Nothing has been changed yet.\n", cfg.MySQL.ConfFile)
	fmt.Fprintln(out, "\nWith enforce_gtid_consistency = ON, MySQL refuses the few statements that GTIDs can't log\n"+
		"safely: mostly writing to a MyISAM table and an InnoDB table in one transaction, and\n"+
		"CREATE TABLE ... SELECT into a MyISAM table. Apps that only use InnoDB aren't affected.")
	if res.NonInnoDBTables > 0 {
		fmt.Fprintf(out, "This server has %d table(s) that aren't InnoDB: check how your app writes to them first.\n", res.NonInnoDBTables)
	}
	cmd := "sudo liops setup --enable-gtid"
	if rootPasswordStdin {
		cmd = "read -rs PW && printf '%s\\n' \"$PW\" | sudo liops setup --enable-gtid --mysql-root-password-stdin"
	}
	fmt.Fprintf(out, "\nWhen you're ready, run:\n\n  %s\n", cmd)
}

func markSetupComplete(p *paths) error {
	st, err := state.Open(p.state)
	if err != nil {
		return err
	}
	return st.SetMeta(service.MetaSetupComplete, time.Now().UTC().Format(time.RFC3339))
}

// skipSetup marks setup complete without touching pgBackRest or PostgreSQL, so the UI,
// the scheduler and dumps can be worked on where pgBackRest setup can't run (a Mac).
func skipSetup(p *paths, out io.Writer) error {
	if err := markSetupComplete(p); err != nil {
		return err
	}
	fmt.Fprintln(out, "Development setup: pgBackRest was skipped and setup is marked complete.")
	fmt.Fprintln(out, "Dumps, the report and the UI work; full and diff backups will fail.")
	return nil
}

// self is how to type a liops command in hints: with sudo on a server, with
// LIOPS_DEV=1 in development mode.
func self() string {
	if devMode() {
		return devEnv + "=1 ./liops"
	}
	return "sudo liops"
}
