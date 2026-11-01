// SPDX-License-Identifier: Apache-2.0

package verdict

import (
	"strings"
	"testing"
	"time"
)

// healthy is a quiet server: nothing to say.
func healthy() Evidence {
	return Evidence{CPUs: 4, CPUBusy: 20, CPUHighShare: 2, Steal: 0.5, IOWait: 1, DiskUtil: 5, MemTotal: 8 << 30, MemAvailMin: 4 << 30,
		Filesystems: []Filesystem{{Mount: "/", Pct: 40}},
		Pools:       []Pool{{Label: "8.3/www", BusyPct: 1, MaxChildren: 20, Affordable: 60}},
		Databases:   []Database{{Title: "MySQL", TotalMS: 600e3, CacheHit: 99.9, HasCacheHit: true}},
		Requests:    Requests{Count: 100000, Slow: 50, P95: 180, TimeMS: 5e6, AppMS: 4e6},
	}
}

func first(t *testing.T, e Evidence) Finding {
	t.Helper()
	f := Judge(e)
	if len(f) == 0 {
		t.Fatal("no finding")
	}
	return f[0]
}

func TestHealthyServerHasNothingToSay(t *testing.T) {
	if f := Judge(healthy()); len(f) != 0 {
		t.Errorf("findings on a healthy server: %+v", f)
	}
}

// The scenarios of MO3 (docs/PLAN-monitor.md §8): the verdict names the right cause first.
func TestScenarios(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*Evidence)
		says string
		area string
	}{
		{"CPU hog", func(e *Evidence) { e.CPUBusy, e.CPUHighShare = 85, 60 }, "outgrown the VM", "Machine"},
		{"memory pressure", func(e *Evidence) { e.OOMKills, e.SwapIn, e.MemAvailMin = 2, 40, 100<<20 }, "short of memory", "Machine"},
		{"bad query", func(e *Evidence) {
			e.CPUBusy, e.CPUHighShare = 75, 40 // the scans make the CPU busy: not "outgrown"
			e.Databases[0].ScanMS, e.Databases[0].SlowMax = 500e3, 4
			e.Databases[0].Scans = []Query{{DB: "tenant_a", Text: "SELECT * FROM `customers` WHERE LOWER ( NAME ) LIKE ?", MS: 400e3, Read: "500K rows per call"}}
		}, "Fix the app first", "Database"},
		{"N+1", func(e *Evidence) {
			e.NPlusOne = []Query{{DB: "shop", Text: "SELECT * FROM `products` WHERE `id` = ?", PerRequest: 240}}
		}, "N+1", "Database"},
		{"small pm.max_children", func(e *Evidence) { e.Pools[0].BusyPct, e.Pools[0].MaxChildren = 85, 2 }, "wait for a free PHP worker", "PHP"},
		{"full-ish disk", func(e *Evidence) { e.Filesystems[0].Pct, e.Filesystems[0].FullInDays = 93, 4 }, "full in about 4 days", "Disk"},
		{"chatty log", func(e *Evidence) {
			e.Logs = []Log{{Path: "/srv/shop/storage/logs/laravel.log", PerHour: 300 << 20}}
		}, "A log grows by 300 MB an hour", "Logs"},
		{"slower after a deploy", func(e *Evidence) {
			e.Deploy = &Deploy{At: time.Date(2026, 11, 3, 14, 2, 0, 0, time.UTC), What: "/srv/shop: a1b2 → c3d4",
				Before: Requests{Count: 50000, P95: 180}, After: Requests{Count: 48000, P95: 950},
				Routes: []string{"GET /dashboard: 150 ms → 1.4 s"}, NewQueries: []string{"SELECT … FROM `orders` WHERE DATE ( `created_at` ) = ?"}}
		}, "Slower since the deploy at Tue 14:02", "Requests"},
		{"steal", func(e *Evidence) { e.Steal = 22 }, "host is overcommitted", "Machine"},
		{"disk-bound", func(e *Evidence) { e.IOWait, e.DiskUtil, e.DiskDevice = 30, 95, "sda" }, "disk is the bottleneck", "Disk"},
		{"waiting elsewhere", func(e *Evidence) {
			e.Requests.Slow, e.Requests.SlowRoutes = 6000, []string{"POST /checkout: 95% under 4.1 s"}
			e.Databases[0].TotalMS = 100e3
		}, "waiting on something else", "Requests"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := healthy()
			c.set(&e)
			f := first(t, e)
			if !strings.Contains(f.Says, c.says) || f.Area != c.area || f.Level != Problem {
				t.Errorf("first finding = %+v\nall: %+v", f, Judge(e))
			}
			if f.Because == "" || f.Link == "" {
				t.Errorf("a finding needs the numbers behind it and where to look: %+v", f)
			}
		})
	}
}

func TestBusyPoolWithBusyCPUIsntMoreWorkers(t *testing.T) {
	e := healthy()
	e.Pools[0].BusyPct, e.CPUBusy = 90, 92
	for _, f := range Judge(e) {
		if f.Area == "PHP" && (f.Level != Warning || !strings.Contains(f.Do, "More workers won't help")) {
			t.Errorf("finding = %+v", f)
		}
	}
}
