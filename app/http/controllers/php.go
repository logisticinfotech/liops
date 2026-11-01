// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/php"
	"github.com/logisticinfotech/liops/app/setup"
)

type phpData struct {
	Problem   string
	Range     string
	Ranges    []string
	Pools     []poolRow
	OPcaches  []opcacheRow
	NeedSetup bool
}

type poolRow struct {
	php.Pool
	Measured                   bool // its status page is read
	Active, Idle, Total, Queue float64
	PerSecond                  float64
	Reached, Slow              float64 // in the range
	QueuePeak                  float64 // the most requests seen waiting for a worker in the range
	SaturatedPct               float64 // share of readings in the range with every worker busy
	MaxQueue                   float64 // the most that ever waited, since the pool started
	Workers                    int     // processes seen
	WorkerMemory               float64 // average resident memory
	Affordable                 int     // workers the free memory allows
	Findings                   []string
	Charts                     []chartBox
	SlowEntries                []string
}

type opcacheRow struct {
	Version  string
	Values   map[string]float64
	Findings []string
}

// phpPage is the PHP page: per pool, its workers and whether requests had to wait for
// one; per PHP version, OPcache; and the slow log.
func (s *Server) phpPage(w http.ResponseWriter, r *http.Request) {
	d := phpData{Range: r.URL.Query().Get("range"), Ranges: monitorRanges}
	span, ok := monitor.Ranges[d.Range]
	if !ok {
		d.Range, span = "24h", 24*time.Hour
	}
	render := func() { s.render(w, r, "php", page{Title: "PHP", Nav: "php", Data: d}) }
	if !s.Svc.Config().Monitor.Enabled {
		d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		render()
		return
	}
	pools, err := php.FindPools(s.PHPEtc)
	if err != nil || len(pools) == 0 {
		d.Problem = "No PHP-FPM pool was found in " + s.PHPEtc + "."
		if err != nil {
			d.Problem = "Couldn't read the PHP-FPM pools: " + err.Error()
		}
		render()
		return
	}
	ctx := r.Context()
	latest, err := s.Monitor.Latest(ctx)
	if err != nil {
		d.Problem = "The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"
		if !errors.Is(err, monitor.ErrNotRunning) {
			d.Problem = "The monitoring service couldn't answer: " + err.Error()
		}
		render()
		return
	}
	v := latest.Values
	markers := s.jobMarkers(time.Now().Add(-span))
	for _, p := range pools {
		row := poolRow{Pool: p}
		l := p.Label()
		_, row.Measured = v["fpm.total:"+l]
		row.Active, row.Idle, row.Total, row.Queue, row.PerSecond = v["fpm.active:"+l], v["fpm.idle:"+l], v["fpm.total:"+l], v["fpm.queue:"+l], v["fpm.per_second:"+l]
		row.MaxQueue = v["fpm.max_queue:"+l]
		if p.StatusPath == "" || !p.SlowLogOn() {
			d.NeedSetup = true
		}
		// Memory: PHP-FPM names its workers "php-fpm: pool <name>".
		for _, pr := range latest.Processes {
			if pr.Command == "php-fpm: pool "+p.Name && pr.Count > 0 {
				row.Workers, row.WorkerMemory = pr.Count, float64(pr.Memory)/float64(pr.Count)
			}
		}
		if row.WorkerMemory > 0 {
			budget := v["mem.available"] + row.WorkerMemory*float64(row.Workers)
			row.Affordable = int(0.8 * budget / row.WorkerMemory)
		}
		if row.Measured {
			res, err := s.Monitor.Series(ctx, d.Range, "fpm.active:"+l, "fpm.idle:"+l, "fpm.max_children:"+l, "fpm.queue:"+l, "fpm.max_children_reached:"+l, "fpm.slow:"+l, "fpm.saturated:"+l)
			if err == nil {
				// Each value is one 15-second reading's count; a minute or hour holds their average.
				perPoint := float64(res.Step) / 15
				for _, x := range res.Series["fpm.max_children_reached:"+l].Avg {
					if x != nil {
						row.Reached += *x * perPoint
					}
				}
				n, sum := 0.0, 0.0
				for _, x := range res.Series["fpm.saturated:"+l].Avg {
					if x != nil {
						n, sum = n+1, sum+*x
					}
				}
				if n > 0 {
					row.SaturatedPct = 100 * sum / n
				}
				for _, x := range res.Series["fpm.queue:"+l].Max {
					if x != nil {
						row.QueuePeak = max(row.QueuePeak, *x)
					}
				}
				for _, x := range res.Series["fpm.slow:"+l].Avg {
					if x != nil {
						row.Slow += *x * perPoint
					}
				}
				if len(res.X) >= 2 {
					spec := chartSpec{Unit: "number", X: res.X, Markers: markers}
					for _, c := range []struct{ name, label string }{{"fpm.active:", "busy"}, {"fpm.idle:", "idle"}, {"fpm.max_children:", "pm.max_children"}, {"fpm.queue:", "waiting for a worker"}} {
						spec.Series = append(spec.Series, chartSeries{Label: c.label, Values: res.Series[c.name+l].Avg})
					}
					if data, err := json.Marshal(spec); err == nil {
						row.Charts = append(row.Charts, chartBox{Title: "Workers: " + l, Spec: string(data)})
					}
				}
			}
		}
		row.Findings = poolFindings(row)
		if p.SlowLogOn() {
			row.SlowEntries = readSlowLog(p.SlowLog, 5)
		}
		d.Pools = append(d.Pools, row)
	}

	versions := map[string]bool{}
	for _, p := range pools {
		versions[p.Version] = true
	}
	for ver := range versions {
		o := opcacheRow{Version: ver, Values: map[string]float64{}}
		for k, x := range v {
			if name, ok := strings.CutSuffix(k, ":"+ver); ok && strings.HasPrefix(name, "opcache.") {
				o.Values[strings.TrimPrefix(name, "opcache.")] = x
			}
		}
		o.Findings = opcacheFindings(o.Values)
		d.OPcaches = append(d.OPcaches, o)
	}
	sort.Slice(d.OPcaches, func(i, j int) bool { return d.OPcaches[i].Version < d.OPcaches[j].Version })
	render()
}

