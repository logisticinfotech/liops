// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// fakeHost is a /proc and /sys tree in a temporary folder.
type fakeHost struct {
	t    *testing.T
	root string
}

func newFakeHost(t *testing.T) *fakeHost {
	h := &fakeHost{t: t, root: t.TempDir()}
	h.write("proc/loadavg", "1.50 0.75 0.40 2/345 6789\n")
	h.write("proc/meminfo", "MemTotal:        4000000 kB\nMemFree:          500000 kB\nMemAvailable:    3000000 kB\nBuffers:          100000 kB\nCached:          1000000 kB\nSwapTotal:       2000000 kB\nSwapFree:        1500000 kB\n")
	h.write("proc/mounts", "/dev/vda1 / ext4 rw,relatime 0 0\nproc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n/dev/vda1 /srv/bind ext4 rw 0 0\n/dev/vdb1 /data\\040disk xfs rw 0 0\n")
	os.MkdirAll(filepath.Join(h.root, "data disk"), 0o755)
	h.counters(0)
	return h
}

func (h *fakeHost) write(rel, content string) {
	h.t.Helper()
	path := filepath.Join(h.root, rel)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// counters writes every counter file at step n; between steps (15 s apart) each counter
// moves by a known amount.
func (h *fakeHost) counters(n uint64) {
	// 2 CPUs, 15 s = 3000 ticks in all: user 600, system 300, iowait 300, steal 150, idle 1650.
	h.write("proc/stat", fmt.Sprintf("cpu  %d 0 %d %d %d 0 0 %d 0 0\ncpu0 1 0 1 1 1 0 0 0 0 0\ncpu1 1 0 1 1 1 0 0 0 0 0\nintr 1\n",
		1000+600*n, 500+300*n, 9000+1650*n, 200+300*n, 50+150*n))
	h.write("proc/vmstat", fmt.Sprintf("pswpin %d\npswpout %d\noom_kill %d\n", 10+15*n, 5, n))
	// sda: 30,000 sectors read (1 MB/s over 15 s), busy 7.5 s (50%); sda1 is a partition.
	h.write("proc/diskstats", fmt.Sprintf("   8       0 sda 100 0 %d 0 200 0 %d 0 0 %d 0 0 0 0 0 0 0\n   8       1 sda1 1 0 1 0 1 0 1 0 0 1 0\n   7       0 loop0 1 0 1 0 1 0 1 0 0 1 0\n",
		1000+30000*n, 2000, 4000+7500*n))
	h.write("proc/net/dev", fmt.Sprintf("Inter-|   Receive |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n    lo: 999 1 0 0 0 0 0 0 999 1 0 0 0 0 0 0\n  eth0: %d 10 0 0 0 0 0 0 %d 10 0 0 0 0 0 0\n",
		5000+150000*n, 7000+30000*n))
	// nginx used 1.5 s of CPU over 15 s: 10% of one CPU.
	h.write("sys/fs/cgroup/system.slice/nginx.service/cpu.stat", fmt.Sprintf("usage_usec %d\nuser_usec 1\n", 1000000+1500000*n))
	h.write("sys/fs/cgroup/system.slice/nginx.service/memory.current", "52428800\n")
	h.write("sys/fs/cgroup/system.slice/stopped.service/memory.current", "0\n")
	// Seven queue workers, each 75 ticks (0.75 s) in 15 s: 5% of a CPU each, 35% together.
	for pid := 100; pid < 107; pid++ {
		h.write(fmt.Sprintf("proc/%d/stat", pid), fmt.Sprintf("%d (php8.3) S 1 1 1 0 -1 0 0 0 0 0 %d 0 0 0 20 0 1 0 5000 1000000 2560 0", pid, 100+75*n))
		h.write(fmt.Sprintf("proc/%d/cmdline", pid), "php8.3\x00/var/www/app/artisan\x00queue:work\x00--sleep=3\x00")
	}
	// A command name with spaces and parentheses, and a kernel thread.
	h.write("proc/200/stat", fmt.Sprintf("200 (my (odd) name) S 1 1 1 0 -1 0 0 0 0 0 %d 0 0 0 20 0 1 0 6000 1000 10 0", 10+15*n))
	h.write("proc/200/cmdline", "/usr/sbin/nginx\x00-g\x00daemon off;\x00")
	h.write("proc/2/stat", "2 (kthreadd) S 0 0 0 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0 0")
	h.write("proc/2/cmdline", "")
}

func near(t *testing.T, s Sample, name string, want float64) {
	t.Helper()
	got, ok := s.Values[name]
	if !ok || math.Abs(got-want) > 0.01*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v (present: %v), want %v", name, got, ok, want)
	}
}

