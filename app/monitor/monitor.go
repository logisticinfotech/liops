// SPDX-License-Identifier: Apache-2.0

// Package monitor is liops's per-server monitoring (docs/PLAN-monitor.md): the
// liops-monitor service collects the machine's numbers every 15 s, keeps their history,
// and answers the UI over a Unix socket. It runs as its own unprivileged user, apart from
// the root backup service and its credentials.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultSocket is where the liops-monitor service answers (its systemd RuntimeDirectory).
const DefaultSocket = "/run/liops-monitor/monitor.sock"

// Interval is how often the machine is sampled.
const Interval = 15 * time.Second

// Ranges are the time ranges the UI offers.
var Ranges = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "90d": 90 * 24 * time.Hour}

// Monitor collects samples into a Store and answers questions about them.
type Monitor struct {
	Collector *Collector
	Store     *Store
	Log       io.Writer
	Now       func() time.Time
	// Systemctl runs systemctl (for failed units); tests replace it.
	Systemctl func(ctx context.Context, args ...string) ([]byte, error)
	// DBs are the database servers to read; FindDBs, when set, finds them again every
	// QueryInterval (a database installed later is picked up).
	DBs     []DBServer
	FindDBs func() []DBServer

	mu        sync.Mutex
	started   time.Time
	processes []Process
	lastErr   string
	checks    Checks
	held      map[string]held // values from the slow checks and from Push
	dbOnce    sync.Once
	dbst      *dbState
	reqOnce   sync.Once
	reqst     *reqAgg
	// LogSources are the access logs to read (FindLogSources); FindLogs, when set, finds
	// them again every few minutes.
	LogSources []LogSource
	FindLogs   func() []LogSource
	// Redis is the local Redis, if any (FindRedis).
	Redis       *RedisServer
	redis       *redisState
	redisQueues []string

	logs      *logSizes
	logGrowth []LogGrowth
}

func New(collector *Collector, store *Store, log io.Writer) *Monitor {
	return &Monitor{Collector: collector, Store: store, Log: log, Now: time.Now, Systemctl: systemctl}
}

func (m *Monitor) logf(format string, args ...any) {
	if m.Log != nil {
		fmt.Fprintf(m.Log, "%s [monitor] %s\n", m.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	}
}

// Run samples every Interval until ctx is done, pruning old history every hour.
func (m *Monitor) Run(ctx context.Context) error {
	m.mu.Lock()
	m.started = m.Now()
	m.mu.Unlock()
	if err := m.collect(); err != nil {
		return err // no /proc: this isn't a machine liops can monitor
	}
	go m.slowLoop(ctx)
	go m.dbLoop(ctx)
	go m.reqLoop(ctx)
	go m.redisLoop(ctx)
	go m.logLoop(ctx)
	tick := time.NewTicker(Interval)
	defer tick.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := m.collect(); err != nil {
				m.logf("collect: %v", err)
			}
		case <-prune.C:
			if err := m.Store.Prune(m.Now()); err != nil {
				m.logf("prune: %v", err)
			}
		}
	}
}

func (m *Monitor) collect() error {
	sample, top, err := m.Collector.Collect(m.Now())
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.lastErr = err.Error()
		return err
	}
	m.lastErr = ""
	m.heldValues(sample.Values, sample.Time)
	if top != nil {
		m.processes = top
	}
	if err := m.Store.Add(sample); err != nil {
		m.lastErr = err.Error()
		return err
	}
	return nil
}

// API types, shared with the Client.

// Status is the monitor's own health.
type Status struct {
	Started time.Time `json:"started"`
	Latest  time.Time `json:"latest"` // zero before the first sample
	Error   string    `json:"error,omitempty"`
}

// Latest is the newest sample, the busiest processes and the checks.
type Latest struct {
	Time      time.Time          `json:"time"`
	Values    map[string]float64 `json:"values"`
	Processes []Process          `json:"processes"`
	Checks    Checks             `json:"checks"`
}

// PushRequest is what the root service sends: database sizes and the like.
type PushRequest struct {
	Values map[string]float64 `json:"values"`
	Keep   int                `json:"keep,omitempty"` // seconds the values count; PushKeep when 0
}

// SeriesResponse is the history of some series over a range.
type SeriesResponse struct {
	Step   int64                   `json:"step"` // seconds between points
	X      []int64                 `json:"x"`
	Series map[string]SeriesValues `json:"series"`
}

