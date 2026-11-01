// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/config"
)

// wizardStep is one step of the setup wizard.
type wizardStep = struct{ ID, Title string }

// wizardSteps are the wizard's steps for the server's engine, in order (PLAN.md §7.2).
// Only the first differs: PostgreSQL picks a cluster, MySQL creates liops's account.
func wizardSteps(cfg *config.Config) []wizardStep {
	first := wizardStep{"postgres", "PostgreSQL"}
	if cfg.IsMySQL() {
		first = wizardStep{"mysql", "MySQL"}
	}
	return []wizardStep{
		first,
		{"storage", "Storage"},
		{"email", "Email"},
		{"schedules", "Schedules and retention"},
		{"labels", "Labels (optional)"},
		{"review", "Review and apply"},
	}
}

// Apply task step statuses.
const (
	stepPending = "pending"
	stepRunning = "running"
	stepDone    = "done"
	stepFailed  = "failed"
	stepWaiting = "waiting" // for the user: PostgreSQL restart
)

type taskStep struct {
	Title  string
	Status string
	Detail string
}

// applyTask is the background part of "Review and apply". Every step is safe to
// repeat, so a failed or interrupted apply is simply started again.
type applyTask struct {
	Steps   []taskStep
	Running bool
	Done    bool
	Restart string // the command to restart PostgreSQL, while waiting for it
	// GTID lists the MySQL setting changes waiting for the user's approval.
	GTID      []string
	NonInnoDB int // user tables that aren't InnoDB, which the GTID change can affect
}

type wizard struct {
	mu   sync.Mutex
	task *applyTask
}

func newWizard() *wizard { return &wizard{} }

func (wz *wizard) snapshot() *applyTask {
	wz.mu.Lock()
	defer wz.mu.Unlock()
	if wz.task == nil {
		return nil
	}
	t := *wz.task
	t.Steps = slices.Clone(wz.task.Steps)
	return &t
}

func (wz *wizard) set(i int, status, detail string) {
	wz.mu.Lock()
	defer wz.mu.Unlock()
	wz.task.Steps[i].Status, wz.task.Steps[i].Detail = status, detail
}

func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup", s.setupStart)
	step := func(h http.HandlerFunc) http.Handler { return s.beforeSetupDone(h) }
	mux.Handle("GET /setup/{step}", step(s.setupStep))
	mux.Handle("POST /setup/{step}", step(s.setupSave))
	mux.HandleFunc("GET /setup/apply/progress", s.setupProgress)
	mux.Handle("POST /setup/apply/start", step(s.setupApply))
	mux.Handle("POST /setup/apply/restart", step(s.setupRestart))
	mux.Handle("POST /setup/apply/gtid", step(s.setupGTID))
}