func TestCollect(t *testing.T) {
	h := newFakeHost(t)
	c := NewCollector(h.root)
	c.pageSize = 4096
	start := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	first, top, err := c.Collect(start)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.Values["cpu.busy"]; ok || top != nil {
		t.Error("rates need two readings: the first has none")
	}
	near(t, first, "cpus", 2)
	near(t, first, "load.1", 1.5)
	near(t, first, "mem.used", 1000000*1024)
	near(t, first, "swap.used", 500000*1024)
	near(t, first, "svc.mem:nginx.service", 52428800)
	if _, ok := first.Values["svc.mem:stopped.service"]; ok {
		t.Error("a stopped service (no memory) isn't a series")
	}
	for _, name := range []string{"disk.pct:/", "disk.pct:/data disk"} {
		if _, ok := first.Values[name]; !ok {
			t.Errorf("missing %s; got %v", name, keys(first.Values, "disk."))
		}
	}
	if _, ok := first.Values["disk.pct:/srv/bind"]; ok {
		t.Error("a bind mount of a disk already counted is counted once")
	}

	h.counters(1)
	second, top, err := c.Collect(start.Add(15 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	near(t, second, "cpu.user", 20)
	near(t, second, "cpu.system", 10)
	near(t, second, "cpu.iowait", 10)
	near(t, second, "cpu.steal", 5)
	near(t, second, "cpu.busy", 35) // 100 - idle 55% - iowait 10%
	near(t, second, "swap.in", 15*4096/15.0)
	near(t, second, "oom.kills", 1)
	near(t, second, "io.read:sda", 30000*512/15.0)
	near(t, second, "io.util:sda", 50)
	near(t, second, "net.rx:eth0", 10000)
	near(t, second, "net.tx:eth0", 2000)
	near(t, second, "svc.cpu:nginx.service", 10)
	for _, unwanted := range []string{"io.read:sda1", "io.read:loop0", "net.rx:lo"} {
		if _, ok := second.Values[unwanted]; ok {
			t.Errorf("%s shouldn't be collected", unwanted)
		}
	}

	if len(top) == 0 || top[0].Command != "php8.3 artisan queue:work" || top[0].Count != 7 || math.Abs(top[0].CPU-35) > 0.5 {
		t.Fatalf("top processes = %+v", top)
	}
	if top[0].Memory != 7*2560*4096 {
		t.Errorf("queue workers' memory = %d", top[0].Memory)
	}
	if !slices.ContainsFunc(top, func(p Process) bool { return p.Command == "nginx" }) {
		t.Errorf("nginx (odd stat name, real command line) should be grouped as nginx: %+v", top)
	}
}

func TestCollectNeedsProc(t *testing.T) {
	if _, _, err := NewCollector(t.TempDir()).Collect(time.Now()); err == nil {
		t.Error("without /proc, Collect should say monitoring needs Linux")
	}
}

func TestCommandGroup(t *testing.T) {
	for _, c := range []struct {
		comm string
		args []string
		want string
	}{
		{"php-fpm8.3", []string{"php-fpm: pool www", "", ""}, "php-fpm: pool www"},
		{"node", []string{"/usr/bin/node", "--max-old-space-size=512", "/srv/app/server.js"}, "node server.js"},
		{"python3", []string{"/usr/bin/python3", "-m", "gunicorn", "app:wsgi"}, "python3 gunicorn app:wsgi"},
		{"goravel", []string{"/srv/goravel/main", "serve"}, "main"},
		{"kworker/u8:0-flush-8:0", nil, "kworker"},
		{"kthreadd", []string{""}, "kthreadd"},
	} {
		if got := commandGroup(c.comm, c.args); got != c.want {
			t.Errorf("commandGroup(%q, %q) = %q, want %q", c.comm, c.args, got, c.want)
		}
	}
}

func keys(m map[string]float64, prefix string) []string {
	var out []string
	for k := range m {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
