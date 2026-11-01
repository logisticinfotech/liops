// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/xtrabackup"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
)

// backup runs a pgBackRest full or differential backup (layer 1).
func (r *Runner) backup(ctx context.Context, typ, trigger string, run *state.Run) error {
	if r.XtraBackup != nil {
		return r.mysqlBackup(ctx, typ, trigger, run)
	}
	r.setProgress(Progress{Kind: typ, Label: backupName(typ), Detail: "Running pgBackRest…"})
	defer r.clearProgress()

	watchCtx, stopWatch := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		r.watchBackup(watchCtx, typ)
	}()
	out, err := r.Backrest.Backup(ctx, typ)
	stopWatch()
	<-watched
	r.logOutput(typ, out)
	run.Output = command.LastLines(string(out), outputLines)
	if err != nil {
		safe := "WAL archiving runs separately and is not affected by this failure"
		if info, ierr := r.Backrest.Info(ctx); ierr == nil {
			if last := info.Last(""); last != nil {
				safe = "earlier backups are fine; the newest good one is the " + last.Type + " from " +
					notify.When(last.Stop, r.now()) + ", and WAL archiving runs separately"
			}
		}
		subject, body := notify.FailureEmail(notify.Failure{
			Server:   r.Server,
			Headline: tonight(trigger) + backupName(typ) + " FAILED",
			What:     "pgBackRest " + backupName(typ) + " (point-in-time backups)",
			Safe:     safe,
			ToDo:     "check the details below, then run: sudo liops run " + typ,
			Output:   string(out),
		})
		r.send(ctx, typ, subject, body)
		return err
	}

	if info, err := r.Backrest.Info(ctx); err == nil {
		if last := info.Last(typ); last != nil {
			run.Size = last.RepoSize
		}
	}
	return nil
}

// backupPollInterval is how often a running backup's progress is read. Each read is
// a `pgbackrest info`, which reads the repository, so not too often.
var backupPollInterval = 5 * time.Second

// watchBackup turns pgBackRest's own progress (bytes copied so far, from `pgbackrest
// info`) into the progress bar until ctx ends.
func (r *Runner) watchBackup(ctx context.Context, typ string) {
	t := time.NewTicker(backupPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		info, err := r.Backrest.Info(ctx)
		if err != nil || !info.Running || info.Size <= 0 || ctx.Err() != nil {
			continue
		}
		r.setProgress(Progress{
			Kind: typ, Label: backupName(typ), Bytes: true,
			Current: int(info.SizeDone), Total: int(info.Size),
			Detail: notify.Bytes(info.SizeDone) + " of " + notify.Bytes(info.Size) + " copied",
		})
	}
}

// mysqlBackup runs an XtraBackup full or differential backup (MySQL's layer 1).
func (r *Runner) mysqlBackup(ctx context.Context, typ, trigger string, run *state.Run) error {
	r.setProgress(Progress{Kind: typ, Label: backupName(typ), Detail: "Running XtraBackup…"})
	defer r.clearProgress()

	fail := func(err error) error {
		out := errOutput(err)
		r.logOutput(typ, []byte(out))
		run.Output = command.LastLines(out, outputLines)
		safe := "binlogs are shipped separately and are not affected by this failure"
		if list, lerr := r.XtraBackup.Backups(ctx); lerr == nil {
			if last := xtrabackup.Last(list, ""); last != nil {
				safe = "earlier backups are fine; the newest good one is the " + last.Type + " from " +
					notify.When(last.Stop, r.now()) + ", and binlogs are shipped separately"
			}
		}
		subject, body := notify.FailureEmail(notify.Failure{
			Server:   r.Server,
			Headline: tonight(trigger) + backupName(typ) + " FAILED",
			What:     "XtraBackup " + backupName(typ) + " (point-in-time backups)",
			Safe:     safe,
			ToDo:     "check the details below, then run: sudo liops run " + typ,
			Output:   out,
		})
		r.send(ctx, typ, subject, body)
		return err
	}

	newGen := r.meta(metaBinlogNewGen) != ""

	inst := r.XtraBackup.Instance
	version, err := inst.Version(ctx)
	if err != nil {
		return fail(fmt.Errorf("couldn't connect to MySQL: %w", err))
	}
	if _, _, err := setup.CheckPointInTime(ctx, inst, version); err != nil {
		return fail(err)
	}

	m, err := r.XtraBackup.Backup(ctx, typ, newGen)
	if err != nil {
		return fail(err)
	}

	if newGen {
		r.setMeta(metaBinlogNewGen, "")
		r.setMeta(metaBinlogCatchUp, "")
		r.setMeta(metaBinlogCatchUpTried, "")
		if status, err := r.XtraBackup.BinlogStatus(ctx); err == nil {
			r.setMeta(metaBinlogPosition, status.Position())
		}
	}

	run.Size = m.Size
	note := fmt.Sprintf("%s %s: %s in %s, consistent with %s:%d", m.Type, m.Label, notify.Bytes(m.Size), notify.Duration(m.Duration()), m.BinlogFile, m.BinlogPos)
	if m.Type != typ {
		note = "no full backup yet, so this one is a full backup\n" + note
	} else if m.Base != "" {
		note += " (against full " + m.Base + ")"
	}
	r.logf(typ, "%s", note)
	if cleaned, err := r.mysqlRetention(ctx); err != nil {
		// The backup itself worked: report the cleanup, but don't fail the run.
		r.logf(typ, "deleting old backups and binlogs FAILED: %v", err)
		note += "\ndeleting old backups and binlogs FAILED: " + err.Error()
		subject, body := notify.FailureEmail(notify.Failure{
			Server:   r.Server,
			Headline: tonight(trigger) + "cleanup of old backups FAILED",
			What:     "deleting backups and binlogs older than mysql.retention_days",
			Safe:     "the " + backupName(m.Type) + " itself worked; old backups are just kept longer than they should be",
			ToDo:     "check the details below; the next backup tries again",
			Output:   errOutput(err),
		})
		r.send(ctx, typ, subject, body)
	} else if cleaned != "" {
		r.logf(typ, "%s", cleaned)
		note += "\n" + cleaned
	}
	run.Output = note
	return nil
}

// mysqlRetention deletes MySQL backups and binlogs older than mysql.retention_days
// (xtrabackup.PlanRetention) and failed attempts. It returns what it deleted, for the log.
func (r *Runner) mysqlRetention(ctx context.Context) (string, error) {
	backups, err := r.XtraBackup.Backups(ctx)
	if err != nil {
		return "", err
	}
	incomplete, err := r.XtraBackup.Incomplete(ctx)
	if err != nil {
		return "", err
	}
	binlogs, err := r.XtraBackup.ShippedBinlogs(ctx)
	if err != nil {
		return "", err
	}
	plan := xtrabackup.PlanRetention(backups, incomplete, binlogs, r.now(), days(r.Cfg.MySQL.RetentionDays))
	if plan.Empty() {
		return "", nil
	}
	if err := r.XtraBackup.Apply(ctx, plan); err != nil {
		return "", err
	}
	return fmt.Sprintf("retention: deleted %d old backup(s), %d failed attempt(s) and %d binlog(s)",
		len(plan.Backups), len(plan.Incomplete), len(plan.Binlogs)), nil
}
