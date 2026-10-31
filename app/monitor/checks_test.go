// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestChecksAndHeldValues(t *testing.T) {
	h := newFakeHost(t)
	h.write("run/reboot-required", "*** System restart required ***\n")
	h.write("run/reboot-required.pkgs", "linux-image-6.17.0-8-generic\nlibc6\nlinux-image-6.17.0-8-generic\n")
	h.write("var/log/syslog", strings.Repeat("x", 64<<10))
	store, err := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(NewCollector(h.root), store, nil)
	clock := time.Now()
	m.Now = func() time.Time { return clock }
	m.Systemctl = func(_ context.Context, args ...string) ([]byte, error) {
		if !slices.Contains(args, "--state=failed") {
			t.Errorf("systemctl %v", args)
		}
		return []byte("queue-worker.service loaded failed failed Laravel queue worker\n● php8.3-fpm.service loaded failed failed PHP 8.3 FastCGI\n"), nil
	}
	m.runChecks(context.Background())
	m.measureDirs()
	if err := m.Push(map[string]float64{"db.size:shop": 5e9, "disk.use:pg_wal": 1e9}); err != nil {
		t.Fatal(err)
	}
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Latest()
	if s.Values["units.failed"] != 2 || s.Values["reboot.required"] != 1 || s.Values["db.size:shop"] != 5e9 || s.Values["disk.use:/var/log"] < 64<<10 {
		t.Errorf("values = %v", s.Values)
	}
	if !slices.Equal(m.checks.FailedUnits, []string{"queue-worker.service", "php8.3-fpm.service"}) ||
		!slices.Equal(m.checks.RebootPackages, []string{"libc6", "linux-image-6.17.0-8-generic"}) {
		t.Errorf("checks = %+v", m.checks)
	}

	// Pushed values stop counting once they're older than PushKeep.
	clock = clock.Add(PushKeep + time.Minute)
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.Latest(); s.Values["db.size:shop"] != 0 || s.Values["cpus"] == 0 {
		t.Errorf("after PushKeep: %v", s.Values)
	}

	// A failing systemctl is reported, not counted as zero failed units.
	m.Systemctl = func(context.Context, ...string) ([]byte, error) {
		return []byte("Failed to connect to bus"), errors.New("exit status 1")
	}
	m.runChecks(context.Background())
	if m.checks.FailedError == "" || m.held["units.failed"].value != 0 {
		t.Errorf("checks = %+v", m.checks)
	}
	if _, ok := m.held["units.failed"]; ok {
		t.Error("units.failed shouldn't be recorded when systemctl can't be asked")
	}
}

func TestPushIsLimited(t *testing.T) {
	m := New(NewCollector(t.TempDir()), nil, nil)
	for _, bad := range []map[string]float64{{"cpu.busy": 1}, {"db.size:": 1}, {"svc.mem:x": 1}} {
		if err := m.Push(bad); err == nil {
			t.Errorf("Push(%v) should be refused", bad)
		}
	}
	if err := m.Push(map[string]float64{"db.size:app": 1, "disk.use:binlogs": 2}); err != nil {
		t.Error(err)
	}
}

func TestPushOverTheSocket(t *testing.T) {
	m := New(NewCollector(t.TempDir()), nil, nil)
	socket := shortSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, socket, m.Handler())
	c := NewClient(socket)
	var err error
	for range 50 {
		if err = c.Push(ctx, map[string]float64{"db.size:app": 7}); !errors.Is(err, ErrNotRunning) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || m.held["db.size:app"].value != 7 {
		t.Errorf("push: %v, held %v", err, m.held)
	}
	if err := c.Push(ctx, map[string]float64{"cpu.busy": 1}); err == nil || !strings.Contains(err.Error(), "only db.size") {
		t.Errorf("a disallowed name: %v", err)
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "f"), make([]byte, 100<<10), 0o644)
	os.Symlink("/", filepath.Join(dir, "link")) // not followed
	size, err := dirSize(dir)
	if err != nil || size < 100<<10 || size > 1<<20 {
		t.Errorf("dirSize = %d, %v", size, err)
	}
}