// beforeSetupDone sends users to the dashboard once setup is complete, except to
// watch the apply progress.
func (s *Server) beforeSetupDone(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Svc.SetupComplete() && s.Svc.Runner() != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

type wizardData struct {
	Steps    []wizardStep
	Step     string
	Prev     string
	Cfg      *config.Config
	Errors   []string
	Result   *testResult
	Clusters []setup.Cluster
	Problems []string // for the review step
	Task     *applyTask
	Progress jobs.Progress // the backup or dumps the apply step is running
}

func (s *Server) wizardPage(w http.ResponseWriter, r *http.Request, name string, d wizardData) {
	if d.Cfg == nil {
		d.Cfg = s.Svc.Config()
	}
	d.Steps = wizardSteps(d.Cfg)
	for i, st := range d.Steps {
		if st.ID == d.Step && i > 0 {
			d.Prev = d.Steps[i-1].ID
		}
	}
	s.render(w, r, name, page{Title: "Set up liops", Wizard: true, Data: d})
}

// setupStart goes to the first step, or the dashboard once setup is complete.
func (s *Server) setupStart(w http.ResponseWriter, r *http.Request) {
	if s.Svc.SetupComplete() && s.Svc.Runner() != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/setup/"+wizardSteps(s.Svc.Config())[0].ID, http.StatusSeeOther)
}

func validStep(cfg *config.Config, id string) bool {
	return slices.ContainsFunc(wizardSteps(cfg), func(s wizardStep) bool { return s.ID == id })
}

func (s *Server) setupStep(w http.ResponseWriter, r *http.Request) {
	step := r.PathValue("step")
	if !validStep(s.Svc.Config(), step) {
		http.NotFound(w, r)
		return
	}
	d := wizardData{Step: step}
	switch step {
	case "postgres":
		clusters, err := setup.DetectClusters(r.Context(), s.Cmd)
		if err != nil {
			d.Errors = []string{err.Error()}
		}
		d.Clusters = clusters
	case "review":
		if err := s.Svc.Config().Validate(); err != nil {
			d.Problems = strings.Split(err.Error(), "\n")
		}
	}
	s.wizardPage(w, r, "setup_"+step, d)
}

// setupSave stores one step in the draft config and moves to the next step.
// The storage step only moves on once its test passes.
func (s *Server) setupSave(w http.ResponseWriter, r *http.Request) {
	step := r.PathValue("step")
	cfg := s.Svc.Config()
	if !validStep(cfg, step) || step == "review" {
		http.NotFound(w, r)
		return
	}
	d := wizardData{Step: step, Cfg: cfg}
	var sections []string
	switch step {
	case "postgres":
		clusters, err := setup.DetectClusters(r.Context(), s.Cmd)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		d.Clusters = clusters
		idx, err := strconv.Atoi(r.PostFormValue("cluster"))
		if err != nil || idx < 0 || idx >= len(clusters) {
			d.Errors = []string{"Choose a cluster."}
			s.wizardPage(w, r, "setup_postgres", d)
			return
		}
		if !clusters[idx].Online() {
			d.Errors = []string{fmt.Sprintf("Cluster %d/%s is %s. Start it first: %s",
				clusters[idx].Version, clusters[idx].Name, clusters[idx].Status, clusters[idx].StartCommand())}
			s.wizardPage(w, r, "setup_postgres", d)
			return
		}
		setup.Apply(cfg, clusters[idx])
		cfg.PgBackRest.Stanza = strings.TrimSpace(r.PostFormValue("stanza"))
		cfg.Server.Name = strings.TrimSpace(r.PostFormValue("name"))
		if errs := filterErrors(cfg, "postgres.", "pgbackrest.stanza"); len(errs) > 0 {
			d.Errors = errs
			s.wizardPage(w, r, "setup_postgres", d)
			return
		}
	case "mysql":
		cfg.MySQL.BackupName = strings.TrimSpace(r.PostFormValue("backup_name"))
		cfg.Server.Name = strings.TrimSpace(r.PostFormValue("name"))
		if errs := filterErrors(cfg, "mysql."); len(errs) > 0 {
			d.Errors = errs
			s.wizardPage(w, r, "setup_mysql", d)
			return
		}
		// Create liops's own MySQL account now, so a root password problem shows here.
		// The root password is used for this one request and never stored.
		inst := mysql.Instance{Cfg: cfg.MySQL, Run: s.Cmd}
		if _, err := setup.EnsureMySQLAccount(r.Context(), inst, r.PostFormValue("root_password")); err != nil {
			msg := err.Error()
			if errors.Is(err, setup.ErrNeedRootPassword) {
				msg = "MySQL's root account needs a password on this server: enter it below. liops uses it once, to create its own MySQL account, and doesn't keep it."
			}
			d.Errors = []string{msg}
			s.audit(r, "setup", "create the MySQL account", false)
			s.wizardPage(w, r, "setup_mysql", d)
			return
		}
		s.audit(r, "setup", "create the MySQL account", true)
	case "storage":
		sections = []string{sectionStorage}
	case "email":
		sections = []string{sectionEmail}
	case "schedules":
		sections = []string{sectionServer, sectionSchedules, sectionDumps}
	case "labels":
		sections = []string{sectionLabels}
	}
	for _, sec := range sections {
		if err := applyForm(cfg, sec, r.PostForm); err != nil {
			d.Errors = append(d.Errors, strings.Split(err.Error(), "\n")...)
		}
		d.Errors = append(d.Errors, sectionErrors(cfg, sec)...)
	}
	if r.PostFormValue("action") == "skip" && step == "labels" {
		cfg.Dumps.Labels = config.LabelQuery{}
		d.Errors = nil
	}
	if len(d.Errors) > 0 {
		s.wizardPage(w, r, "setup_"+step, d)
		return
	}

	test := r.PostFormValue("action") == "test"
	if test || step == "storage" {
		sec := map[string]string{"storage": sectionStorage, "email": sectionEmail, "labels": sectionLabels}[step]
		if sec != "" {
			res := s.runTest(r.Context(), cfg, sec)
			s.audit(r, "test", sec, res.OK)
			d.Result = &res
			if test || !res.OK {
				// Keep the typed values in the draft, so they survive going back.
				if err := s.Svc.SaveDraft(cfg); err != nil {
					s.fail(w, r, err)
					return
				}
				s.wizardPage(w, r, "setup_"+step, d)
				return
			}
		}
	}

	if err := s.Svc.SaveDraft(cfg); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "setup", "saved step "+step, true)
	steps := wizardSteps(cfg)
	for i, st := range steps {
		if st.ID == step {
			http.Redirect(w, r, "/setup/"+steps[i+1].ID, http.StatusSeeOther)
			return
		}
	}
}

