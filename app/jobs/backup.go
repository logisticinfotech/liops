// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/notify"
	"github.com/logisticinfotech/pgtijori/app/state"
)

// backup runs a pgBackRest full or differential backup (layer 1).
func (r *Runner) backup(ctx context.Context, typ, trigger string, run *state.Run) error {
	out, err := r.Backrest.Backup(ctx, typ)
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
			ToDo:     "check the details below, then run: sudo pgtijori run " + typ,
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
