// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeRedis answers like Redis to the monitor's user, with INFO counters the test sets.
type fakeRedis struct {
	mu      sync.Mutex
	evicted int
	noUser  bool
	cmds    []string
}

func (f *fakeRedis) serve(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.conn(c)
		}
	}()
	return ln.Addr().String()
}

func (f *fakeRedis) conn(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		v, err := readRESP(r, 0)
		if err != nil {
			return
		}
		args, _ := v.([]any)
		var words []string
		for _, a := range args {
			s, _ := a.(string)
			words = append(words, s)
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, strings.Join(words, " "))
		evicted, noUser := f.evicted, f.noUser
		f.mu.Unlock()
		switch strings.ToUpper(words[0]) {
		case "AUTH":
			if noUser {
				fmt.Fprint(c, "-WRONGPASS invalid username-password pair or user is disabled.\r\n")
			} else {
				fmt.Fprint(c, "+OK\r\n")
			}
		case "INFO":
			info := fmt.Sprintf("# Server\r\nredis_version:7.2.4\r\n# Memory\r\nused_memory:1040000000\r\nmaxmemory:1073741824\r\nmaxmemory_policy:allkeys-lru\r\nmem_fragmentation_ratio:1.10\r\n# Persistence\r\nrdb_saves:0\r\nrdb_last_save_time:1793400000\r\naof_enabled:0\r\n# Stats\r\nevicted_keys:%d\r\nkeyspace_hits:%d\r\nkeyspace_misses:100\r\nconnected_clients:7\r\ninstantaneous_ops_per_sec:250\r\n# Keyspace\r\ndb0:keys=1200,expires=1100,avg_ttl=0\r\ndb1:keys=300,expires=0,avg_ttl=0\r\n", evicted, 900+evicted*9)
			fmt.Fprintf(c, "$%d\r\n%s\r\n", len(info), info)
		case "SLOWLOG":
			fmt.Fprint(c, "*1\r\n*6\r\n:1\r\n:1793400000\r\n:25000\r\n*2\r\n$4\r\nKEYS\r\n$9\r\nsession:*\r\n$15\r\n127.0.0.1:50000\r\n$0\r\n\r\n")
		default:
			fmt.Fprint(c, "-NOPERM this user has no permissions to run this command\r\n")
		}
	}
}

func TestRedis(t *testing.T) {
	f := &fakeRedis{noUser: true}
	addr := f.serve(t)
	m, clock := newDBMonitor(t, &fakeMySQL{})
	m.Redis = FindRedis(addr)
	ctx := context.Background()
	m.readRedis(ctx)
	if st := m.redis.status; !strings.Contains(st.Error, "Set up Redis access") || st.Absent {
		t.Errorf("no user: %+v", st)
	}

	f.mu.Lock()
	f.noUser = false
	f.mu.Unlock()
	m.readRedis(ctx)
	f.mu.Lock()
	f.evicted = 50
	f.mu.Unlock()
	*clock = clock.Add(Interval)
	m.readRedis(ctx)
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	s, _ := m.Store.Latest()
	if s.Values["redis.evicted"] != 50 || s.Values["redis.hit_rate"] != 100 || s.Values["redis.keys:db0"] != 1200 || s.Values["redis.maxmemory"] != 1<<30 {
		t.Errorf("values = %v", s.Values)
	}
	if st := m.redis.status; st.Policy != "allkeys-lru" || !st.LastSave.IsZero() || st.AOF || st.Version != "7.2.4" {
		t.Errorf("status = %+v (rdb_saves 0 means it never saved)", st)
	}
	slow, err := m.RedisSlowlog(ctx)
	if err != nil || len(slow) != 1 || slow[0].MS != 25 || strings.Join(slow[0].Args, " ") != "KEYS session:*" {
		t.Errorf("slowlog = %+v, %v", slow, err)
	}
	for _, c := range f.cmds {
		if !strings.HasPrefix(c, "AUTH liops-monitor") && c != "INFO" && c != "SLOWLOG GET 20" {
			t.Errorf("the monitor should only send AUTH, INFO and SLOWLOG GET, not %q", c)
		}
	}

	m.Redis = FindRedis("127.0.0.1:1") // nothing listens
	m.readRedis(ctx)
	if !m.redis.status.Absent {
		t.Errorf("absent: %+v", m.redis.status)
	}
}

func TestRedisExposed(t *testing.T) {
	head := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	local := head + "   0: 0100007F:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000   112        0 1234\n"
	if got := listeningOn(local, 6379); got != "" {
		t.Errorf("loopback only: %q", got)
	}
	open := head + "   0: 00000000:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000   112        0 1234\n"
	if got := listeningOn(open, 6379); got != "all addresses, port 6379" {
		t.Errorf("open: %q", got)
	}
}
