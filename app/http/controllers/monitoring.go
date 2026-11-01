// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
)

// monitorRanges are the Resources page's time ranges, in order.
var monitorRanges = []string{"1h", "24h", "7d", "90d"}

type resourcesData struct {
	Enabled bool
	Problem string // why there's nothing to show
	Detail  string // the error behind Problem
	Range   string
	Ranges  []string
	Updated time.Time

	CPUBusy, Load, CPUs, MemUsed, MemTotal, SwapUsed float64
	FullestDisk                                      string
	FullestPct                                       float64

	Checks      monitor.Checks
	Filesystems []fsRow
	DiskUses    []diskUseRow
	NoDBSizes   bool // no database sizes from liops yet

	Charts    []chartBox
	Services  []serviceRow
	Processes []monitor.Process
}

// fsRow is one filesystem: how full it is and how fast it fills.
type fsRow struct {
	Mount           string
	Used, Size, Pct float64
	Growth          float64 // bytes per day
	HasGrowth       bool
	FullInDays      int // 0: not filling, or not within 90 days
}

// diskUseRow is one thing that takes disk space: a database, the WAL, logs.
type diskUseRow struct {
	What      string
	Size      float64
	Growth    float64 // bytes per day
	HasGrowth bool
}

type chartBox struct {
	Title string
	Spec  string // JSON for app.js
}

// chartMarker is a band on a chart where one of liops's own jobs ran.
type chartMarker struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Label string `json:"label"`
}

type serviceRow struct {
	Name   string
	CPU    float64 // percent of one CPU
	Memory float64
}

// chartGroup is one chart: series picked from the latest sample's names.
type chartGroup struct {
	title, unit string
	series      func(names []string) []string
	label       func(name string) string
}

// with returns the names that have one of the prefixes, in a stable order.
func with(prefixes ...string) func([]string) []string {
	return func(names []string) []string {
		var out []string
		for _, p := range prefixes {
			for _, n := range names {
				if strings.HasPrefix(n, p) {
					out = append(out, n)
				}
			}
		}
		return out
	}
}

// labelAfter turns "io.read:sda" into "read sda" and "cpu.iowait" into "iowait".
func labelAfter(name string) string {
	metric, label, found := strings.Cut(name, ":")
	if i := strings.LastIndexByte(metric, '.'); i >= 0 {
		metric = metric[i+1:]
	}
	if !found {
		return metric
	}
	if metric == "pct" {
		return label
	}
	return metric + " " + label
}

var resourceCharts = []chartGroup{
	{"CPU (% of all CPUs)", "percent", func([]string) []string { return []string{"cpu.user", "cpu.system", "cpu.iowait", "cpu.steal"} }, labelAfter},
	{"Load (against the number of CPUs)", "number", func([]string) []string { return []string{"load.1", "cpus"} }, func(n string) string {
		return map[string]string{"load.1": "load (1 min)", "cpus": "CPUs"}[n]
	}},
	{"Memory", "bytes", func([]string) []string { return []string{"mem.used", "mem.cache", "swap.used"} }, func(n string) string {
		return map[string]string{"mem.used": "used", "mem.cache": "cache", "swap.used": "swap used"}[n]
	}},
	{"Disk space (% used)", "percent", with("disk.pct:"), labelAfter},
	{"Disk I/O", "bytes/s", with("io.read:", "io.write:"), labelAfter},
	{"Network", "bytes/s", with("net.rx:", "net.tx:"), labelAfter},
	{"Databases and logs on disk", "bytes", with("db.size:", "disk.use:"), diskUseLabel},
}

// diskUseLabel names a db.size: or disk.use: series for people.
func diskUseLabel(name string) string {
	if db, ok := strings.CutPrefix(name, "db.size:"); ok {
		return db
	}
	switch what := strings.TrimPrefix(name, "disk.use:"); what {
	case "pg_wal":
		return "WAL (pg_wal)"
	case "binlogs":
		return "binlogs"
	default:
		return what
	}
}

