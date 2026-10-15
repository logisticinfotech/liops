// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/pgbackrest"
	"github.com/logisticinfotech/pgtijori/app/notify"
	"github.com/logisticinfotech/pgtijori/app/state"
)

// backupGrace is how late a scheduled backup may finish before the report calls it missing.
const backupGrace = 6 * time.Hour

// Health is the state of both backup layers, for the daily summary and the dashboard.
type Health struct {
	Summary notify.Summary
	Info    *pgbackrest.Info // nil when pgBackRest couldn't be asked
	Output  string           // details of the problems found
}

// Health checks the backups. deep also runs pgBackRest check (which archives a WAL
// segment end to end) and the storage probe: right for the daily report, too heavy
// for every page view.
func (r *Runner) Health(ctx context.Context, deep bool) Health {
	now := r.now()
	h := Health{Summary: notify.Summary{Server: r.Server, Now: now, DumpsEnabled: r.Cfg.Dumps.Enabled, DiskUsedPct: -1}}
	s := &h.Summary
	var output strings.Builder
	problem := func(format string, args ...any) {
		p := fmt.Sprintf(format, args...)
		s.Problems = append(s.Problems, p)
		output.WriteString("problem: " + p + "\n")
	}

	if deep {
		if out, err := r.Backrest.Check(ctx); err != nil {
			r.logOutput(Report, out)
			problem("pgBackRest check failed: %s", notify.LikelyReason(string(out)))
			output.WriteString(command.LastLines(string(out), 30) + "\n")
		}
		if err := r.Store.Probe(ctx); err != nil {
			problem("storage test failed: %s", notify.LikelyReason(err.Error()))
			output.WriteString(err.Error() + "\n")
		}
	}

	if info, err := r.Backrest.Info(ctx); err != nil {
		problem("couldn't read the backup list: %s", notify.LikelyReason(errOutput(err)))
	} else {
		h.Info = info
		if last := info.Last(""); last != nil {
			s.LastBackupType, s.LastBackupAt, s.LastBackupDuration = last.Type, last.Stop, last.Duration()
			s.RestoreFrom = info.RestoreFrom()
			if last.Error {
				problem("the newest backup (%s) reported checksum errors: see pgbackrest info", last.Label)
			}
		}
		if due := r.lastDueBackup(now); !due.IsZero() && (s.LastBackupAt.IsZero() || s.LastBackupAt.Before(due)) {
			problem("the backup scheduled for %s didn't complete", notify.When(due, now))
		}
	}

	if archived, failed, err := r.archiver(ctx); err != nil {
		problem("couldn't ask PostgreSQL about WAL archiving: %s", notify.LikelyReason(errOutput(err)))
	} else {
		s.LastWAL = archived
		if !failed.IsZero() && failed.After(archived) {
			problem("WAL archiving is failing (last failure %s): changes since then aren't backed up yet", notify.When(failed, now))
		}
	}

	if r.Cfg.Dumps.Enabled {
		r.summarizeDumps(s, now, problem)
	}

	if objs, err := r.Store.List(ctx, ""); err != nil {
		problem("couldn't list storage: %s", notify.LikelyReason(err.Error()))
	} else {
		for _, o := range objs {
			s.StorageUsed += o.Size
		}
	}

	if pct, err := diskUsedPct(r.Cfg.Postgres.DataDir); err == nil {
		s.DiskUsedPct = pct
		if pct >= 90 {
			problem("the server disk is %d%% full", pct)
		}
	}

	if sched, err := cron.ParseStandard(r.Cfg.PgBackRest.FullSchedule); err == nil {
		s.NextFull = sched.Next(now)
	}
	h.Output = output.String()
	return h
}

// report checks everything and sends the daily summary email.
func (r *Runner) report(ctx context.Context, run *state.Run) error {
	h := r.Health(ctx, true)
	output := h.Output
	now := r.now()
	if n, err := r.State.PruneRuns(now.Add(-keepRuns)); err == nil && n > 0 {
		output += fmt.Sprintf("removed %d job run(s) older than 90 days from the history\n", n)
	}
	if n, err := r.State.PruneAudit(now.Add(-keepAudit)); err == nil && n > 0 {
		output += fmt.Sprintf("removed %d audit log entries older than a year\n", n)
	}
	run.Output = output

	subject, body := notify.DailySummary(h.Summary)
	if err := r.Mail.Send(ctx, subject, body); err != nil {
		return fmt.Errorf("send daily summary: %w", err)
	}
	if len(h.Summary.Problems) > 0 {
		r.logf(Report, "summary sent with %d problem(s)", len(h.Summary.Problems))
	}
	return nil
}

func (r *Runner) summarizeDumps(s *notify.Summary, now time.Time, problem func(string, ...any)) {
	last, err := r.State.LastRun(Dumps, "")
	switch {
	case err != nil:
		problem("couldn't read the job history: %v", err)
	case last == nil:
		problem("no database dumps have run yet")
	default:
		for _, d := range last.Dumps {
			s.DumpsTotal++
			if d.Error == "" {
				s.DumpsOK++
				s.DumpsSize += d.Size
			}
		}
		if last.Status != state.StatusOK {
			problem("the last database dumps (%s) %s: %s", notify.When(last.Start, now), last.Status, last.Error)
		}
		if now.Sub(last.Start) > 36*time.Hour {
			problem("no database dumps since %s", notify.When(last.Start, now))
		}
	}
}

// lastDueBackup is the most recent scheduled full or diff backup that should have
// finished by now (allowing backupGrace). Zero if none in the last 8 days.
func (r *Runner) lastDueBackup(now time.Time) time.Time {
	var due time.Time
	cutoff := now.Add(-backupGrace)
	for _, spec := range []string{r.Cfg.PgBackRest.FullSchedule, r.Cfg.PgBackRest.DiffSchedule} {
		sched, err := cron.ParseStandard(spec)
		if err != nil {
			continue
		}
		for t := sched.Next(now.AddDate(0, 0, -8)); !t.IsZero() && t.Before(cutoff); t = sched.Next(t) {
			if t.After(due) {
				due = t
			}
		}
	}
	return due
}

// archiver returns when PostgreSQL last archived a WAL segment, and last failed to.
func (r *Runner) archiver(ctx context.Context) (archived, failed time.Time, err error) {
	rows, err := r.Cluster.Query(ctx, "postgres",
		"SELECT coalesce(extract(epoch FROM last_archived_time)::bigint, 0), coalesce(extract(epoch FROM last_failed_time)::bigint, 0) FROM pg_stat_archiver")
	if err != nil {
		return archived, failed, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return archived, failed, fmt.Errorf("pg_stat_archiver: unexpected output %q", rows)
	}
	unix := func(s string) time.Time {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n == 0 {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	return unix(rows[0][0]), unix(rows[0][1]), nil
}

func diskUsedPct(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	total := uint64(st.Blocks) * uint64(st.Bsize)
	avail := uint64(st.Bavail) * uint64(st.Bsize)
	if total == 0 {
		return 0, fmt.Errorf("statfs %s: no size", path)
	}
	return int((total - avail) * 100 / total), nil
}
