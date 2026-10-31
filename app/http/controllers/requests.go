// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
)

type requestsData struct {
	Problem string
	Range   string
	Ranges  []string
	Host    string
	Hosts   []routeRow
	Routes  []routeRow
	Total   routeRow
	Slowest []monitor.SlowRequest
	Clients []monitor.ClientStats
	Sources []monitor.LogSource
	Charts  []chartBox
	Heat    []heatRow
	Hours   []int
}

// routeRow is a route (or a host) with what the page shows.
type routeRow struct {
	monitor.RouteTotal
	P50, P95, P99, AvgMS, UpAvgMS, PerSecond, SlowShare float64
}

func newRouteRow(t monitor.RouteTotal, span time.Duration) routeRow {
	r := routeRow{RouteTotal: t, P50: t.Percentile(50), P95: t.Percentile(95), P99: t.Percentile(99)}
	if t.Count > 0 {
		r.AvgMS = t.TimeMS / t.Count
		r.SlowShare = 100 * t.Slow() / t.Count
	}
	if t.UpCount > 0 {
		r.UpAvgMS = t.UpMS / t.UpCount
	}
	r.PerSecond = t.Count / span.Seconds()
	return r
}

// heatRow is one day of the heatmap: 24 hours.
type heatRow struct {
	Day   string
	Cells [24]heatCell
}

type heatCell struct {
	Level int // 0 (none) to 5, by slow requests
	Title string
}

// requests is the Requests page: which sites and routes take the time, the slowest
// requests, the busiest clients, and when the slow requests happen.
func (s *Server) requests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := requestsData{Range: q.Get("range"), Ranges: monitorRanges, Host: q.Get("host")}
	span, ok := monitor.Ranges[d.Range]
	if !ok {
		d.Range, span = "24h", 24*time.Hour
	}
	render := func() { s.render(w, r, "requests", page{Title: "Requests", Nav: "requests", Data: d}) }
	if !s.Svc.Config().Monitor.Enabled {
		d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		render()
		return
	}
	ctx := r.Context()
	res, err := s.Monitor.Requests(ctx, d.Range, d.Host)
	if err != nil {
		d.Problem = "The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"
		if !errors.Is(err, monitor.ErrNotRunning) {
			d.Problem = "The monitoring service couldn't answer: " + err.Error()
		}
		render()
		return
	}
	d.Sources, d.Slowest, d.Clients = res.Sources, res.Slowest, res.Clients
	var total monitor.RouteTotal
	for _, h := range res.Hosts {
		d.Hosts = append(d.Hosts, newRouteRow(h, span))
		if d.Host == "" || h.Host == d.Host {
			mergeStats(&total.RouteStats, h.RouteStats)
		}
	}
	d.Total = newRouteRow(total, span)
	for _, rt := range res.Routes[:min(50, len(res.Routes))] {
		d.Routes = append(d.Routes, newRouteRow(rt, span))
	}

	// The whole server's minute series (they aren't per host).
	if d.Host == "" {
		markers := s.jobMarkers(time.Now().Add(-span))
		for _, c := range []struct {
			title, unit string
			names       []string
			labels      []string
		}{
			{"Requests per second", "number", []string{"req.per_second"}, []string{"requests per second"}},
			{"Response time", "ms", []string{"req.p50", "req.p95"}, []string{"median", "95th percentile"}},
			{"Errors and slow requests (per minute)", "number", []string{"req.c5xx", "req.slow"}, []string{"5xx errors", "over 1 s"}},
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
	}
	d.Heat, d.Hours = s.heatmap(res.Heatmap)
	render()
}

func mergeStats(dst *monitor.RouteStats, o monitor.RouteStats) {
	dst.Count += o.Count
	dst.C4xx += o.C4xx
	dst.C5xx += o.C5xx
	dst.TimeMS += o.TimeMS
	dst.UpMS += o.UpMS
	dst.UpCount += o.UpCount
	dst.KB += o.KB
	dst.MaxMS = max(dst.MaxMS, o.MaxMS)
	for i := range dst.Hist {
		dst.Hist[i] += o.Hist[i]
	}
}

// heatmap lays the last 7 days' hours out as days × hours in the server's timezone,
// shaded by slow requests.
func (s *Server) heatmap(cells []monitor.HeatCell) ([]heatRow, []int) {
	loc, err := s.Svc.Config().Location()
	if err != nil {
		loc = time.UTC
	}
	today := time.Now().In(loc)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	rows := make([]heatRow, 7)
	for i := range rows {
		rows[i].Day = today.AddDate(0, 0, i-6).Format("Mon 2 Jan")
	}
	most := 0.0
	for _, c := range cells {
		most = max(most, c.Slow)
	}
	for _, c := range cells {
		at := time.Unix(c.At, 0).In(loc)
		day := int(time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc).Sub(today).Hours()/24) + 6
		if day < 0 || day > 6 {
			continue
		}
		cell := &rows[day].Cells[at.Hour()]
		if c.Slow > 0 && most > 0 {
			cell.Level = 1 + int(4*c.Slow/most)
			cell.Level = min(cell.Level, 5)
		}
		cell.Title = fmt.Sprintf("%s %02d:00: %s requests, %s over 1 s", at.Format("Mon"), at.Hour(), notify.Count(c.Count), notify.Count(c.Slow))
	}
	hours := make([]int, 24)
	for i := range hours {
		hours[i] = i
	}
	return rows, hours
}
