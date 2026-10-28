// SPDX-License-Identifier: Apache-2.0

package xtrabackup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// restoreBackups: a full 8 days ago (binlog 1), diffs 7 and 6 days ago (binlogs 3, 5),
// and a full 1 day ago (binlog 9).
func restoreBackups() []Manifest {
	at := func(daysAgo int) time.Time { return now.Add(-days(daysAgo)) }
	mk := func(typ string, daysAgo, binlog int, base string) Manifest {
		return Manifest{Label: at(daysAgo).Format(labelTime) + "-" + typ, Type: typ, Start: at(daysAgo).Add(-time.Minute), Stop: at(daysAgo),
			Base: base, BinlogFile: fmt.Sprintf("binlog.%06d", binlog), BinlogPos: uint64(100 + binlog)}
	}
	full1 := mk(Full, 8, 1, "")
	full2 := mk(Full, 1, 9, "")
	return []Manifest{full1, mk(Diff, 7, 3, full1.Label), mk(Diff, 6, 5, full1.Label), full2}
}

func binlogNames(from, to int) []string {
	var names []string
	for n := from; n <= to; n++ {
		names = append(names, fmt.Sprintf("binlog.%06d", n))
	}
	return names
}

func TestPlanRestore(t *testing.T) {
	backups := restoreBackups()
	binlogs := binlogNames(1, 10)

	// Six and a half days ago: the first full, its newer diff, binlogs from 5 on.
	plan, err := PlanRestore(backups, binlogs, now.Add(-days(6)).Add(12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Full.Label != backups[0].Label || plan.Diff == nil || plan.Diff.Label != backups[2].Label {
		t.Errorf("full %s, diff %+v", plan.Full.Label, plan.Diff)
	}
	if !slices.Equal(plan.Binlogs, binlogNames(5, 10)) || plan.Start().BinlogPos != 105 {
		t.Errorf("binlogs %q, start %d", plan.Binlogs, plan.Start().BinlogPos)
	}

	// The latest moment: the newest full, no diff, binlogs from 9 on.
	plan, err = PlanRestore(backups, binlogs, time.Time{})
	if err != nil || plan.Full.Label != backups[3].Label || plan.Diff != nil || !slices.Equal(plan.Binlogs, binlogNames(9, 10)) {
		t.Errorf("latest: %+v, %v", plan, err)
	}

	// Before the oldest backup: refused, saying how far back a restore can go.
	if _, err := PlanRestore(backups, binlogs, now.Add(-days(9))); err == nil || !strings.Contains(err.Error(), "can go back to") {
		t.Errorf("too early: %v", err)
	}
	if _, err := PlanRestore(nil, nil, time.Time{}); err == nil {
		t.Error("no backups: want an error")
	}
}

func TestPlanRestoreRefusesABinlogGap(t *testing.T) {
	binlogs := slices.DeleteFunc(binlogNames(1, 10), func(n string) bool { return n == "binlog.000007" })
	_, err := PlanRestore(restoreBackups(), binlogs, now.Add(-days(6)).Add(12*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "binlog.000007") {
		t.Errorf("want a gap error naming binlog.000007, got %v", err)
	}
	// After the next full backup, the gap doesn't matter.
	if _, err := PlanRestore(restoreBackups(), binlogs, time.Time{}); err != nil {
		t.Errorf("a restore after the gap should work: %v", err)
	}
}

func TestFetch(t *testing.T) {
	store := storage.NewMemory()
	full := Manifest{Label: "2026-10-20T013000Z-full", Type: Full}
	diff := Manifest{Label: "2026-10-21T013000Z-diff", Type: Diff, Base: full.Label}
	store.Add(full.Key(StreamFile), []byte("FULL-STREAM"), now)
	store.Add(diff.Key(StreamFile), []byte("DIFF-STREAM"), now)
	for _, b := range []string{"binlog.000003", "binlog.000004"} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte("events of " + b))
		zw.Close()
		store.Add(BinlogPrefix+b+".gz", buf.Bytes(), now)
	}

	var calls []string
	fake := &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		if program == "xbstream" {
			data, _ := io.ReadAll(c.Stdin)
			os.WriteFile(filepath.Join(c.Args[2], "stream"), data, 0o600)
		}
		calls = append(calls, program+" "+strings.Join(c.Args, " "))
		return nil, nil
	}}
	cfg := config.Default()
	e := New(cfg, fake, store, t.TempDir())
	dir := t.TempDir()
	plan := RestorePlan{Full: full, Diff: &diff, Binlogs: []string{"binlog.000003", "binlog.000004"}}
	if err := e.Fetch(context.Background(), plan, dir, func(string) {}); err != nil {
		t.Fatal(err)
	}

	if data, _ := os.ReadFile(filepath.Join(dir, "full", "stream")); string(data) != "FULL-STREAM" {
		t.Errorf("the full backup should be streamed into xbstream, got %q", data)
	}
	want := []string{
		"xbstream -x -C " + dir + "/full",
		"xtrabackup --decompress --remove-original --target-dir=" + dir + "/full",
		"xbstream -x -C " + dir + "/diff",
		"xtrabackup --decompress --remove-original --target-dir=" + dir + "/diff",
		"xtrabackup --prepare --apply-log-only --target-dir=" + dir + "/full",
		"xtrabackup --prepare --apply-log-only --target-dir=" + dir + "/full --incremental-dir=" + dir + "/diff",
		"xtrabackup --prepare --target-dir=" + dir + "/full",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("commands:\n got %q\nwant %q", calls, want)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "binlogs", "binlog.000004")); string(data) != "events of binlog.000004" {
		t.Errorf("binlog.000004 = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "diff")); err == nil {
		t.Error("the applied diff should be removed")
	}
}

func TestRestoreCommands(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	diff := Manifest{Type: Diff, Stop: time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC), BinlogFile: "binlog.000003", BinlogPos: 198}
	plan := RestorePlan{
		Target:  time.Date(2026, 10, 22, 9, 4, 30, 0, time.UTC),
		Diff:    &diff,
		Binlogs: []string{"binlog.000003", "binlog.000004"},
	}
	out := RestoreCommands(plan, "/restore/new", "/var/lib/mysql", ist)
	for _, want := range []string{
		"sudo mv /var/lib/mysql /var/lib/mysql.before-restore",
		"sudo xtrabackup --copy-back --target-dir=/restore/new/full --datadir=/var/lib/mysql",
		"It is now as of 22 Oct 2026 01:30:00 IST",
		"sudo mysqlbinlog --start-position=198 --stop-datetime='2026-10-22 14:34:30' /restore/new/binlogs/binlog.000003 /restore/new/binlogs/binlog.000004 | sudo mysql",
		"new backup name",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("commands are missing %q:\n%s", want, out)
		}
	}
}
