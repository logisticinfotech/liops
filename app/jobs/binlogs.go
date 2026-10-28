// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/engines/xtrabackup"
	"github.com/logisticinfotech/liops/app/notify"
)

// Binlog shipping runs every few minutes, so its runs aren't kept in the job history;
// these state meta keys hold what the UI, the report and the next run need.
const (
	MetaBinlogShippedAt = "binlog_shipped_at"    // RFC 3339: the last run that worked
	metaBinlogPosition  = "binlog_position"      // where MySQL was writing after that run
	metaBinlogFailing   = "binlog_failing_since" // RFC 3339, while runs are failing
	metaBinlogGaps      = "binlog_gaps_reported" // binlog names already reported as missing
)

// Automatic is the trigger of a job liops starts by itself, e.g. a full backup after a
// binlog gap.
const Automatic = "automatic"

// binlogs ships MySQL's binlogs (layer 1, between base backups): if MySQL wrote
// anything since the last run, it closes the current binlog, then uploads every closed
// binlog a restore can need. It has its own lock, so a long backup doesn't hold it up.
func (r *Runner) binlogs(ctx context.Context, trigger string) error {
	if r.XtraBackup == nil {
		return errors.New("binlogs are only shipped for MySQL (engine: mysql); PostgreSQL archives WAL through pgBackRest")
	}
	unlock, err := lockFile(ctx, filepath.Join(filepath.Dir(r.LockPath), "binlog.lock"))
	if err != nil {
		return err
	}
	res, err := r.shipBinlogs(ctx)
	unlock()
	if err != nil {
		r.binlogsFailed(ctx, err)
		return err
	}

	if since := r.meta(metaBinlogFailing); since != "" {
		r.logf(Binlogs, "shipping works again (it had been failing since %s)", since)
		r.setMeta(metaBinlogFailing, "")
	}
	r.setMeta(MetaBinlogShippedAt, r.now().UTC().Format(time.RFC3339))

	if gaps := r.newGaps(res.Gaps); len(gaps) > 0 {
		r.logf(Binlogs, "binlogs missing before they were shipped: %s; taking a new full backup", strings.Join(gaps, ", "))
		subject, body := notify.FailureEmail(notify.Failure{
			Server:   r.Server,
			Headline: "binlogs missing: a gap in point-in-time restores",
			What:     "binlogs " + strings.Join(gaps, ", ") + " were removed from the MySQL server before liops shipped them",
			Reason:   "MySQL deletes binlogs after binlog_expire_logs_seconds, or someone purged or reset them, while shipping wasn't working",
			Safe:     "restores to moments before the gap still work, and so will restores after the new full backup liops is taking now",
			ToDo:     "check that binlog_expire_logs_seconds is at least 7 days, and look for earlier shipping failures in: sudo liops logs",
		})
		r.send(ctx, Binlogs, subject, body)
		return r.Run(ctx, Full, Automatic)
	}
	return nil
}

// shipBinlogs does one shipping run.
func (r *Runner) shipBinlogs(ctx context.Context) (xtrabackup.ShipResult, error) {
	var res xtrabackup.ShipResult
	backups, err := r.XtraBackup.Backups(ctx)
	if err != nil {
		return res, fmt.Errorf("list the backups: %w", err)
	}
	if len(backups) == 0 {
		return res, nil // nothing to replay binlogs onto yet
	}
	status, err := r.XtraBackup.BinlogStatus(ctx)
	if err != nil {
		return res, err
	}
	if status.Position() != r.meta(metaBinlogPosition) {
		// MySQL wrote something since the last run: close the binlog so it can be shipped.
		if err := r.XtraBackup.Rotate(ctx); err != nil {
			return res, fmt.Errorf("close the current binlog: %w", err)
		}
		if status, err = r.XtraBackup.BinlogStatus(ctx); err != nil {
			return res, err
		}
	}
	if res, err = r.XtraBackup.ShipBinlogs(ctx, backups[0].BinlogFile); err != nil {
		return res, err
	}
	r.setMeta(metaBinlogPosition, status.Position())
	if len(res.Shipped) > 0 {
		names := res.Shipped[0]
		if len(res.Shipped) > 1 {
			names += " to " + res.Shipped[len(res.Shipped)-1]
		}
		r.logf(Binlogs, "shipped %d binlog(s), %s: %s", len(res.Shipped), notify.Bytes(res.Bytes), names)
	}
	return res, nil
}

// binlogsFailed logs a failed run, and emails the first failure only: runs every few
// minutes mustn't send an email each.
func (r *Runner) binlogsFailed(ctx context.Context, err error) {
	r.logf(Binlogs, "FAILED: %v", err)
	if r.meta(metaBinlogFailing) != "" {
		return
	}
	r.setMeta(metaBinlogFailing, r.now().Format("2 Jan 15:04"))
	expiry := "for the days set in binlog_expire_logs_seconds"
	subject, body := notify.FailureEmail(notify.Failure{
		Server:   r.Server,
		Headline: "binlog shipping FAILED",
		What:     "shipping binlogs (the changes since the last backup) to storage",
		Safe:     "the nightly backups and dumps are not affected, and MySQL keeps its binlogs " + expiry + ", so nothing is lost if this is fixed before then",
		ToDo:     "check the details below, then run: sudo liops run binlogs (you get one email per failure, not one per try)",
		Output:   errOutput(err),
	})
	r.send(ctx, Binlogs, subject, body)
}

// newGaps returns the gaps not reported before, and remembers them.
func (r *Runner) newGaps(gaps []string) []string {
	reported := strings.Fields(r.meta(metaBinlogGaps))
	var fresh []string
	for _, g := range gaps {
		if !slices.Contains(reported, g) {
			fresh = append(fresh, g)
		}
	}
	if len(fresh) > 0 {
		r.setMeta(metaBinlogGaps, strings.Join(append(reported, fresh...), " "))
	}
	return fresh
}

// meta reads a state meta value; a broken state file reads as empty, and never stops a job.
func (r *Runner) meta(key string) string {
	v, err := r.State.GetMeta(key)
	if err != nil {
		r.logf(Binlogs, "state: %v", err)
	}
	return v
}

func (r *Runner) setMeta(key, value string) {
	if err := r.State.SetMeta(key, value); err != nil {
		r.logf(Binlogs, "state: %v", err)
	}
}