// perDay is how fast a series grew over its range, per day. ok is false with less than
// six hours between the first and last values.
func perDay(x []int64, values []*float64) (float64, bool) {
	first, last := -1, -1
	for i, v := range values {
		if v != nil {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || x[last]-x[first] < 6*3600 {
		return 0, false
	}
	return (*values[last] - *values[first]) / float64(x[last]-x[first]) * 86400, true
}

// growth returns the per-day growth of each series over the last 24 hours, asking the
// monitor in batches of 50 (its limit).
func (s *Server) growth(ctx context.Context, names []string) map[string]float64 {
	out := map[string]float64{}
	for len(names) > 0 {
		batch := names[:min(50, len(names))]
		names = names[len(batch):]
		res, err := s.Monitor.Series(ctx, "24h", batch...)
		if err != nil {
			continue
		}
		for _, n := range batch {
			if g, ok := perDay(res.X, res.Series[n].Avg); ok {
				out[n] = g
			}
		}
	}
	return out
}

// jobMarkers are liops's own jobs in the range, joined when one starts right after
// another (a backup and its dumps), so the charts show when backups put load on the
// machine.
func (s *Server) jobMarkers(since time.Time) []chartMarker {
	runs, err := s.Svc.State.Runs("", 2000)
	if err != nil {
		return nil
	}
	var out []chartMarker
	for i := len(runs) - 1; i >= 0; i-- { // oldest first
		r := runs[i]
		end := r.End
		if end.IsZero() {
			end = time.Now()
		}
		if end.Before(since) {
			continue
		}
		if n := len(out); n > 0 && r.Start.Unix() <= out[n-1].End+600 {
			out[n-1].End = max(out[n-1].End, end.Unix())
			out[n-1].Label += " + " + r.Job
			continue
		}
		out = append(out, chartMarker{Start: r.Start.Unix(), End: end.Unix(), Label: r.Job})
	}
	// Deploys (service.WatchDeploys records them in the audit log): a thin marker each.
	if entries, err := s.Svc.State.AuditLog(0, 1000); err == nil {
		for _, e := range entries {
			if e.Action == "deploy" && e.Time.After(since) {
				out = append(out, chartMarker{Start: e.Time.Unix(), End: e.Time.Unix(), Label: "deploy"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// resources is the Resources page: the machine's CPU, memory, disk and network, now and
// over a range, with services and the busiest processes.
func (s *Server) resources(w http.ResponseWriter, r *http.Request) {
	d := resourcesData{Enabled: s.Svc.Config().Monitor.Enabled, Range: r.URL.Query().Get("range"), Ranges: monitorRanges}
	if _, ok := monitor.Ranges[d.Range]; !ok {
		d.Range = "24h"
	}
	render := func() { s.render(w, r, "resources", page{Title: "Resources", Nav: "resources", Data: d}) }
	ctx := r.Context()

	latest, err := s.Monitor.Latest(ctx)
	if err != nil {
		switch {
		case !d.Enabled:
			d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		case errors.Is(err, monitor.ErrNotRunning):
			d.Problem = "The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"
		default:
			d.Problem = "The monitoring service has no data yet; it samples every 15 seconds."
		}
		if d.Enabled {
			d.Detail = err.Error()
		}
		render()
		return
	}
	v := latest.Values
	d.Updated = latest.Time
	d.CPUBusy, d.Load, d.CPUs = v["cpu.busy"], v["load.1"], v["cpus"]
	d.MemUsed, d.MemTotal, d.SwapUsed = v["mem.used"], v["mem.total"], v["swap.used"]
	names := make([]string, 0, len(v))
	for n, val := range v {
		names = append(names, n)
		if mount, ok := strings.CutPrefix(n, "disk.pct:"); ok && val >= d.FullestPct {
			d.FullestDisk, d.FullestPct = mount, val
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if unit, ok := strings.CutPrefix(n, "svc.mem:"); ok {
			d.Services = append(d.Services, serviceRow{Name: strings.TrimSuffix(unit, ".service"), Memory: v[n], CPU: v["svc.cpu:"+unit]})
		}
	}
	sort.Slice(d.Services, func(i, j int) bool { return d.Services[i].Memory > d.Services[j].Memory })
	d.Processes = latest.Processes
	d.Checks = latest.Checks

	// Disk: each filesystem and what takes the space, with growth over the last day.
	var growing []string
	for _, n := range names {
		if strings.HasPrefix(n, "disk.used:") || strings.HasPrefix(n, "db.size:") || strings.HasPrefix(n, "disk.use:") {
			growing = append(growing, n)
		}
	}
	grow := s.growth(ctx, growing)
	for _, n := range names {
		if mount, ok := strings.CutPrefix(n, "disk.pct:"); ok {
			f := fsRow{Mount: mount, Pct: v[n], Used: v["disk.used:"+mount], Size: v["disk.size:"+mount]}
			f.Growth, f.HasGrowth = grow["disk.used:"+mount]
			if f.Growth > 0 {
				if days := (f.Size - f.Used) / f.Growth; days < 90 {
					f.FullInDays = max(1, int(math.Round(days)))
				}
			}
			d.Filesystems = append(d.Filesystems, f)
		}
		if strings.HasPrefix(n, "db.size:") || strings.HasPrefix(n, "disk.use:") {
			u := diskUseRow{What: diskUseLabel(n), Size: v[n]}
			if strings.HasPrefix(n, "db.size:") {
				u.What = "database " + u.What
			}
			u.Growth, u.HasGrowth = grow[n]
			d.DiskUses = append(d.DiskUses, u)
		}
	}
	sort.Slice(d.DiskUses, func(i, j int) bool { return d.DiskUses[i].Size > d.DiskUses[j].Size })
	hasDB := false
	for _, n := range names {
		hasDB = hasDB || strings.HasPrefix(n, "db.size:")
	}
	d.NoDBSizes = !hasDB
	markers := s.jobMarkers(time.Now().Add(-monitor.Ranges[d.Range]))

	for _, g := range resourceCharts {
		series := g.series(names)
		if len(series) == 0 {
			continue
		}
		if len(series) > 10 { // the largest ten: a chart of 60 databases can't be read
			sort.SliceStable(series, func(i, j int) bool { return v[series[i]] > v[series[j]] })
			series = series[:10]
		}
		res, err := s.Monitor.Series(ctx, d.Range, series...)
		if err != nil || len(res.X) < 2 {
			continue
		}
		spec := chartSpec{Unit: g.unit, X: res.X, Markers: markers}
		for _, n := range series {
			spec.Series = append(spec.Series, chartSeries{Label: g.label(n), Values: res.Series[n].Avg})
		}
		if data, err := json.Marshal(spec); err == nil {
			d.Charts = append(d.Charts, chartBox{Title: g.title, Spec: string(data)})
		}
	}
	render()
}

// formatPerDay prints a growth in bytes per day: "+12 MB", "−3.0 MB", "no change".
func formatPerDay(b float64) string {
	switch {
	case b >= 1024:
		return "+" + notify.Bytes(int64(b))
	case b <= -1024:
		return "−" + notify.Bytes(int64(-b))
	}
	return "no change"
}
