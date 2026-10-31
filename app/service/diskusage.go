// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"time"
)

// How often the databases' sizes go to the monitor, and how often the service looks
// whether it's time (so turning monitoring on is picked up within minutes).
var (
	diskUsageEvery = time.Hour
	diskUsagePoll  = 5 * time.Minute
)

// PushDiskUsage sends the databases' sizes (jobs.Runner.DiskUsage) to the monitor every
// hour while monitoring is on: the monitor's user can't read the database folders. It
// returns when the service stops. A failure is logged once until the next success.
func (s *Service) PushDiskUsage(push func(context.Context, map[string]float64) error) {
	t := time.NewTicker(diskUsagePoll)
	defer t.Stop()
	var last time.Time
	var lastErr string
	for {
		if r := s.Runner(); r != nil && s.Config().Monitor.Enabled && s.SetupComplete() && time.Since(last) >= diskUsageEvery {
			ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
			usage, err := r.DiskUsage(ctx)
			if err == nil {
				err = push(ctx, usage)
			}
			cancel()
			switch {
			case err == nil:
				last, lastErr = time.Now(), ""
			case err.Error() != lastErr:
				lastErr = err.Error()
				s.logf("database sizes for monitoring: %v", err)
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
	}
}
