// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// RouteMatch is a route whose requests rise and fall with a query shape's calls: the
// route that runs it, and how many times per request.
type RouteMatch struct {
	Host        string  `json:"host"`
	Route       string  `json:"route"`
	Correlation float64 `json:"correlation"` // Pearson's r over the 5-minute periods
	PerRequest  float64 `json:"per_request"`
}

// QueryRoutes finds the routes that move with a query shape (calls per 5 minutes against
// requests per 5 minutes), best first.
func (s *Store) QueryRoutes(server, db, id string, from, to time.Time) ([]RouteMatch, error) {
	_, points, ok, err := s.QueryDetail(server, db, id, from, to)
	if err != nil || !ok || len(points) < 3 {
		return nil, err
	}
	calls := map[int64]float64{}
	for _, p := range points {
		calls[p.At] += p.Calls
	}
	perRoute := map[routeKey]map[int64]float64{}
	totals := map[routeKey]float64{}
	if err := s.scanRoutes(from, to, func(at int64, k routeKey, st RouteStats) {
		m := perRoute[k]
		if m == nil {
			m = map[int64]float64{}
			perRoute[k] = m
		}
		m[at/300*300] += st.Count
		totals[k] += st.Count
	}); err != nil {
		return nil, err
	}
	keys := make([]routeKey, 0, len(totals))
	for k, n := range totals {
		if n >= 50 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return totals[keys[i]] > totals[keys[j]] })
	keys = keys[:min(30, len(keys))]
	var out []RouteMatch
	for _, k := range keys {
		var xs, ys []float64
		sumCalls, sumReq := 0.0, 0.0
		for at, c := range calls {
			r := perRoute[k][at]
			xs, ys = append(xs, r), append(ys, c)
			if r > 0 {
				sumCalls, sumReq = sumCalls+c, sumReq+r
			}
		}
		if r := pearson(xs, ys); r >= 0.6 && sumReq > 0 {
			out = append(out, RouteMatch{Host: k.host, Route: k.route, Correlation: r, PerRequest: sumCalls / sumReq})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Correlation > out[j].Correlation })
	return out[:min(3, len(out))], nil
}

func pearson(x, y []float64) float64 {
	n := float64(len(x))
	if n < 3 {
		return 0
	}
	var sx, sy, sxx, syy, sxy float64
	for i := range x {
		sx, sy = sx+x[i], sy+y[i]
		sxx, syy, sxy = sxx+x[i]*x[i], syy+y[i]*y[i], sxy+x[i]*y[i]
	}
	den := math.Sqrt((n*sxx - sx*sx) * (n*syy - sy*sy))
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

func (m *Monitor) nplusoneHandler(mux *http.ServeMux, writeJSON func(http.ResponseWriter, any)) {
	mux.HandleFunc("GET /v1/db/query/routes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		now := m.Now()
		out, err := m.Store.QueryRoutes(q.Get("server"), q.Get("db"), q.Get("id"), now.Add(-24*time.Hour), now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
}

// QueryRoutes returns the routes that run a query shape, over the last day.
func (c *Client) QueryRoutes(ctx context.Context, server, db, id string) ([]RouteMatch, error) {
	var out []RouteMatch
	return out, c.get(ctx, "/v1/db/query/routes?"+url.Values{"server": {server}, "db": {db}, "id": {id}}.Encode(), &out)
}
