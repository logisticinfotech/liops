// SPDX-License-Identifier: Apache-2.0

package php

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFPM answers FastCGI requests like a PHP-FPM pool, with whatever reply returns.
func fakeFPM(t *testing.T, reply func(params map[string]string) string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lp") // socket paths are short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fpm.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				params := map[string]string{}
				var raw []byte
				for {
					h := make([]byte, 8)
					if _, err := io.ReadFull(r, h); err != nil {
						return
					}
					content := make([]byte, int(binary.BigEndian.Uint16(h[4:]))+int(h[6]))
					io.ReadFull(r, content)
					if h[1] == fcgiParams {
						raw = append(raw, content...)
					}
					if h[1] == fcgiStdin {
						break
					}
				}
				for len(raw) > 0 {
					kl, n1 := length(raw)
					vl, n2 := length(raw[n1:])
					raw = raw[n1+n2:]
					params[string(raw[:kl])] = string(raw[kl : kl+vl])
					raw = raw[kl+vl:]
				}
				out := []byte(reply(params))
				h := []byte{1, fcgiStdout, 0, 1, 0, 0, 0, 0}
				binary.BigEndian.PutUint16(h[4:], uint16(len(out)))
				c.Write(append(h, out...))
				c.Write([]byte{1, fcgiEndRequest, 0, 1, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
			}(conn)
		}
	}()
	return sock
}

func length(b []byte) (int, int) {
	if b[0] < 128 {
		return int(b[0]), 1
	}
	return int(binary.BigEndian.Uint32(b) &^ (1 << 31)), 4
}

func TestFindPools(t *testing.T) {
	etc := t.TempDir()
	for _, v := range []string{"8.3", "8.5"} {
		os.MkdirAll(filepath.Join(etc, v, "fpm", "pool.d"), 0o755)
	}
	os.WriteFile(filepath.Join(etc, "8.3", "fpm", "pool.d", "www.conf"), []byte(`; Ubuntu's default, trimmed
[www]
user = www-data
listen = /run/php/php8.3-fpm.sock
pm = dynamic
pm.max_children = 5
;pm.status_path = /status
`), 0o644)
	os.WriteFile(filepath.Join(etc, "8.5", "fpm", "pool.d", "apps.conf"), []byte(`[shop]
listen = 127.0.0.1:9001
pm.max_children = 20
pm.status_path = /fpm-status
request_slowlog_timeout = 3s
slowlog = /var/log/php8.5-fpm.$pool.slow.log
[api]
listen = 9002
`), 0o644)
	pools, err := FindPools(etc)
	if err != nil || len(pools) != 3 {
		t.Fatalf("pools = %+v, %v", pools, err)
	}
	if p := pools[0]; p.Label() != "8.3/www" || p.MaxChildren != 5 || p.StatusPath != "" || p.Service() != "php8.3-fpm" || p.SlowLogOn() {
		t.Errorf("www = %+v", p)
	}
	if p := pools[1]; p.Name != "shop" || p.SlowLog != "/var/log/php8.5-fpm.shop.slow.log" || !p.SlowLogOn() {
		t.Errorf("shop = %+v", p)
	}
	if n, a := listenAddr(pools[2].Listen); n != "tcp" || a != "127.0.0.1:9002" {
		t.Errorf("listen 9002 = %s %s", n, a)
	}
}

func TestCollect(t *testing.T) {
	var mu sync.Mutex
	maxHit, start := 4.0, 100.0
	sock := fakeFPM(t, func(p map[string]string) string {
		mu.Lock()
		defer mu.Unlock()
		if p["SCRIPT_NAME"] != StatusPath || p["QUERY_STRING"] != "json" {
			return "Status: 404 Not Found\r\n\r\nFile not found."
		}
		return fmt.Sprintf("Content-Type: application/json\r\n\r\n{\"pool\":\"www\",\"start since\":%g,\"accepted conn\":%g,\"listen queue\":3,\"idle processes\":0,\"active processes\":5,\"total processes\":5,\"max children reached\":%g,\"slow requests\":0}",
			start, start*10, maxHit)
	})
	etc := t.TempDir()
	os.MkdirAll(filepath.Join(etc, "8.3", "fpm", "pool.d"), 0o755)
	os.WriteFile(filepath.Join(etc, "8.3", "fpm", "pool.d", "www.conf"), []byte("[www]\nlisten = "+sock+"\npm.max_children = 5\npm.status_path = "+StatusPath+"\n"), 0o644)

	c := &Collector{Etc: etc}
	now := time.Now()
	fpm, _, err := c.Collect(context.Background(), now)
	if err != nil || fpm["fpm.queue:8.3/www"] != 3 || fpm["fpm.active:8.3/www"] != 5 || fpm["fpm.max_children:8.3/www"] != 5 {
		t.Fatalf("fpm = %v, %v", fpm, err)
	}
	if _, ok := fpm["fpm.max_children_reached:8.3/www"]; ok {
		t.Error("counters need two readings")
	}
	mu.Lock()
	maxHit, start = 7, 115
	mu.Unlock()
	fpm, _, _ = c.Collect(context.Background(), now.Add(15*time.Second))
	if fpm["fpm.max_children_reached:8.3/www"] != 3 || fpm["fpm.per_second:8.3/www"] != 10 {
		t.Errorf("deltas = %v", fpm)
	}
	// Reloaded: the counters start again.
	mu.Lock()
	maxHit, start = 1, 5
	mu.Unlock()
	fpm, _, _ = c.Collect(context.Background(), now.Add(30*time.Second))
	if fpm["fpm.max_children_reached:8.3/www"] != 1 {
		t.Errorf("after a reload = %v", fpm)
	}
}

func TestOPcacheSeries(t *testing.T) {
	var o OPcache
	sock := fakeFPM(t, func(p map[string]string) string {
		if p["SCRIPT_FILENAME"] != OPcacheScript {
			return "Status: 404\r\n\r\n"
		}
		return "Content-Type: application/json\r\n\r\n" + `{"php":"8.3.6","enabled":true,"memory":{"used_memory":120000000,"free_memory":0,"wasted_memory":14217728},
"strings":{"buffer_size":8388608,"used_memory":8388000},"stats":{"num_cached_scripts":9800,"num_cached_keys":10000,"max_cached_keys":16229,"hits":900,"misses":100,"oom_restarts":2,"hash_restarts":0},"full":true,"max_files":10000}`
	})
	o, err := ReadOPcache(context.Background(), Pool{Listen: sock})
	if err != nil {
		t.Fatal(err)
	}
	v := opcacheSeries(o)
	if v["opcache.full"] != 1 || v["opcache.memory_pct"] != 100 || v["opcache.hit_rate"] != 90 || v["opcache.restarts"] != 2 || v["opcache.strings_pct"] < 99 {
		t.Errorf("series = %v", v)
	}
	if !strings.HasPrefix(o.PHP, "8.3") {
		t.Errorf("o = %+v", o)
	}
}
