// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/pgtijori/app/engines/pgbackrest"
	"github.com/logisticinfotech/pgtijori/app/engines/pgdump"
	"github.com/logisticinfotech/pgtijori/app/jobs"
	"github.com/logisticinfotech/pgtijori/app/state"
	"github.com/logisticinfotech/pgtijori/app/storage"
)

// downloadTTL is how long a download link works. The browser downloads straight
// from storage; pgtijori isn't in the path.
const downloadTTL = 10 * time.Minute

// Overall health on the dashboard.
const (
	HealthOK        = "OK"
	HealthAttention = "ATTENTION"
	HealthFailed    = "FAILED"
)

type dashboardData struct {
	Health     string
	Problems   []string
	LastFull   *pgbackrest.Backup
	LastDiff   *pgbackrest.Backup
	Restore    time.Time
	LastWAL    time.Time
	Storage    int64
	Disk       int
	Dumps      *state.Run
	DumpsOK    int
	Next       []jobs.Next
	FailedRuns []state.Run
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	h := s.Svc.Health(r.Context())
	if h == nil {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	d := dashboardData{
		Problems: h.Summary.Problems,
		Restore:  h.Summary.RestoreFrom,
		LastWAL:  h.Summary.LastWAL,
		Storage:  h.Summary.StorageUsed,
		Disk:     h.Summary.DiskUsedPct,
		Next:     s.Svc.Next(),
	}
	if h.Info != nil {
		d.LastFull, d.LastDiff = h.Info.Last("full"), h.Info.Last("diff")
	}

	// FAILED when the newest backup run or the newest dumps run failed.
	d.Health = HealthOK
	if len(d.Problems) > 0 {
		d.Health = HealthAttention
	}
	var newestBackup *state.Run
	for _, job := range []string{jobs.Full, jobs.Diff} {
		if run, err := s.Svc.State.LastRun(job, ""); err == nil && run != nil && run.Status != state.StatusRunning {
			if newestBackup == nil || run.Start.After(newestBackup.Start) {
				newestBackup = run
			}
		}
	}
	dumps, _ := s.Svc.State.LastRun(jobs.Dumps, "")
	for _, run := range []*state.Run{newestBackup, dumps} {
		if run != nil && (run.Status == state.StatusFailed || run.Status == state.StatusInterrupted) {
			d.Health = HealthFailed
			d.FailedRuns = append(d.FailedRuns, *run)
		}
	}
	if len(d.FailedRuns) > 0 {
		// The failed run is listed already; don't repeat it as a problem.
		d.Problems = slices.DeleteFunc(slices.Clone(d.Problems), func(p string) bool { return strings.HasPrefix(p, "the last database dumps") })
	}
	if dumps != nil {
		d.Dumps = dumps
		for _, x := range dumps.Dumps {
			if x.Error == "" {
				d.DumpsOK++
			}
		}
	}
	s.render(w, r, "dashboard", page{Title: "Dashboard", Nav: "dashboard", Data: d})
}

type backupsData struct {
	Info    *pgbackrest.Info
	Backups []pgbackrest.Backup // newest first
}

func (s *Server) backups(w http.ResponseWriter, r *http.Request) {
	info, err := s.Svc.Runner().Backrest.Info(r.Context())
	if err != nil {
		s.fail(w, r, errors.New("couldn't read the backup list from pgBackRest: "+err.Error()))
		return
	}
	d := backupsData{Info: info, Backups: slices.Clone(info.Backups)}
	slices.Reverse(d.Backups)
	s.render(w, r, "backups", page{Title: "Point-in-time backups", Nav: "backups", Data: d})
}

// dbRow is one database on the Databases page.
type dbRow struct {
	Name         string
	Label        string
	Last         *pgdump.Dump
	Count        int
	Total        int64
	LastError    string // from the newest dumps run
	DeletedSince time.Time
	KeptUntil    time.Time
}

type databasesData struct {
	Rows    []dbRow
	Globals []storage.Object
	LastRun *state.Run
}

// dumpsInventory lists stored dumps and joins them with the last run and deleted databases.
func (s *Server) dumpsInventory(r *http.Request) (databasesData, error) {
	var d databasesData
	runner := s.Svc.Runner()
	objs, err := runner.Store.List(r.Context(), storage.DumpsPrefix)
	if err != nil {
		return d, err
	}
	globals, err := runner.Store.List(r.Context(), storage.GlobalsPrefix)
	if err != nil {
		return d, err
	}
	sort.Slice(globals, func(i, j int) bool { return globals[i].Key > globals[j].Key })
	d.Globals = globals

	grouped := pgdump.Group(objs)
	rows := map[string]*dbRow{}
	row := func(db string) *dbRow {
		if rows[db] == nil {
			rows[db] = &dbRow{Name: db}
		}
		return rows[db]
	}
	for db, dumps := range grouped {
		x := row(db)
		x.Count = len(dumps)
		x.Last = &dumps[len(dumps)-1]
		for _, dump := range dumps {
			x.Total += dump.Size
		}
	}
	if run, err := s.Svc.State.LastRun(jobs.Dumps, ""); err == nil && run != nil {
		d.LastRun = run
		for _, res := range run.Dumps {
			x := row(res.DB)
			x.Label, x.LastError = res.Label, res.Error
		}
	}
	keep := time.Duration(runner.Cfg.Dumps.DeletedRetentionDays) * 24 * time.Hour
	if deleted, err := s.Svc.State.Deleted(); err == nil {
		for db, since := range deleted {
			x := row(db)
			x.DeletedSince, x.KeptUntil, x.LastError = since, since.Add(keep), ""
		}
	}
	for _, x := range rows {
		d.Rows = append(d.Rows, *x)
	}
	sort.Slice(d.Rows, func(i, j int) bool {
		if d.Rows[i].DeletedSince.IsZero() != d.Rows[j].DeletedSince.IsZero() {
			return d.Rows[i].DeletedSince.IsZero() // live databases first
		}
		return d.Rows[i].Name < d.Rows[j].Name
	})
	return d, nil
}

func (s *Server) databases(w http.ResponseWriter, r *http.Request) {
	d, err := s.dumpsInventory(r)
	if err != nil {
		s.fail(w, r, errors.New("couldn't list the dumps in storage: "+err.Error()))
		return
	}
	s.render(w, r, "databases", page{Title: "Databases", Nav: "databases", Data: d})
}

type databaseData struct {
	Row   dbRow
	Dumps []pgdump.Dump // newest first
	Runs  []dbRun
}

// dbRun is one night's result for a database.
type dbRun struct {
	At time.Time
	state.DumpRun
}

func (s *Server) database(w http.ResponseWriter, r *http.Request) {
	db := r.PathValue("db")
	objs, err := s.Svc.Runner().Store.List(r.Context(), pgdump.Prefix(db))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dumps := pgdump.Group(objs)[db]
	slices.Reverse(dumps)
	inv, err := s.dumpsInventory(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := databaseData{Dumps: dumps}
	for _, row := range inv.Rows {
		if row.Name == db {
			d.Row = row
		}
	}
	if d.Row.Name == "" {
		http.NotFound(w, r)
		return
	}
	// The last 14 nights' results for this database.
	runs, _ := s.Svc.State.Runs(jobs.Dumps, 14)
	for _, run := range runs {
		for _, res := range run.Dumps {
			if res.DB == db {
				d.Runs = append(d.Runs, dbRun{At: run.Start, DumpRun: res})
			}
		}
	}
	s.render(w, r, "database", page{Title: d.Row.Name, Nav: "databases", Data: d})
}

// download sends the browser to a short-lived link for one dump. Every download is audited.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	if !strings.HasPrefix(key, storage.DumpsPrefix) && !strings.HasPrefix(key, storage.GlobalsPrefix) {
		http.Error(w, "only database dumps can be downloaded", http.StatusBadRequest)
		return
	}
	store := s.Svc.Runner().Store
	objs, err := store.List(r.Context(), key)
	if err != nil || !slices.ContainsFunc(objs, func(o storage.Object) bool { return o.Key == key }) {
		s.audit(r, "download", key+" (not found)", false)
		s.flash(w, r, "/databases", "That dump doesn't exist any more.")
		return
	}
	url, err := store.DownloadURL(r.Context(), key, downloadTTL)
	if err != nil {
		s.audit(r, "download", key, false)
		s.fail(w, r, err)
		return
	}
	s.audit(r, "download", key, true)
	http.Redirect(w, r, url, http.StatusSeeOther)
}

