// SPDX-License-Identifier: Apache-2.0

// Package service is the long-running liops process (`liops serve`): it holds
// the current config, the job runner and the scheduler, and lets the UI change
// settings and start jobs without a restart.
//
// Before setup is complete (no valid config, or the stanza not yet created), only
// the UI runs, so the setup wizard can be used. The scheduler starts once setup is done.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// MetaSetupComplete is set in the state file once the stanza exists and WAL
// archiving works (by `liops setup` or the wizard).
const MetaSetupComplete = "setup_complete"

// healthTTL is how long the dashboard reuses a health check.
const healthTTL = time.Minute

// Service is the running liops.
type Service struct {
	ConfigPath string
	LockPath   string
	State      *state.Store
	Log        io.Writer
	NewRunner  func(cfg *config.Config) (*jobs.Runner, error)
	// WriteSettings rewrites the pgBackRest and PostgreSQL settings; tests replace it.
	WriteSettings func(ctx context.Context, cfg *config.Config) (setup.Archiving, error)
	// SetMonitor starts (and enables) or stops (and disables) the liops-monitor service;
	// tests replace it.
	SetMonitor func(on bool) error
	// Verdict is the monitoring verdict for the daily summary, in lines (serve sets it from
	// the UI, which gathers the evidence); nil leaves it out.
	Verdict func(ctx context.Context) []string

	ctx    context.Context // jobs started by the UI or scheduler run with this
	mu     sync.Mutex
	cfg    *config.Config
	runner *jobs.Runner // nil while the config isn't valid
	sched  *jobs.Scheduler
	closed bool
	active map[string]string // job → "queued" or "running", for jobs started from the UI

	healthMu sync.Mutex
	health   *jobs.Health
	healthAt time.Time

	logMu       sync.Mutex
	appLogSizes map[string]int64
	appLogAt    time.Time
	appLogRates []LogRate
}

// New loads the config (a draft is fine) and starts the scheduler if setup is
// complete. newRunner builds job runners; nil means the real one (tests pass fakes).
func New(ctx context.Context, configPath, lockPath string, st *state.Store, log io.Writer, newRunner func(*config.Config) (*jobs.Runner, error)) (*Service, error) {
	s := &Service{
		ConfigPath: configPath,
		LockPath:   lockPath,
		State:      st,
		Log:        log,
		NewRunner:  newRunner,
		ctx:        ctx,
		active:     map[string]string{},
	}
	s.WriteSettings = func(ctx context.Context, cfg *config.Config) (setup.Archiving, error) {
		store, err := storage.Open(cfg)
		if err != nil {
			return setup.NeedsRestart, err
		}
		return setup.WriteSettings(ctx, pgbackrest.New(cfg, command.Exec{}), store)
	}
	s.SetMonitor = func(on bool) error {
		args := []string{"disable", "--now", "liops-monitor"}
		if on {
			args[0] = "enable"
		}
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if s.NewRunner == nil {
		s.NewRunner = func(cfg *config.Config) (*jobs.Runner, error) { return jobs.New(cfg, st, lockPath, log) }
	}
	cfg, err := config.LoadDraft(configPath)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyLocked(cfg); err != nil {
		return nil, err
	}
	go s.pickUpSetup()
	return s, nil
}

// setupPoll is how often a service whose scheduler isn't running yet looks for setup
// finished outside it.
var setupPoll = 5 * time.Second

// pickUpSetup notices setup finished outside the service: `liops setup` on the command
// line, after install.sh started the service, writes the config and marks setup
// complete in the state file. Until the scheduler runs, it reloads the config when setup
// is complete and the config file has changed since it last looked.
func (s *Service) pickUpSetup() {
	t := time.NewTicker(setupPoll)
	defer t.Stop()
	var seen time.Time
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		if !s.closed && s.sched == nil && s.setupComplete() {
			if info, err := os.Stat(s.ConfigPath); err == nil && !info.ModTime().Equal(seen) {
				seen = info.ModTime()
				if cfg, err := config.LoadDraft(s.ConfigPath); err != nil {
					s.logf("setup was completed outside the service, but the config can't be read: %v", err)
				} else if err := s.applyLocked(cfg); err != nil {
					s.logf("setup was completed outside the service, but the scheduler can't start: %v", err)
				} else if s.sched != nil {
					s.logf("setup was completed outside the service (liops setup): backups now follow the schedule")
				}
			}
		}
		s.mu.Unlock()
	}
}

func (s *Service) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "%s [serve] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	}
}

