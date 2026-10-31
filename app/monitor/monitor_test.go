// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortSocket is a socket path short enough for macOS (104 bytes) and Linux (108).
func shortSocket(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "lm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "m.sock")
}

func TestMonitorAnswersOverItsSocket(t *testing.T) {
	h := newFakeHost(t)
	store, err := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(NewCollector(h.root), store, nil)
	clock := time.Now()
	m.Now = func() time.Time { return clock }
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	h.counters(1)
	clock = clock.Add(15 * time.Second)
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}

	socket := shortSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, socket, m.Handler())
	c := NewClient(socket)
	var latest Latest
	for range 50 {
		if latest, err = c.Latest(ctx); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if latest.Values["cpu.busy"] < 30 || len(latest.Processes) == 0 || latest.Processes[0].Count != 7 {
		t.Errorf("latest = %+v", latest)
	}
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the socket must be the owner's only: %v %v", info.Mode(), err)
	}

	series, err := c.Series(ctx, "1h", "cpu.busy", "disk.pct:/data disk")
	if err != nil {
		t.Fatal(err)
	}
	if series.Step != 15 || len(series.X) != 2 || series.Series["cpu.busy"].Avg[0] != nil || *series.Series["cpu.busy"].Avg[1] < 30 {
		t.Errorf("series = %+v", series)
	}
	if series.Series["disk.pct:/data disk"].Avg[0] == nil {
		t.Error("a label with a space should work through the query string")
	}
	if _, err := c.Series(ctx, "1y", "cpu.busy"); err == nil {
		t.Error("an unknown range should be refused")
	}

	if _, err := NewClient(filepath.Join(filepath.Dir(socket), "none.sock")).Status(ctx); !errors.Is(err, ErrNotRunning) {
		t.Errorf("no monitor: want ErrNotRunning, got %v", err)
	}
}
