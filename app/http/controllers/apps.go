// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/laravel"
	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/setup"
)

type appsData struct {
	Problem string
	Apps    []appRow
}

type appRow struct {
	laravel.App
	Release   string
	Check     laravel.Check
	Sessions  []string // problems with sessions in Redis
	Jobs      []string // the queue tables' sizes
	Scheduler string   // how often schedule:run ran in the last hour
	Errors    []laravel.ErrorGroup
}

// redisFacts is what the session checks need to know about Redis.
type redisFacts struct {
	Status    *monitor.RedisStatus
	Memory    float64
	MaxMemory float64
	Evicted   float64 // keys evicted in the last 24 hours
}

func (s *Server) redisFacts(ctx context.Context) redisFacts {
	var f redisFacts
	if res, err := s.Monitor.Redis(ctx); err == nil && res.Status != nil && !res.Status.Absent {
		f.Status = res.Status
	}
	if latest, err := s.Monitor.Latest(ctx); err == nil {
		f.Memory, f.MaxMemory = latest.Values["redis.memory"], latest.Values["redis.maxmemory"]
	}
	if res, err := s.Monitor.Series(ctx, "24h", "redis.evicted"); err == nil {
		for _, x := range res.Series["redis.evicted"].Avg {
			if x != nil {
				f.Evicted += *x * float64(res.Step) / 15
			}
		}
	}
	return f
}

// redisFindings are the problems with Redis itself.
func redisFindings(f redisFacts) []string {
	if f.Status == nil || f.Status.Error != "" {
		return nil
	}
	var out []string
	if f.Status.Exposed != "" {
		out = append(out, "Redis listens on "+f.Status.Exposed+": other machines can reach it. Bind it to 127.0.0.1 (bind in redis.conf) unless something else needs it, and give it a password.")
	}
	if f.Evicted > 0 {
		keys := "keys"
		if f.Evicted < 1.5 {
			keys = "key"
		}
		out = append(out, fmt.Sprintf("Redis evicted %s %s in the last 24 hours (policy %s): it's full, and it throws data away to make room.", notify.Count(f.Evicted), keys, f.Status.Policy))
	}
	return out
}

// sessionFindings are the ways a Redis setup can silently log people out.
func sessionFindings(c laravel.Check, f redisFacts) []string {
	if !c.SessionsInRedis {
		return nil
	}
	var out []string
	if c.SessionsShareDB {
		out = append(out, "Sessions and the cache share Redis database "+c.SessionDB+": php artisan cache:clear empties it, which logs everyone out. Give the cache its own database (REDIS_CACHE_DB=1).")
	}
	if f.Status == nil || f.Status.Error != "" {
		return out
	}
	evicting := strings.HasPrefix(f.Status.Policy, "allkeys-") || strings.HasPrefix(f.Status.Policy, "volatile-")
	full := f.MaxMemory > 0 && f.Memory >= 0.9*f.MaxMemory
	if evicting && (f.Evicted > 0 || full) {
		out = append(out, fmt.Sprintf("Redis can evict session keys (maxmemory-policy %s) and is %s: people get logged out at random. Keep sessions in a Redis with noeviction, or give it more maxmemory.",
			f.Status.Policy, map[bool]string{true: "evicting keys already", false: "nearly full"}[f.Evicted > 0]))
	}
	if f.Status.LastSave.IsZero() && !f.Status.AOF {
		out = append(out, "Redis never saves to disk: a restart (or a reboot) ends every session. Turn on appendonly yes, or RDB snapshots (save), in redis.conf.")
	}
	return out
}

// apps is the Apps page: each Laravel app's production checks, sessions, queues,
// scheduler and errors.
func (s *Server) apps(w http.ResponseWriter, r *http.Request) {
	var d appsData
	render := func() { s.render(w, r, "apps", page{Title: "Apps", Nav: "apps", Data: d}) }
	if !s.Svc.Config().Monitor.Enabled {
		d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		render()
		return
	}
	apps := laravel.FindApps(s.NginxDir, s.Caddyfile)
	if len(apps) == 0 {
		d.Problem = "No Laravel app found: liops looks for sites whose root is an app's public/ folder (with artisan next to it) in nginx's and Caddy's configs."
		render()
		return
	}
	ctx := r.Context()
	facts := s.redisFacts(ctx)
	tables := map[string]monitor.DBTables{}
	if dbs, err := s.Monitor.DBs(ctx); err == nil {
		for _, db := range dbs {
			if t, err := s.Monitor.Tables(ctx, db.Label); err == nil {
				tables[db.Label] = t
			}
		}
	}
	cron := s.cronLines(ctx)
	for _, a := range apps {
		row := appRow{App: a, Release: laravel.Release(a), Check: laravel.CheckApp(a)}
		row.Sessions = sessionFindings(row.Check, facts)
		for _, t := range tables {
			for _, ti := range t.Tables {
				if ti.DB == row.Check.Database && (ti.Name == "jobs" || ti.Name == "failed_jobs") {
					row.Jobs = append(row.Jobs, fmt.Sprintf("%s: about %s rows", ti.Name, notify.Count(ti.Rows)))
				}
			}
		}
		runs := 0
		for _, l := range cron {
			if strings.Contains(l, "schedule:run") && (len(apps) == 1 || strings.Contains(l, a.Dir)) {
				runs++
			}
		}
		switch {
		case cron == nil:
		case runs == 0:
			row.Scheduler = "schedule:run didn't run from cron in the last hour. If the app has scheduled tasks, add * * * * * cd " + a.Dir + " && php artisan schedule:run to the web user's crontab."
		case runs < 55:
			row.Scheduler = fmt.Sprintf("schedule:run ran %d times in the last hour (it should run every minute: 60).", runs)
		default:
			row.Scheduler = fmt.Sprintf("schedule:run ran %d times in the last hour.", runs)
		}
		row.Errors = laravel.Errors(a, time.Now().Add(-24*time.Hour))
		if len(row.Errors) > 15 {
			row.Errors = row.Errors[:15]
		}
		d.Apps = append(d.Apps, row)
	}
	render()
}

