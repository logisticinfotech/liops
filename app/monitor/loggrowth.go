// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LogGrowthInterval is how often the log files' sizes are compared.
const LogGrowthInterval = 10 * time.Minute

// LogDirs are the folders whose log files the monitor watches grow.
var LogDirs = []string{"/var/log"}

// LogGrowth is one log file and how fast it grows.
type LogGrowth struct {
	Path    string  `json:"path"`
	Size    float64 `json:"size"`
	PerHour float64 `json:"per_hour"` // bytes a hour, from the last two measurements
}

// logSizes is one measurement of the log files.
type logSizes struct {
	at    time.Time
	sizes map[string]float64
}

// measureLogs compares the log files' sizes with the last measurement. A file that
// shrank was rotated: it counts from zero.
func (m *Monitor) measureLogs() {
	now := m.Now()
	sizes := map[string]float64{}
	for _, dir := range LogDirs {
		root := filepath.Join(m.Collector.Root, dir)
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() && path != root {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() || strings.HasSuffix(path, ".gz") || strings.Contains(filepath.Base(path), ".log.") {
				return nil
			}
			if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
				sizes[strings.TrimPrefix(path, strings.TrimSuffix(m.Collector.Root, "/"))] = float64(fi.Size())
			}
			return nil
		})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.logs
	m.logs = &logSizes{at: now, sizes: sizes}
	if prev == nil {
		return
	}
	hours := now.Sub(prev.at).Hours()
	if hours <= 0 {
		return
	}
	var growth []LogGrowth
	for path, size := range sizes {
		grew := size
		if old, ok := prev.sizes[path]; ok && size >= old {
			grew = size - old
		}
		if grew > 0 {
			growth = append(growth, LogGrowth{Path: path, Size: size, PerHour: grew / hours})
		}
	}
	sort.Slice(growth, func(i, j int) bool { return growth[i].PerHour > growth[j].PerHour })
	m.logGrowth = growth[:min(20, len(growth))]
}

// logLoop measures the log files every LogGrowthInterval.
func (m *Monitor) logLoop(ctx context.Context) {
	t := time.NewTicker(LogGrowthInterval)
	defer t.Stop()
	for {
		m.measureLogs()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Monitor) logHandlers(mux *http.ServeMux, writeJSON func(http.ResponseWriter, any)) {
	mux.HandleFunc("GET /v1/logs/growth", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		out := append([]LogGrowth{}, m.logGrowth...)
		m.mu.Unlock()
		writeJSON(w, out)
	})
}

// LogGrowth returns the fastest-growing log files under /var/log.
func (c *Client) LogGrowth(ctx context.Context) ([]LogGrowth, error) {
	var out []LogGrowth
	return out, c.get(ctx, "/v1/logs/growth", &out)
}