type jobsData struct {
	Runs   []state.Run
	Job    string
	Active map[string]string
	Names  []string
	Manual []string
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	job := r.URL.Query().Get("job")
	if job != "" && !slices.Contains(jobs.Names, job) {
		job = ""
	}
	runs, err := s.Svc.State.Runs(job, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := jobsData{Runs: runs, Job: job, Active: s.Svc.Active(), Names: jobs.Names, Manual: []string{jobs.Diff, jobs.Dumps, jobs.Report}}
	refresh := 0
	if len(d.Active) > 0 || slices.ContainsFunc(runs, func(r state.Run) bool { return r.Status == state.StatusRunning }) {
		refresh = 5
	}
	s.render(w, r, "jobs", page{Title: "Jobs", Nav: "jobs", Refresh: refresh, Data: d})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.Svc.State.GetRun(id)
	if errors.Is(err, state.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	refresh := 0
	if run.Status == state.StatusRunning {
		refresh = 5
	}
	s.render(w, r, "job", page{Title: "Job #" + strconv.FormatUint(id, 10), Nav: "jobs", Refresh: refresh, Data: run})
}

func (s *Server) runNow(w http.ResponseWriter, r *http.Request) {
	job := r.PostFormValue("job")
	if job != jobs.Diff && job != jobs.Dumps && job != jobs.Report {
		http.Error(w, "this job can't be started from the UI", http.StatusBadRequest)
		return
	}
	if err := s.Svc.RunNow(job); err != nil {
		s.audit(r, "run", job, false)
		s.flash(w, r, "/jobs", capitalize(err.Error())+".")
		return
	}
	s.audit(r, "run", job, true)
	s.flash(w, r, "/jobs", "Started "+job+". It waits if another job is running. This page refreshes until it's done.")
}

type auditData struct {
	Entries []state.AuditEntry
	Older   uint64
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseUint(r.URL.Query().Get("before"), 10, 64)
	const pageSize = 100
	entries, err := s.Svc.State.AuditLog(before, pageSize)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := auditData{Entries: entries}
	if len(entries) == pageSize {
		d.Older = entries[len(entries)-1].ID
	}
	s.render(w, r, "audit", page{Title: "Audit log", Nav: "audit", Data: d})
}
