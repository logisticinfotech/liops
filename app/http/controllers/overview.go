// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/laravel"
	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/php"
	"github.com/logisticinfotech/liops/app/verdict"
)

type overviewData struct {
	Problem  string
	Findings []verdict.Finding
	Headline string
}

// overview is the Monitoring overview: the verdict over the last 24 hours.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	var d overviewData
	render := func() { s.render(w, r, "overview", page{Title: "Overview", Nav: "overview", Data: d}) }
	if !s.Svc.Config().Monitor.Enabled {
		d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		render()
		return
	}
	e, err := s.evidence(r.Context())
	if err != nil {
		d.Problem = "The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"
		if !errors.Is(err, monitor.ErrNotRunning) {
			d.Problem = "The monitoring service couldn't answer: " + err.Error()
		}
		render()
		return
	}
	d.Findings = verdict.Judge(e)
	switch {
	case len(d.Findings) == 0:
		d.Headline = "Nothing in the last 24 hours points to a problem."
	case d.Findings[0].Level == verdict.Problem:
		d.Headline = "Most likely: " + d.Findings[0].Says + "."
	default:
		d.Headline = "Nothing serious; a few things worth a look."
	}
	render()
}

// stats are a series' numbers over a window.
type stats struct {
	Avg, Max, Min, Sum, HighShare float64
	ok                            bool
}

// seriesStats reads series over the last 24 hours. Sum adds up per-reading counts
// (each point holds the average of 15-second readings); HighShare is the share of
// points over high.
func (s *Server) seriesStats(ctx context.Context, high float64, names ...string) map[string]stats {
	out := map[string]stats{}
	for len(names) > 0 {
		batch := names[:min(50, len(names))]
		names = names[len(batch):]
		res, err := s.Monitor.Series(ctx, "24h", batch...)
		if err != nil {
			continue
		}
		perPoint := float64(res.Step) / 15
		for _, n := range batch {
			st := stats{Min: -1}
			count, over := 0.0, 0.0
			for i, x := range res.Series[n].Avg {
				if x == nil {
					continue
				}
				count++
				st.Avg += *x
				st.Sum += *x * perPoint
				if *x > high {
					over++
				}
				if st.Min < 0 || *x < st.Min {
					st.Min = *x
				}
				if mx := res.Series[n].Max[i]; mx != nil && *mx > st.Max {
					st.Max = *mx
				}
			}
			if count > 0 {
				st.Avg /= count
				st.HighShare = 100 * over / count
				st.ok = true
			}
			out[n] = st
		}
	}
	return out
}