// SeriesValues is one series' average and maximum at each X.
type SeriesValues struct {
	Avg []*float64 `json:"avg"`
	Max []*float64 `json:"max"`
}

// Handler is the read API the UI uses over the Unix socket.
func (m *Monitor) Handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		st := Status{Started: m.started, Error: m.lastErr}
		m.mu.Unlock()
		if s, ok := m.Store.Latest(); ok {
			st.Latest = s.Time
		}
		writeJSON(w, st)
	})
	mux.HandleFunc("GET /v1/latest", func(w http.ResponseWriter, r *http.Request) {
		s, ok := m.Store.Latest()
		if !ok {
			http.Error(w, "no sample yet", http.StatusServiceUnavailable)
			return
		}
		m.mu.Lock()
		procs, checks := append([]Process(nil), m.processes...), m.checks
		m.mu.Unlock()
		writeJSON(w, Latest{Time: s.Time, Values: s.Values, Processes: procs, Checks: checks})
	})
	m.dbHandlers(mux, writeJSON)
	m.reqHandlers(mux, writeJSON)
	m.logHandlers(mux, writeJSON)
	m.nplusoneHandler(mux, writeJSON)
	m.redisQueueHandler(mux)
	mux.HandleFunc("GET /v1/redis", func(w http.ResponseWriter, r *http.Request) {
		var res RedisResponse
		m.mu.Lock()
		if m.redis != nil {
			st := m.redis.status
			res.Status = &st
		}
		m.mu.Unlock()
		if res.Status != nil && res.Status.Error == "" {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			res.Slowlog, _ = m.RedisSlowlog(ctx)
			cancel()
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("POST /v1/push", func(w http.ResponseWriter, r *http.Request) {
		var req PushRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := m.Push(req.Values, time.Duration(req.Keep)*time.Second); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/series", func(w http.ResponseWriter, r *http.Request) {
		span, ok := Ranges[r.URL.Query().Get("range")]
		if !ok {
			http.Error(w, "range must be 1h, 24h, 7d or 90d", http.StatusBadRequest)
			return
		}
		names := r.URL.Query()["name"]
		if len(names) == 0 || len(names) > 50 {
			http.Error(w, "give 1 to 50 series names", http.StatusBadRequest)
			return
		}
		now := m.Now()
		res, err := m.Store.Query(names, now.Add(-span), now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := SeriesResponse{Step: int64(res.Step / time.Second), X: res.X, Series: map[string]SeriesValues{}}
		for _, n := range names {
			out.Series[n] = SeriesValues{Avg: res.Values[n], Max: res.Max[n]}
		}
		writeJSON(w, out)
	})
	return mux
}

// Serve answers on a Unix socket until ctx is done. The socket is the owner's only
// (0600): the root UI can reach it, other users can't.
func Serve(ctx context.Context, socket string, h http.Handler) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Client asks the monitor over its Unix socket. The UI uses it.
type Client struct {
	Socket string
	http   *http.Client
}

func NewClient(socket string) *Client {
	return &Client{Socket: socket, http: &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}}
}

// ErrNotRunning: nothing answers on the socket.
var ErrNotRunning = errors.New("the monitor isn't running")

func (c *Client) get(ctx context.Context, path string, v any) error {
	return c.do(ctx, "GET", path, nil, v)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://monitor"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%s): %v", ErrNotRunning, c.Socket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("monitor: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, c.get(ctx, "/v1/status", &s)
}

func (c *Client) Latest(ctx context.Context) (Latest, error) {
	var l Latest
	return l, c.get(ctx, "/v1/latest", &l)
}

func (c *Client) Series(ctx context.Context, rng string, names ...string) (SeriesResponse, error) {
	q := "range=" + rng
	for _, n := range names {
		q += "&name=" + url.QueryEscape(n)
	}
	var s SeriesResponse
	return s, c.get(ctx, "/v1/series?"+q, &s)
}

// Push sends values (database sizes) for the monitor to keep with its samples.
func (c *Client) Push(ctx context.Context, values map[string]float64) error {
	return c.PushFor(ctx, values, 0)
}

// PushFor sends values that count for keep (rounded to seconds; PushKeep when 0).
func (c *Client) PushFor(ctx context.Context, values map[string]float64, keep time.Duration) error {
	data, err := json.Marshal(PushRequest{Values: values, Keep: int(keep / time.Second)})
	if err != nil {
		return err
	}
	return c.do(ctx, "POST", "/v1/push", bytes.NewReader(data), nil)
}
