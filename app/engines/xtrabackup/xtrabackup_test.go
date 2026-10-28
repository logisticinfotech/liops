// SPDX-License-Identifier: Apache-2.0

package xtrabackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeXtraBackup writes what XtraBackup writes: the stream on stdout, and its
// checkpoints and info file into --extra-lsndir. Each backup ends at a higher LSN.
type fakeXtraBackup struct {
	lsn    uint64
	args   [][]string
	fail   bool
	noInfo bool
}

func (f *fakeXtraBackup) handle(program string, c command.Cmd) ([]byte, error) {
	if program != "xtrabackup" {
		return nil, fmt.Errorf("unexpected program %s", program)
	}
	f.args = append(f.args, c.Args)
	if f.fail {
		return []byte("partial"), errors.New("xtrabackup: Error: failed to connect to the server")
	}
	var dir string
	from := uint64(0)
	for _, a := range c.Args {
		if v, ok := strings.CutPrefix(a, "--extra-lsndir="); ok {
			dir = v
		}
		if v, ok := strings.CutPrefix(a, "--incremental-lsn="); ok {
			fmt.Sscan(v, &from)
		}
	}
	f.lsn += 1000
	typ := "full-backuped"
	if from > 0 {
		typ = "incremental"
	}
	os.WriteFile(filepath.Join(dir, "xtrabackup_checkpoints"),
		fmt.Appendf(nil, "backup_type = %s\nfrom_lsn = %d\nto_lsn = %d\nlast_lsn = %d\n", typ, from, f.lsn, f.lsn+10), 0o600)
	info := "tool_version = 8.4.0-7\nserver_version = 8.4.11-0ubuntu0.26.04.1\n"
	if !f.noInfo {
		info += "binlog_pos = filename 'binlog.000008', position '198', GTID of the last change 'a102517a-d2e9-11f1-b24f-525400707196:1-2,\nb203628b-d2e9-11f1-b24f-525400707196:1-7'\n"
	}
	os.WriteFile(filepath.Join(dir, "xtrabackup_info"), []byte(info), 0o600)
	return []byte("XBSTREAM-DATA"), nil
}

func newEngine(t *testing.T, f *fakeXtraBackup) (*Engine, *storage.Memory) {
	cfg := config.Default()
	cfg.Engine = config.EngineMySQL
	store := storage.NewMemory()
	e := New(cfg, &command.Fake{Handle: f.handle}, store, t.TempDir())
	clock := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)
	e.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	return e, store
}

func TestFullThenDiff(t *testing.T) {
	f := &fakeXtraBackup{}
	e, store := newEngine(t, f)
	ctx := context.Background()

	full, err := e.Backup(ctx, Full)
	if err != nil {
		t.Fatal(err)
	}
	if full.Label != "2026-10-25T013100Z-full" || full.ToLSN != 1000 || full.BinlogFile != "binlog.000008" || full.BinlogPos != 198 {
		t.Errorf("full = %+v", full)
	}
	if full.GTIDs != "a102517a-d2e9-11f1-b24f-525400707196:1-2,b203628b-d2e9-11f1-b24f-525400707196:1-7" || full.ToolVersion != "8.4.0-7" {
		t.Errorf("GTIDs %q, version %q", full.GTIDs, full.ToolVersion)
	}
	if data, ok := store.Get(full.Key(StreamFile)); !ok || string(data) != "XBSTREAM-DATA" || full.Size != int64(len(data)) {
		t.Errorf("stream = %q (ok=%v), size %d", data, ok, full.Size)
	}
	args := strings.Join(f.args[0], " ")
	for _, want := range []string{"--defaults-extra-file=", "--backup", "--stream=xbstream", "--compress=zstd"} {
		if !strings.Contains(args, want) {
			t.Errorf("xtrabackup args %q are missing %q", args, want)
		}
	}

	diff, err := e.Backup(ctx, Diff)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Type != Diff || diff.Base != full.Label || diff.FromLSN != 1000 {
		t.Errorf("diff = %+v", diff)
	}
	if !slices.Contains(f.args[1], "--incremental-lsn=1000") {
		t.Errorf("a diff must start from the full backup's LSN: %q", f.args[1])
	}

	list, err := e.Backups(ctx)
	if err != nil || len(list) != 2 || list[0].Label != full.Label || Last(list, "").Label != diff.Label || Last(list, Full).Label != full.Label {
		t.Errorf("Backups = %+v, %v", list, err)
	}
	var stored Manifest
	data, _ := store.Get(diff.Key(ManifestFile))
	if err := json.Unmarshal(data, &stored); err != nil || stored.Base != full.Label {
		t.Errorf("stored manifest %s: %v", data, err)
	}
}

func TestDiffWithoutAFullIsAFull(t *testing.T) {
	e, _ := newEngine(t, &fakeXtraBackup{})
	m, err := e.Backup(context.Background(), Diff)
	if err != nil || m.Type != Full || m.Base != "" || !strings.HasSuffix(m.Label, "-full") {
		t.Errorf("first diff = %+v, %v", m, err)
	}
}

func TestFailedBackupLeavesNothing(t *testing.T) {
	for name, f := range map[string]*fakeXtraBackup{
		"xtrabackup fails":   {fail: true},
		"no binlog position": {noInfo: true},
	} {
		e, store := newEngine(t, f)
		if _, err := e.Backup(context.Background(), Full); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
			t.Errorf("%s: a failed backup must leave nothing behind: %v", name, objs)
		}
	}
}

func TestBackupsIgnoresIncompleteFolders(t *testing.T) {
	e, store := newEngine(t, &fakeXtraBackup{})
	store.Add(Prefix+"2026-10-24T013000Z-full/"+StreamFile, []byte("cut short"), time.Now())
	m, _ := e.Backup(context.Background(), Diff)
	if m.Type != Full {
		t.Errorf("a folder without a manifest isn't a backup to take a diff against: %+v", m)
	}
	if list, _ := e.Backups(context.Background()); len(list) != 1 {
		t.Errorf("Backups = %+v", list)
	}
}