// setupApply starts the background apply task.
func (s *Server) setupApply(w http.ResponseWriter, r *http.Request) {
	cfg := s.Svc.Config()
	if err := cfg.Validate(); err != nil {
		s.flash(w, r, "/setup/review", "Some settings are still missing; see the list below.")
		return
	}
	s.setup.mu.Lock()
	if s.setup.task != nil && s.setup.task.Running {
		s.setup.mu.Unlock()
		http.Redirect(w, r, "/setup/apply/progress", http.StatusSeeOther)
		return
	}
	if cfg.IsMySQL() {
		s.setup.task = &applyTask{Running: true, Steps: []taskStep{
			{Title: "Check MySQL, XtraBackup and the backup name, and turn on GTIDs", Status: stepPending},
			{Title: "Take the first full backup and dump every database", Status: stepPending},
			{Title: "Send the “setup finished” email", Status: stepPending},
		}}
		s.setup.mu.Unlock()
		s.audit(r, "setup", "apply", true)
		go s.runApplyMySQL(cfg, setup.MySQLOptions{})
		http.Redirect(w, r, "/setup/apply/progress", http.StatusSeeOther)
		return
	}
	s.setup.task = &applyTask{Running: true, Steps: []taskStep{
		{Title: "Write the pgBackRest and PostgreSQL settings", Status: stepPending},
		{Title: "Create the pgBackRest stanza and check WAL archiving", Status: stepPending},
		{Title: "Take the first full backup and dump every database", Status: stepPending},
		{Title: "Send the “setup finished” email", Status: stepPending},
	}}
	s.setup.mu.Unlock()
	s.audit(r, "setup", "apply", true)
	go s.runApply(cfg, false)
	http.Redirect(w, r, "/setup/apply/progress", http.StatusSeeOther)
}

// setupRestart restarts PostgreSQL (the user pressed the button) and continues the apply.
func (s *Server) setupRestart(w http.ResponseWriter, r *http.Request) {
	s.setup.mu.Lock()
	t := s.setup.task
	if t == nil || t.Running || t.Restart == "" {
		s.setup.mu.Unlock()
		http.Redirect(w, r, "/setup/apply/progress", http.StatusSeeOther)
		return
	}
	t.Running, t.Restart = true, ""
	s.setup.mu.Unlock()
	restart := r.PostFormValue("restart") == "now"
	s.audit(r, "setup", map[bool]string{true: "restart PostgreSQL now", false: "continue after a manual restart"}[restart], true)
	go s.runApply(s.Svc.Config(), restart)
	http.Redirect(w, r, "/setup/apply/progress", http.StatusSeeOther)
}

