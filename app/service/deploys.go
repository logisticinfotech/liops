// SPDX-License-Identifier: Apache-2.0

package service

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/logisticinfotech/liops/app/laravel"
	"github.com/logisticinfotech/liops/app/state"
)

// deployEvery is how often the apps are looked at for a new deploy.
var deployEvery = time.Minute

// WatchDeploys notices deploys while monitoring is on: an app's current symlink or git
// commit changing. Each one goes into the audit log (action "deploy"), which the charts
// show as a marker, so "slower since the deploy at 14:02" is visible. It returns when the
// service stops.
func (s *Service) WatchDeploys(apps func() []laravel.App) {
	t := time.NewTicker(deployEvery)
	defer t.Stop()
	var logsAt time.Time
	for {
		if s.Config().Monitor.Enabled {
			found := apps()
			if time.Since(logsAt) >= appLogEvery {
				logsAt = time.Now()
				s.measureAppLogs(found)
			}
			for _, a := range found {
				release := laravel.Release(a)
				if release == "" {
					continue
				}
				key := "deploy:" + a.Dir
				old, err := s.State.GetMeta(key)
				if err != nil || old == release {
					continue
				}
				if old != "" {
					s.State.Audit(state.AuditEntry{Time: time.Now(), User: "liops", Action: "deploy", Detail: a.Dir + ": " + old + " → " + release, OK: true})
				}
				s.State.SetMeta(key, release)
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// appLogEvery is how often the apps' log files are measured.
var appLogEvery = 10 * time.Minute

// LogRate is one log file and how fast it grows.
type LogRate struct {
	Path    string
	PerHour float64 // bytes
}

// measureAppLogs compares the apps' log files (storage/logs) with the last measurement;
// the monitor can't read them, so root does.
func (s *Service) measureAppLogs(apps []laravel.App) {
	now := time.Now()
	sizes := map[string]int64{}
	for _, a := range apps {
		files, _ := filepath.Glob(filepath.Join(a.Dir, "storage", "logs", "*.log"))
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil {
				sizes[f] = fi.Size()
			}
		}
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	prev, prevAt := s.appLogSizes, s.appLogAt
	s.appLogSizes, s.appLogAt = sizes, now
	if prev == nil {
		return
	}
	hours := now.Sub(prevAt).Hours()
	var rates []LogRate
	for f, size := range sizes {
		grew := size
		if old, ok := prev[f]; ok && size >= old {
			grew = size - old
		}
		if grew > 0 && hours > 0 {
			rates = append(rates, LogRate{f, float64(grew) / hours})
		}
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i].PerHour > rates[j].PerHour })
	s.appLogRates = rates
}

// AppLogGrowth returns the apps' log files that grew since the last measurement.
func (s *Service) AppLogGrowth() []LogRate {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	return append([]LogRate(nil), s.appLogRates...)
}