// poolFindings says in plain words what's wrong with a pool.
func poolFindings(r poolRow) []string {
	var out []string
	if r.Reached > 0 {
		f := fmt.Sprintf("It ran out of workers %s times: requests waited for a free worker, even if the CPU was idle. Raise pm.max_children (now %d)", notify.Count(r.Reached), r.MaxChildren)
		if r.Affordable > r.MaxChildren {
			f += fmt.Sprintf("; the memory free allows about %d", r.Affordable)
		} else if r.Affordable > 0 {
			f += fmt.Sprintf(", but the memory free allows only about %d: find what keeps workers busy (slow queries, slow outside APIs) first", r.Affordable)
		}
		out = append(out, f+".")
	}
	if r.Queue > 0 {
		out = append(out, fmt.Sprintf("%.0f requests are waiting for a worker right now.", r.Queue))
	}
	// A static pool never counts "max children reached", and PHP-FPM measures its queue
	// only on TCP sockets: every worker being busy is the signal that always works.
	if r.Reached == 0 && r.SaturatedPct >= 10 {
		f := fmt.Sprintf("All %d workers were busy in %.0f%% of the readings in this range: requests that came then waited for one, even if the CPU was idle.", r.MaxChildren, r.SaturatedPct)
		if r.Affordable > r.MaxChildren {
			f += fmt.Sprintf(" Raise pm.max_children; the memory free allows about %d.", r.Affordable)
		}
		out = append(out, f)
	} else if r.Reached == 0 && (r.QueuePeak > 0 || r.MaxQueue > 0) {
		f := "Requests have had to wait for a free worker"
		if r.QueuePeak > 0 {
			f += fmt.Sprintf(" in this range (up to %.0f at once)", r.QueuePeak)
		} else {
			f += fmt.Sprintf(" since PHP-FPM started (up to %.0f at once)", r.MaxQueue)
		}
		f += fmt.Sprintf(": all %d workers were busy.", r.MaxChildren)
		if r.Affordable > r.MaxChildren {
			f += fmt.Sprintf(" Raise pm.max_children; the memory free allows about %d.", r.Affordable)
		}
		out = append(out, f)
	}
	if r.Slow > 0 {
		out = append(out, fmt.Sprintf("%s requests took over %s; their stack traces are in the slow log below.", notify.Count(r.Slow), r.SlowTimeout))
	}
	if r.Affordable > 0 && r.MaxChildren > r.Affordable*5/4 {
		out = append(out, fmt.Sprintf("pm.max_children is %d, but the memory free allows only about %d workers of this size: at full load the server would swap or kill processes.", r.MaxChildren, r.Affordable))
	}
	return out
}