// evidence gathers what the verdict looks at, over the last 24 hours.
func (s *Server) evidence(ctx context.Context) (verdict.Evidence, error) {
	var e verdict.Evidence
	latest, err := s.Monitor.Latest(ctx)
	if err != nil {
		return e, err
	}
	v := latest.Values
	e.CPUs, e.MemTotal = v["cpus"], v["mem.total"]

	var names []string
	for n := range v {
		if strings.HasPrefix(n, "io.util:") {
			names = append(names, n)
		}
	}
	cpu := s.seriesStats(ctx, 80, append(names, "cpu.busy", "cpu.steal", "cpu.iowait", "mem.available", "swap.in", "oom.kills")...)
	e.CPUBusy, e.CPUHighShare = cpu["cpu.busy"].Avg, cpu["cpu.busy"].HighShare
	e.Steal, e.IOWait, e.SwapIn, e.OOMKills = cpu["cpu.steal"].Avg, cpu["cpu.iowait"].Avg, cpu["swap.in"].Avg, cpu["oom.kills"].Sum
	if m := cpu["mem.available"]; m.ok {
		e.MemAvailMin = m.Min
	} else {
		e.MemAvailMin = v["mem.available"]
	}
	for _, n := range names {
		if st := cpu[n]; st.Avg > e.DiskUtil {
			e.DiskUtil, e.DiskDevice = st.Avg, strings.TrimPrefix(n, "io.util:")
		}
	}

	// Filesystems, and how fast they fill.
	var used []string
	for n := range v {
		if strings.HasPrefix(n, "disk.used:") {
			used = append(used, n)
		}
	}
	grow := s.growth(ctx, used)
	for _, n := range used {
		mount := strings.TrimPrefix(n, "disk.used:")
		f := verdict.Filesystem{Mount: mount, Pct: v["disk.pct:"+mount]}
		if g := grow[n]; g > 0 {
			if days := (v["disk.size:"+mount] - v[n]) / g; days < 90 {
				f.FullInDays = max(1, int(days+0.5))
			}
		}
		e.Filesystems = append(e.Filesystems, f)
	}

	// PHP-FPM pools and OPcache.
	if pools, err := php.FindPools(s.PHPEtc); err == nil {
		versions := map[string]bool{}
		for _, p := range pools {
			l := p.Label()
			st := s.seriesStats(ctx, 0.5, "fpm.saturated:"+l, "fpm.max_children_reached:"+l)
			pool := verdict.Pool{Label: l, MaxChildren: p.MaxChildren, BusyPct: 100 * st["fpm.saturated:"+l].Avg, Reached: st["fpm.max_children_reached:"+l].Sum}
			for _, pr := range latest.Processes {
				if pr.Command == "php-fpm: pool "+p.Name && pr.Count > 0 {
					per := float64(pr.Memory) / float64(pr.Count)
					pool.Affordable = int(0.8 * (v["mem.available"] + float64(pr.Memory)) / per)
				}
			}
			e.Pools = append(e.Pools, pool)
			versions[p.Version] = true
		}
		for ver := range versions {
			vals := map[string]float64{}
			for k, x := range v {
				if name, ok := strings.CutSuffix(k, ":"+ver); ok && strings.HasPrefix(name, "opcache.") {
					vals[strings.TrimPrefix(name, "opcache.")] = x
				}
			}
			if f := opcacheFindings(vals); len(f) > 0 {
				e.OPcache = append(e.OPcache, verdict.OPcache{Version: ver, Problems: f})
			}
		}
	}

	// Requests.
	req, reqErr := s.Monitor.Requests(ctx, "24h", "")
	if reqErr == nil {
		e.Requests = requestSummary(req)
	}

	// Databases: the scans, N+1, locks and cache.
	if dbs, err := s.Monitor.DBs(ctx); err == nil {
		for _, sv := range dbs {
			if sv.Error != "" {
				continue
			}
			d := verdict.Database{Title: sv.Title}
			top, _ := s.Monitor.Queries(ctx, "24h", sv.Label, "", 500)
			// N+1 candidates: the most-called shapes (an N+1 route can be a small share of
			// the traffic, so its query's average over all requests can be low).
			candidates := map[string]bool{}
			byCalls := append([]monitor.QueryTotal(nil), top...)
			sort.Slice(byCalls, func(i, j int) bool { return byCalls[i].Calls > byCalls[j].Calls })
			for _, q := range byCalls[:min(20, len(byCalls))] {
				if q.Calls >= 1000 && q.Calls >= e.Requests.Count {
					candidates[q.ID+q.DB] = true
				}
			}
			for _, q := range top {
				d.TotalMS += q.TimeMS
				eff := monitor.QueryEfficiency(sv.Engine, q.QueryCounters)
				if eff.Scans {
					d.ScanMS += q.TimeMS
					d.Scans = append(d.Scans, verdict.Query{DB: q.DB, Text: q.Text, MS: q.TimeMS, Read: readText(sv.Engine, eff)})
				}
				if e.Requests.Count >= 1000 && q.Calls >= 1000 {
					// A query called many times per request: find the route whose requests
					// move with it, for the real per-request count.
					per := q.Calls / e.Requests.Count
					n := verdict.Query{DB: q.DB, Text: q.Text, MS: q.TimeMS, PerRequest: per}
					if candidates[q.ID+q.DB] {
						if routes, err := s.Monitor.QueryRoutes(ctx, sv.Label, q.DB, q.ID); err == nil && len(routes) > 0 && routes[0].PerRequest >= 20 {
							n.PerRequest, n.Route = routes[0].PerRequest, routes[0].Host+" "+routes[0].Route
						}
					}
					if n.PerRequest >= 20 {
						e.NPlusOne = append(e.NPlusOne, n)
					}
				}
			}
			l := sv.Label
			st := s.seriesStats(ctx, 0, "db.slow:"+l, "db.lock_waits:"+l, "db.deadlocks:"+l, "db.cache_hit:"+l)
			d.SlowMax, d.LockWaitMax, d.Deadlocks = st["db.slow:"+l].Max, st["db.lock_waits:"+l].Max, st["db.deadlocks:"+l].Sum
			d.CacheHit, d.HasCacheHit = st["db.cache_hit:"+l].Avg, st["db.cache_hit:"+l].ok
			e.Databases = append(e.Databases, d)
		}
	}

	// Logs: /var/log from the monitor, the apps' from root.
	if logs, err := s.Monitor.LogGrowth(ctx); err == nil {
		for _, l := range logs {
			e.Logs = append(e.Logs, verdict.Log{Path: l.Path, PerHour: l.PerHour})
		}
	}
	for _, l := range s.Svc.AppLogGrowth() {
		e.Logs = append(e.Logs, verdict.Log{Path: l.Path, PerHour: l.PerHour})
	}

	// Apps.
	facts := s.redisFacts(ctx)
	for _, a := range laravel.FindApps(s.NginxDir, s.Caddyfile) {
		c := laravel.CheckApp(a)
		app := verdict.App{Dir: a.Dir}
		for _, f := range c.Findings {
			if f.Bad {
				app.Problems = append(app.Problems, f.Text)
			}
		}
		app.Problems = append(app.Problems, sessionFindings(c, facts)...)
		if c.QueueKey != "" {
			if n, ok := v["redis.queue:"+c.QueueKey]; ok {
				e.Queues = append(e.Queues, verdict.Queue{Key: c.QueueKey, Waiting: n})
			}
		}
		e.Apps = append(e.Apps, app)
	}

	// The latest deploy of the last week, before against after.
	if reqErr == nil {
		e.Deploy = s.deployEvidence(ctx)
	}
	return e, nil
}

