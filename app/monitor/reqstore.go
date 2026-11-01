// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketRoutes     = []byte("routes")     // id → host \x00 route
	bucketReqMinute  = []byte("reqminute")  // unix minute → rows
	bucketReqHour    = []byte("reqhour")    // unix hour → rows
	bucketReqSlow    = []byte("reqslow")    // unix hour → the hour's slowest requests (JSON)
	bucketReqClients = []byte("reqclients") // unix hour → the hour's top clients (JSON)
)

// histBounds are the upper bounds, in ms, of the request time histogram's buckets; the
// last bucket has none.
var histBounds = [...]float64{10, 25, 50, 75, 100, 150, 200, 300, 500, 750, 1000, 1500, 2000, 3000, 5000, 10000, 30000}

const nHist = len(histBounds) + 1

// SlowMS is when a request counts as slow (the heatmap, the top clients).
const SlowMS = 1000

// Bounds on what one minute or hour keeps, so a scan of random URLs or Host headers can't
// fill the disk: beyond them, requests count under {other}.
const (
	maxRoutes  = 2000
	maxHosts   = 300
	maxClients = 50000
	keepSlow   = 20
	keepTop    = 20
)

// maxRouteIDs is the most distinct host+route keys kept in bbolt. Extra keys collapse
// onto otherRoute. A var so tests can lower it.
var maxRouteIDs = 4096

var otherRoute = routeKey{"{other}", "{other}"}

// RouteStats are a route's numbers over a period.
type RouteStats struct {
	Count   float64        `json:"count"`
	C4xx    float64        `json:"c4xx"`
	C5xx    float64        `json:"c5xx"`
	TimeMS  float64        `json:"time_ms"`
	UpMS    float64        `json:"up_ms"`    // total upstream (PHP, app) time
	UpCount float64        `json:"up_count"` // requests with an upstream time
	KB      float64        `json:"kb"`       // response bytes, in KB
	MaxMS   float64        `json:"max_ms"`   // the slowest request
	Hist    [nHist]float64 `json:"hist"`
}

func (s *RouteStats) add(r Request) {
	s.Count++
	switch {
	case r.Status >= 500:
		s.C5xx++
	case r.Status >= 400:
		s.C4xx++
	}
	s.TimeMS += r.MS
	if r.UpstreamMS >= 0 {
		s.UpMS += r.UpstreamMS
		s.UpCount++
	}
	s.KB += float64(r.Bytes) / 1024
	s.MaxMS = math.Max(s.MaxMS, r.MS)
	i := sort.SearchFloat64s(histBounds[:], r.MS)
	s.Hist[i]++
}

func (s *RouteStats) merge(o RouteStats) {
	s.Count += o.Count
	s.C4xx += o.C4xx
	s.C5xx += o.C5xx
	s.TimeMS += o.TimeMS
	s.UpMS += o.UpMS
	s.UpCount += o.UpCount
	s.KB += o.KB
	s.MaxMS = math.Max(s.MaxMS, o.MaxMS)
	for i := range s.Hist {
		s.Hist[i] += o.Hist[i]
	}
}

// Percentile estimates the p-th percentile time (0 < p < 100) from the histogram,
// interpolating inside the bucket it falls in.
func (s RouteStats) Percentile(p float64) float64 {
	if s.Count == 0 {
		return 0
	}
	want := s.Count * p / 100
	seen := 0.0
	for i, n := range s.Hist {
		if n == 0 {
			continue
		}
		if seen+n >= want {
			lo := 0.0
			if i > 0 {
				lo = histBounds[i-1]
			}
			hi := lo * 2
			if i < len(histBounds) {
				hi = histBounds[i]
			}
			v := lo + (hi-lo)*(want-seen)/n
			if s.MaxMS > 0 {
				v = math.Min(v, s.MaxMS) // never past the slowest request seen
			}
			return v
		}
		seen += n
	}
	return histBounds[len(histBounds)-1]
}

