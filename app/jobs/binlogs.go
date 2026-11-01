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
	MetaBinlogShippedAt    = "binlog_shipped_at"     // RFC 3339: the last run that worked
	metaBinlogPosition     = "binlog_position"       // where MySQL was writing after that run
	metaBinlogFailing      = "binlog_failing_since"  // RFC 3339, while runs are failing
	metaBinlogGaps         = "binlog_gaps_reported"  // binlog names already reported as missing
	metaBinlogCatchUp      = "binlog_catch_up"       // RFC 3339 of the gap; empty when none
	metaBinlogCatchUpTried = "binlog_catch_up_tried" // RFC 3339 of the last catch-up full attempt
	binlogShipTimeout      = time.Hour
	catchUpEvery           = time.Hour
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
	unlock, err := tryLockFile(filepath.Join(filepath.Dir(r.LockPath), "binlog.lock"))
	if errors.Is(err, errLocked) {
		r.logf(Binlogs, "skipped: a previous shipping run is still going")
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	shipCtx, cancel := context.WithTimeout(ctx, binlogShipTimeout)
	res, err := r.shipBinlogs(shipCtx)
	cancel()
	if err != nil {
		r.binlogsFailed(ctx, err)
		return err
	}

	if since := r.meta(metaBinlogFailing); since != "" {
		r.logf(Binlogs, "shipping works again (it had been failing since %s)", since)
		r.setMeta(metaBinlogFailing, "")
	}
	r.setMeta(MetaBinlogShippedAt, r.now().UTC().Format(time.RFC3339))

	fresh := r.newGaps(res.Gaps)
	if len(fresh) > 0 {
		r.logf(Binlogs, "binlogs missing before they were shipped: %s; taking a new full backup", strings.Join(fresh, ", "))
		subject, body := notify.FailureEmail(notify.Failure{
			Server:   r.Server,
			Headline: "binlogs missing: a gap in point-in-time restores",
			What:     "binlogs " + strings.Join(fresh, ", ") + " were removed from the MySQL server before liops shipped them",
			Reason:   "MySQL deletes binlogs after binlog_expire_logs_seconds, or someone purged or reset them, while shipping wasn't working",
			Safe:     "restores to moments before the gap still work, and so will restores after the new full backup liops is taking now",
			ToDo:     "check that binlog_expire_logs_seconds is at least 7 days, and look for earlier shipping failures in: sudo liops logs",
		})
		r.send(ctx, Binlogs, subject, body)
		r.setMeta(metaBinlogCatchUp, r.now().UTC().Format(time.RFC3339))
	}
	if r.meta(metaBinlogCatchUp) == "" {
		return nil
	}
	if r.skipCatchUp() {
		r.setMeta(metaBinlogCatchUp, "")
		r.setMeta(metaBinlogCatchUpTried, "")
		return nil
	}
	if !r.catchUpDue() {
		return nil
	}
	r.setMeta(metaBinlogCatchUpTried, r.now().UTC().Format(time.RFC3339))
	unlock()
	unlock = nil
	if r.skipCatchUp() {
		r.setMeta(metaBinlogCatchUp, "")
		r.setMeta(metaBinlogCatchUpTried, "")
		return nil
	}
	if err := r.Run(ctx, Full, Automatic); err != nil {
		return err
	}
	r.setMeta(metaBinlogCatchUp, "")
	r.setMeta(metaBinlogCatchUpTried, "")
	return nil
}

// skipCatchUp reports whether a full backup has already finished since the gap.
func (r *Runner) skipCatchUp() bool {
	gapAt, err := time.Parse(time.RFC3339, r.meta(metaBinlogCatchUp))
	if err != nil {
		return false // legacy "1" or unparseable: still try (with backoff)
	}
	list, err := r.XtraBackup.Backups(context.Background())
	if err != nil {
		return false
	}
	last := xtrabackup.Last(list, xtrabackup.Full)
	return last != nil && !last.Stop.Before(gapAt)
}

func (r *Runner) catchUpDue() bool {
	v := r.meta(metaBinlogCatchUpTried)
	if v == "" {
		return true
	}
	tried, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return true
	}
	return !r.now().Before(tried.Add(catchUpEvery))
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
	if res, err = r.XtraBackup.ShipBinlogs(ctx, backups); err != nil {
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