// opcacheFindings says in plain words what's wrong with a PHP version's OPcache.
func opcacheFindings(v map[string]float64) []string {
	if len(v) == 0 {
		return nil
	}
	if v["enabled"] == 0 {
		return []string{"OPcache is off: PHP compiles every file on every request. Turn on opcache.enable in the FPM php.ini."}
	}
	var out []string
	if v["full"] == 1 || v["memory_pct"] >= 95 {
		out = append(out, fmt.Sprintf("OPcache is full: files that don't fit are compiled again on every request. Raise opcache.memory_consumption (now %s).", notify.Bytes(int64(v["memory"]))))
	}
	if v["keys_pct"] >= 90 {
		out = append(out, "OPcache is near its file limit: raise opcache.max_accelerated_files (to about twice the files the apps have).")
	}
	if v["strings_pct"] >= 90 {
		out = append(out, "OPcache's interned strings buffer is nearly full: raise opcache.interned_strings_buffer (16 or 32 MB is usual for Laravel).")
	}
	if v["restarts"] > 0 {
		out = append(out, fmt.Sprintf("OPcache restarted %.0f times because it ran out of room: each restart empties it, and every file is compiled again.", v["restarts"]))
	}
	if h, ok := v["hit_rate"]; ok && h < 95 {
		out = append(out, fmt.Sprintf("Only %.1f%% of file loads come from the cache (healthy is over 99%%).", h))
	}
	return out
}

// readSlowLog returns the last n entries of a PHP-FPM slow log (a stack trace each). It
// reads at most the file's last 256 KB; the entries are shown, never stored.
func readSlowLog(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 256<<10 {
		f.Seek(-256<<10, io.SeekEnd)
	}
	data, _ := io.ReadAll(f)
	var entries []string
	for _, e := range strings.Split(string(data), "\n\n") {
		if e = strings.TrimSpace(e); strings.HasPrefix(e, "[") {
			entries = append(entries, e)
		}
	}
	return entries[max(0, len(entries)-n):]
}

// phpPlan is the PHP-FPM part of Settings → Monitoring.
func (s *Server) phpPlan() (*setup.PHPPlan, string) {
	if _, err := os.Stat(s.PHPEtc); err != nil {
		return nil, ""
	}
	p, err := setup.PlanPHPFPM(s.PHPEtc)
	if err != nil {
		return nil, err.Error()
	}
	if len(p.Pools) == 0 {
		return nil, ""
	}
	return &p, ""
}

// monitorPHP applies the PHP-FPM changes (the Apply button).
func (s *Server) monitorPHP(w http.ResponseWriter, r *http.Request) {
	plan, problem := s.phpPlan()
	if plan == nil {
		s.fail(w, r, fmt.Errorf("no PHP-FPM pools found %s", problem))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	err := setup.ApplyPHPFPM(ctx, s.Cmd, *plan, s.WebBackupDir)
	s.audit(r, "monitoring", "PHP-FPM status pages and slow log", err == nil)
	if err != nil {
		cfg := s.Svc.Config()
		d := settingsData{Cfg: cfg, Tabs: settingsTabs, Tab: "monitoring", DBProblem: err.Error()}
		s.databaseAccess(r.Context(), cfg, &d)
		s.render(w, r, "settings", page{Title: "Settings", Nav: "settings", Data: d})
		return
	}
	s.flash(w, r, "/settings?tab=monitoring", "Done. PHP-FPM was reloaded; its numbers show in Monitoring → PHP within a minute.")
}