// Slow counts the requests that took SlowMS or more.
func (s RouteStats) Slow() float64 {
	n := 0.0
	for i, c := range s.Hist {
		if i > 0 && histBounds[i-1] >= SlowMS {
			n += c
		}
	}
	return n
}

// RouteTotal is one host's route with its numbers.
type RouteTotal struct {
	Host  string `json:"host"`
	Route string `json:"route"` // "GET /orders/{n}"; empty in per-host totals
	RouteStats
}

// SlowRequest is one slow request, kept by route: no raw URL (it can hold tokens or
// personal data).
type SlowRequest struct {
	At         time.Time `json:"at"`
	Host       string    `json:"host"`
	Route      string    `json:"route"`
	Status     int       `json:"status"`
	MS         float64   `json:"ms"`
	UpstreamMS float64   `json:"upstream_ms"`
}

// ClientStats is one client's requests over an hour or more.
type ClientStats struct {
	IP     string  `json:"ip"`
	Class  string  `json:"class"` // ClientClass of its user agent
	Count  float64 `json:"count"`
	TimeMS float64 `json:"time_ms"`
	Slow   float64 `json:"slow"`
	C5xx   float64 `json:"c5xx"`
}

type routeKey struct{ host, route string }

// reqAgg gathers requests until their minute (and hour) is over.
type reqAgg struct {
	mu      sync.Mutex
	minute  time.Time
	cur     map[routeKey]*RouteStats
	hosts   map[string]bool
	hour    time.Time
	hourly  map[routeKey]*RouteStats
	slow    []SlowRequest
	clients map[string]*ClientStats
	sources bool // some log is read: a quiet minute is a zero, not a gap
}

func newReqAgg() *reqAgg {
	return &reqAgg{cur: map[routeKey]*RouteStats{}, hosts: map[string]bool{}, hourly: map[routeKey]*RouteStats{}, clients: map[string]*ClientStats{}}
}

// AddRequest counts one request. Requests count in the minute they're read in.
func (m *Monitor) AddRequest(r Request) {
	a := m.reqs()
	now := m.Now()
	a.mu.Lock()
	m.flushRequestsLocked(now)
	host := normalHost(r.Host)
	if !a.hosts[host] && len(a.hosts) >= maxHosts {
		host = "{other}"
	}
	a.hosts[host] = true
	k := routeKey{host, strings.ToUpper(r.Method) + " " + Route(r.Path)}
	if _, ok := a.cur[k]; !ok && len(a.cur) >= maxRoutes {
		k.route = "{other}"
	}
	s := a.cur[k]
	if s == nil {
		s = &RouteStats{}
		a.cur[k] = s
	}
	s.add(r)

	if len(a.slow) < keepSlow || r.MS > a.slow[len(a.slow)-1].MS {
		a.slow = append(a.slow, SlowRequest{At: r.At, Host: k.host, Route: k.route, Status: r.Status, MS: r.MS, UpstreamMS: r.UpstreamMS})
		sort.Slice(a.slow, func(i, j int) bool { return a.slow[i].MS > a.slow[j].MS })
		if len(a.slow) > keepSlow {
			a.slow = a.slow[:keepSlow]
		}
	}
	ip := r.IP
	if _, ok := a.clients[ip]; !ok && len(a.clients) >= maxClients {
		ip = "{other}"
	}
	c := a.clients[ip]
	if c == nil {
		c = &ClientStats{IP: ip, Class: ClientClass(r.UA)}
		a.clients[ip] = c
	}
	c.Count++
	c.TimeMS += r.MS
	if r.MS >= SlowMS {
		c.Slow++
	}
	if r.Status >= 500 {
		c.C5xx++
	}
	a.mu.Unlock()
}

func (m *Monitor) reqs() *reqAgg {
	m.reqOnce.Do(func() { m.reqst = newReqAgg() })
	return m.reqst
}

// flushRequests writes the minute (and hour) that's over, if any.
func (m *Monitor) flushRequests(now time.Time) {
	a := m.reqs()
	a.mu.Lock()
	m.flushRequestsLocked(now)
	a.mu.Unlock()
}

