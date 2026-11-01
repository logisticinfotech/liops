// SPDX-License-Identifier: Apache-2.0

package service

import (
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
	for {
		if s.Config().Monitor.Enabled {
			for _, a := range apps() {
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
