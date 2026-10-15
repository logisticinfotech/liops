// SPDX-License-Identifier: Apache-2.0

// Package service is the long-running pgtijori process (`pgtijori serve`): it holds
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
	"sync"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/pgbackrest"
	"github.com/logisticinfotech/pgtijori/app/jobs"
	"github.com/logisticinfotech/pgtijori/app/setup"
	"github.com/logisticinfotech/pgtijori/app/state"
	"github.com/logisticinfotech/pgtijori/config"
)

// MetaSetupComplete is set in the state file once the stanza exists and WAL
// archiving works (by `pgtijori setup` or the wizard).
const MetaSetupComplete = "setup_complete"

// healthTTL is how long the dashboard reuses a health check.
const healthTTL = time.Minute

// Service is the running pgtijori.
type Service struct {
	ConfigPath string
	LockPath   string
	State      *state.Store
	Log        io.Writer
	NewRunner  func(cfg *config.Config) (*jobs.Runner, error)
	// WriteSettings rewrites the pgBackRest and PostgreSQL settings; tests replace it.
	WriteSettings func(ctx context.Context, cfg *config.Config) (setup.Archiving, error)

	ctx    context.Context // jobs started by the UI or scheduler run with this
	mu     sync.Mutex
	cfg    *config.Config
	runner *jobs.Runner // nil while the config isn't valid
	sched  *jobs.Scheduler
	active map[string]string // job → "queued" or "running", for jobs started from the UI

	healthMu sync.Mutex
	health   *jobs.Health
	healthAt time.Time
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
		return setup.WriteSettings(ctx, pgbackrest.New(cfg, command.Exec{}))
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
	return s, nil
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
	if s.setupComplete() {
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
	s.sched = nil
	s.mu.Unlock()
	if sched != nil {
		<-sched.Stop().Done()
	}
}