func (m *Monitor) flushRequestsLocked(now time.Time) {
	a := m.reqst
	minute := now.Truncate(time.Minute)
	if a.minute.IsZero() {
		a.minute, a.hour = minute, now.Truncate(time.Hour)
		return
	}
	if minute.Equal(a.minute) {
		return
	}
	// The minute is over: store it, fold it into the hour, and hold the totals as series.
	var total RouteStats
	for k, s := range a.cur {
		total.merge(*s)
		h := a.hourly[k]
		if h == nil {
			h = &RouteStats{}
			a.hourly[k] = h
		}
		h.merge(*s)
	}
	if len(a.cur) > 0 {
		if err := m.Store.writeRoutes(bucketReqMinute, uint32(a.minute.Unix()/60), a.cur); err != nil {
			m.logf("requests: %v", err)
		}
	}
	if a.sources || total.Count > 0 {
		m.hold(map[string]float64{"req.per_second": total.Count / 60, "req.p50": total.Percentile(50), "req.p95": total.Percentile(95),
			"req.c5xx": total.C5xx, "req.c4xx": total.C4xx, "req.slow": total.Slow()}, 75*time.Second)
	}
	a.cur, a.hosts, a.minute = map[routeKey]*RouteStats{}, map[string]bool{}, minute

	if hour := now.Truncate(time.Hour); !hour.Equal(a.hour) {
		key := uint32(a.hour.Unix() / 3600)
		if len(a.hourly) > 0 {
			if err := m.Store.writeRoutes(bucketReqHour, key, a.hourly); err != nil {
				m.logf("requests: %v", err)
			}
		}
		clients := make([]ClientStats, 0, len(a.clients))
		for _, c := range a.clients {
			clients = append(clients, *c)
		}
		sort.Slice(clients, func(i, j int) bool { return clients[i].Count > clients[j].Count })
		clients = clients[:min(keepTop, len(clients))]
		for b, v := range map[string]any{string(bucketReqSlow): a.slow, string(bucketReqClients): clients} {
			if err := m.Store.putJSON([]byte(b), key, v); err != nil {
				m.logf("requests: %v", err)
			}
		}
		a.hourly, a.slow, a.clients, a.hour = map[routeKey]*RouteStats{}, nil, map[string]*ClientStats{}, hour
	}
}

// routeIDsLocked loads the route ids. The caller holds s.mu.
func (s *Store) routeIDsLocked(tx *bolt.Tx) {
	if s.routeIDs != nil {
		return
	}
	s.routeIDs, s.routeNames = map[routeKey]uint32{}, map[uint32]routeKey{}
	tx.Bucket(bucketRoutes).ForEach(func(k, v []byte) error {
		host, route, _ := strings.Cut(string(v), "\x00")
		id := binary.BigEndian.Uint32(k)
		s.routeIDs[routeKey{host, route}], s.routeNames[id] = id, routeKey{host, route}
		return nil
	})
}

// writeRoutes stores one minute's or hour's rows.
func (s *Store) writeRoutes(bucket []byte, key uint32, rows map[routeKey]*RouteStats) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		s.routeIDsLocked(tx)
		routes := tx.Bucket(bucketRoutes)
		var rec []byte
		for k, st := range rows {
			id, ok := s.routeIDs[k]
			if !ok && len(s.routeIDs) >= maxRouteIDs {
				k = otherRoute
				id, ok = s.routeIDs[k]
			}
			if !ok {
				next, err := routes.NextSequence()
				if err != nil {
					return err
				}
				id = uint32(next)
				if err := routes.Put(binary.BigEndian.AppendUint32(nil, id), []byte(k.host+"\x00"+k.route)); err != nil {
					return err
				}
				s.routeIDs[k], s.routeNames[id] = id, k
			}
			rec = binary.AppendUvarint(rec, uint64(id))
			for _, v := range []float64{st.Count, st.C4xx, st.C5xx, st.TimeMS, st.UpMS, st.UpCount, st.KB, st.MaxMS} {
				rec = binary.AppendUvarint(rec, uint64(math.Max(0, math.Round(v))))
			}
			for _, v := range st.Hist {
				rec = binary.AppendUvarint(rec, uint64(v))
			}
		}
		b := tx.Bucket(bucket)
		k := binary.BigEndian.AppendUint32(nil, key)
		return b.Put(k, append(append([]byte(nil), b.Get(k)...), rec...))
	})
}

