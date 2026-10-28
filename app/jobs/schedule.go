// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler runs full, diff and report on their schedules, in server.timezone.
type Scheduler struct {
	c     *cron.Cron
	names map[cron.EntryID]string
}

// Next is the next scheduled run of a job.
type Next struct {
	Job string
	At  time.Time
}

// StartScheduler starts the schedules. Jobs run with ctx: cancelling it stops running
// commands (pgBackRest resumes an interrupted backup next time).
func (r *Runner) StartScheduler(ctx context.Context) (*Scheduler, error) {
	s := &Scheduler{c: cron.New(cron.WithLocation(r.location())), names: map[cron.EntryID]string{}}
	type entry struct{ job, spec string }
	entries := []entry{
		{Full, r.Cfg.PgBackRest.FullSchedule},
		{Diff, r.Cfg.PgBackRest.DiffSchedule},
		{Report, r.Cfg.Report.Schedule},
	}
	if r.Cfg.IsMySQL() {
		entries[0].spec, entries[1].spec = r.Cfg.MySQL.FullSchedule, r.Cfg.MySQL.DiffSchedule
		entries = append(entries, entry{Binlogs, "@every " + time.Duration(r.Cfg.MySQL.BinlogInterval).String()})
	}
	for _, e := range entries {
		job := e.job
		id, err := s.c.AddFunc(e.spec, func() {
			if err := r.Run(ctx, job, Scheduled); err != nil && ctx.Err() == nil {
				r.logf(job, "run failed: %v", err)
			}
		})
		if err != nil {
			return nil, fmt.Errorf("schedule %s %q: %w", job, e.spec, err)
		}
		s.names[id] = job
	}
	s.c.Start()
	for _, n := range s.Next() {
		r.logf("serve", "next %s at %s", n.Job, n.At.Format("Mon 2 Jan 15:04 MST"))
	}
	return s, nil
}

// Next lists the next run of each scheduled job, soonest first.
func (s *Scheduler) Next() []Next {
	var next []Next
	for _, e := range s.c.Entries() {
		next = append(next, Next{Job: s.names[e.ID], At: e.Next})
	}
	sort.Slice(next, func(i, j int) bool { return next[i].At.Before(next[j].At) })
	return next
}

// Stop stops scheduling new runs. Jobs already running carry on; the returned
// context is done when they have finished.
func (s *Scheduler) Stop() context.Context { return s.c.Stop() }

// Serve runs the schedules until ctx is cancelled, then waits for running jobs.
func (r *Runner) Serve(ctx context.Context) error {
	s, err := r.StartScheduler(ctx)
	if err != nil {
		return err
	}
	r.logf("serve", "liops is running (schedules in %s)", r.location())
	<-ctx.Done()
	r.logf("serve", "stopping")
	<-s.Stop().Done()
	return nil
}
