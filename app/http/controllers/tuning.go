// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/laravel"
	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/php"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/tuning"
)

// metaTuning is the state key holding the applied tuning changes.
const metaTuning = "tuning:applied"

// appliedChange is a tuning change a person applied: what it set, and what it replaced
// (a revert sets that again).
type appliedChange struct {
	At        time.Time         `json:"at"`
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Target    string            `json:"target,omitempty"`
	Component string            `json:"component"`
	Setting   string            `json:"setting"`
	Old       map[string]string `json:"old"`
	New       map[string]string `json:"new"`
	Restart   bool              `json:"restart,omitempty"`
	Reverted  time.Time         `json:"reverted,omitzero"`
}

type tuningData struct {
	Problem          string
	Recs             []tuning.Recommendation
	Applied          []appliedRow
	NeedMySQLPass    bool
	NeedRedisPass    bool
	PGRestart        bool
	PGRestartCommand string
	MemTotal         float64
}

type appliedRow struct {
	appliedChange
	Index         int
	Before, After string // the comparison, in words
	Waiting       bool   // less than an hour since: too early to compare
}

func (s *Server) loadApplied() []appliedChange {
	var out []appliedChange
	if v, err := s.Svc.State.GetMeta(metaTuning); err == nil && v != "" {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

func (s *Server) saveApplied(a []appliedChange) error {
	if len(a) > 100 {
		a = a[len(a)-100:]
	}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.Svc.State.SetMeta(metaTuning, string(data))
}

// ssd reports whether the server's disks are all solid-state (Linux's rotational flag).
func ssd() bool {
	files, _ := filepath.Glob("/sys/block/*/queue/rotational")
	found := false
	for _, f := range files {
		if strings.Contains(f, "/loop") || strings.Contains(f, "/ram") || strings.Contains(f, "/zram") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil || strings.TrimSpace(string(data)) != "0" {
			return false
		}
		found = true
	}
	return found
}

// tuningFacts gathers what the recommendations are made from.
func (s *Server) tuningFacts(ctx context.Context) (tuning.Facts, error) {
	var f tuning.Facts
	latest, err := s.Monitor.Latest(ctx)
	if err != nil {
		return f, err
	}
	v := latest.Values
	f.MemTotal, f.MemAvailable, f.SSD = v["mem.total"], v["mem.available"], ssd()
	cfg := s.Svc.Config()
	runner := s.Svc.Runner()
	if runner != nil && cfg.IsMySQL() {
		inst := runner.XtraBackup.Instance
		rows, err := inst.Query(ctx, "", `SELECT @@innodb_buffer_pool_size, @@max_connections, LEAST(@@tmp_table_size, @@max_heap_table_size),
(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Max_used_connections'),
(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Created_tmp_disk_tables'),
(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Created_tmp_tables'),
(SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.TABLES WHERE table_schema NOT IN ('mysql', 'sys', 'performance_schema', 'information_schema'))`)
		if err == nil && len(rows) == 1 && len(rows[0]) == 7 {
			n := func(i int) float64 { x, _ := strconv.ParseFloat(rows[0][i], 64); return x }
			m := &tuning.MySQL{BufferPool: n(0), MaxConnections: n(1), TmpTableSize: n(2), MaxUsed: n(3), DataBytes: n(6), CacheHit: 100}
			if n(5) > 0 {
				m.TmpDiskShare = 100 * n(4) / n(5)
			}
			if st := s.seriesStats(ctx, 0, "db.cache_hit:mysql")["db.cache_hit:mysql"]; st.ok {
				m.CacheHit = st.Avg
			}
			f.MySQL = m
		}
	}
	if runner != nil && !cfg.IsMySQL() {
		rows, err := runner.Cluster.Query(ctx, "postgres", `SELECT pg_size_bytes(current_setting('shared_buffers')), pg_size_bytes(current_setting('effective_cache_size')),
pg_size_bytes(current_setting('work_mem')), current_setting('random_page_cost'), (SELECT sum(pg_database_size(oid)) FROM pg_database)`)
		if err == nil && len(rows) == 1 && len(rows[0]) == 5 {
			n := func(i int) float64 { x, _ := strconv.ParseFloat(rows[0][i], 64); return x }
			p := &tuning.Postgres{SharedBuffers: n(0), EffectiveCache: n(1), WorkMem: n(2), RandomPageCost: n(3), DataBytes: n(4), CacheHit: 100}
			st := s.seriesStats(ctx, 0, "db.cache_hit:postgres", "db.temp_bytes:postgres")
			if c := st["db.cache_hit:postgres"]; c.ok {
				p.CacheHit = c.Avg
			}
			p.TempBytesPerHour = st["db.temp_bytes:postgres"].Avg * 3600
			f.Postgres = p
		}
	}
	if pools, err := php.FindPools(s.PHPEtc); err == nil {
		versions := map[string]bool{}
		for _, p := range pools {
			l := p.Label()
			st := s.seriesStats(ctx, 0.5, "fpm.saturated:"+l, "fpm.max_children_reached:"+l)
			pool := tuning.Pool{Version: p.Version, Name: p.Name, PM: p.PM, MaxChildren: p.MaxChildren, MaxRequests: p.MaxRequests,
				BusyPct: 100 * st["fpm.saturated:"+l].Avg, Reached: st["fpm.max_children_reached:"+l].Sum}
			for _, pr := range latest.Processes {
				if pr.Command == "php-fpm: pool "+p.Name && pr.Count > 0 {
					pool.Workers, pool.WorkerMemory = pr.Count, float64(pr.Memory)/float64(pr.Count)
				}
			}
			f.Pools = append(f.Pools, pool)
			versions[p.Version] = true
		}
		for ver := range versions {
			o := tuning.OPcache{Version: ver, MemoryMB: v["opcache.memory_mb:"+ver], MemoryPct: v["opcache.memory_pct:"+ver], KeysPct: v["opcache.keys_pct:"+ver],
				MaxFiles: v["opcache.max_files:"+ver], StringsMB: v["opcache.strings_mb:"+ver], StringsPct: v["opcache.strings_pct:"+ver], Restarts: v["opcache.restarts:"+ver]}
			if v["opcache.full:"+ver] == 1 {
				o.MemoryPct = max(o.MemoryPct, 95)
			}
			f.OPcache = append(f.OPcache, o)
		}
		sort.Slice(f.OPcache, func(i, j int) bool { return f.OPcache[i].Version < f.OPcache[j].Version })
	}
	if facts := s.redisFacts(ctx); facts.Status != nil && facts.Status.Error == "" {
		r := &tuning.Redis{MaxMemory: facts.MaxMemory, Used: facts.Memory, Evicted: facts.Evicted, Policy: facts.Status.Policy}
		for _, a := range laravel.FindApps(s.NginxDir, s.Caddyfile) {
			r.Sessions = r.Sessions || laravel.CheckApp(a).SessionsInRedis
		}
		f.Redis = r
	}
	return f, nil
}

// tuningPage is Monitoring → Tuning: the recommendations, each applied only when a person
// presses its button, and the changes applied, with before/after and a revert.
func (s *Server) tuningPage(w http.ResponseWriter, r *http.Request) {
	s.renderTuning(w, r, tuningData{})
}

func (s *Server) renderTuning(w http.ResponseWriter, r *http.Request, d tuningData) {
	render := func() { s.render(w, r, "tuning", page{Title: "Tuning", Nav: "tuning", Data: d}) }
	if !s.Svc.Config().Monitor.Enabled {
		d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		render()
		return
	}
	ctx := r.Context()
	f, err := s.tuningFacts(ctx)
	if err != nil {
		d.Problem = "The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"
		if !errors.Is(err, monitor.ErrNotRunning) {
			d.Problem = "The monitoring service couldn't answer: " + err.Error()
		}
		render()
		return
	}
	d.MemTotal = f.MemTotal
	d.Recs = tuning.Recommend(f)
	applied := s.loadApplied()
	for i := len(applied) - 1; i >= 0; i-- {
		row := appliedRow{appliedChange: applied[i], Index: i}
		if row.Reverted.IsZero() {
			row.Before, row.After, row.Waiting = s.beforeAfter(ctx, applied[i])
		}
		d.Applied = append(d.Applied, row)
	}
	if runner := s.Svc.Runner(); runner != nil && !s.Svc.Config().IsMySQL() {
		rows, err := runner.Cluster.Query(ctx, "postgres", "SELECT count(*) FROM pg_settings WHERE pending_restart")
		d.PGRestart = err == nil && len(rows) == 1 && rows[0][0] != "0"
		d.PGRestartCommand = setup.RestartCommand(s.Svc.Config())
	}
	render()
}

// beforeAfter compares the day before a change with the time since (up to a day): the
// requests' 95th percentile and the number the change was meant to move.
func (s *Server) beforeAfter(ctx context.Context, a appliedChange) (before, after string, waiting bool) {
	since := time.Since(a.At)
	if since < time.Hour {
		return "", "", true
	}
	end := a.At.Add(min(since, 24*time.Hour))
	describe := func(from, to time.Time) string {
		var parts []string
		if res, err := s.Monitor.RequestsBetween(ctx, from, to, ""); err == nil {
			if sum := requestSummary(res); sum.Count > 0 {
				parts = append(parts, fmt.Sprintf("95%% of requests under %s, %.1f%% over 1 s", notify.MS(sum.P95), 100*sum.Slow/sum.Count))
			}
		}
		if name, label, scale := tuningMetric(a); name != "" {
			if v, ok := s.windowAvg(ctx, name, from, to); ok {
				parts = append(parts, fmt.Sprintf("%s %.1f", label, v*scale))
			}
		}
		if len(parts) == 0 {
			return "no data"
		}
		return strings.Join(parts, "; ")
	}
	return describe(a.At.Add(-24*time.Hour), a.At), describe(a.At, end), false
}

// tuningMetric is the series a change was meant to move.
func tuningMetric(a appliedChange) (name, label string, scale float64) {
	switch {
	case a.Setting == "innodb_buffer_pool_size":
		return "db.cache_hit:mysql", "reads from memory (%)", 1
	case a.Setting == "shared_buffers":
		return "db.cache_hit:postgres", "reads from memory (%)", 1
	case a.Kind == tuning.KindPool:
		return "fpm.saturated:" + a.Target, "all workers busy (% of readings)", 100
	case a.Kind == tuning.KindPHP:
		return "opcache.hit_rate:" + a.Target, "OPcache hits (%)", 1
	case a.Kind == tuning.KindRedis:
		return "redis.evicted", "keys evicted per reading", 1
	}
	return "", "", 0
}

// windowAvg averages a series between from and to (minute records, kept 7 days).
func (s *Server) windowAvg(ctx context.Context, name string, from, to time.Time) (float64, bool) {
	res, err := s.Monitor.Series(ctx, "7d", name)
	if err != nil {
		return 0, false
	}
	sum, n := 0.0, 0.0
	for i, x := range res.X {
		if v := res.Series[name].Avg[i]; v != nil && x >= from.Unix() && x < to.Unix() {
			sum, n = sum+*v, n+1
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / n, true
}

func (s *Server) tuningEnv(r *http.Request) setup.TuningEnv {
	env := setup.TuningEnv{Run: s.Cmd, MySQLConf: setup.MySQLTuningConf, PHPEtc: s.PHPEtc, RedisAddr: monitor.DefaultRedis, BackupDir: s.WebBackupDir,
		MySQLPassword: r.PostFormValue("mysql_root_password"), RedisPassword: r.PostFormValue("redis_password")}
	if s.MySQLTuningConf != "" {
		env.MySQLConf = s.MySQLTuningConf
	}
	if res, err := s.Monitor.Redis(r.Context()); err == nil && res.Status != nil {
		env.RedisAddr = res.Status.Addr
	}
	if runner := s.Svc.Runner(); runner != nil {
		if s.Svc.Config().IsMySQL() {
			inst := runner.XtraBackup.Instance
			env.MySQL = &inst
		} else {
			c := runner.Cluster
			env.Postgres = &c
		}
	}
	return env
}

// tuningFailed shows the page again with what went wrong (and a password field when
// that's what's missing).
func (s *Server) tuningFailed(w http.ResponseWriter, r *http.Request, err error) {
	d := tuningData{Problem: err.Error()}
	if errors.Is(err, setup.ErrNeedRootPassword) {
		d.NeedMySQLPass, d.Problem = true, "MySQL's root account needs its password to change settings. It's used once and never stored."
	}
	if errors.Is(err, setup.ErrRedisPassword) {
		d.NeedRedisPass, d.Problem = true, err.Error()
	}
	s.renderTuning(w, r, d)
}

// tuningApply applies one recommendation (its Apply button).
func (s *Server) tuningApply(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	f, err := s.tuningFacts(ctx)
	if err != nil {
		s.tuningFailed(w, r, err)
		return
	}
	id := r.PostFormValue("id")
	for _, rec := range tuning.Recommend(f) {
		if rec.ID != id {
			continue
		}
		values := map[string]string{rec.Setting: rec.Proposed}
		for k, v := range rec.Extra {
			values[k] = v
		}
		old, restart, err := setup.ApplyTuning(ctx, s.tuningEnv(r), rec.Kind, rec.Target, values)
		detail := fmt.Sprintf("%s %s: %s → %s", rec.Component, rec.Setting, rec.Current, rec.Proposed)
		s.Svc.State.Audit(state.AuditEntry{Time: time.Now(), User: "ui", Action: "tuning", Detail: detail, OK: err == nil})
		if err != nil {
			s.tuningFailed(w, r, err)
			return
		}
		applied := append(s.loadApplied(), appliedChange{At: time.Now(), ID: rec.ID, Kind: rec.Kind, Target: rec.Target, Component: rec.Component,
			Setting: rec.Setting, Old: old, New: values, Restart: restart})
		if err := s.saveApplied(applied); err != nil {
			s.logf("tuning: %v", err)
		}
		msg := "Applied: " + detail + "."
		if restart {
			msg += " It takes effect after PostgreSQL restarts: use the button below when it suits."
		}
		s.flash(w, r, "/monitoring/tuning", msg)
		return
	}
	s.tuningFailed(w, r, fmt.Errorf("that recommendation no longer applies (the numbers changed); here are the current ones"))
}

// tuningRevert sets an applied change's old values again (its Revert button).
func (s *Server) tuningRevert(w http.ResponseWriter, r *http.Request) {
	applied := s.loadApplied()
	i, err := strconv.Atoi(r.PostFormValue("index"))
	if err != nil || i < 0 || i >= len(applied) || !applied[i].Reverted.IsZero() {
		s.tuningFailed(w, r, fmt.Errorf("no such change to revert"))
		return
	}
	a := applied[i]
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	_, restart, err := setup.ApplyTuning(ctx, s.tuningEnv(r), a.Kind, a.Target, a.Old)
	detail := fmt.Sprintf("revert %s %s", a.Component, a.Setting)
	s.Svc.State.Audit(state.AuditEntry{Time: time.Now(), User: "ui", Action: "tuning", Detail: detail, OK: err == nil})
	if err != nil {
		s.tuningFailed(w, r, err)
		return
	}
	applied[i].Reverted = time.Now()
	s.saveApplied(applied)
	msg := "Reverted: " + a.Component + " " + a.Setting + " is back to what it was."
	if restart {
		msg += " It takes effect after PostgreSQL restarts."
	}
	s.flash(w, r, "/monitoring/tuning", msg)
}
