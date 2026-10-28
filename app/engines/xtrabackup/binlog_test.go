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

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// fakeBinlogs is a MySQL server's binlog directory: files on disk, the newest open.
type fakeBinlogs struct {
	dir     string
	files   []string // names on the server, oldest first; the last is being written
	flushed int
}

func newFakeBinlogs(t *testing.T, names ...string) *fakeBinlogs {
	f := &fakeBinlogs{dir: t.TempDir(), files: names}
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

func TestShipBinlogs(t *testing.T) {
	f := newFakeBinlogs(t, "binlog.000005", "binlog.000006", "binlog.000007", "binlog.000008")
	e, store := binlogEngine(f)
	ctx := context.Background()

	// The oldest backup starts in binlog.000006: 000005 is of no use, 000008 is still open.
	res, err := e.ShipBinlogs(ctx, "binlog.000006")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Shipped, []string{"binlog.000006", "binlog.000007"}) || len(res.Gaps) != 0 {
		t.Errorf("result = %+v", res)
	}
	data, ok := store.Get(BinlogPrefix + "binlog.000007.gz")
	if !ok || gunzip(t, data) != "events of binlog.000007" {
		t.Errorf("stored binlog.000007 = %q (ok=%v)", data, ok)
	}

	// Nothing new: nothing shipped. After a rotation, the closed file goes up.
	if res, _ := e.ShipBinlogs(ctx, "binlog.000006"); len(res.Shipped) != 0 {
		t.Errorf("second run shipped %q again", res.Shipped)
	}
	if err := e.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if res, _ := e.ShipBinlogs(ctx, "binlog.000006"); !slices.Equal(res.Shipped, []string{"binlog.000008"}) {
		t.Errorf("after a rotation, shipped %q", res.Shipped)
	}
	if names, _ := e.ShippedBinlogs(ctx); !slices.Equal(names, []string{"binlog.000006", "binlog.000007", "binlog.000008"}) {
		t.Errorf("ShippedBinlogs = %q", names)
	}
}

func TestShipBinlogsFindsGaps(t *testing.T) {
	// binlog.000006 was purged from the server before it was shipped.
	f := newFakeBinlogs(t, "binlog.000007", "binlog.000008")
	e, _ := binlogEngine(f)
	res, err := e.ShipBinlogs(context.Background(), "binlog.000005")
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
	if res, err := e.ShipBinlogs(context.Background(), ""); err != nil || len(res.Shipped) != 0 {
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
	if _, err := e.ShipBinlogs(context.Background(), "binlog.000001"); err == nil || !strings.Contains(err.Error(), "MySQL says") {
		t.Errorf("want a size error, got %v", err)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("a short upload must not be kept: %v", objs)
	}
}

func TestBinlogStatus(t *testing.T) {
	e, _ := binlogEngine(newFakeBinlogs(t, "binlog.000003"))
	s, err := e.BinlogStatus(context.Background())
	if err != nil || s.Position() != "binlog.000003:4711" || s.GTIDs != "a102517a-d2e9-11f1-b24f-525400707196:1-12" {
		t.Errorf("status = %+v, %v", s, err)
	}
}
