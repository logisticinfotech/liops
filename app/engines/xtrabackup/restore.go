// SPDX-License-Identifier: Apache-2.0

package xtrabackup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/notify"
)

// RestorePlan is what a restore to a moment needs: a full backup, maybe a diff on top
// of it, and the binlogs to replay from the backup's binlog position on.
type RestorePlan struct {
	Target  time.Time // zero: as late as the binlogs go
	Full    Manifest
	Diff    *Manifest
	Binlogs []Binlog // shipped binlogs of this generation, oldest first; the first is where replay starts
}

// Start is the backup the binlogs are replayed onto: the diff if there is one.
func (p RestorePlan) Start() Manifest {
	if p.Diff != nil {
		return *p.Diff
	}
	return p.Full
}

// PlanRestore picks what restoring to target needs (zero target: the latest moment):
// the newest full backup finished by then, the newest diff of that full finished by
// then, and every shipped binlog from that backup's binlog on. It refuses a target
// before the oldest backup, and a gap in the binlogs it needs.
func PlanRestore(backups []Manifest, binlogs []Binlog, target time.Time) (RestorePlan, error) {
	plan := RestorePlan{Target: target}
	before := func(m Manifest) bool { return target.IsZero() || !m.Stop.After(target) }

	var full *Manifest
	for i := range backups {
		if backups[i].Type == Full && before(backups[i]) {
			full = &backups[i]
		}
	}
	if full == nil {
		if len(backups) == 0 {
			return plan, fmt.Errorf("there are no backups to restore from yet")
		}
		return plan, fmt.Errorf("the oldest backup finished at %s: a restore can go back to then, not earlier",
			backups[0].Stop.Format(time.RFC3339))
	}
	plan.Full = *full
	for i := range backups {
		if backups[i].Type == Diff && backups[i].Base == full.Label && before(backups[i]) {
			plan.Diff = &backups[i]
		}
	}

	start := plan.Start()
	for _, b := range binlogs {
		if b.Gen != start.BinlogGen {
			continue
		}
		if !binlogBefore(b.Name, start.BinlogFile) {
			plan.Binlogs = append(plan.Binlogs, b)
		}
	}
	if len(plan.Binlogs) == 0 {
		return plan, nil // nothing shipped since the backup: the restore ends at the backup
	}
	want, err := binlogRange(start.BinlogFile, nextBinlog(plan.Binlogs[len(plan.Binlogs)-1].Name))
	if err != nil {
		return plan, err
	}
	var missing []string
	for _, name := range want {
		found := false
		for _, b := range plan.Binlogs {
			found = found || b.Name == name
		}
		if !found {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return plan, fmt.Errorf("binlogs %s are missing from storage, so changes can't be replayed past %s; "+
			"pick an earlier moment, or one after a later backup", strings.Join(missing, ", "), missing[0])
	}
	return plan, nil
}

// nextBinlog returns the name after name: binlog.000007 → binlog.000008.
func nextBinlog(name string) string {
	base, n, width, err := splitBinlogName(name)
	if err != nil {
		return name
	}
	return fmt.Sprintf("%s.%0*d", base, width, n+1)
}

// Fetch downloads and prepares everything plan needs into dir (which must exist and be
// empty): dir/full, ready to copy back into MySQL's data directory, and dir/binlogs. It
// never touches the running server. progress gets a line per step.
func (e *Engine) Fetch(ctx context.Context, plan RestorePlan, dir string, progress func(string)) error {
	backups := []struct {
		m   Manifest
		sub string
	}{{plan.Full, "full"}}
	if plan.Diff != nil {
		backups = append(backups, struct {
			m   Manifest
			sub string
		}{*plan.Diff, "diff"})
	}
	run := func(c command.Cmd) error {
		if out, err := e.Instance.Run.Run(ctx, c); err != nil {
			return fmt.Errorf("%w\n%s", err, command.LastLines(string(out), 20))
		}
		return nil
	}
	xtrabackup := func(args ...string) error { return run(command.Cmd{Name: e.Instance.Bin("xtrabackup"), Args: args}) }

	for _, b := range backups {
		target := filepath.Join(dir, b.sub)
		if err := os.Mkdir(target, 0o700); err != nil {
			return err
		}
		progress(fmt.Sprintf("Downloading the %s backup %s (%s)…", b.m.Type, b.m.Label, notify.Bytes(b.m.Size)))
		r, err := e.Store.Read(ctx, b.m.Key(StreamFile))
		if err != nil {
			return err
		}
		err = run(command.Cmd{Name: e.Instance.Bin("xbstream"), Args: []string{"-x", "-C", target}, Stdin: r})
		r.Close()
		if err != nil {
			return fmt.Errorf("unpack %s: %w", b.m.Label, err)
		}
		if err := xtrabackup("--decompress", "--remove-original", "--target-dir="+target); err != nil {
			return fmt.Errorf("decompress %s: %w", b.m.Label, err)
		}
	}

	full := filepath.Join(dir, "full")
	if plan.Diff != nil {
		progress("Preparing the full backup, then applying the diff…")
		if err := xtrabackup("--prepare", "--apply-log-only", "--target-dir="+full); err != nil {
			return fmt.Errorf("prepare the full backup: %w", err)
		}
		if err := xtrabackup("--prepare", "--apply-log-only", "--target-dir="+full, "--incremental-dir="+filepath.Join(dir, "diff")); err != nil {
			return fmt.Errorf("apply the diff: %w", err)
		}
	} else {
		progress("Preparing the full backup…")
	}
	if err := xtrabackup("--prepare", "--target-dir="+full); err != nil {
		return fmt.Errorf("prepare the backup: %w", err)
	}
	if plan.Diff != nil {
		if err := os.RemoveAll(filepath.Join(dir, "diff")); err != nil { // applied: no longer needed
			return err
		}
	}

	if len(plan.Binlogs) > 0 {
		progress(fmt.Sprintf("Downloading %d binlog(s)…", len(plan.Binlogs)))
		binlogs := filepath.Join(dir, "binlogs")
		if err := os.Mkdir(binlogs, 0o700); err != nil {
			return err
		}
		for _, b := range plan.Binlogs {
			if err := e.fetchBinlog(ctx, b, filepath.Join(binlogs, b.Name)); err != nil {
				return fmt.Errorf("download %s: %w", b.Name, err)
			}
		}
	}
	return nil
}

func (e *Engine) fetchBinlog(ctx context.Context, b Binlog, path string) error {
	r, err := e.Store.Read(ctx, b.Key())
	if err != nil {
		return err
	}
	defer r.Close()
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, zr); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RestoreCommands returns the commands that finish a restore from dir, for a person to
// run: they replace MySQL's data and replay the binlogs. loc is the timezone mysqlbinlog
// reads --stop-datetime in: the server's own.
func RestoreCommands(plan RestorePlan, dir, dataDir string, loc *time.Location) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	aside := dataDir + ".before-restore"
	w("# 1. Stop MySQL and liops, and keep the current data aside (nothing is deleted).")
	w("sudo systemctl stop liops mysql")
	w("sudo mv %s %s", shellQuote(dataDir), shellQuote(aside))
	w("")
	w("# 2. Put the backup in its place, and start MySQL. It is now as of %s.", plan.Start().Stop.In(loc).Format("2 Jan 2006 15:04:05 MST"))
	w("sudo xtrabackup --copy-back --target-dir=%s --datadir=%s", shellQuote(filepath.Join(dir, "full")), shellQuote(dataDir))
	w("sudo chown -R mysql:mysql %s", shellQuote(dataDir))
	w("sudo systemctl start mysql")
	if len(plan.Binlogs) > 0 {
		w("")
		until := "the newest change shipped"
		stop := ""
		if !plan.Target.IsZero() {
			until = plan.Target.In(loc).Format("2 Jan 2006 15:04:05 MST")
			stop = " --stop-datetime=" + shellQuote(plan.Target.In(loc).Format("2006-01-02 15:04:05"))
		}
		files := make([]string, len(plan.Binlogs))
		for i, n := range plan.Binlogs {
			files[i] = shellQuote(filepath.Join(dir, "binlogs", n.Name))
		}
		w("# 3. Replay the changes made after the backup, up to %s.", until)
		w("#    To skip one bad statement instead (e.g. a DROP TABLE), add --exclude-gtids='<its GTID>'.")
		w("sudo mysqlbinlog --start-position=%d%s %s | sudo mysql", plan.Start().BinlogPos, stop, strings.Join(files, " "))
	}
	w("")
	w("# 4. Check the data. Then give the restored server a new backup name (mysql.backup_name in")
	w("#    /etc/liops/config.yaml), and start liops again: its binlogs would collide with the old")
	w("#    ones, so setup refuses the old name. The old backups stay where they are.")
	w("sudo liops setup && sudo liops run full && sudo systemctl start liops")
	w("#    When you're sure: sudo rm -rf %s %s", shellQuote(aside), shellQuote(dir))
	return b.String()
}

// shellQuote quotes s for a shell, when it needs quoting.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-:") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