// cronLines reads cron's journal for the last hour; nil when it can't be read.
func (s *Server) cronLines(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := s.Cmd.Run(ctx, command.Cmd{Name: "journalctl", Args: []string{"--identifier=CRON", "--since=-1h", "--output=cat", "--no-pager"}})
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}
	}
	return lines
}

type redisData struct {
	Problem  string
	Facts    redisFacts
	Findings []string
	Sessions []appSessions
	Slowlog  []monitor.SlowCommand
	Keys     map[string]float64
	Charts   []chartBox
	Range    string
	Ranges   []string
}

type appSessions struct {
	Dir      string
	Findings []string
}

// redisPage is the Redis page: memory against maxmemory, evictions, hit ratio,
// persistence, the slow log, and the session checks of the apps that use it.
func (s *Server) redisPage(w http.ResponseWriter, r *http.Request) {
	d := redisData{Range: r.URL.Query().Get("range"), Ranges: monitorRanges}
	span, ok := monitor.Ranges[d.Range]
	if !ok {
		d.Range, span = "24h", 24*time.Hour
	}
	render := func() { s.render(w, r, "redis", page{Title: "Redis", Nav: "redis", Data: d}) }
	if !s.Svc.Config().Monitor.Enabled {
		d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		render()
		return
	}
	ctx := r.Context()
	res, err := s.Monitor.Redis(ctx)
	switch {
	case err != nil:
		d.Problem = "The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"
		if !errors.Is(err, monitor.ErrNotRunning) {
			d.Problem = "The monitoring service couldn't answer: " + err.Error()
		}
	case res.Status == nil || res.Status.Absent:
		d.Problem = "No Redis found on this server."
	case res.Status.Error != "":
		d.Problem = res.Status.Error
	}
	if d.Problem != "" {
		render()
		return
	}
	d.Facts, d.Slowlog = s.redisFacts(ctx), res.Slowlog
	d.Findings = redisFindings(d.Facts)
	for _, a := range laravel.FindApps(s.NginxDir, s.Caddyfile) {
		if f := sessionFindings(laravel.CheckApp(a), d.Facts); len(f) > 0 {
			d.Sessions = append(d.Sessions, appSessions{a.Dir, f})
		}
	}
	if latest, err := s.Monitor.Latest(ctx); err == nil {
		d.Keys = map[string]float64{}
		for k, v := range latest.Values {
			if db, ok := strings.CutPrefix(k, "redis.keys:"); ok {
				d.Keys[db] = v
			}
		}
	}
	markers := s.jobMarkers(time.Now().Add(-span))
	for _, c := range []struct {
		title, unit string
		names       []string
		labels      []string
	}{
		{"Memory", "bytes", []string{"redis.memory", "redis.maxmemory"}, []string{"used", "maxmemory"}},
		{"Hits and evictions", "number", []string{"redis.hit_rate", "redis.evicted"}, []string{"hit ratio (%)", "keys evicted"}},
		{"Commands per second", "number", []string{"redis.ops"}, []string{"commands per second"}},
	} {
		res, err := s.Monitor.Series(ctx, d.Range, c.names...)
		if err != nil || len(res.X) < 2 {
			continue
		}
		spec := chartSpec{Unit: c.unit, X: res.X, Markers: markers}
		for i, n := range c.names {
			spec.Series = append(spec.Series, chartSeries{Label: c.labels[i], Values: res.Series[n].Avg})
		}
		if data, err := json.Marshal(spec); err == nil {
			d.Charts = append(d.Charts, chartBox{Title: c.title, Spec: string(data)})
		}
	}
	render()
}

// monitorRedis adds the monitor's Redis user (the Set up Redis access button).
func (s *Server) monitorRedis(w http.ResponseWriter, r *http.Request) {
	addr := monitor.DefaultRedis
	if res, err := s.Monitor.Redis(r.Context()); err == nil && res.Status != nil {
		addr = res.Status.Addr
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := setup.RedisMonitorAccess(ctx, s.Cmd, addr, r.PostFormValue("redis_password"))
	s.audit(r, "monitoring", "Redis access", err == nil)
	if err != nil {
		cfg := s.Svc.Config()
		d := settingsData{Cfg: cfg, Tabs: settingsTabs, Tab: "monitoring", DBProblem: err.Error(), NeedRedisPassword: errors.Is(err, setup.ErrRedisPassword)}
		s.databaseAccess(r.Context(), cfg, &d)
		s.render(w, r, "settings", page{Title: "Settings", Nav: "settings", Data: d})
		return
	}
	s.flash(w, r, "/settings?tab=monitoring", "Done. The monitor reads Redis's numbers within 15 seconds.")
}
