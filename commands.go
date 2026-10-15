// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/pgbackrest"
	"github.com/logisticinfotech/pgtijori/app/http/controllers"
	"github.com/logisticinfotech/pgtijori/app/jobs"
	"github.com/logisticinfotech/pgtijori/app/notify"
	"github.com/logisticinfotech/pgtijori/app/service"
	"github.com/logisticinfotech/pgtijori/app/setup"
	"github.com/logisticinfotech/pgtijori/app/state"
	"github.com/logisticinfotech/pgtijori/config"
)

const defaultLogPath = "/var/log/pgtijori/pgtijori.log"

// paths are the file locations shared by setup, run, status and serve.
type paths struct {
	config, state, log string
}

func parseFlags(name string, args []string) (*flag.FlagSet, *paths, error) {
	p := &paths{}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.StringVar(&p.config, "config", config.DefaultPath, "config file path")
	flags.StringVar(&p.state, "state", state.DefaultPath, "state file path")
	flags.StringVar(&p.log, "log", defaultLogPath, "log file path")
	return flags, p, flags.Parse(args)
}

func (p *paths) lock() string  { return filepath.Join(filepath.Dir(p.state), "job.lock") }
func (p *paths) token() string { return filepath.Join(filepath.Dir(p.state), "setup-token") }

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
	if os.Geteuid() != 0 {
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
	if err := requireRoot("pgtijori run"); err != nil {
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
	if err := requireRoot("pgtijori serve"); err != nil {
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
	ui, err := controllers.New(svc, p.token())
	if err != nil {
		return err
	}
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
	_, p, err := parseFlags("setup", args)
	if err != nil {
		return err
	}
	if err := requireRoot("pgtijori setup"); err != nil {
		return err
	}
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	engine := pgbackrest.New(cfg, command.Exec{})
	archiving, err := setup.WriteSettings(ctx, engine)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Settings are in %s and %s.\n", pgbackrest.ConfPath, engine.Cluster.ConfDPath())
	if archiving == setup.NeedsRestart {
		fmt.Fprintf(out, "\nPostgreSQL must be restarted once to turn on WAL archiving (a few seconds of downtime).\n"+
			"When it suits you, run:\n\n  %s\n  sudo pgtijori setup\n", setup.RestartCommand(cfg))
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
	st, err := state.Open(p.state)
	if err != nil {
		return err
	}
	if err := st.SetMeta(service.MetaSetupComplete, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nSetup is complete. WAL archiving works.")
	fmt.Fprintln(out, "Next: take the first full backup and dumps, then start the service:")
	fmt.Fprintln(out, "\n  sudo pgtijori run full\n  sudo systemctl enable --now pgtijori")
	return nil
}