// setupGTID makes the MySQL setting changes the user approved (the button on the apply
// page) and continues the apply. A root password, if given, is used and forgotten.
func (s *Server) setupGTID(w http.ResponseWriter, r *http.Request) {
	s.setup.mu.Lock()
	t := s.setup.task
	if t == nil || t.Running || len(t.GTID) == 0 {
		s.setup.mu.Unlock()
		http.Redirect(w, r, "/setup/apply/progress", http.StatusSeeOther)
		return
	}
	t.Running, t.GTID = true, nil
	s.setup.mu.Unlock()
	s.audit(r, "setup", "turn on GTIDs", true)
	go s.runApplyMySQL(s.Svc.Config(), setup.MySQLOptions{ApplySettings: true, RootPassword: r.PostFormValue("root_password")})
	http.Redirect(w, r, "/setup/apply/progress", http.StatusSeeOther)
}

// runApplyMySQL is the apply task for MySQL: check the server and claim the backup
// name, wait for the user to approve the GTID change if it's needed, then the first
// backup and the email.
func (s *Server) runApplyMySQL(cfg *config.Config, opts setup.MySQLOptions) {
	wz := s.setup
	ctx := context.Background()
	defer func() {
		wz.mu.Lock()
		wz.task.Running = false
		wz.mu.Unlock()
	}()
	runner := s.Svc.Runner()
	if runner == nil {
		wz.set(0, stepFailed, "the config is no longer valid")
		return
	}

	wz.set(0, stepRunning, "")
	res, err := setup.SetupMySQL(ctx, cfg, s.Cmd, runner.Store, opts)
	if errors.Is(err, setup.ErrNeedRootPassword) && opts.ApplySettings {
		// Changing the settings needs root, and root needs a password: ask again.
		res, err = setup.SetupMySQL(ctx, cfg, s.Cmd, runner.Store, setup.MySQLOptions{})
		if err == nil && len(res.Pending) > 0 {
			err = errors.New("MySQL's root account needs a password on this server to change these settings: enter it below")
		}
	}
	if len(res.Pending) > 0 {
		changes := make([]string, len(res.Pending))
		for i, c := range res.Pending {
			changes[i] = c.String()
		}
		detail := "Waiting for you: point-in-time backups need these MySQL settings changed. Nothing has been changed yet."
		if err != nil {
			detail = err.Error()
		}
		wz.set(0, stepWaiting, detail)
		wz.mu.Lock()
		wz.task.GTID, wz.task.NonInnoDB = changes, res.NonInnoDBTables
		wz.mu.Unlock()
		return
	}
	if err != nil {
		wz.set(0, stepFailed, err.Error())
		return
	}
	if err := s.Svc.MarkSetupComplete(); err != nil {
		wz.set(0, stepFailed, err.Error())
		return
	}
	wz.set(0, stepDone, fmt.Sprintf("MySQL %s, with XtraBackup; GTIDs are on (kept in %s). Scheduled backups are now on.", res.Version, cfg.MySQL.ConfFile))
	s.firstBackupAndEmail(ctx, cfg, runner, 1)
}

