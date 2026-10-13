// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"fmt"

	"github.com/robfig/cron/v3"
)

// Serve runs the scheduled jobs until ctx is cancelled. Schedules are interpreted in
// server.timezone. Cancelling ctx stops running commands (pgBackRest resumes an
// interrupted backup next time).
func (r *Runner) Serve(ctx context.Context) error {
	c := cron.New(cron.WithLocation(r.location()))
	schedules := []struct{ job, spec string }{
		{Full, r.Cfg.PgBackRest.FullSchedule},
		{Diff, r.Cfg.PgBackRest.DiffSchedule},
		{Report, r.Cfg.Report.Schedule},
	}
	names := map[cron.EntryID]string{}
	for _, s := range schedules {
		job := s.job
		id, err := c.AddFunc(s.spec, func() {
			if err := r.Run(ctx, job, Scheduled); err != nil && ctx.Err() == nil {
				r.logf(job, "run failed: %v", err)
			}
		})
		if err != nil {
			return fmt.Errorf("schedule %s %q: %w", job, s.spec, err)
		}
		names[id] = job
	}

	c.Start()
	for _, e := range c.Entries() {
		r.logf("serve", "next %s at %s", names[e.ID], e.Next.Format("Mon 2 Jan 15:04 MST"))
	}
	r.logf("serve", "pgtijori is running (schedules in %s)", r.location())
	<-ctx.Done()
	r.logf("serve", "stopping")
	<-c.Stop().Done() // wait for running jobs to wind down
	return nil
}
