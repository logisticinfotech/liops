// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/xtrabackup"
	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// runRestoreFiles prepares a MySQL point-in-time restore in an empty directory: it
// downloads and prepares the backups and binlogs the moment needs, then prints the
// commands that finish the restore. It never touches the running server: replacing a
// live database is a person's decision (docs/restore-mysql.md).
func runRestoreFiles(args []string, out io.Writer) error {
	flags, p := newFlags("restore-files")
	to := flags.String("to", "", `the moment to restore to, e.g. "2026-10-28 14:05" (server.timezone); empty: as late as possible`)
	dir := flags.String("dir", "", "an empty directory, with room for the whole database")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := requireRoot("liops restore-files"); err != nil {
		return err
	}
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	if !cfg.IsMySQL() {
		return errors.New("restore-files is for MySQL; a PostgreSQL server restores with pgbackrest restore (see docs/restore.md)")
	}
	target, err := parseMoment(*to, cfg)
	if err != nil {
		return err
	}
	if err := emptyRestoreDir(*dir, cfg.MySQL.DataDir); err != nil {
		return err
	}

	store, err := storage.Open(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	x := xtrabackup.New(cfg, command.Exec{}, store, "")
	backups, err := x.Backups(ctx)
	if err != nil {
		return err
	}
	binlogs, err := x.ShippedBinlogs(ctx)
	if err != nil {
		return err
	}
	plan, err := xtrabackup.PlanRestore(backups, binlogs, target)
	if err != nil {
		return err
	}

	// mysqlbinlog --stop-datetime is the OS timezone of this machine, not server.timezone.
	loc := time.Local
	moment := "the newest change shipped"
	if !target.IsZero() {
		moment = target.In(loc).Format("2 Jan 2006 15:04:05 MST")
	}
	fmt.Fprintf(out, "Restoring MySQL to %s, from:\n", moment)
	fmt.Fprintf(out, "  full backup %s (finished %s)\n", plan.Full.Label, plan.Full.Stop.In(loc).Format("2 Jan 15:04"))
	if plan.Diff != nil {
		fmt.Fprintf(out, "  diff %s (finished %s)\n", plan.Diff.Label, plan.Diff.Stop.In(loc).Format("2 Jan 15:04"))
	}
	if n := len(plan.Binlogs); n > 0 {
		fmt.Fprintf(out, "  %d binlog(s), %s to %s\n", n, plan.Binlogs[0].Name, plan.Binlogs[n-1].Name)
	} else {
		fmt.Fprintln(out, "  no binlogs: nothing was shipped after this backup, so the restore ends at the backup")
	}
	if st, err := state.Open(p.state); err == nil {
		if v, _ := st.GetMeta(jobs.MetaBinlogShippedAt); v != "" {
			if shipped, err := time.Parse(time.RFC3339, v); err == nil && (target.IsZero() || target.After(shipped)) {
				fmt.Fprintf(out, "Binlogs were last shipped %s: changes after that can't be restored from storage.\n", notify.When(shipped, time.Now().In(loc)))
			}
		}
	}
	fmt.Fprintln(out)

	if err := x.Fetch(ctx, plan, *dir, func(line string) { fmt.Fprintln(out, line) }); err != nil {
		return fmt.Errorf("%w\nThe files so far are in %s; remove it before trying again", err, *dir)
	}
	fmt.Fprintf(out, "\nThe files are ready in %s. liops hasn't changed MySQL. To finish the restore, which\n"+
		"replaces this server's data with it (see docs/restore-mysql.md):\n\n", *dir)
	fmt.Fprint(out, xtrabackup.RestoreCommands(plan, *dir, cfg.MySQL.DataDir, loc))
	return nil
}

// parseMoment reads a --to value in the schedule timezone. Empty is the zero time.
func parseMoment(s string, cfg *config.Config) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	loc, err := cfg.Location()
	if err != nil {
		return time.Time{}, err
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(`--to %q: give a moment like "2026-10-28 14:05" or "2026-10-28 14:05:30" (in %s)`, s, loc)
}

// emptyRestoreDir makes sure dir can take a restore: an absolute path outside MySQL's
// data directory (which the restore moves aside) that is empty or doesn't exist yet.
func emptyRestoreDir(dir, dataDir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return errors.New("--dir: give an absolute path to an empty directory, with room for the whole database")
	}
	dir, dataDir = filepath.Clean(dir), filepath.Clean(dataDir)
	if dir == dataDir || strings.HasPrefix(dir, dataDir+"/") {
		return fmt.Errorf("--dir must be outside %s, which the restore moves aside", dataDir)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("--dir %s isn't empty", dir)
	}
	return nil
}