func (s *Server) runApply(cfg *config.Config, restart bool) {
	wz := s.setup
	ctx := context.Background()
	finish := func() {
		wz.mu.Lock()
		wz.task.Running = false
		wz.mu.Unlock()
	}
	defer finish()
	engine := pgbackrest.New(cfg, s.Cmd)

	// 1. Settings, and the restart that turning on archive_mode needs.
	wz.set(0, stepRunning, "")
	if restart {
		wz.set(0, stepRunning, "Restarting PostgreSQL…")
		if err := setup.RestartPostgres(ctx, engine); err != nil {
			wz.set(0, stepFailed, err.Error())
			return
		}
	}
	runner := s.Svc.Runner()
	if runner == nil {
		wz.set(0, stepFailed, "the config is no longer valid")
		return
	}
	archiving, err := setup.WriteSettings(ctx, engine, runner.Store)
	if err != nil {
		wz.set(0, stepFailed, err.Error())
		return
	}
	if archiving == setup.NeedsRestart {
		wz.set(0, stepWaiting, "PostgreSQL must be restarted once to turn on WAL archiving. That's a few seconds of downtime.")
		wz.mu.Lock()
		wz.task.Restart = setup.RestartCommand(cfg)
		wz.mu.Unlock()
		return
	}
	wz.set(0, stepDone, "Wrote "+pgbackrest.ConfPath(cfg)+" and "+engine.Cluster.ConfDPath()+"; WAL archiving is on.")

	// 2. Stanza.
	wz.set(1, stepRunning, "")
	if err := setup.CreateStanza(ctx, engine); err != nil {
		detail := err.Error()
		var se *setup.StepError
		if errors.As(err, &se) {
			detail += "\n\n" + command.LastLines(se.Output, 20)
		}
		wz.set(1, stepFailed, detail)
		return
	}
	if err := s.Svc.MarkSetupComplete(); err != nil {
		wz.set(1, stepFailed, err.Error())
		return
	}
	wz.set(1, stepDone, "WAL archiving works end to end. Scheduled backups are now on.")
	s.firstBackupAndEmail(ctx, cfg, runner, 2)
}

// firstBackupAndEmail runs the apply task's last two steps, from task step i: the
// first full backup and dumps, then the "setup finished" email.
func (s *Server) firstBackupAndEmail(ctx context.Context, cfg *config.Config, runner *jobs.Runner, i int) {
	wz := s.setup
	// First backup and dumps. Failures are emailed by the job itself.
	wz.set(i, stepRunning, "This can take a while for a large database. You can leave this page; progress is also on the Jobs page.")
	backupErr := runner.Run(ctx, jobs.Full, jobs.Manual)
	backupOK := true
	if run, err := s.Svc.State.LastRun(jobs.Full, ""); err != nil || run == nil || run.Status != state.StatusOK {
		backupOK = false
	}
	dumped, total := 0, 0
	if run, err := s.Svc.State.LastRun(jobs.Dumps, ""); err == nil && run != nil {
		for _, d := range run.Dumps {
			total++
			if d.Error == "" {
				dumped++
			}
		}
	}
	if backupErr != nil {
		wz.set(i, stepFailed, backupErr.Error()+"\n\nSee the Jobs page for details. Scheduled backups are on, so it will be retried.")
	} else {
		wz.set(i, stepDone, fmt.Sprintf("Full backup done; %d of %d databases dumped.", dumped, total))
	}

	// Email.
	wz.set(i+1, stepRunning, "")
	var nextFull time.Time
	spec := cfg.PgBackRest.FullSchedule
	if cfg.IsMySQL() {
		spec = cfg.MySQL.FullSchedule
	}
	if sched, err := cron.ParseStandard(spec); err == nil {
		if loc, err := cfg.Location(); err == nil {
			nextFull = sched.Next(time.Now().In(loc))
		}
	}
	subject, body := notify.SetupFinished(runner.Server, backupOK, dumped, total, nextFull)
	if err := runner.Mail.Send(ctx, subject, body); err != nil {
		wz.set(i+1, stepFailed, err.Error())
	} else {
		wz.set(i+1, stepDone, "Sent to "+strings.Join(cfg.Email.To, ", ")+".")
	}
	wz.mu.Lock()
	wz.task.Done = true
	wz.mu.Unlock()
}

func (s *Server) setupProgress(w http.ResponseWriter, r *http.Request) {
	t := s.setup.snapshot()
	if t == nil {
		if s.Svc.SetupComplete() {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/setup/review", http.StatusSeeOther)
		}
		return
	}
	refresh := 0
	if t.Running {
		refresh = 3
	}
	d := wizardData{Step: "review", Task: t}
	if runner := s.Svc.Runner(); runner != nil && t.Running {
		d.Progress = runner.Progress()
	}
	d.Cfg = s.Svc.Config()
	d.Steps = wizardSteps(d.Cfg)
	s.render(w, r, "setup_apply", page{Title: "Set up liops", Wizard: true, Refresh: refresh, Data: d})
}
