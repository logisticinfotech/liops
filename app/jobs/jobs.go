// SPDX-License-Identifier: Apache-2.0

// Package jobs runs liops's jobs (full, diff, dumps, report), records each run in
// the state file, and sends failure emails. Only one job runs at a time per server,
// across the service and CLI commands: a job that would overlap waits its turn.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysqldump"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/engines/pgdump"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/engines/xtrabackup"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// Job names.
const (
	Full   = "full"
	Diff   = "diff"
	Dumps  = "dumps"
	Report = "report"
)

// Names lists the jobs that can be run by hand.
var Names = []string{Full, Diff, Dumps, Report}

// Triggers.
const (
	Scheduled = "schedule"
	Manual    = "manual"
)

// How long job history and the audit log are kept.
const (
	keepRuns  = 90 * 24 * time.Hour
	keepAudit = 365 * 24 * time.Hour
)

// outputLines is how much command output is kept per run in the state file.
// The log file has all of it.
const outputLines = 200

// Dumper is layer 2 for one database engine: it finds the databases and streams a
// dump of each to storage (pgdump for PostgreSQL).
type Dumper interface {
	Discover(ctx context.Context) ([]string, error)
	Labels(ctx context.Context) (map[string]string, error)
	Dump(ctx context.Context, db string, at time.Time) (key string, size int64, err error)
	DumpGlobals(ctx context.Context, at time.Time) (key string, size int64, err error)
}

// Runner runs jobs.
type Runner struct {
	Cfg    *config.Config
	Server string // name in emails
	State  *state.Store
	Store  storage.Store
	Mail   notify.Mailer
	// Layer 1: pgBackRest for PostgreSQL, XtraBackup for MySQL. The other one is nil.
	Backrest   *pgbackrest.Engine
	XtraBackup *xtrabackup.Engine
	Dumps      Dumper
	Cluster    postgres.Cluster
	LockPath   string
	Now        func() time.Time

	logMu      sync.Mutex
	Log        io.Writer
	progressMu sync.Mutex
	progress   Progress
}

// New builds a Runner with the real storage, SMTP and command runner.
func New(cfg *config.Config, st *state.Store, lockPath string, log io.Writer) (*Runner, error) {
	store, err := storage.Open(cfg)
	if err != nil {
		return nil, err
	}
	run := command.Exec{}
	server := cfg.Server.Name
	if server == "" {
		server, _ = os.Hostname()
	}
	r := &Runner{
		Cfg:      cfg,
		Server:   server,
		State:    st,
		Store:    store,
		Mail:     notify.SMTP{Cfg: cfg.Email},
		LockPath: lockPath,
		Now:      time.Now,
		Log:      log,
	}
	if cfg.IsMySQL() {
		r.XtraBackup = xtrabackup.New(cfg, run, store, filepath.Join(filepath.Dir(lockPath), "xtrabackup"))
		r.Dumps = mysqldump.New(cfg, run, store)
	} else {
		r.Backrest = pgbackrest.New(cfg, run)
		r.Dumps = pgdump.New(cfg, run, store)
		r.Cluster = postgres.Cluster{Cfg: cfg.Postgres, Run: run}
	}
	return r, nil
}

func (r *Runner) location() *time.Location {
	loc, err := r.Cfg.Location()
	if err != nil {
		return time.UTC
	}
	return loc
}

func (r *Runner) now() time.Time { return r.Now().In(r.location()) }

// Run runs one job. full and diff are followed by dumps when dumps are enabled,
// recorded as a separate run.
func (r *Runner) Run(ctx context.Context, job, trigger string) error {
	switch job {
	case Full, Diff, Dumps, Report:
	default:
		return fmt.Errorf("unknown job %q (expected one of: %s)", job, strings.Join(Names, ", "))
	}
	if job == Dumps && !r.Cfg.Dumps.Enabled {
		return errors.New("database dumps are turned off (dumps.enabled: false)")
	}

	r.logf(job, "waiting for any running job to finish")
	unlock, err := r.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Holding the lock means no job is running, so any run still marked running was cut short.
	if n, err := r.State.MarkInterrupted(r.now()); err != nil {
		r.logf(job, "state: %v", err)
	} else if n > 0 {
		r.logf(job, "marked %d earlier run(s) as interrupted", n)
	}

	switch job {
	case Full, Diff:
		err := r.record(ctx, job, trigger, func(run *state.Run) error { return r.backup(ctx, job, trigger, run) })
		if r.Cfg.Dumps.Enabled {
			// Dumps don't depend on the backup, so they run even if it failed.
			err = errors.Join(err, r.record(ctx, Dumps, trigger, func(run *state.Run) error { return r.dumps(ctx, trigger, run) }))
		}
		return err
	case Dumps:
		return r.record(ctx, Dumps, trigger, func(run *state.Run) error { return r.dumps(ctx, trigger, run) })
	default:
		return r.record(ctx, Report, trigger, func(run *state.Run) error { return r.report(ctx, run) })
	}
}

// record wraps a job in a state.Run. A broken state file is logged but never stops a backup.
func (r *Runner) record(ctx context.Context, job, trigger string, fn func(*state.Run) error) error {
	run := &state.Run{Job: job, Trigger: trigger, Start: r.now()}
	if err := r.State.StartRun(run); err != nil {
		r.logf(job, "state: %v (continuing without recording this run)", err)
	}
	r.logf(job, "started (%s)", trigger)

	err := fn(run)

	run.End = r.now()
	run.Status = state.StatusOK
	if err != nil {
		run.Status = state.StatusFailed
		run.Error = firstLine(err.Error())
	}
	if run.ID != 0 {
		if serr := r.State.SaveRun(run); serr != nil {
			r.logf(job, "state: %v", serr)
		}
	}
	if err != nil {
		r.logf(job, "FAILED after %s: %v", notify.Duration(run.End.Sub(run.Start)), err)
	} else {
		r.logf(job, "finished in %s", notify.Duration(run.End.Sub(run.Start)))
	}
	return err
}

// lock takes the server-wide job lock, waiting while another job holds it.
func (r *Runner) lock(ctx context.Context) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(r.LockPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(r.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("job lock %s: %w", r.LockPath, err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// send sends an email. A failure is logged: there is nobody else to tell.
func (r *Runner) send(ctx context.Context, job, subject, body string) {
	if err := r.Mail.Send(ctx, subject, body); err != nil {
		r.logf(job, "could not send email %q: %v", subject, err)
	}
}

func (r *Runner) logf(job, format string, args ...any) {
	if r.Log == nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.Log, "%s [%s] %s\n", r.now().Format("2006-01-02 15:04:05"), job, fmt.Sprintf(format, args...))
}

// logOutput writes a command's full output to the log, indented under its job.
func (r *Runner) logOutput(job string, out []byte) {
	if r.Log == nil || len(out) == 0 {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Fprintf(r.Log, "    %s\n", line)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// tonight is "tonight's " for scheduled runs, so that manual runs don't claim to be nightly.
func tonight(trigger string) string {
	if trigger == Scheduled {
		return "tonight's "
	}
	return "the "
}

// globals names the server-wide dump, for the progress bar (label) and logs (detail).
func (r *Runner) globals() (label, detail string) {
	if r.Cfg.IsMySQL() {
		return "Users and grants", "users and grants"
	}
	return "Roles and tablespaces", "roles and tablespaces (pg_dumpall --globals-only)"
}

func backupName(typ string) string {
	if typ == Full {
		return "full backup"
	}
	return "differential backup"
}
