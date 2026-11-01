// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RedisUser is the monitor's Redis ACL user: allowed only INFO, SLOWLOG GET, PING, and
// LLEN/ZCARD on Laravel's queue keys, so it reads no values and no config (CONFIG GET
// would show requirepass).
const RedisUser = "liops-monitor"

// DefaultRedis is where Ubuntu's Redis listens.
const DefaultRedis = "127.0.0.1:6379"

// RedisServer is a Redis the monitor reads.
type RedisServer struct {
	Addr string // host:port, or a socket path
	// Dial connects; tests replace it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (r RedisServer) dial(ctx context.Context) (net.Conn, error) {
	network := "tcp"
	if strings.HasPrefix(r.Addr, "/") {
		network = "unix"
	}
	dial := r.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return dial(ctx, network, r.Addr)
}

// redisCommands logs in as RedisUser and runs each command, returning their replies.
func (r RedisServer) redisCommands(ctx context.Context, cmds ...[]string) ([]any, error) {
	conn, err := r.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	all := append([][]string{{"AUTH", RedisUser, "liops"}}, cmds...) // the user has nopass: any password works
	var b strings.Builder
	for _, c := range all {
		fmt.Fprintf(&b, "*%d\r\n", len(c))
		for _, a := range c {
			fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
		}
	}
	if _, err := io.WriteString(conn, b.String()); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	var out []any
	for i := range all {
		v, err := readRESP(br, 0)
		if err != nil {
			return nil, err
		}
		if e, ok := v.(error); ok {
			if i == 0 {
				return nil, fmt.Errorf("the monitor can't log in to Redis as %s (use Set up Redis access in Settings → Monitoring): %w", RedisUser, e)
			}
			return nil, e
		}
		if i > 0 {
			out = append(out, v)
		}
	}
	return out, nil
}

// readRESP reads one reply: a string, an int64, an error, nil, or a []any.
func readRESP(r *bufio.Reader, depth int) (any, error) {
	if depth > 4 {
		return nil, errors.New("redis: reply nested too deep")
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("redis: empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return errors.New(line[1:]), nil
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		if n > 8<<20 {
			return nil, errors.New("redis: reply too large")
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		if n > 10000 {
			return nil, errors.New("redis: reply too large")
		}
		items := make([]any, n)
		for i := range items {
			if items[i], err = readRESP(r, depth+1); err != nil {
				return nil, err
			}
		}
		return items, nil
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line)
}

// parseInfo reads INFO's "key:value" lines.
func parseInfo(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && !strings.HasPrefix(k, "#") {
			out[k] = v
		}
	}
	return out
}

var queueKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)

// SetRedisQueues sets the queue keys to count (root works them out from the apps'
// settings): names only, at most 50.
func (m *Monitor) SetRedisQueues(keys []string) error {
	if len(keys) > 50 {
		return errors.New("at most 50 queues")
	}
	for _, k := range keys {
		if !queueKeyRE.MatchString(k) || !strings.Contains(k, "queues:") {
			return fmt.Errorf("%q isn't a queue key", k)
		}
	}
	m.mu.Lock()
	m.redisQueues = append([]string(nil), keys...)
	m.mu.Unlock()
	return nil
}

// RedisStatus is what the monitor knows about Redis.
type RedisStatus struct {
	Addr    string    `json:"addr"`
	Absent  bool      `json:"absent,omitempty"` // nothing listens there: no Redis on this server
	Version string    `json:"version,omitempty"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked"`
	Policy  string    `json:"policy,omitempty"` // maxmemory-policy
	// Persistence: when it last saved to disk (zero: never), and whether AOF is on.
	LastSave time.Time `json:"last_save,omitzero"`
	AOF      bool      `json:"aof"`
	// Exposed: it listens on an address other machines can reach.
	Exposed string `json:"exposed,omitempty"`
}

// redisState is the monitor's memory of Redis between readings.
type redisState struct {
	status RedisStatus
	prev   map[string]float64
	prevAt time.Time
}

// readRedis reads INFO and holds the numbers as series (redis.*).
func (m *Monitor) readRedis(ctx context.Context) {
	if m.Redis == nil {
		return
	}
	now := m.Now()
	st := RedisStatus{Addr: m.Redis.Addr, Checked: now, Exposed: m.redisExposed()}
	replies, err := m.Redis.redisCommands(ctx, []string{"INFO"})
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.redis == nil {
		m.redis = &redisState{}
	}
	rs := m.redis
	if err != nil {
		var op *net.OpError
		st.Absent = errors.As(err, &op) && op.Op == "dial"
		st.Error = err.Error()
		rs.status = st
		return
	}
	text, _ := replies[0].(string)
	info := parseInfo(text)
	num := func(k string) float64 { f, _ := strconv.ParseFloat(info[k], 64); return f }
	st.Version, st.Policy, st.AOF = info["redis_version"], info["maxmemory_policy"], info["aof_enabled"] == "1"
	if t := num("rdb_last_save_time"); t > 0 && info["rdb_saves"] != "0" {
		st.LastSave = time.Unix(int64(t), 0)
	}
	rs.status = st
	values := map[string]float64{
		"redis.memory": num("used_memory"), "redis.maxmemory": num("maxmemory"), "redis.clients": num("connected_clients"),
		"redis.ops": num("instantaneous_ops_per_sec"), "redis.fragmentation": num("mem_fragmentation_ratio"),
	}
	keys := 0.0
	for k, v := range info {
		if strings.HasPrefix(k, "db") && strings.HasPrefix(v, "keys=") {
			n, _ := strconv.ParseFloat(strings.TrimPrefix(strings.SplitN(v, ",", 2)[0], "keys="), 64)
			values["redis.keys:"+k] = n
			keys += n
		}
	}
	values["redis.keys"] = keys
	// Laravel's queues: waiting (a list), delayed and reserved (sorted sets).
	if qs := m.redisQueues; len(qs) > 0 {
		var cmds [][]string
		for _, k := range qs {
			cmds = append(cmds, []string{"LLEN", k}, []string{"ZCARD", k + ":delayed"}, []string{"ZCARD", k + ":reserved"})
		}
		m.mu.Unlock()
		replies, err := m.Redis.redisCommands(ctx, cmds...)
		m.mu.Lock()
		if err == nil && len(replies) == 3*len(qs) {
			for i, k := range qs {
				for j, what := range []string{"redis.queue:", "redis.queue_delayed:", "redis.queue_reserved:"} {
					if n, ok := replies[3*i+j].(int64); ok {
						values[what+k] = float64(n)
					}
				}
			}
		} else if err != nil {
			rs.status.Error = "reading the queues: " + err.Error()
		}
	}
	counters := map[string]float64{"evicted": num("evicted_keys"), "hits": num("keyspace_hits"), "misses": num("keyspace_misses"), "expired": num("expired_keys")}
	if rs.prev != nil {
		d := func(k string) float64 { return max(0, counters[k]-rs.prev[k]) }
		values["redis.evicted"] = d("evicted")
		if h, mi := d("hits"), d("misses"); h+mi > 0 {
			values["redis.hit_rate"] = 100 * h / (h + mi)
		}
	}
	rs.prev, rs.prevAt = counters, now
	m.held = holdLocked(m.held, values, now.Add(3*Interval))
}