func (s *Store) putJSON(bucket []byte, key uint32, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(binary.BigEndian.AppendUint32(nil, key), data)
	})
}

// scanRoutes calls fn for each stored row between from and to: minutes for up to a day,
// hours beyond.
func (s *Store) scanRoutes(from, to time.Time, fn func(at int64, k routeKey, st RouteStats)) error {
	bucket, unit := bucketReqMinute, int64(60)
	if to.Sub(from) > 24*time.Hour {
		bucket, unit = bucketReqHour, 3600
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.View(func(tx *bolt.Tx) error {
		s.routeIDsLocked(tx)
		c := tx.Bucket(bucket).Cursor()
		end := uint32(to.Unix() / unit)
		for k, v := c.Seek(binary.BigEndian.AppendUint32(nil, uint32(from.Unix()/unit))); k != nil && binary.BigEndian.Uint32(k) <= end; k, v = c.Next() {
			at := int64(binary.BigEndian.Uint32(k)) * unit
			for len(v) > 0 {
				var nums [9 + nHist]uint64
				for i := range nums {
					n, size := binary.Uvarint(v)
					if size <= 0 {
						return nil // a damaged record
					}
					nums[i], v = n, v[size:]
				}
				st := RouteStats{Count: float64(nums[1]), C4xx: float64(nums[2]), C5xx: float64(nums[3]), TimeMS: float64(nums[4]),
					UpMS: float64(nums[5]), UpCount: float64(nums[6]), KB: float64(nums[7]), MaxMS: float64(nums[8])}
				for i := range st.Hist {
					st.Hist[i] = float64(nums[9+i])
				}
				fn(at, s.routeNames[uint32(nums[0])], st)
			}
		}
		return nil
	})
}

// RequestTotals adds up the requests between from and to, for one host ("" for all):
// per route (most total time first) and per host.
func (s *Store) RequestTotals(from, to time.Time, host string) (routes, hosts []RouteTotal, err error) {
	byRoute, byHost := map[routeKey]*RouteTotal{}, map[string]*RouteTotal{}
	err = s.scanRoutes(from, to, func(_ int64, k routeKey, st RouteStats) {
		h := byHost[k.host]
		if h == nil {
			h = &RouteTotal{Host: k.host}
			byHost[k.host] = h
		}
		h.merge(st)
		if host != "" && k.host != host {
			return
		}
		r := byRoute[k]
		if r == nil {
			r = &RouteTotal{Host: k.host, Route: k.route}
			byRoute[k] = r
		}
		r.merge(st)
	})
	for _, r := range byRoute {
		routes = append(routes, *r)
	}
	for _, h := range byHost {
		hosts = append(hosts, *h)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].TimeMS > routes[j].TimeMS })
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].TimeMS > hosts[j].TimeMS })
	return routes, hosts, err
}

// HeatCell is one hour's requests and slow requests.
type HeatCell struct {
	At    int64   `json:"at"` // the hour, Unix seconds
	Count float64 `json:"count"`
	Slow  float64 `json:"slow"`
}