// requestSummary adds up a Requests response for the verdict.
func requestSummary(res monitor.RequestsResponse) verdict.Requests {
	var total monitor.RouteStats
	for _, h := range res.Hosts {
		mergeStats(&total, h.RouteStats)
	}
	r := verdict.Requests{Count: total.Count, Slow: total.Slow(), P95: total.Percentile(95), TimeMS: total.TimeMS, AppMS: total.UpMS}
	routes := append([]monitor.RouteTotal(nil), res.Routes...)
	sort.Slice(routes, func(i, j int) bool { return routes[i].Slow() > routes[j].Slow() })
	for _, rt := range routes[:min(3, len(routes))] {
		if rt.Slow() > 0 {
			r.SlowRoutes = append(r.SlowRoutes, fmt.Sprintf("%s %s: 95%% under %s", rt.Host, rt.Route, notify.MS(rt.Percentile(95))))
		}
	}
	return r
}

func readText(engine string, e monitor.Efficiency) string {
	switch {
	case engine == monitor.EnginePostgres:
		return notify.Bytes(int64(e.BytesPerCall)) + " read per call"
	case e.Ratio > 0:
		return notify.Count(e.Ratio) + " rows read per row returned"
	}
	return notify.Count(e.PerCall) + " rows read per call, none returned"
}

// deployEvidence compares the day before the latest deploy (in the last week) with the
// time since, up to a day.
func (s *Server) deployEvidence(ctx context.Context) *verdict.Deploy {
	entries, err := s.Svc.State.AuditLog(0, 1000)
	if err != nil {
		return nil
	}
	var at time.Time
	var what string
	for _, en := range entries {
		if en.Action == "deploy" && en.Time.After(at) {
			at, what = en.Time, en.Detail
		}
	}
	if at.IsZero() || time.Since(at) > 7*24*time.Hour || time.Since(at) < 30*time.Minute {
		return nil
	}
	end := at.Add(24 * time.Hour)
	if now := time.Now(); now.Before(end) {
		end = now
	}
	before, err1 := s.Monitor.RequestsBetween(ctx, at.Add(-24*time.Hour), at, "")
	after, err2 := s.Monitor.RequestsBetween(ctx, at, end, "")
	if err1 != nil || err2 != nil {
		return nil
	}
	d := &verdict.Deploy{At: at, What: what, Before: requestSummary(before), After: requestSummary(after)}
	old := map[string]monitor.RouteTotal{}
	for _, rt := range before.Routes {
		old[rt.Host+" "+rt.Route] = rt
	}
	for _, rt := range after.Routes {
		b, ok := old[rt.Host+" "+rt.Route]
		if !ok || b.Count < 20 || rt.Count < 20 {
			continue
		}
		if p0, p1 := b.Percentile(95), rt.Percentile(95); p1 > 1.5*p0 && p1-p0 >= 100 && len(d.Routes) < 3 {
			d.Routes = append(d.Routes, fmt.Sprintf("%s %s: %s → %s", rt.Host, rt.Route, notify.MS(p0), notify.MS(p1)))
		}
	}
	if dbs, err := s.Monitor.DBs(ctx); err == nil {
		for _, sv := range dbs {
			top, _ := s.Monitor.Queries(ctx, "7d", sv.Label, "", 200)
			for _, q := range top {
				if q.FirstSeen.After(at) && len(d.NewQueries) < 3 {
					d.NewQueries = append(d.NewQueries, strings.Join(strings.Fields(q.Text), " "))
				}
			}
		}
	}
	return d
}

// VerdictLines is the verdict for the daily summary email: the headline and the main
// findings, in plain text. Nil when monitoring is off or the monitor doesn't answer.
func (s *Server) VerdictLines(ctx context.Context) []string { return s.verdictLines(ctx, 4) }

// AllVerdictLines is the whole verdict, every finding in full (liops verdict).
func (s *Server) AllVerdictLines(ctx context.Context) []string { return s.verdictLines(ctx, 0) }

func (s *Server) verdictLines(ctx context.Context, limit int) []string {
	if !s.Svc.Config().Monitor.Enabled {
		return nil
	}
	e, err := s.evidence(ctx)
	if err != nil {
		return nil
	}
	findings := verdict.Judge(e)
	if len(findings) == 0 {
		return []string{"Verdict: nothing in the last 24 hours points to a problem."}
	}
	head := "Verdict: nothing serious; a few things worth a look."
	if findings[0].Level == verdict.Problem {
		head = "Verdict: most likely, " + strings.ToLower(findings[0].Says[:1]) + findings[0].Says[1:] + "."
	}
	lines := []string{head}
	if limit > 0 {
		findings = findings[:min(limit, len(findings))]
	}
	for _, f := range findings {
		because := f.Because
		if limit > 0 && len(because) > 300 {
			because = because[:299] + "…"
		}
		lines = append(lines, "  - "+f.Area+": "+f.Says+". "+because)
		if limit == 0 && f.Do != "" {
			lines = append(lines, "    What to do: "+f.Do)
		}
	}
	return append(lines, "The full verdict, with what to do: Monitoring → Overview in the liops UI.", "")
}
