// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// RequestsResponse is the Requests page's data for a range.
type RequestsResponse struct {
	Sources []LogSource   `json:"sources"`
	Routes  []RouteTotal  `json:"routes"` // most total time first, at most 200
	Hosts   []RouteTotal  `json:"hosts"`
	Slowest []SlowRequest `json:"slowest"`
	Clients []ClientStats `json:"clients"`
	Heatmap []HeatCell    `json:"heatmap"` // the last 7 days, by hour
}

func (m *Monitor) reqHandlers(mux *http.ServeMux, writeJSON func(http.ResponseWriter, any)) {
	mux.HandleFunc("GET /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		span, ok := Ranges[r.URL.Query().Get("range")]
		if !ok {
			http.Error(w, "range must be 1h, 24h, 7d or 90d", http.StatusBadRequest)
			return
		}
		host := r.URL.Query().Get("host")
		now := m.Now()
		from := now.Add(-span)
		// An explicit window (Unix seconds) instead of "the last <range>": the verdict
		// compares before and after a deploy.
		if f, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64); err == nil {
			if t, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64); err == nil && t > f {
				from, now, span = time.Unix(f, 0), time.Unix(t, 0), time.Duration(t-f)*time.Second
			}
		}
		var res RequestsResponse
		var err error
		if res.Routes, res.Hosts, err = m.Store.RequestTotals(from, now, host); err == nil {
			res.Routes = res.Routes[:min(200, len(res.Routes))]
			slowFrom := from
			if span > MinuteKeep {
				slowFrom = now.Add(-MinuteKeep)
			}
			if res.Slowest, err = m.Store.SlowestRequests(slowFrom, now, host, 50); err == nil {
				if res.Clients, err = m.Store.TopClients(slowFrom, now, 20); err == nil {
					res.Heatmap, err = m.Store.Heatmap(now.Add(-7*24*time.Hour), now, host)
				}
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.addCurrentHour(&res, host)
		res.Sources = m.logSources()
		writeJSON(w, res)
	})
}

// logSources returns the access logs being read.
func (m *Monitor) logSources() []LogSource {
	if m.FindLogs != nil {
		return m.FindLogs()
	}
	return m.LogSources
}

// Requests returns the Requests page's data for a range and one host ("" for all).
func (c *Client) Requests(ctx context.Context, rng, host string) (RequestsResponse, error) {
	var out RequestsResponse
	return out, c.get(ctx, "/v1/requests?"+url.Values{"range": {rng}, "host": {host}}.Encode(), &out)
}

// addCurrentHour adds the hour in progress (kept in memory until it's over) to the
// slowest requests, the clients and the heatmap, so a new install shows them at once.
func (m *Monitor) addCurrentHour(res *RequestsResponse, host string) {
	a := m.reqs()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hour.IsZero() {
		return
	}
	for _, r := range a.slow {
		if host == "" || r.Host == host {
			res.Slowest = append(res.Slowest, r)
		}
	}
	sort.Slice(res.Slowest, func(i, j int) bool { return res.Slowest[i].MS > res.Slowest[j].MS })
	res.Slowest = res.Slowest[:min(50, len(res.Slowest))]

	by := map[string]int{}
	for i, c := range res.Clients {
		by[c.IP] = i
	}
	for _, c := range a.clients {
		if i, ok := by[c.IP]; ok {
			t := &res.Clients[i]
			t.Count, t.TimeMS, t.Slow, t.C5xx = t.Count+c.Count, t.TimeMS+c.TimeMS, t.Slow+c.Slow, t.C5xx+c.C5xx
		} else {
			res.Clients = append(res.Clients, *c)
		}
	}
	sort.Slice(res.Clients, func(i, j int) bool { return res.Clients[i].Count > res.Clients[j].Count })
	res.Clients = res.Clients[:min(20, len(res.Clients))]

	cell := HeatCell{At: a.hour.Unix()}
	for _, set := range []map[routeKey]*RouteStats{a.hourly, a.cur} {
		for k, s := range set {
			if host == "" || k.host == host {
				cell.Count += s.Count
				cell.Slow += s.Slow()
			}
		}
	}
	if cell.Count > 0 {
		res.Heatmap = append(res.Heatmap, cell)
	}
}

// RequestsBetween returns the requests between from and to (whole minutes, or hours
// beyond a day), for one host ("" for all).
func (c *Client) RequestsBetween(ctx context.Context, from, to time.Time, host string) (RequestsResponse, error) {
	q := url.Values{"range": {"90d"}, "host": {host}, "from": {strconv.FormatInt(from.Unix(), 10)}, "to": {strconv.FormatInt(to.Unix(), 10)}}
	var out RequestsResponse
	return out, c.get(ctx, "/v1/requests?"+q.Encode(), &out)
}
