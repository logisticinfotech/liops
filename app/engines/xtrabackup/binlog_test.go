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

// fakeBinlogs is a MySQL server's binlog directory: files on disk, the newest open.
type fakeBinlogs struct {
	dir      string
	files    []string // names on the server, oldest first; the last is being written
	flushed  int
	gtidHeld bool // GTID_SUBSET answer; default true
	noGTID   bool
}

func newFakeBinlogs(t *testing.T, names ...string) *fakeBinlogs {
	f := &fakeBinlogs{dir: t.TempDir(), files: names, gtidHeld: true}
	for _, n := range names {
		os.WriteFile(filepath.Join(f.dir, n), []byte("events of "+n), 0o600)
	}
	return f
}

func (f *fakeBinlogs) handle(_ string, c command.Cmd) ([]byte, error) {
	q := strings.TrimPrefix(c.Args[len(c.Args)-1], "--execute=")
	switch q {
	case "SHOW BINARY LOGS":
		var b strings.Builder
		for _, n := range f.files {
			fmt.Fprintf(&b, "%s\t%d\tNo\n", n, len("events of "+n))
		}
		return []byte(b.String()), nil
	case "SELECT @@log_bin_basename":
		return []byte(filepath.Join(f.dir, "binlog") + "\n"), nil
	case "SHOW BINARY LOG STATUS":
		return []byte(f.files[len(f.files)-1] + "\t4711\t\t\ta102517a-d2e9-11f1-b24f-525400707196:1-12\n"), nil
	case "FLUSH BINARY LOGS":
		f.flushed++
		last := f.files[len(f.files)-1]
		var n int
		fmt.Sscanf(last[strings.LastIndexByte(last, '.')+1:], "%d", &n)
		next := fmt.Sprintf("binlog.%06d", n+1)
		os.WriteFile(filepath.Join(f.dir, next), []byte("events of "+next), 0o600)
		f.files = append(f.files, next)
		return nil, nil
	}
	if strings.HasPrefix(q, "SELECT GTID_SUBSET(") {
		if f.noGTID {
			return nil, fmt.Errorf("GTID_SUBSET unknown")
		}
		if f.gtidHeld {
			return []byte("1\n"), nil
		}
		return []byte("0\n"), nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

func binlogEngine(f *fakeBinlogs) (*Engine, *storage.Memory) {
	cfg := config.Default()
	cfg.Engine = config.EngineMySQL
	store := storage.NewMemory()
	return New(cfg, &command.Fake{Handle: f.handle}, store, f.dir), store
}

func gunzip(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	return string(plain)
}

func shipFrom(file, gen string) []Manifest {
	return []Manifest{{Type: Full, Label: "full", BinlogFile: file, BinlogGen: gen, Start: now, Stop: now}}
}

func TestShipBinlogs(t *testing.T) {
	f := newFakeBinlogs(t, "binlog.000005", "binlog.000006", "binlog.000007", "binlog.000008")
	e, store := binlogEngine(f)
	ctx := context.Background()
	backups := shipFrom("binlog.000006", "gen1")

	// The oldest backup starts in binlog.000006: 000005 is of no use, 000008 is still open.
	res, err := e.ShipBinlogs(ctx, backups, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Shipped, []string{"binlog.000006", "binlog.000007"}) || len(res.Gaps) != 0 {
		t.Errorf("result = %+v", res)
	}
	data, ok := store.Get(Binlog{Name: "binlog.000007", Gen: "gen1"}.Key())
	if !ok || gunzip(t, data) != "events of binlog.000007" {
		t.Errorf("stored binlog.000007 = %q (ok=%v)", data, ok)
	}

	// Nothing new: nothing shipped. After a rotation, the closed file goes up.
	if res, _ := e.ShipBinlogs(ctx, backups, "", false); len(res.Shipped) != 0 {
		t.Errorf("second run shipped %q again", res.Shipped)
	}
	if err := e.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if res, _ := e.ShipBinlogs(ctx, backups, "", false); !slices.Equal(res.Shipped, []string{"binlog.000008"}) {
		t.Errorf("after a rotation, shipped %q", res.Shipped)
	}
	if names, _ := e.ShippedBinlogs(ctx); !slices.Equal(names, []Binlog{
		{Name: "binlog.000006", Gen: "gen1"},
		{Name: "binlog.000007", Gen: "gen1"},
		{Name: "binlog.000008", Gen: "gen1"},
	}) {
		t.Errorf("ShippedBinlogs = %+v", names)
	}
}

func TestShipBinlogsTreatsAResetAsAGap(t *testing.T) {
	// The oldest backup started in binlog.000050; RESET BINARY LOGS started a new chain.
	f := newFakeBinlogs(t, "binlog.000001", "binlog.000002")
	e, store := binlogEngine(f)
	res, err := e.ShipBinlogs(context.Background(), shipFrom("binlog.000050", "gen1"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Gaps, []string{"binlog.000050"}) {
		t.Errorf("gaps = %q", res.Gaps)
	}
	if len(res.Shipped) != 0 {
		t.Errorf("nothing of the new chain ships until a catch-up full: %+v", res)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("stored %v", objs)
	}
}

func TestShipBinlogsAfterCatchUpFullUsesANewGeneration(t *testing.T) {
	f := newFakeBinlogs(t, "binlog.000001", "binlog.000002")
	e, store := binlogEngine(f)
	ctx := context.Background()
	old := Manifest{Type: Full, Label: "old", BinlogFile: "binlog.000050", BinlogGen: "gen1", Start: now.Add(-time.Hour), Stop: now.Add(-time.Hour)}
	store.Add(Binlog{Name: "binlog.000050", Gen: "gen1"}.Key(), []byte("old"), now)
	catchUp := Manifest{Type: Full, Label: "new", BinlogFile: "binlog.000001", BinlogGen: "gen2", Start: now, Stop: now}

	res, err := e.ShipBinlogs(ctx, []Manifest{old, catchUp}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Shipped, []string{"binlog.000001"}) || len(res.Gaps) != 0 {
		t.Errorf("after catch-up: %+v", res)
	}
	if _, ok := store.Get(Binlog{Name: "binlog.000001", Gen: "gen2"}.Key()); !ok {
		t.Error("the new chain should be stored under its generation")
	}
	if _, ok := store.Get(Binlog{Name: "binlog.000001", Gen: "gen1"}.Key()); ok {
		t.Error("the new chain must not overwrite the old generation")
	}
}

func TestShipBinlogsFindsGaps(t *testing.T) {
	// binlog.000006 was purged from the server before it was shipped.
	f := newFakeBinlogs(t, "binlog.000007", "binlog.000008")
	e, _ := binlogEngine(f)
	res, err := e.ShipBinlogs(context.Background(), shipFrom("binlog.000005", ""), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Gaps, []string{"binlog.000005", "binlog.000006"}) || !slices.Equal(res.Shipped, []string{"binlog.000007"}) {
		t.Errorf("result = %+v", res)
	}
}

func TestShipBinlogsNeedsABackup(t *testing.T) {
	f := newFakeBinlogs(t, "binlog.000001", "binlog.000002")
	e, store := binlogEngine(f)
	if res, err := e.ShipBinlogs(context.Background(), nil, "", false); err != nil || len(res.Shipped) != 0 {
		t.Errorf("with no backup, nothing is shipped: %+v, %v", res, err)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("stored %v", objs)
	}
}

func TestShipBinlogsChecksTheSize(t *testing.T) {
	f := newFakeBinlogs(t, "binlog.000001", "binlog.000002")
	os.WriteFile(filepath.Join(f.dir, "binlog.000001"), []byte("cut"), 0o600) // shorter than MySQL says
	e, store := binlogEngine(f)
	if _, err := e.ShipBinlogs(context.Background(), shipFrom("binlog.000001", ""), "", false); err == nil || !strings.Contains(err.Error(), "MySQL says") {
		t.Errorf("want a size error, got %v", err)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("a short upload must not be kept: %v", objs)
	}
}

func TestAssignBinlogGen(t *testing.T) {
	start := now
	m := Manifest{BinlogFile: "binlog.000001", Start: start}
	assignBinlogGen(nil, &m, false)
	if m.BinlogGen != start.UTC().Format(labelTime) {
		t.Errorf("first backup gen = %q", m.BinlogGen)
	}
	prev := Manifest{BinlogFile: "binlog.000050", BinlogGen: "gen1"}
	m = Manifest{BinlogFile: "binlog.000001", Start: start}
	assignBinlogGen([]Manifest{prev}, &m, false)
	if m.BinlogGen == "gen1" || m.BinlogGen == "" {
		t.Errorf("a reset should start a new generation, got %q", m.BinlogGen)
	}
	m = Manifest{BinlogFile: "binlog.000051", Start: start}
	assignBinlogGen([]Manifest{prev}, &m, false)
	if m.BinlogGen != "gen1" {
		t.Errorf("the same chain keeps the generation, got %q", m.BinlogGen)
	}
	m = Manifest{BinlogFile: "binlog.000051", Start: start}
	assignBinlogGen([]Manifest{prev}, &m, true)
	if m.BinlogGen == "gen1" || m.BinlogGen == "" {
		t.Errorf("a sticky reset should start a new generation even when numbers went up, got %q", m.BinlogGen)
	}
}

func TestShipBinlogsPendingNewGenDoesNotShip(t *testing.T) {
	// Numbers have caught up past the backup's file, but a reset is still pending.
	f := newFakeBinlogs(t, "binlog.000003", "binlog.000004")
	e, store := binlogEngine(f)
	res, err := e.ShipBinlogs(context.Background(), shipFrom("binlog.000003", "gen1"), "binlog.000003", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NewGen || len(res.Shipped) != 0 {
		t.Errorf("pending reset must not ship: %+v", res)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("stored %v", objs)
	}
}

func TestShipBinlogsDetectsResetAgainstLastSeen(t *testing.T) {
	// Current 000004 is not below the backup's 000003, but it is below lastSeen 000008.
	f := newFakeBinlogs(t, "binlog.000003", "binlog.000004")
	e, _ := binlogEngine(f)
	res, err := e.ShipBinlogs(context.Background(), shipFrom("binlog.000003", "gen1"), "binlog.000008", false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NewGen || len(res.Shipped) != 0 {
		t.Errorf("lastSeen going backwards is a reset: %+v", res)
	}
}

func TestShipBinlogsDetectsLostGTIDs(t *testing.T) {
	f := newFakeBinlogs(t, "binlog.000008", "binlog.000009")
	f.gtidHeld = false
	e, _ := binlogEngine(f)
	backups := []Manifest{{Type: Full, Label: "full", BinlogFile: "binlog.000008", BinlogGen: "gen1",
		GTIDs: "a102517a-d2e9-11f1-b24f-525400707196:1-12", Start: now, Stop: now}}
	res, err := e.ShipBinlogs(context.Background(), backups, "binlog.000008", false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NewGen || len(res.Shipped) != 0 {
		t.Errorf("lost GTIDs are a reset: %+v", res)
	}
}

func TestBinlogBeforeIsNumeric(t *testing.T) {
	if !binlogBefore("binlog.9", "binlog.10") {
		t.Error("9 should be before 10 numerically (string compare would put 9 after 10)")
	}
	if binlogBefore("binlog.000010", "binlog.000002") {
		t.Error("10 should not be before 2")
	}
}

func TestBinlogStatus(t *testing.T) {
	e, _ := binlogEngine(newFakeBinlogs(t, "binlog.000003"))
	s, err := e.BinlogStatus(context.Background())
	if err != nil || s.Position() != "binlog.000003:4711" || s.GTIDs != "a102517a-d2e9-11f1-b24f-525400707196:1-12" {
		t.Errorf("status = %+v, %v", s, err)
	}
}