// applyLocked makes cfg the live config: rebuilds the runner and (re)starts the
// scheduler when the config is valid and setup is complete.
func (s *Service) applyLocked(cfg *config.Config) error {
	if s.sched != nil {
		s.sched.Stop() // running jobs finish with the runner they started with
		s.sched = nil
	}
	s.cfg, s.runner = cfg, nil
	s.invalidateHealth()
	if cfg.Validate() != nil {
		s.logf("config is incomplete: the setup wizard is waiting in the UI")
		return nil
	}
	r, err := s.NewRunner(cfg)
	if err != nil {
		return err
	}
	r.Verdict = func(ctx context.Context) []string {
		if s.Verdict == nil {
			return nil
		}
		return s.Verdict(ctx)
	}
	s.runner = r
	if !s.setupComplete() {
		s.logf("setup isn't finished: backups start once it is")
		return nil
	}
	sched, err := r.StartScheduler(s.ctx)
	if err != nil {
		return err
	}
	s.sched = sched
	return nil
}

func (s *Service) setupComplete() bool {
	v, err := s.State.GetMeta(MetaSetupComplete)
	return err == nil && v != ""
}

// SetupComplete reports whether setup has finished.
func (s *Service) SetupComplete() bool { return s.setupComplete() }

// Config returns a copy of the current config (possibly an incomplete draft).
func (s *Service) Config() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Clone()
}

// Runner returns the job runner, or nil while the config isn't valid.
func (s *Service) Runner() *jobs.Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runner
}

// Next lists the next scheduled runs, or nothing while the scheduler is off.
func (s *Service) Next() []jobs.Next {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sched == nil {
		return nil
	}
	return s.sched.Next()
}

// SaveDraft saves a config that may be incomplete (the wizard's progress) and makes it live.
func (s *Service) SaveDraft(cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cfg.Save(s.ConfigPath); err != nil {
		return err
	}
	return s.applyLocked(cfg)
}

// SaveSettings validates and saves a changed config, rewrites the pgBackRest and
// PostgreSQL settings when setup is complete, and makes it live.
func (s *Service) SaveSettings(ctx context.Context, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if s.setupComplete() && !cfg.IsMySQL() {
		archiving, err := s.WriteSettings(ctx, cfg)
		if err != nil {
			return err
		}
		if archiving == setup.NeedsRestart {
			// Settings changed from the UI never turn archiving off, so this means
			// someone changed PostgreSQL by hand. Save anyway, but say so.
			defer s.logf("warning: archive_mode is off; WAL archiving needs: %s", setup.RestartCommand(cfg))
		}
	}
	if on := cfg.Monitor.Enabled; on != s.Config().Monitor.Enabled {
		if err := s.SetMonitor(on); err != nil {
			return fmt.Errorf("couldn't %s the monitoring service: %w", map[bool]string{true: "start", false: "stop"}[on], err)
		}
	}
	return s.SaveDraft(cfg)
}

// MarkSetupComplete records that setup has finished and starts the scheduler.
func (s *Service) MarkSetupComplete() error {
	if err := s.State.SetMeta(MetaSetupComplete, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(s.cfg)
}

// RunNow starts a job in the background. It's queued behind any running job.
func (s *Service) RunNow(job string) error {
	r := s.Runner()
	if r == nil {
		return errors.New("the config is incomplete: finish setup first")
	}
	s.mu.Lock()
	if _, busy := s.active[job]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%s is already queued or running", job)
	}
	s.active[job] = "queued"
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.active, job)
			s.mu.Unlock()
			s.invalidateHealth()
		}()
		if err := r.Run(s.ctx, job, jobs.Manual); err != nil && s.ctx.Err() == nil {
			s.logf("%s (run from the UI) failed: %v", job, err)
		}
	}()
	return nil
}

// Active returns jobs started from the UI that haven't finished.
func (s *Service) Active() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]string, len(s.active))
	for k, v := range s.active {
		m[k] = v
	}
	return m
}

// Health returns a recent health check (at most a minute old), or nil while the
// config isn't valid.
func (s *Service) Health(ctx context.Context) *jobs.Health {
	r := s.Runner()
	if r == nil {
		return nil
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.health != nil && time.Since(s.healthAt) < healthTTL {
		return s.health
	}
	// Not tied to the request: a closed browser tab mustn't cache errors for everyone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	h := r.Health(ctx, false)
	s.health, s.healthAt = &h, time.Now()
	return s.health
}

func (s *Service) invalidateHealth() {
	s.healthMu.Lock()
	s.health = nil
	s.healthMu.Unlock()
}

// Close stops the scheduler and waits for running jobs (whose context the caller
// has already cancelled).
func (s *Service) Close() {
	s.mu.Lock()
	sched := s.sched
	s.sched, s.closed = nil, true
	s.mu.Unlock()
	if sched != nil {
		<-sched.Stop().Done()
	}
}
