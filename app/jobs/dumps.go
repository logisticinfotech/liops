// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/dumps"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
)

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// dumps is layer 2: discover the databases, dump each one to storage, then apply
// retention and handle databases that appeared or disappeared.
func (r *Runner) dumps(ctx context.Context, trigger string, run *state.Run) error {
	var output strings.Builder
	defer func() { run.Output = command.LastLines(output.String(), outputLines) }()
	note := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		output.WriteString(msg + "\n")
		r.logf(Dumps, "%s", msg)
	}

	dbs, err := r.Dumps.Discover(ctx)
	if err != nil {
		subject, body := notify.FailureEmail(notify.Failure{
			Server:   r.Server,
			Headline: tonight(trigger) + "database dumps FAILED",
			What:     "finding the list of databases to dump",
			Safe:     "point-in-time backups are not affected, and earlier dumps are kept",
			ToDo:     "check the details below, then run: sudo liops run dumps",
			Output:   errOutput(err),
		})
		r.send(ctx, Dumps, subject, body)
		return fmt.Errorf("discover databases: %w", err)
	}
	note("found %d database(s) to dump", len(dbs))

	labels, err := r.Dumps.Labels(ctx)
	if err != nil {
		// Labels are cosmetic: carry on without them.
		note("warning: %v (dumps continue without labels)", err)
	}

	// What was in storage before tonight, to spot databases dumped for the first time.
	before, listErr := r.Store.List(ctx, storage.DumpsPrefix)
	if listErr != nil {
		note("warning: %v (retention is skipped tonight)", listErr)
	}
	previous := dumps.Group(before)

	total := len(dbs) + 1 // +1 for globals
	r.setProgress(Progress{Kind: Dumps, Label: "Database dumps", Detail: "Starting…", Total: total})
	defer r.clearProgress()

	run.Dumps = r.dumpAll(ctx, dbs, labels)
	var failed []state.DumpRun
	for _, d := range run.Dumps {
		run.Size += d.Size
		if d.Error != "" {
			failed = append(failed, d)
			note("%s: FAILED: %s", notify.Name(d.DB, d.Label), d.Error)
		} else {
			note("%s: %s in %s", notify.Name(d.DB, d.Label), notify.Bytes(d.Size), notify.Duration(d.Duration))
		}
	}

	globalsLabel, globalsDetail := r.globals()
	r.bumpProgress(len(dbs), globalsLabel+"…")
	var problems []error
	if _, size, err := r.Dumps.DumpGlobals(ctx, r.now()); err != nil {
		note("%s: FAILED: %v", globalsDetail, err)
		problems = append(problems, fmt.Errorf("globals: %w", err))
	} else {
		run.Size += size
	}
	r.bumpProgress(total, "Finishing…")

	// First dumps ever (e.g. right after setup): don't send an email per database.
	if listErr == nil && len(previous) > 0 {
		for _, d := range run.Dumps {
			if d.Error == "" && len(previous[d.DB]) == 0 {
				subject, body := notify.NewDatabase(r.Server, d.DB, d.Label, d.Size)
				r.send(ctx, Dumps, subject, body)
			}
		}
	}

	if listErr == nil {
		if err := r.retention(ctx, dbs, labels, note); err != nil {
			problems = append(problems, err)
		}
	} else {
		problems = append(problems, listErr)
	}

	if len(failed) > 0 || len(problems) > 0 {
		r.dumpFailureEmail(ctx, trigger, failed, problems, previous, output.String())
	}
	if len(failed) > 0 {
		problems = append([]error{fmt.Errorf("%d of %d database dump(s) failed", len(failed), len(dbs))}, problems...)
	}
	return errors.Join(problems...)
}

// dumpAll dumps each database, dumps.concurrency at a time.
func (r *Runner) dumpAll(ctx context.Context, dbs []string, labels map[string]string) []state.DumpRun {
	results := make([]state.DumpRun, len(dbs))
	sem := make(chan struct{}, max(1, r.Cfg.Dumps.Concurrency))
	var wg sync.WaitGroup
	var doneMu sync.Mutex
	done := 0
	dbTotal := len(dbs)
	for i, db := range dbs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			start := r.now()
			key, size, err := r.Dumps.Dump(ctx, db, start)
			res := state.DumpRun{DB: db, Label: labels[db], Duration: r.now().Sub(start)}
			if err != nil {
				res.Error = firstLine(err.Error())
				r.logOutput(Dumps, []byte(errOutput(err)))
			} else {
				res.Key, res.Size = key, size
			}
			results[i] = res
			doneMu.Lock()
			done++
			n := done
			doneMu.Unlock()
			r.bumpProgress(n, fmt.Sprintf("%s (%d of %d)", notify.Name(db, labels[db]), n, dbTotal))
		}()
	}
	wg.Wait()
	return results
}

