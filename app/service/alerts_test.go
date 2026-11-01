// SPDX-License-Identifier: Apache-2.0

package service

import (
	"strings"
	"testing"
)

func TestJudgeAlerts(t *testing.T) {
	quiet := alertInput{Disks: []alertDisk{{Mount: "/", Pct: 40, PerDay: 1 << 30, Free: 60 << 30}}, Requests: 3000, C5xx: 2, WAL: 64 << 20, WALDayAgo: 64 << 20}
	if a := judgeAlerts(quiet); len(a) != 0 {
		t.Errorf("a quiet server: %+v", a)
	}
	for _, c := range []struct {
		name string
		set  func(*alertInput)
		kind string
		says string
	}{
		{"disk filling", func(in *alertInput) { in.Disks[0].PerDay, in.Disks[0].Free = 30<<30, 40<<30 }, "disk:/", "full in about 32 hours"},
		{"disk full", func(in *alertInput) { in.Disks[0].Pct, in.Disks[0].PerDay = 96, 0 }, "disk:/", "96% full"},
		{"oom", func(in *alertInput) { in.OOMKills = 3 }, "oom", "killed 3 process"},
		{"5xx burst", func(in *alertInput) { in.C5xx = 600 }, "5xx", "600 of the last 5 minutes' 3000 requests (20%)"},
		{"wal", func(in *alertInput) { in.WAL, in.WALDayAgo = 12<<30, 1<<30 }, "wal", "pg_wal holds 12 GB, up from 1.0 GB"},
	} {
		in := quiet
		in.Disks = append([]alertDisk(nil), quiet.Disks...)
		c.set(&in)
		a := judgeAlerts(in)
		if len(a) != 1 || a[0].Kind != c.kind || !strings.Contains(a[0].Body, c.says) {
			t.Errorf("%s: %+v", c.name, a)
		}
	}
	// A few errors in a quiet minute aren't a burst.
	if a := judgeAlerts(alertInput{C5xx: 60, Requests: 10000}); len(a) != 0 {
		t.Errorf("60 of 10000: %+v", a)
	}
}
