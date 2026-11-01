// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogGrowth(t *testing.T) {
	h := newFakeHost(t)
	h.write("var/log/syslog", strings.Repeat("x", 1000))
	h.write("var/log/nginx/error.log", "")
	h.write("var/log/nginx/access.log.1", strings.Repeat("y", 5000)) // rotated: not watched
	m := New(NewCollector(h.root), nil, nil)
	clock := time.Now()
	m.Now = func() time.Time { return clock }
	m.measureLogs()
	h.write("var/log/syslog", strings.Repeat("x", 61000))        // +60 KB in 10 minutes
	h.write("var/log/nginx/error.log", strings.Repeat("e", 100)) // +100 B
	h.write("var/log/app.log", strings.Repeat("a", 500))         // new
	os.Remove(filepath.Join(h.root, "var/log/nginx/access.log.1"))
	clock = clock.Add(10 * time.Minute)
	m.measureLogs()
	g := m.logGrowth
	if len(g) != 3 || g[0].Path != "/var/log/syslog" || g[0].PerHour != 360000 || g[2].Path != "/var/log/nginx/error.log" {
		t.Errorf("growth = %+v", g)
	}
	// Rotated (smaller): counts from zero.
	h.write("var/log/syslog", strings.Repeat("x", 100))
	clock = clock.Add(10 * time.Minute)
	m.measureLogs()
	for _, x := range m.logGrowth {
		if x.Path == "/var/log/syslog" && x.PerHour != 600 {
			t.Errorf("after rotation: %+v", x)
		}
	}
}