// retention deletes old dumps and handles databases that disappeared or came back.
func (r *Runner) retention(ctx context.Context, live []string, labels map[string]string, note func(string, ...any)) error {
	objs, err := r.Store.List(ctx, storage.DumpsPrefix)
	if err != nil {
		return err
	}
	deleted, err := r.State.Deleted()
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	now := r.now()
	plan := dumps.PlanRetention(dumps.RetentionInput{
		Stored:       dumps.Group(objs),
		Live:         live,
		DeletedSince: deleted,
		Now:          now,
		Keep:         days(r.Cfg.Dumps.RetentionDays),
		KeepDeleted:  days(r.Cfg.Dumps.DeletedRetentionDays),
	})

	var errs []error
	globals, err := r.Store.List(ctx, storage.GlobalsPrefix)
	if err != nil {
		errs = append(errs, err)
	}
	toDelete := append(plan.Delete, dumps.PlanGlobalsRetention(globals, now, days(r.Cfg.Dumps.RetentionDays))...)
	for _, key := range toDelete {
		if err := r.Store.Delete(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	if len(toDelete) > 0 {
		note("retention: deleted %d old dump(s)", len(toDelete))
	}

	for _, db := range plan.NewlyDeleted {
		if err := r.State.SetDeleted(db, now); err != nil {
			errs = append(errs, fmt.Errorf("state: %w", err))
			continue
		}
		keptUntil := now.Add(days(r.Cfg.Dumps.DeletedRetentionDays))
		note("%s is no longer found; its newest dump is kept until %s", db, keptUntil.Format("2 Jan"))
		subject, body := notify.DeletedDatabase(r.Server, db, labels[db], keptUntil)
		r.send(ctx, Dumps, subject, body)
	}
	for _, db := range plan.Reappeared {
		note("%s is back; it's dumped again", db)
		if err := r.State.ClearDeleted(db); err != nil {
			errs = append(errs, fmt.Errorf("state: %w", err))
		}
	}
	for _, db := range plan.Forgotten {
		note("%s: its last dump was deleted after the deleted-database retention", db)
		if err := r.State.ClearDeleted(db); err != nil {
			errs = append(errs, fmt.Errorf("state: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) dumpFailureEmail(ctx context.Context, trigger string, failed []state.DumpRun, problems []error, previous map[string][]dumps.Dump, output string) {
	var headline, what, safe string
	var details strings.Builder
	switch {
	case len(failed) > 0:
		names := make([]string, len(failed))
		for i, d := range failed {
			names[i] = notify.Name(d.DB, d.Label)
		}
		plural := "database"
		if len(failed) > 1 {
			plural = "databases"
		}
		headline = fmt.Sprintf("%sdatabase dumps FAILED for %d %s", tonight(trigger), len(failed), plural)
		what = "dump of " + strings.Join(names, ", ")
		safe = "point-in-time backups are not affected"
		if prev := previous[failed[0].DB]; len(failed) == 1 && len(prev) > 0 {
			safe += "; " + failed[0].DB + "'s last good dump is from " + prev[len(prev)-1].Taken.In(r.location()).Format("2 Jan")
		} else {
			safe += "; earlier dumps are kept"
		}
	default:
		headline = tonight(trigger) + "dump cleanup FAILED"
		what = "storing roles or deleting old dumps (every database was dumped)"
		if r.Cfg.IsMySQL() {
			what = "storing users and grants or deleting old dumps (every database was dumped)"
		}
		safe = "all databases were dumped tonight"
	}
	details.WriteString(output)
	for _, p := range problems {
		details.WriteString(p.Error() + "\n")
	}
	subject, body := notify.FailureEmail(notify.Failure{
		Server:   r.Server,
		Headline: headline,
		What:     what,
		Safe:     safe,
		ToDo:     "check the details below, then run: sudo liops run dumps",
		Output:   details.String(),
	})
	r.send(ctx, Dumps, subject, body)
}

// errOutput returns the command output carried by err, or err's text.
func errOutput(err error) string {
	if out := command.OutputOf(err); out != "" {
		return out
	}
	return err.Error()
}