// Heatmap returns each hour's requests and slow requests since from, for one host ("" for all).
func (s *Store) Heatmap(from, to time.Time, host string) ([]HeatCell, error) {
	cells := map[int64]*HeatCell{}
	if to.Sub(from) <= 24*time.Hour {
		from = to.Add(-25 * time.Hour) // force the hourly records
	}
	err := s.scanRoutes(from, to, func(at int64, k routeKey, st RouteStats) {
		if host != "" && k.host != host {
			return
		}
		c := cells[at]
		if c == nil {
			c = &HeatCell{At: at}
			cells[at] = c
		}
		c.Count += st.Count
		c.Slow += st.Slow()
	})
	out := make([]HeatCell, 0, len(cells))
	for _, c := range cells {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, err
}

// readHours decodes the JSON kept per hour in bucket between from and to.
func readHours[T any](s *Store, bucket []byte, from, to time.Time) ([]T, error) {
	var out []T
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucket).Cursor()
		end := uint32(to.Unix() / 3600)
		for k, v := c.Seek(binary.BigEndian.AppendUint32(nil, uint32(from.Unix()/3600))); k != nil && binary.BigEndian.Uint32(k) <= end; k, v = c.Next() {
			var items []T
			if json.Unmarshal(v, &items) == nil {
				out = append(out, items...)
			}
		}
		return nil
	})
	return out, err
}

// SlowestRequests returns the slowest requests kept between from and to.
func (s *Store) SlowestRequests(from, to time.Time, host string, limit int) ([]SlowRequest, error) {
	all, err := readHours[SlowRequest](s, bucketReqSlow, from, to)
	out := all[:0]
	for _, r := range all {
		if host == "" || r.Host == host {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MS > out[j].MS })
	return out[:min(limit, len(out))], err
}

// TopClients adds up the hourly top clients between from and to.
func (s *Store) TopClients(from, to time.Time, limit int) ([]ClientStats, error) {
	all, err := readHours[ClientStats](s, bucketReqClients, from, to)
	by := map[string]*ClientStats{}
	for _, c := range all {
		t := by[c.IP]
		if t == nil {
			t = &ClientStats{IP: c.IP, Class: c.Class}
			by[c.IP] = t
		}
		t.Count += c.Count
		t.TimeMS += c.TimeMS
		t.Slow += c.Slow
		t.C5xx += c.C5xx
	}
	out := make([]ClientStats, 0, len(by))
	for _, c := range by {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out[:min(limit, len(out))], err
}

// pruneRequests deletes what's past its keep: minutes, slow requests and clients after
// 7 days (MinuteKeep), hours after 90 (HourKeep).
func pruneRequests(tx *bolt.Tx, now time.Time) error {
	for _, p := range []struct {
		bucket []byte
		cut    uint32
	}{
		{bucketReqMinute, uint32(now.Add(-MinuteKeep).Unix() / 60)},
		{bucketReqHour, uint32(now.Add(-HourKeep).Unix() / 3600)},
		{bucketReqSlow, uint32(now.Add(-MinuteKeep).Unix() / 3600)},
		{bucketReqClients, uint32(now.Add(-MinuteKeep).Unix() / 3600)},
	} {
		c := tx.Bucket(p.bucket).Cursor()
		for k, _ := c.First(); k != nil && binary.BigEndian.Uint32(k) < p.cut; k, _ = c.Next() {
			if err := c.Delete(); err != nil {
				return err
			}
		}
	}
	return pruneUnusedRouteIDs(tx)
}

func usedRouteIDs(tx *bolt.Tx) map[uint32]bool {
	used := map[uint32]bool{}
	for _, name := range [][]byte{bucketReqMinute, bucketReqHour} {
		c := tx.Bucket(name).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			for len(v) > 0 {
				id, size := binary.Uvarint(v)
				if size <= 0 {
					break
				}
				used[uint32(id)] = true
				v = v[size:]
				for range 8 + nHist {
					_, n := binary.Uvarint(v)
					if n <= 0 {
						v = nil
						break
					}
					v = v[n:]
				}
			}
		}
	}
	return used
}

func pruneUnusedRouteIDs(tx *bolt.Tx) error {
	used := usedRouteIDs(tx)
	routes := tx.Bucket(bucketRoutes)
	c := routes.Cursor()
	for k, _ := c.First(); k != nil; {
		id := binary.BigEndian.Uint32(k)
		next, _ := c.Next()
		if !used[id] {
			if err := routes.Delete(k); err != nil {
				return err
			}
		}
		k = next
	}
	return nil
}
