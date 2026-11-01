// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
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
	DBs               []monitor.DBStatus
	DBError           string // the monitor couldn't be asked
	DBProblem         string // setting up database access failed
	NeedRootPassword  bool   // MySQL's root needs a password to set it up
	PGRestart         bool   // PostgreSQL waits for a restart to load pg_stat_statements
	PGRestartCommand  string
	WebLogs           []setup.WebLogPlan // what each web server needs so its requests are read
	WebLogsError      string
	PHP               *setup.PHPPlan // what PHP-FPM needs; nil without pools
	PHPError          string
	Redis             *monitor.RedisStatus // nil without Redis
	NeedRedisPassword bool
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
	d.WebLogs, d.WebLogsError = s.webLogPlans()
	d.PHP, d.PHPError = s.phpPlan()
	if res, err := s.Monitor.Redis(ctx); err == nil && res.Status != nil && !res.Status.Absent {
		d.Redis = res.Status
	}
	if runner := s.Svc.Runner(); runner != nil && !cfg.IsMySQL() {
		d.PGRestart = setup.SharedPreloadPendingRestart(ctx, runner.Cluster)
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
	pending := false
	if runner := s.Svc.Runner(); runner != nil {
		pending = setup.SharedPreloadPendingRestart(ctx, runner.Cluster)
	}
	if !pending {
		s.flash(w, r, "/settings?tab=monitoring", "PostgreSQL doesn't need a restart right now.")
		return
	}
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
	if r.PostFormValue("action") == "test" {
		cfg := s.Svc.Config()
		if err := applyForm(cfg, section, r.PostForm); err != nil {
			s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
			return
		}
		res := s.runTest(r.Context(), cfg, section)
		s.audit(r, "test", section, res.OK)
		s.settingsPage(w, r, tab, cfg, &res, nil)
		return
	}
	var cfg *config.Config
	err := s.Svc.UpdateSettings(r.Context(), func(c *config.Config) error {
		cfg = c
		return applyForm(c, section, r.PostForm)
	})
	if err != nil {
		if cfg == nil {
			cfg = s.Svc.Config()
		}
		s.audit(r, "settings", section, false)
		s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
		return
	}
	s.audit(r, "settings", "saved "+section, true)
	s.flash(w, r, "/settings?tab="+tab, "Saved. The new settings are in use.")
}

// webLogPlans works out the access log changes for the web servers on this machine.
func (s *Server) webLogPlans() ([]setup.WebLogPlan, string) {
	var plans []setup.WebLogPlan
	var problems []string
	if _, err := os.Stat(s.NginxDir); err == nil {
		p, err := setup.PlanNginxLogs(s.NginxDir)
		if err != nil {
			problems = append(problems, "nginx: "+err.Error())
		} else {
			plans = append(plans, p)
		}
	}
	if _, err := os.Stat(s.ApacheDir); err == nil {
		p, err := setup.PlanApacheLogs(s.ApacheDir)
		if err != nil {
			problems = append(problems, "Apache: "+err.Error())
		} else {
			plans = append(plans, p)
		}
	}
	if _, err := os.Stat(s.Caddyfile); err == nil {
		p, err := setup.PlanCaddyLogs(s.Caddyfile)
		if err != nil {
			problems = append(problems, "Caddy: "+err.Error())
		} else {
			plans = append(plans, p)
		}
	}
	return plans, strings.Join(problems, "; ")
}

// monitorWebLogs applies one web server's access log changes (the Apply button): files
// backed up, the config checked, then a reload; on a failed check everything is put back.
func (s *Server) monitorWebLogs(w http.ResponseWriter, r *http.Request) {
	plans, problem := s.webLogPlans()
	want := r.PostFormValue("server")
	for _, p := range plans {
		if p.Server != want {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		err := setup.ApplyWebLogs(ctx, s.Cmd, p, s.WebBackupDir)
		s.audit(r, "monitoring", "access log for "+p.Server, err == nil)
		if err != nil {
			cfg := s.Svc.Config()
			d := settingsData{Cfg: cfg, Tabs: settingsTabs, Tab: "monitoring", DBProblem: err.Error()}
			s.databaseAccess(r.Context(), cfg, &d)
			s.render(w, r, "settings", page{Title: "Settings", Nav: "settings", Data: d})
			return
		}
		s.flash(w, r, "/settings?tab=monitoring", "Done. "+p.Server+" was reloaded with the new access log; requests show in Monitoring → Requests within a minute.")
		return
	}
	s.fail(w, r, fmt.Errorf("no %s config found to change %s", want, problem))
}
