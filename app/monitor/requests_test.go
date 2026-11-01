// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoute(t *testing.T) {
	for in, want := range map[string]string{
		"/orders/12345?page=2": "/orders/{n}",
		"/":                    "/",
		"/api/v1/devices/550e8400-e29b-41d4-a716-446655440000/readings":                    "/api/v1/devices/{uuid}/readings",
		"/build/assets/app-4f3a2b9c.js":                                                    "/build/assets/{file}.js",
		"/reset-password/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08": "/reset-password/{hex}",
		"/users/someone@example.com":                                                       "/users/{email}",
		"/s/aB3dE5fG7hJ9kL1mN3pQ5r":                                                        "/s/{token}",
		"/t/acme/orders":                                                                   "/t/acme/orders",
		"/a/b/c/d/e/f/g/h":                                                                 "/a/b/c/d/e/f/…",
		"example.com:443":                                                                  "/{other}",
	} {
		if got := Route(in); got != want {
			t.Errorf("Route(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLogs(t *testing.T) {
	r, ok := parseNginx([]byte(`{"t":1793500000.250,"host":"Shop.Example.com:443","ip":"203.0.113.9","m":"GET","u":"/orders/7?x=1","s":200,"b":5120,"rt":0.482,"ut":"0.300, 0.150","ua":"Mozilla/5.0 (iPhone) Mobile"}`))
	if !ok || r.Status != 200 || math.Abs(r.MS-482) > 0.001 || math.Abs(r.UpstreamMS-450) > 0.001 || r.Path != "/orders/7?x=1" || r.At.Unix() != 1793500000 {
		t.Errorf("nginx: %+v %v", r, ok)
	}
	if r, _ := parseNginx([]byte(`{"t":1,"host":"a","ip":"x","m":"GET","u":"/x.css","s":304,"b":0,"rt":0.000,"ut":"-","ua":"-"}`)); r.UpstreamMS != -1 {
		t.Errorf("no upstream: %+v", r)
	}
	if _, ok := parseNginx([]byte(`203.0.113.9 - - [01/Nov/2026] "GET / HTTP/1.1" 200`)); ok {
		t.Error("the combined format isn't liops's")
	}
	r, ok = parseApache([]byte("1793500000.250\tshop.example.com\t203.0.113.9\tGET\t/orders/7?x=1\t200\t5120\t482000\tMozilla/5.0 (X11; Linux) \\x22quoted\\x22"))
	if !ok || r.Status != 200 || r.MS != 482 || r.Path != "/orders/7?x=1" || r.Host != "shop.example.com" || r.UpstreamMS != -1 || r.At.Unix() != 1793500000 {
		t.Errorf("apache: %+v %v", r, ok)
	}
	if _, ok := parseApache([]byte(`203.0.113.9 - - [01/Nov/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 12`)); ok {
		t.Error("the combined format isn't liops's")
	}
	r, ok = parseCaddy([]byte(`{"level":"info","ts":1793500000.5,"logger":"http.log.access.log0","msg":"handled request","request":{"remote_ip":"10.0.0.1","client_ip":"203.0.113.7","proto":"HTTP/2.0","method":"POST","host":"api.example.com","uri":"/v1/push","headers":{"User-Agent":["okhttp/4.12"]}},"bytes_read":12,"duration":1.25,"size":2,"status":201}`))
	if !ok || r.IP != "203.0.113.7" || r.MS != 1250 || r.Host != "api.example.com" || ClientClass(r.UA) != "script or app" {
		t.Errorf("caddy: %+v %v", r, ok)
	}
	if _, ok := parseCaddy([]byte(`{"level":"info","ts":1,"logger":"tls","msg":"certificate obtained"}`)); ok {
		t.Error("only access log lines count")
	}
}

func TestPercentile(t *testing.T) {
	var s RouteStats
	for i := 0; i < 90; i++ {
		s.add(Request{MS: 40, Status: 200, UpstreamMS: -1})
	}
	for i := 0; i < 10; i++ {
		s.add(Request{MS: 2500, Status: 504, UpstreamMS: 2400})
	}
	if p := s.Percentile(50); p < 25 || p > 50 {
		t.Errorf("p50 = %v", p)
	}
	if p := s.Percentile(95); p < 2000 || p > 2500 {
		t.Errorf("p95 = %v", p)
	}
	if p := s.Percentile(99.9); p != 2500 {
		t.Errorf("p99.9 = %v: never past the slowest request", p)
	}
	if s.Slow() != 10 || s.C5xx != 10 || s.UpCount != 10 {
		t.Errorf("stats = %+v", s)
	}
}

func TestRequestsAddUp(t *testing.T) {
	m, clock := newDBMonitor(t, &fakeMySQL{})
	*clock = time.Date(2026, 11, 2, 9, 59, 10, 0, time.UTC)
	m.AddRequest(Request{}) // starts the clock
	for i := 0; i < 120; i++ {
		ms, host := 30.0, "a.example.com"
		if i%10 == 0 {
			ms, host = 1800, "b.example.com" // the slow tenant
		}
		m.AddRequest(Request{Host: host, Method: "get", Path: fmt.Sprintf("/orders/%d", i), Status: 200, MS: ms, UpstreamMS: ms - 1, IP: fmt.Sprintf("203.0.113.%d", i%3), UA: "Mozilla/5.0"})
	}
	*clock = clock.Add(time.Minute) // 10:00:10: the minute, and the 9 o'clock hour, are over
	m.flushRequests(*clock)

	routes, hosts, err := m.Store.RequestTotals(clock.Add(-time.Hour), *clock, "")
	if err != nil || len(hosts) != 3 || len(routes) != 3 {
		t.Fatalf("routes %+v hosts %+v %v", routes, hosts, err)
	}
	if routes[0].Host != "b.example.com" || routes[0].Route != "GET /orders/{n}" || routes[0].Count != 12 || routes[0].Slow() != 12 {
		t.Errorf("the slow tenant should lead: %+v", routes[0])
	}
	if only, _, _ := m.Store.RequestTotals(clock.Add(-time.Hour), *clock, "a.example.com"); len(only) != 1 || only[0].Count != 108 {
		t.Errorf("one host: %+v", only)
	}
	slow, err := m.Store.SlowestRequests(clock.Add(-2*time.Hour), *clock, "", 5)
	if err != nil || len(slow) != 5 || slow[0].MS != 1800 || slow[0].Route != "GET /orders/{n}" {
		t.Errorf("slowest = %+v %v", slow, err)
	}
	clients, _ := m.Store.TopClients(clock.Add(-2*time.Hour), *clock, 10)
	if len(clients) != 4 || clients[0].Class != "browser" { // three IPs, and the empty first request
		t.Errorf("clients = %+v", clients)
	}
	heat, _ := m.Store.Heatmap(clock.Add(-7*24*time.Hour), *clock, "")
	if len(heat) != 1 || heat[0].Count != 121 || heat[0].Slow != 12 {
		t.Errorf("heatmap = %+v", heat)
	}
	m.collect()
	if s, _ := m.Store.Latest(); s.Values["req.per_second"] != 121.0/60 || s.Values["req.slow"] != 12 {
		t.Errorf("series = %v", s.Values)
	}
}

func TestFileTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	os.WriteFile(path, []byte("old line\n"), 0o644)
	tail := &fileTail{path: path}
	var got []string
	collect := func(l []byte) { got = append(got, string(l)) }
	tail.poll(collect)
	if len(got) != 0 {
		t.Errorf("what was there before is skipped: %q", got)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("one\ntw")
	tail.poll(collect)
	f.WriteString("o\n")
	f.Close()
	// Rotated: renamed, and a new file in its place.
	os.Rename(path, path+".1")
	f, _ = os.OpenFile(path+".1", os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("three\n") // written before nginx reopened its log
	f.Close()
	os.WriteFile(path, []byte("four\n"), 0o644)
	tail.poll(collect)
	// Truncated in place (copytruncate).
	os.WriteFile(path, []byte("5\n"), 0o644) // shorter than before: caught at the next poll
	tail.poll(collect)
	tail.close()
	if fmt.Sprint(got) != "[one two three four 5]" {
		t.Errorf("got %q", got)
	}
}

func TestRequestsAPI(t *testing.T) {
	m, _ := newDBMonitor(t, &fakeMySQL{})
	m.LogSources = []LogSource{{Format: FormatNginx, Path: NginxLogPath}}
	clock := time.Now().Add(-2 * time.Minute)
	m.Now = func() time.Time { return clock }
	m.AddRequest(Request{})
	m.AddRequest(Request{Host: "a.example.com", Method: "GET", Path: "/", Status: 200, MS: 12})
	clock = time.Now()
	m.flushRequests(clock)
	socket := shortSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, socket, m.Handler())
	c := NewClient(socket)
	var res RequestsResponse
	var err error
	for range 50 {
		if res, err = c.Requests(ctx, "1h", ""); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || len(res.Routes) != 2 || len(res.Sources) != 1 || len(res.Hosts) != 2 {
		t.Errorf("res = %+v, %v", res, err)
	}
	// The hour isn't over, but its slowest requests, clients and heatmap cell show.
	if len(res.Slowest) != 2 || len(res.Clients) != 1 || len(res.Heatmap) != 1 || res.Heatmap[0].Count != 2 {
		t.Errorf("current hour: slowest %+v clients %+v heat %+v", res.Slowest, res.Clients, res.Heatmap)
	}
}
