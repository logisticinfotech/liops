// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/config"
)

type settingsData struct {
	Cfg    *config.Config
	Result *testResult
	Errors map[string][]string // per section
	Tabs   []settingsTab
	Tab    string // the tab shown

	// The Monitoring tab: the databases the monitor found, and what's left to set up.
	DBs              []monitor.DBStatus
	DBError          string // the monitor couldn't be asked
	DBProblem        string // setting up database access failed
	NeedRootPassword bool   // MySQL's root needs a password to set it up
	PGRestart        bool   // PostgreSQL waits for a restart to load pg_stat_statements
	PGRestartCommand string
}

// settingsTab is one tab of the Settings page and the sections (forms) on it.
type settingsTab struct {
	ID, Title string
	Sections  []string
}

var settingsTabs = []settingsTab{
	{"server", "Server", []string{sectionServer}},
	{"storage", "Storage", []string{sectionStorage}},
	{"email", "Email", []string{sectionEmail}},
	{"backups", "Backups", []string{sectionSchedules}},
	{"dumps", "Dumps", []string{sectionDumps, sectionLabels}},
	{"monitoring", "Monitoring", []string{sectionMonitor}},
}

// tabOf returns the tab a section's form is on.
func tabOf(section string) string {
	for _, t := range settingsTabs {
		if slices.Contains(t.Sections, section) {
			return t.ID
		}
	}
	return settingsTabs[0].ID
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, tab string, cfg *config.Config, res *testResult, errs map[string][]string) {
	if !slices.ContainsFunc(settingsTabs, func(t settingsTab) bool { return t.ID == tab }) {
		tab = settingsTabs[0].ID
	}
	d := settingsData{Cfg: cfg, Result: res, Errors: errs, Tabs: settingsTabs, Tab: tab}
	if tab == "monitoring" && cfg.Monitor.Enabled {
		s.databaseAccess(r.Context(), cfg, &d)
	}
	s.render(w, r, "settings", page{Title: "Settings", Nav: "settings", Data: d})
}

// databaseAccess fills in the Monitoring tab's databases.
func (s *Server) databaseAccess(ctx context.Context, cfg *config.Config, d *settingsData) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dbs, err := s.Monitor.DBs(ctx)
	if err != nil {
		d.DBError = "The monitoring service isn't answering (it starts when you save with monitoring on): " + err.Error()
	}
	d.DBs = dbs
	if runner := s.Svc.Runner(); runner != nil && !cfg.IsMySQL() {
		rows, err := runner.Cluster.Query(ctx, "postgres", "SELECT pending_restart FROM pg_settings WHERE name = 'shared_preload_libraries'")
		d.PGRestart = err == nil && len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] == "t"
		d.PGRestartCommand = setup.RestartCommand(cfg)
	}
}

// monitorDatabases gives the monitoring service its read-only database login (the
// Set up database access button).
func (s *Server) monitorDatabases(w http.ResponseWriter, r *http.Request) {
	cfg := s.Svc.Config()
	runner := s.Svc.Runner()
	fail := func(d settingsData) {
		d.Cfg, d.Tabs, d.Tab = cfg, settingsTabs, "monitoring"
		s.databaseAccess(r.Context(), cfg, &d)
		s.render(w, r, "settings", page{Title: "Settings", Nav: "settings", Data: d})
	}
	if runner == nil {
		fail(settingsData{DBProblem: "Finish setting up backups first: database access uses liops's connection to the database."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	msg := "Done. The monitor can now read the database's statistics; query statistics start within 5 minutes."
	var err error
	if cfg.IsMySQL() {
		err = setup.MySQLMonitorAccess(ctx, runner.XtraBackup.Instance, r.PostFormValue("mysql_root_password"))
		if errors.Is(err, setup.ErrNeedRootPassword) {
			s.audit(r, "monitoring", "database access: MySQL root needs a password", false)
			fail(settingsData{NeedRootPassword: true, DBProblem: "MySQL's root account needs its password here. It's used once to create the monitoring account, and never stored."})
			return
		}
	} else {
		var restart bool
		if restart, err = setup.PostgresMonitorAccess(ctx, runner.Cluster); restart {
			msg = "Done. PostgreSQL needs one restart to load pg_stat_statements (the query statistics); see below."
		}
	}
	s.audit(r, "monitoring", "database access", err == nil)
	if err != nil {
		fail(settingsData{DBProblem: "Setting up database access failed: " + err.Error()})
		return
	}
	s.flash(w, r, "/settings?tab=monitoring", msg)
}

// monitorRestart restarts PostgreSQL to load pg_stat_statements, when a person clicks
// the button for it.
func (s *Server) monitorRestart(w http.ResponseWriter, r *http.Request) {
	cfg := s.Svc.Config()
	if cfg.IsMySQL() {
		http.Error(w, "only PostgreSQL needs this restart", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	err := setup.RestartPostgres(ctx, pgbackrest.New(cfg, s.Cmd))
	s.audit(r, "monitoring", "restart PostgreSQL for pg_stat_statements", err == nil)
	if err != nil {
		s.fail(w, r, fmt.Errorf("restarting PostgreSQL failed: %w", err))
		return
	}
	s.flash(w, r, "/settings?tab=monitoring", "PostgreSQL restarted. Query statistics start within 5 minutes.")
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.settingsPage(w, r, r.URL.Query().Get("tab"), s.Svc.Config(), nil, nil)
}

// saveSettings handles each section's Save and Test buttons.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	tab := tabOf(section)
	cfg := s.Svc.Config()
	if err := applyForm(cfg, section, r.PostForm); err != nil {
		s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
		return
	}
	if r.PostFormValue("action") == "test" {
		res := s.runTest(r.Context(), cfg, section)
		s.audit(r, "test", section, res.OK)
		s.settingsPage(w, r, tab, cfg, &res, nil)
		return
	}
	if err := cfg.Validate(); err != nil {
		s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
		return
	}
	if err := s.Svc.SaveSettings(r.Context(), cfg); err != nil {
		s.audit(r, "settings", section, false)
		s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: {err.Error()}})
		return
	}
	s.audit(r, "settings", "saved "+section, true)
	s.flash(w, r, "/settings?tab="+tab, "Saved. The new settings are in use.")
}