// holdLocked is hold for a caller that already holds m.mu.
func holdLocked(h map[string]held, values map[string]float64, until time.Time) map[string]held {
	if h == nil {
		h = map[string]held{}
	}
	for n, v := range values {
		h[n] = held{v, until}
	}
	return h
}

// redisExposed reports a non-loopback address Redis listens on (from /proc/net/tcp),
// or "".
func (m *Monitor) redisExposed() string {
	_, port, err := net.SplitHostPort(m.Redis.Addr)
	if err != nil {
		return ""
	}
	p, _ := strconv.Atoi(port)
	for _, f := range []string{"proc/net/tcp", "proc/net/tcp6"} {
		data, err := m.Collector.read(f)
		if err != nil {
			continue
		}
		if addr := listeningOn(data, p); addr != "" {
			return addr
		}
	}
	return ""
}

// listeningOn finds a listening socket on port bound to an address other than loopback.
func listeningOn(procNetTCP string, port int) string {
	for _, line := range strings.Split(procNetTCP, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" { // 0A: LISTEN
			continue
		}
		hostHex, portHex, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		if p, _ := strconv.ParseInt(portHex, 16, 32); int(p) != port {
			continue
		}
		switch hostHex {
		case "0100007F", "00000000000000000000000001000000": // 127.0.0.1, ::1
			continue
		case "00000000", "00000000000000000000000000000000":
			return fmt.Sprintf("all addresses, port %d", port)
		}
		return fmt.Sprintf("%s, port %d", hostHex, port)
	}
	return ""
}

// SlowCommand is one entry of Redis's slow log. Args can hold keys and values: read live
// for a page, never stored.
type SlowCommand struct {
	At   time.Time `json:"at"`
	MS   float64   `json:"ms"`
	Args []string  `json:"args"`
}

// RedisSlowlog reads Redis's slow log, live.
func (m *Monitor) RedisSlowlog(ctx context.Context) ([]SlowCommand, error) {
	if m.Redis == nil {
		return nil, errors.New("no Redis found")
	}
	replies, err := m.Redis.redisCommands(ctx, []string{"SLOWLOG", "GET", "20"})
	if err != nil {
		return nil, err
	}
	entries, _ := replies[0].([]any)
	var out []SlowCommand
	for _, e := range entries {
		f, ok := e.([]any)
		if !ok || len(f) < 4 {
			continue
		}
		at, _ := f[1].(int64)
		us, _ := f[2].(int64)
		var args []string
		if a, ok := f[3].([]any); ok {
			for _, x := range a {
				s, _ := x.(string)
				if len(s) > 80 {
					s = s[:77] + "…"
				}
				args = append(args, s)
			}
		}
		out = append(out, SlowCommand{At: time.Unix(at, 0), MS: float64(us) / 1000, Args: args})
	}
	return out, nil
}

// FindRedis returns the Redis to read at addr; until something listens there it's
// reported as absent, so one installed later is picked up.
func FindRedis(addr string) *RedisServer {
	if addr == "" {
		return nil
	}
	return &RedisServer{Addr: addr}
}

// redisLoop reads Redis every Interval.
func (m *Monitor) redisLoop(ctx context.Context) {
	if m.Redis == nil {
		return
	}
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		m.readRedis(rctx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RedisResponse is the Redis page's data: nil Status when there's no Redis.
type RedisResponse struct {
	Status  *RedisStatus  `json:"status"`
	Slowlog []SlowCommand `json:"slowlog,omitempty"`
}

// Redis returns what the monitor knows about Redis, and its slow log read live.
func (c *Client) Redis(ctx context.Context) (RedisResponse, error) {
	var out RedisResponse
	return out, c.get(ctx, "/v1/redis", &out)
}

func (m *Monitor) redisQueueHandler(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/redis/queues", func(w http.ResponseWriter, r *http.Request) {
		var keys []string
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&keys); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := m.SetRedisQueues(keys); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// SetRedisQueues tells the monitor which Laravel queue keys to count.
func (c *Client) SetRedisQueues(ctx context.Context, keys []string) error {
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return c.do(ctx, "POST", "/v1/redis/queues", strings.NewReader(string(data)), nil)
}
