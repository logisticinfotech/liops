// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
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

	Charts    []chartBox
	Services  []serviceRow
	Processes []monitor.Process
}

type chartBox struct {
	Title string
	Spec  string // JSON for app.js
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
}

// resources is the Resources page: the machine's CPU, memory, disk and network, now and
// over a range, with services and the busiest processes.
func (s *Server) resources(w http.ResponseWriter, r *http.Request) {
	d := resourcesData{Enabled: s.Svc.Config().Monitor.Enabled, Range: r.URL.Query().Get("range"), Ranges: monitorRanges}
	if _, ok := monitor.Ranges[d.Range]; !ok {
		d.Range = "24h"
	}
	render := func() { s.render(w, r, "resources", page{Title: "Resources", Nav: "resources", Data: d}) }

	latest, err := s.Monitor.Latest(r.Context())
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

	for _, g := range resourceCharts {
		series := g.series(names)
		if len(series) == 0 {
			continue
		}
		res, err := s.Monitor.Series(r.Context(), d.Range, series...)
		if err != nil || len(res.X) < 2 {
			continue
		}
		spec := chartSpec{Unit: g.unit, X: res.X}
		for _, n := range series {
			spec.Series = append(spec.Series, chartSeries{Label: g.label(n), Values: res.Series[n].Avg})
		}
		if data, err := json.Marshal(spec); err == nil {
			d.Charts = append(d.Charts, chartBox{Title: g.title, Spec: string(data)})
		}
	}
	render()
}
